// Package proxy is the request path: an httputil.ReverseProxy whose transport
// picks a backend for every outbound request.
//
// Doing the selection inside the RoundTripper (rather than in Rewrite) means
// ReverseProxy still handles hop-by-hop headers, X-Forwarded-*, streaming and
// trailers, while this package owns which backend gets the request.
package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
	"github.com/RahulBruh/go-load-balancer/internal/balancer"
)

// ErrNoHealthyBackend is returned when every backend is out of rotation.
var ErrNoHealthyBackend = errors.New("no healthy backend")

// UpstreamHeader names the response header that says which backend answered.
const UpstreamHeader = "X-Upstream"

type Options struct {
	// Retries is how many other backends an idempotent request may be retried
	// on after a connection error or a 502/503/504.
	Retries int
	// PassiveMaxFails is how many consecutive failed requests take a backend
	// out of rotation. 0 disables passive health checks.
	PassiveMaxFails int
	// Transport performs the actual upstream round trip. Defaults to
	// http.DefaultTransport.
	Transport http.RoundTripper
	Logger    *slog.Logger
}

type Proxy struct {
	backends []*backend.Backend
	strategy balancer.Strategy
	opts     Options
	base     http.RoundTripper
	log      *slog.Logger
	rp       *httputil.ReverseProxy
}

func New(backends []*backend.Backend, strategy balancer.Strategy, opts Options) *Proxy {
	p := &Proxy{
		backends: backends,
		strategy: strategy,
		opts:     opts,
		base:     opts.Transport,
		log:      opts.Logger,
	}
	if p.base == nil {
		p.base = http.DefaultTransport
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
			// The real scheme and host are filled in per attempt by RoundTrip.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "upstream"
		},
		Transport:    roundTripper{p},
		ErrorHandler: p.handleError,
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.rp.ServeHTTP(w, r)
}

type roundTripper struct{ p *Proxy }

func (rt roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt.p.roundTrip(req)
}

// roundTrip sends the request to a backend chosen by the strategy. A request
// that is safe to replay is retried on a different backend when the attempt
// fails at the connection level or the backend answers 502, 503 or 504, up to
// Options.Retries times. Every failure also counts toward that backend's
// passive health threshold.
func (p *Proxy) roundTrip(req *http.Request) (*http.Response, error) {
	attempts := 1
	if canRetry(req) {
		attempts += p.opts.Retries
	}
	tried := make(map[*backend.Backend]bool, attempts)
	var (
		lastResp *http.Response
		lastErr  error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		b := p.strategy.Next(backend.Filter(p.backends, func(b *backend.Backend) bool {
			return b.Alive() && !tried[b]
		}))
		if b == nil {
			break
		}
		tried[b] = true
		if lastResp != nil {
			discard(lastResp)
			lastResp = nil
		}
		if attempt > 0 {
			p.log.Debug("retrying on another backend", "backend", b.Name(), "attempt", attempt+1)
		}

		resp, err := p.send(req, b)
		if req.Context().Err() != nil {
			// The client gave up; that says nothing about the backend.
			return resp, err
		}
		if err == nil && !failedStatus(resp.StatusCode) {
			if p.opts.PassiveMaxFails > 0 {
				b.ResetFailures()
			}
			return resp, nil
		}
		p.passiveFailure(b, resp, err)
		lastResp, lastErr = resp, err
	}
	if lastResp != nil {
		return lastResp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoHealthyBackend
}

func (p *Proxy) passiveFailure(b *backend.Backend, resp *http.Response, err error) {
	if p.opts.PassiveMaxFails == 0 {
		return
	}
	if b.RecordFailure(p.opts.PassiveMaxFails) {
		attrs := []any{"backend", b.Name(), "reason", "passive"}
		if err != nil {
			attrs = append(attrs, "err", err)
		} else {
			attrs = append(attrs, "status", resp.StatusCode)
		}
		p.log.Warn("backend down", attrs...)
	}
}

// canRetry reports whether replaying req on another backend is safe: the
// method must be idempotent and there must be no body to re-send.
func canRetry(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
	default:
		return false
	}
	return req.Body == nil || req.Body == http.NoBody
}

// failedStatus is true for statuses that mean the backend (or something in
// front of it) could not handle the request, as opposed to an application
// error like 500 that another backend would most likely repeat.
func failedStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func discard(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// send forwards one attempt to b. The backend's in-flight count stays raised
// until the response body is closed, so least-connections sees requests that
// are still streaming.
func (p *Proxy) send(req *http.Request, b *backend.Backend) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = b.URL.Scheme
	out.URL.Host = b.URL.Host

	release := b.Acquire()
	resp, err := p.base.RoundTrip(out)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releaseOnClose{ReadCloser: resp.Body, release: release}
	resp.Header.Set(UpstreamHeader, b.Name())
	return resp, nil
}

func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNoHealthyBackend):
		w.WriteHeader(http.StatusServiceUnavailable)
	case r.Context().Err() != nil:
		// The client went away; nobody is reading the status.
		return
	default:
		p.log.Warn("upstream request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		w.WriteHeader(http.StatusBadGateway)
	}
}

type releaseOnClose struct {
	io.ReadCloser
	release func()
}

func (r *releaseOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}
