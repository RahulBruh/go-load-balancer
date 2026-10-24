// Package health runs active health checks: it polls every backend's health
// endpoint on an interval and moves backends in and out of rotation using
// rise/fall thresholds, so one slow probe doesn't flap a backend.
package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
)

type Checker struct {
	Backends []*backend.Backend
	Path     string
	Interval time.Duration
	Timeout  time.Duration
	Rise     int
	Fall     int
	Client   *http.Client
	Logger   *slog.Logger
	// OnChange, if set, is called after a backend enters or leaves rotation.
	OnChange func(b *backend.Backend, alive bool)
}

// Run checks every backend immediately and then once per Interval until ctx
// is cancelled.
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		c.CheckAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckAll probes every backend concurrently and waits for all probes.
func (c *Checker) CheckAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range c.Backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.check(ctx, b)
		}()
	}
	wg.Wait()
}

func (c *Checker) check(ctx context.Context, b *backend.Backend) {
	err := c.probe(ctx, b)
	if ctx.Err() != nil {
		return // shutting down; don't count the cancelled probe
	}
	if err == nil {
		if b.RecordSuccess(c.Rise) {
			c.logger().Info("backend up", "backend", b.Name())
			c.changed(b, true)
		}
		return
	}
	if b.RecordFailure(c.Fall) {
		c.logger().Warn("backend down", "backend", b.Name(), "reason", "active check", "err", err)
		c.changed(b, false)
	}
}

// probe passes when the health endpoint answers 2xx within Timeout.
func (c *Checker) probe(ctx context.Context, b *backend.Backend) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.JoinPath(c.Path).String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Code: resp.StatusCode}
	}
	return nil
}

func (c *Checker) changed(b *backend.Backend, alive bool) {
	if c.OnChange != nil {
		c.OnChange(b, alive)
	}
}

func (c *Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c *Checker) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// StatusError is a health response outside 2xx.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return "health endpoint returned " + http.StatusText(e.Code) }
