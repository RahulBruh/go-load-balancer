package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
)

// flakyServer serves /health with 200 or 503 depending on healthy.
func flakyServer(t *testing.T, healthy *atomic.Bool) *backend.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probed %s, want /health", r.URL.Path)
		}
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return backend.New(u, 1)
}

func newChecker(bs ...*backend.Backend) *Checker {
	return &Checker{Backends: bs, Path: "/health", Interval: 10 * time.Millisecond, Timeout: 200 * time.Millisecond, Rise: 2, Fall: 3}
}

func TestFallThenRise(t *testing.T) {
	var healthy atomic.Bool
	b := flakyServer(t, &healthy)
	c := newChecker(b)
	var changes []bool
	c.OnChange = func(_ *backend.Backend, alive bool) { changes = append(changes, alive) }
	ctx := context.Background()

	// Two failures are below fall=3: still in rotation.
	for range 2 {
		c.CheckAll(ctx)
	}
	if !b.Alive() {
		t.Fatal("backend removed before reaching fall threshold")
	}
	c.CheckAll(ctx)
	if b.Alive() {
		t.Fatal("backend still alive after 3 failed checks")
	}

	healthy.Store(true)
	c.CheckAll(ctx)
	if b.Alive() {
		t.Fatal("backend restored after 1 passing check, want rise=2")
	}
	c.CheckAll(ctx)
	if !b.Alive() {
		t.Fatal("backend not restored after 2 passing checks")
	}
	if len(changes) != 2 || changes[0] || !changes[1] {
		t.Errorf("OnChange calls = %v, want [false true]", changes)
	}
}

func TestAFailureResetsRise(t *testing.T) {
	var healthy atomic.Bool
	b := flakyServer(t, &healthy)
	b.RecordFailure(1)
	c := newChecker(b)
	ctx := context.Background()

	healthy.Store(true)
	c.CheckAll(ctx)
	healthy.Store(false)
	c.CheckAll(ctx)
	healthy.Store(true)
	c.CheckAll(ctx)
	if b.Alive() {
		t.Fatal("non-consecutive passes brought the backend back")
	}
}

func TestUnreachableAndSlowBackendsFail(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	slowURL, _ := url.Parse(slow.URL)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()

	bs := []*backend.Backend{backend.New(slowURL, 1), backend.New(deadURL, 1)}
	c := newChecker(bs...)
	c.Timeout = 50 * time.Millisecond
	c.Fall = 1
	c.CheckAll(context.Background())
	for _, b := range bs {
		if b.Alive() {
			t.Errorf("%s still alive", b.Name())
		}
	}
}

func TestRunDetectsRecovery(t *testing.T) {
	var healthy atomic.Bool
	b := flakyServer(t, &healthy)
	b.RecordFailure(1)
	healthy.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { newChecker(b).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(2 * time.Second)
	for !b.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("Run never restored the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
