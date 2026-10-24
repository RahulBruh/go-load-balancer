// Command loadgen drives closed-loop HTTP load at a URL: c workers each send
// a request, wait for the answer, and send the next, for a fixed duration.
// It reports throughput, latency percentiles and outcomes, as text or JSON.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

type result struct {
	URL         string         `json:"url"`
	Concurrency int            `json:"concurrency"`
	Seconds     float64        `json:"seconds"`
	Requests    int            `json:"requests"`
	RPS         float64        `json:"rps"`
	P50ms       float64        `json:"p50_ms"`
	P90ms       float64        `json:"p90_ms"`
	P99ms       float64        `json:"p99_ms"`
	MaxMs       float64        `json:"max_ms"`
	Codes       map[string]int `json:"codes"`
	// Failures are transport errors plus 5xx responses: what a client would
	// count as the service being broken.
	Failures int `json:"failures"`
	// Timeline is failures per second of the run, when -timeline is set.
	Timeline []int `json:"timeline,omitempty"`
}

func main() {
	url := flag.String("url", "http://localhost:8080/", "target URL")
	conc := flag.Int("c", 50, "concurrent workers")
	dur := flag.Duration("d", 10*time.Second, "test duration")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout")
	timeline := flag.Bool("timeline", false, "record failures per second")
	asJSON := flag.Bool("json", false, "print JSON instead of text")
	flag.Parse()

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *conc,
			MaxIdleConnsPerHost: *conc,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()

	type sample struct {
		at      time.Duration
		latency time.Duration
		code    int // 0 = transport error
	}
	start := time.Now()
	samples := make([][]sample, *conc)
	var wg sync.WaitGroup
	for w := range *conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				t0 := time.Now()
				code := 0
				resp, err := client.Get(*url)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					code = resp.StatusCode
				} else if ctx.Err() != nil {
					return // cut off by the end of the run, not a failure
				}
				samples[w] = append(samples[w], sample{t0.Sub(start), time.Since(t0), code})
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	res := result{URL: *url, Concurrency: *conc, Seconds: elapsed.Seconds(), Codes: map[string]int{}}
	var lat []time.Duration
	if *timeline {
		res.Timeline = make([]int, int(elapsed.Seconds())+1)
	}
	for _, ws := range samples {
		for _, s := range ws {
			lat = append(lat, s.latency)
			key := "error"
			if s.code != 0 {
				key = strconv.Itoa(s.code)
			}
			res.Codes[key]++
			if s.code == 0 || s.code >= 500 {
				res.Failures++
				if *timeline {
					res.Timeline[int(s.at.Seconds())]++
				}
			}
		}
	}
	res.Requests = len(lat)
	res.RPS = float64(res.Requests) / elapsed.Seconds()
	slices.Sort(lat)
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(p * float64(len(lat)-1))
		return float64(lat[i].Microseconds()) / 1000
	}
	res.P50ms, res.P90ms, res.P99ms, res.MaxMs = pct(0.50), pct(0.90), pct(0.99), pct(1)

	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	fmt.Printf("%s  c=%d  %.1fs\n", res.URL, res.Concurrency, res.Seconds)
	fmt.Printf("requests %d  (%.0f req/s)\n", res.Requests, res.RPS)
	fmt.Printf("latency  p50 %.2fms  p90 %.2fms  p99 %.2fms  max %.2fms\n", res.P50ms, res.P90ms, res.P99ms, res.MaxMs)
	fmt.Printf("codes    %v\nfailures %d\n", res.Codes, res.Failures)
	if *timeline {
		fmt.Printf("failures per second %v\n", res.Timeline)
	}
}
