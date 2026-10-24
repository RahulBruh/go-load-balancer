// Package backend holds the state the load balancer tracks for each upstream
// server: whether it is in rotation, how many requests it is serving, and the
// consecutive success/failure counts that drive health transitions.
package backend

import (
	"net/url"
	"sync"
	"sync/atomic"
)

// Backend is one upstream server. All methods are safe for concurrent use.
type Backend struct {
	URL    *url.URL
	Weight int

	alive  atomic.Bool
	active atomic.Int64

	mu         sync.Mutex
	consecOK   int
	consecFail int
}

// New returns a backend that starts in rotation. Weights below 1 become 1.
func New(u *url.URL, weight int) *Backend {
	if weight < 1 {
		weight = 1
	}
	b := &Backend{URL: u, Weight: weight}
	b.alive.Store(true)
	return b
}

// Name is the backend's host:port, used in logs, metrics and headers.
func (b *Backend) Name() string { return b.URL.Host }

// Alive reports whether the backend is currently in rotation.
func (b *Backend) Alive() bool { return b.alive.Load() }

// Active is the number of requests currently in flight to this backend.
func (b *Backend) Active() int64 { return b.active.Load() }

// Acquire marks the start of a request; the returned func marks its end.
func (b *Backend) Acquire() (release func()) {
	b.active.Add(1)
	var once sync.Once
	return func() { once.Do(func() { b.active.Add(-1) }) }
}

// RecordSuccess counts a passing check. After rise consecutive successes a
// down backend is put back in rotation. It reports whether the state changed.
func (b *Backend) RecordSuccess(rise int) (changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecFail = 0
	b.consecOK++
	if !b.alive.Load() && b.consecOK >= rise {
		b.alive.Store(true)
		return true
	}
	return false
}

// RecordFailure counts a failed check or request. After fall consecutive
// failures an alive backend is taken out of rotation. It reports whether the
// state changed.
func (b *Backend) RecordFailure(fall int) (changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecOK = 0
	b.consecFail++
	if b.alive.Load() && b.consecFail >= fall {
		b.alive.Store(false)
		return true
	}
	return false
}

// ResetFailures clears the failure streak after a successful proxied request.
// Unlike RecordSuccess it never brings a backend back: only the active checker
// does that, so requests still finishing on a just-removed backend can't
// restore it.
func (b *Backend) ResetFailures() {
	b.mu.Lock()
	b.consecFail = 0
	b.mu.Unlock()
}

// Filter returns the backends for which keep returns true.
func Filter(bs []*Backend, keep func(*Backend) bool) []*Backend {
	out := make([]*Backend, 0, len(bs))
	for _, b := range bs {
		if keep(b) {
			out = append(out, b)
		}
	}
	return out
}
