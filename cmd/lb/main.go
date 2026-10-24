// Command lb runs the load balancer described by a YAML config file.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RahulBruh/go-load-balancer/internal/backend"
	"github.com/RahulBruh/go-load-balancer/internal/balancer"
	"github.com/RahulBruh/go-load-balancer/internal/config"
	"github.com/RahulBruh/go-load-balancer/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML config")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, spec := range cfg.Backends {
		u, _ := url.Parse(spec.URL) // validated by config.Load
		backends = append(backends, backend.New(u, spec.Weight))
	}
	strategy, err := balancer.New(cfg.Strategy)
	if err != nil {
		return err
	}

	p := proxy.New(backends, strategy, proxy.Options{
		Transport: newTransport(cfg.Timeouts),
		Logger:    log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("load balancer listening", "addr", cfg.Listen, "strategy", cfg.Strategy, "backends", len(backends))
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down, draining in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newTransport(t config.Timeouts) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: t.Dial, KeepAlive: 30 * time.Second}).DialContext
	tr.ResponseHeaderTimeout = t.ResponseHeader
	tr.MaxIdleConnsPerHost = 256
	return tr
}
