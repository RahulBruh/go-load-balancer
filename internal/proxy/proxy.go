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
	// Transport performs the actual upstream round trip. Defaults to
	// http.DefaultTransport.
	Transport http.RoundTripper
	Logger    *slog.Logger
}

type Proxy struct {
	backends []*backend.Backend
	strategy balancer.Strategy
	base     http.RoundTripper
	log      *slog.Logger
	rp       *httputil.ReverseProxy
}

func New(backends []*backend.Backend, strategy balancer.Strategy, opts Options) *Proxy {
	p := &Proxy{
		backends: backends,
		strategy: strategy,
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

func (p *Proxy) roundTrip(req *http.Request) (*http.Response, error) {
	b := p.strategy.Next(backend.Filter(p.backends, (*backend.Backend).Alive))
	if b == nil {
		return nil, ErrNoHealthyBackend
	}
	return p.send(req, b)
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
