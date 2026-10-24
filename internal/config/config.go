// Package config loads and validates the load balancer's YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Strategy names accepted in the config file.
const (
	RoundRobin         = "round_robin"
	WeightedRoundRobin = "weighted_round_robin"
	LeastConnections   = "least_connections"
)

type Config struct {
	Listen      string        `yaml:"listen"`
	AdminListen string        `yaml:"admin_listen"`
	Strategy    string        `yaml:"strategy"`
	Backends    []BackendSpec `yaml:"backends"`
	HealthCheck HealthCheck   `yaml:"health_check"`
	Passive     Passive       `yaml:"passive"`
	Retries     int           `yaml:"retries"`
	Timeouts    Timeouts      `yaml:"timeouts"`
}

type BackendSpec struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

type HealthCheck struct {
	Path     string        `yaml:"path"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// Rise is how many consecutive passing checks bring a backend back.
	Rise int `yaml:"rise"`
	// Fall is how many consecutive failing checks take a backend out.
	Fall int `yaml:"fall"`
}

type Passive struct {
	// MaxFails is how many consecutive failed proxied requests take a backend
	// out of rotation before the next active check. 0 disables passive checks.
	MaxFails int `yaml:"max_fails"`
}

type Timeouts struct {
	Dial           time.Duration `yaml:"dial"`
	ResponseHeader time.Duration `yaml:"response_header"`
}

// Default returns a config with every optional field filled in.
func Default() Config {
	return Config{
		Listen:      ":8080",
		AdminListen: ":9090",
		Strategy:    RoundRobin,
		HealthCheck: HealthCheck{
			Path:     "/health",
			Interval: 2 * time.Second,
			Timeout:  time.Second,
			Rise:     2,
			Fall:     3,
		},
		Passive:  Passive{MaxFails: 3},
		Retries:  2,
		Timeouts: Timeouts{Dial: 2 * time.Second, ResponseHeader: 10 * time.Second},
	}
}

// Load reads a config file, layers it over Default and validates it.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

// Parse decodes YAML over Default and validates the result. Unknown keys are
// rejected so a typo doesn't silently fall back to a default.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports every problem in the config at once.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	switch c.Strategy {
	case RoundRobin, WeightedRoundRobin, LeastConnections:
	default:
		errs = append(errs, fmt.Errorf("unknown strategy %q", c.Strategy))
	}
	if len(c.Backends) == 0 {
		errs = append(errs, errors.New("at least one backend is required"))
	}
	seen := map[string]bool{}
	for i, b := range c.Backends {
		u, err := url.Parse(b.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("backends[%d]: url %q must be http(s)://host[:port]", i, b.URL))
			continue
		}
		if seen[u.Host] {
			errs = append(errs, fmt.Errorf("backends[%d]: duplicate backend %s", i, u.Host))
		}
		seen[u.Host] = true
		if b.Weight < 0 {
			errs = append(errs, fmt.Errorf("backends[%d]: weight must be >= 0", i))
		}
	}
	hc := c.HealthCheck
	if hc.Interval <= 0 || hc.Timeout <= 0 {
		errs = append(errs, errors.New("health_check interval and timeout must be positive"))
	}
	if hc.Rise < 1 || hc.Fall < 1 {
		errs = append(errs, errors.New("health_check rise and fall must be >= 1"))
	}
	if c.Passive.MaxFails < 0 {
		errs = append(errs, errors.New("passive.max_fails must be >= 0"))
	}
	if c.Retries < 0 {
		errs = append(errs, errors.New("retries must be >= 0"))
	}
	return errors.Join(errs...)
}
