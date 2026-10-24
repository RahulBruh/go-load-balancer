// Package balancer contains the strategies that choose which backend serves
// the next request.
package balancer

import (
	"fmt"
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
