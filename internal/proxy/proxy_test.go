package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
	"github.com/RahulBruh/go-load-balancer/internal/balancer"
)

// startBackends starts n upstream servers that answer with their index.
func startBackends(t *testing.T, n int) ([]*backend.Backend, []*httptest.Server) {
	t.Helper()
	var bs []*backend.Backend
	var srvs []*httptest.Server
	for i := range n {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Seen-XFF", r.Header.Get("X-Forwarded-For"))
			fmt.Fprintf(w, "backend-%d", i)
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		bs = append(bs, backend.New(u, 1))
		srvs = append(srvs, srv)
	}
	return bs, srvs
}

func get(t *testing.T, h http.Handler, method string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/hello", nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestRoundRobinSpreadsEvenly(t *testing.T) {
	bs, _ := startBackends(t, 3)
	p := New(bs, &balancer.RoundRobin{}, Options{})

	counts := map[string]int{}
	for range 30 {
		resp, body := get(t, p, http.MethodGet)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		counts[body]++
	}
	for i := range 3 {
		if got := counts[fmt.Sprintf("backend-%d", i)]; got != 10 {
			t.Errorf("backend-%d served %d requests, want 10 (counts %v)", i, got, counts)
		}
	}
}

func TestSetsForwardingAndUpstreamHeaders(t *testing.T) {
	bs, _ := startBackends(t, 1)
	p := New(bs, &balancer.RoundRobin{}, Options{})

	resp, _ := get(t, p, http.MethodGet)
	if got := resp.Header.Get("X-Seen-XFF"); got != "192.0.2.1" {
		t.Errorf("backend saw X-Forwarded-For %q, want the client address", got)
	}
	if got := resp.Header.Get(UpstreamHeader); got != bs[0].Name() {
		t.Errorf("%s = %q, want %q", UpstreamHeader, got, bs[0].Name())
	}
}

func TestReleasesActiveCountAfterBodyClosed(t *testing.T) {
	bs, _ := startBackends(t, 1)
	p := New(bs, &balancer.RoundRobin{}, Options{})
	get(t, p, http.MethodGet)
	if got := bs[0].Active(); got != 0 {
		t.Errorf("active = %d after request finished, want 0", got)
	}
}

func TestNoHealthyBackendReturns503(t *testing.T) {
	bs, _ := startBackends(t, 2)
	for _, b := range bs {
		b.RecordFailure(1)
	}
	p := New(bs, &balancer.RoundRobin{}, Options{})
	if resp, _ := get(t, p, http.MethodGet); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

func TestSkipsBackendsOutOfRotation(t *testing.T) {
	bs, _ := startBackends(t, 3)
	bs[1].RecordFailure(1)
	p := New(bs, &balancer.RoundRobin{}, Options{})
	for range 20 {
		if _, body := get(t, p, http.MethodGet); body == "backend-1" {
			t.Fatal("request routed to a backend that is out of rotation")
		}
	}
}

func TestDeadBackendReturns502(t *testing.T) {
	bs, srvs := startBackends(t, 1)
	srvs[0].Close()
	p := New(bs, &balancer.RoundRobin{}, Options{})
	if resp, _ := get(t, p, http.MethodGet); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
}

// statusBackend answers every request with code and counts the hits.
func statusBackend(t *testing.T, code int, hits *atomic.Int64) *backend.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return backend.New(u, 1)
}

func TestRetriesConnectionErrorOnAnotherBackend(t *testing.T) {
	bs, srvs := startBackends(t, 3)
	srvs[0].Close()
	p := New(bs, &balancer.RoundRobin{}, Options{Retries: 2})
	for i := range 30 {
		if resp, _ := get(t, p, http.MethodGet); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d with a healthy backend available", i, resp.StatusCode)
		}
	}
}

func TestRetriesGatewayStatusesButNot500(t *testing.T) {
	var badHits, okHits, appErrHits atomic.Int64
	bad := statusBackend(t, http.StatusServiceUnavailable, &badHits)
	ok := statusBackend(t, http.StatusOK, &okHits)
	p := New([]*backend.Backend{bad, ok}, &balancer.RoundRobin{}, Options{Retries: 1})
	for range 10 {
		if resp, _ := get(t, p, http.MethodGet); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want the 503 retried onto the healthy backend", resp.StatusCode)
		}
	}

	appErr := statusBackend(t, http.StatusInternalServerError, &appErrHits)
	p = New([]*backend.Backend{appErr, ok}, &balancer.RoundRobin{}, Options{Retries: 1})
	if resp, _ := get(t, p, http.MethodGet); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want the 500 passed through unretried", resp.StatusCode)
	}
}

func TestDoesNotRetryNonIdempotentRequests(t *testing.T) {
	var badHits, okHits atomic.Int64
	bad := statusBackend(t, http.StatusBadGateway, &badHits)
	ok := statusBackend(t, http.StatusOK, &okHits)
	p := New([]*backend.Backend{bad, ok}, &balancer.RoundRobin{}, Options{Retries: 3})
	if resp, _ := get(t, p, http.MethodPost); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("POST status %d, want 502 passed through", resp.StatusCode)
	}
	if okHits.Load() != 0 {
		t.Error("POST was replayed on a second backend")
	}
}

func TestReturnsLastFailureWhenAllAttemptsFail(t *testing.T) {
	var a, b atomic.Int64
	bs := []*backend.Backend{statusBackend(t, http.StatusBadGateway, &a), statusBackend(t, http.StatusGatewayTimeout, &b)}
	p := New(bs, &balancer.RoundRobin{}, Options{Retries: 5})
	resp, _ := get(t, p, http.MethodGet)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status %d, want the second backend's 504", resp.StatusCode)
	}
	if a.Load()+b.Load() != 2 {
		t.Errorf("%d attempts, want each backend tried once", a.Load()+b.Load())
	}
}

func TestPassiveFailuresTakeBackendOut(t *testing.T) {
	bs, srvs := startBackends(t, 2)
	srvs[0].Close()
	p := New(bs, &balancer.RoundRobin{}, Options{Retries: 1, PassiveMaxFails: 3})
	for range 10 {
		get(t, p, http.MethodGet)
	}
	if bs[0].Alive() {
		t.Error("dead backend still in rotation after repeated failures")
	}
	if !bs[1].Alive() {
		t.Error("healthy backend taken out")
	}
}

func TestPassiveSuccessResetsStreak(t *testing.T) {
	var hits atomic.Int64
	flip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(flip.Close)
	u, _ := url.Parse(flip.URL)
	b := backend.New(u, 1)
	p := New([]*backend.Backend{b}, &balancer.RoundRobin{}, Options{PassiveMaxFails: 2})
	for range 20 {
		get(t, p, http.MethodGet)
	}
	if !b.Alive() {
		t.Error("alternating success/failure took the backend out with max_fails=2")
	}
}
