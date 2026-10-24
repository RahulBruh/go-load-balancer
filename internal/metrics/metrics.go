// Package metrics records what the load balancer does and serves it in the
// Prometheus text exposition format, plus a JSON status view for humans.
//
// It is hand-rolled on atomics rather than pulling in client_golang: the set
// of series is small and fixed, and the hot path stays lock-free.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
)

// Buckets are the latency histogram's upper bounds, in seconds.
var Buckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type Registry struct {
	strategy string
	backends []*backend.Backend
	stats    map[*backend.Backend]*backendStats

	retries   atomic.Int64
	responses codeCounter // status codes sent to clients
	latency   histogram
}

type backendStats struct {
	codes        codeCounter // status codes received from this backend
	errors       atomic.Int64
	stateChanges [2]atomic.Int64 // [0] went down, [1] came up
}

func New(strategy string, backends []*backend.Backend) *Registry {
	r := &Registry{
		strategy: strategy,
		backends: backends,
		stats:    make(map[*backend.Backend]*backendStats, len(backends)),
		latency:  histogram{counts: make([]atomic.Int64, len(Buckets))},
	}
	for _, b := range backends {
		r.stats[b] = &backendStats{}
	}
	return r
}

// UpstreamResponse records one response received from a backend.
func (r *Registry) UpstreamResponse(b *backend.Backend, code int) {
	if s := r.stats[b]; s != nil {
		s.codes.inc(code)
	}
}

// UpstreamError records an attempt that got no response (connection refused,
// timeout, reset).
func (r *Registry) UpstreamError(b *backend.Backend) {
	if s := r.stats[b]; s != nil {
		s.errors.Add(1)
	}
}

// Retry records a request being re-sent to another backend.
func (r *Registry) Retry() { r.retries.Add(1) }

// StateChange records a backend entering or leaving rotation.
func (r *Registry) StateChange(b *backend.Backend, alive bool) {
	if s := r.stats[b]; s != nil {
		i := 0
		if alive {
			i = 1
		}
		s.stateChanges[i].Add(1)
	}
}

// Middleware wraps the proxy handler to record the status and latency of
// every client request.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, req)
		r.responses.inc(sw.code)
		r.latency.observe(time.Since(start).Seconds())
	})
}

// Handler serves /metrics, /admin/status and /healthz.
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WritePrometheus(w)
	})
	mux.HandleFunc("GET /admin/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(r.Status())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	return mux
}

type Status struct {
	Strategy string          `json:"strategy"`
	Healthy  int             `json:"healthy"`
	Total    int             `json:"total"`
	Retries  int64           `json:"retries"`
	Backends []BackendStatus `json:"backends"`
}

type BackendStatus struct {
	URL      string `json:"url"`
	Alive    bool   `json:"alive"`
	Weight   int    `json:"weight"`
	Active   int64  `json:"active_requests"`
	Requests int64  `json:"responses"`
	Errors   int64  `json:"connection_errors"`
	Downs    int64  `json:"times_marked_down"`
}

func (r *Registry) Status() Status {
	st := Status{Strategy: r.strategy, Total: len(r.backends), Retries: r.retries.Load()}
	for _, b := range r.backends {
		s := r.stats[b]
		if b.Alive() {
			st.Healthy++
		}
		st.Backends = append(st.Backends, BackendStatus{
			URL:      b.URL.String(),
			Alive:    b.Alive(),
			Weight:   b.Weight,
			Active:   b.Active(),
			Requests: s.codes.total(),
			Errors:   s.errors.Load(),
			Downs:    s.stateChanges[0].Load(),
		})
	}
	return st
}

// WritePrometheus writes every series in the text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) {
	header := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	header("lb_http_requests_total", "counter", "Requests answered to clients, by status code.")
	for _, c := range r.responses.snapshot() {
		fmt.Fprintf(w, "lb_http_requests_total{code=%q} %d\n", strconv.Itoa(c.code), c.n)
	}

	header("lb_http_request_duration_seconds", "histogram", "Time to answer client requests, including retries.")
	cum := int64(0)
	for i, le := range Buckets {
		cum += r.latency.counts[i].Load()
		fmt.Fprintf(w, "lb_http_request_duration_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(le, 'g', -1, 64), cum)
	}
	count := r.latency.count.Load()
	fmt.Fprintf(w, "lb_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", count)
	fmt.Fprintf(w, "lb_http_request_duration_seconds_sum %g\n", float64(r.latency.sumMicros.Load())/1e6)
	fmt.Fprintf(w, "lb_http_request_duration_seconds_count %d\n", count)

	header("lb_retries_total", "counter", "Requests re-sent to another backend.")
	fmt.Fprintf(w, "lb_retries_total %d\n", r.retries.Load())

	header("lb_upstream_responses_total", "counter", "Responses received from backends, by status code.")
	for _, b := range r.backends {
		for _, c := range r.stats[b].codes.snapshot() {
			fmt.Fprintf(w, "lb_upstream_responses_total{backend=%q,code=%q} %d\n", b.Name(), strconv.Itoa(c.code), c.n)
		}
	}

	header("lb_upstream_connection_errors_total", "counter", "Attempts that got no response from the backend.")
	for _, b := range r.backends {
		fmt.Fprintf(w, "lb_upstream_connection_errors_total{backend=%q} %d\n", b.Name(), r.stats[b].errors.Load())
	}

	header("lb_backend_up", "gauge", "1 if the backend is in rotation.")
	for _, b := range r.backends {
		up := 0
		if b.Alive() {
			up = 1
		}
		fmt.Fprintf(w, "lb_backend_up{backend=%q} %d\n", b.Name(), up)
	}

	header("lb_backend_active_requests", "gauge", "Requests currently in flight to the backend.")
	for _, b := range r.backends {
		fmt.Fprintf(w, "lb_backend_active_requests{backend=%q} %d\n", b.Name(), b.Active())
	}

	header("lb_backend_state_changes_total", "counter", "Times the backend left (to=down) or rejoined (to=up) rotation.")
	for _, b := range r.backends {
		s := r.stats[b]
		fmt.Fprintf(w, "lb_backend_state_changes_total{backend=%q,to=\"down\"} %d\n", b.Name(), s.stateChanges[0].Load())
		fmt.Fprintf(w, "lb_backend_state_changes_total{backend=%q,to=\"up\"} %d\n", b.Name(), s.stateChanges[1].Load())
	}
}

// codeCounter counts by HTTP status code. Codes are few, so after the first
// request with a given code the count is a lock-free atomic add.
type codeCounter struct {
	m sync.Map // int -> *atomic.Int64
}

func (c *codeCounter) inc(code int) {
	v, ok := c.m.Load(code)
	if !ok {
		v, _ = c.m.LoadOrStore(code, new(atomic.Int64))
	}
	v.(*atomic.Int64).Add(1)
}

type codeCount struct {
	code int
	n    int64
}

func (c *codeCounter) snapshot() []codeCount {
	var out []codeCount
	c.m.Range(func(k, v any) bool {
		out = append(out, codeCount{k.(int), v.(*atomic.Int64).Load()})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].code < out[j].code })
	return out
}

func (c *codeCounter) total() int64 {
	var n int64
	for _, cc := range c.snapshot() {
		n += cc.n
	}
	return n
}

type histogram struct {
	counts    []atomic.Int64 // per bucket, not cumulative
	count     atomic.Int64
	sumMicros atomic.Int64
}

func (h *histogram) observe(seconds float64) {
	h.count.Add(1)
	h.sumMicros.Add(int64(seconds * 1e6))
	i := sort.SearchFloat64s(Buckets, seconds)
	if i < len(h.counts) {
		h.counts[i].Add(1)
	}
}

type statusWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// ReverseProxy uses to flush streamed responses.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
