// Command demo-backend is a tiny upstream server for trying the load balancer
// locally. It answers every path with its own name, serves /health, and can
// add latency, fail a fraction of requests, or be flipped unhealthy at runtime.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	name := flag.String("name", "", "name to report (default: hostname + addr)")
	delay := flag.Duration("delay", 0, "latency added to every response")
	failRate := flag.Float64("fail-rate", 0, "fraction of requests answered with 500")
	flag.Parse()

	if *name == "" {
		host, _ := os.Hostname()
		*name = host + *addr
	}

	var healthy atomic.Bool
	healthy.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	// POST /toggle-health flips the health endpoint, to demo active checks
	// without killing the process.
	mux.HandleFunc("POST /toggle-health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "healthy=%v\n", !healthy.Load())
		healthy.Store(!healthy.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if *delay > 0 {
			time.Sleep(*delay)
		}
		if *failRate > 0 && rand.Float64() < *failRate {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Served-By", *name)
		fmt.Fprintf(w, "hello from %s (%s %s)\n", *name, r.Method, r.URL.Path)
	})

	log.Printf("demo backend %s listening on %s", *name, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
