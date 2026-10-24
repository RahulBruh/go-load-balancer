// Package balancer contains the strategies that choose which backend serves
// the next request.
package balancer

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
	"github.com/RahulBruh/go-load-balancer/internal/config"
)

// Strategy picks one backend from candidates, which the caller has already
// narrowed to healthy backends not yet tried for this request. It returns nil
// only when candidates is empty.
type Strategy interface {
	Next(candidates []*backend.Backend) *backend.Backend
}

// New returns the strategy named in the config.
func New(name string) (Strategy, error) {
	switch name {
	case config.RoundRobin:
		return &RoundRobin{}, nil
	case config.WeightedRoundRobin:
		return NewWeightedRoundRobin(), nil
	case config.LeastConnections:
		return &LeastConnections{}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", name)
	}
}

// RoundRobin hands requests to each candidate in turn.
type RoundRobin struct {
	n atomic.Uint64
}

func (r *RoundRobin) Next(candidates []*backend.Backend) *backend.Backend {
	if len(candidates) == 0 {
		return nil
	}
	i := (r.n.Add(1) - 1) % uint64(len(candidates))
	return candidates[i]
}

// WeightedRoundRobin is nginx's smooth weighted round-robin: with weights
// 5:1:1 it yields a a b a c a a rather than a a a a a b c, so a heavy backend
// never gets a burst.
//
// The running weights only make sense for the set they were built on, so they
// are reset whenever the candidate set changes (a backend leaves or rejoins
// rotation, or a retry excludes one). Otherwise debt carried over from the old
// set sends a burst of requests to one backend.
type WeightedRoundRobin struct {
	mu      sync.Mutex
	current map[*backend.Backend]int
	last    []*backend.Backend
}

func NewWeightedRoundRobin() *WeightedRoundRobin {
	return &WeightedRoundRobin{current: map[*backend.Backend]int{}}
}

func (w *WeightedRoundRobin) Next(candidates []*backend.Backend) *backend.Backend {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Equal(candidates, w.last) {
		clear(w.current)
		w.last = slices.Clone(candidates)
	}
	var best *backend.Backend
	total := 0
	for _, b := range candidates {
		w.current[b] += b.Weight
		total += b.Weight
		if best == nil || w.current[b] > w.current[best] {
			best = b
		}
	}
	if best != nil {
		w.current[best] -= total
	}
	return best
}

// LeastConnections sends each request to the backend with the fewest
// in-flight requests per unit of weight. Ties are broken by rotating the
// starting point, so an idle pool still spreads evenly.
type LeastConnections struct {
	n atomic.Uint64
}

func (l *LeastConnections) Next(candidates []*backend.Backend) *backend.Backend {
	if len(candidates) == 0 {
		return nil
	}
	start := int(l.n.Add(1)-1) % len(candidates)
	best := candidates[start]
	bestActive := best.Active()
	for i := 1; i < len(candidates); i++ {
		b := candidates[(start+i)%len(candidates)]
		active := b.Active()
		// active/weight < bestActive/best.Weight without division.
		if active*int64(best.Weight) < bestActive*int64(b.Weight) {
			best, bestActive = b, active
		}
	}
	return best
}
