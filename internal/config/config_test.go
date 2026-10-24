package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseFillsDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
backends:
  - url: http://localhost:9001
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.Strategy != RoundRobin {
		t.Errorf("defaults not applied: listen=%q strategy=%q", cfg.Listen, cfg.Strategy)
	}
	if cfg.HealthCheck.Interval != 2*time.Second {
		t.Errorf("interval = %v, want 2s", cfg.HealthCheck.Interval)
	}
}

func TestParseOverridesAndDurations(t *testing.T) {
	cfg, err := Parse([]byte(`
listen: ":7000"
strategy: least_connections
backends:
  - url: http://a:1
    weight: 5
health_check:
  interval: 500ms
  fall: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":7000" || cfg.Strategy != LeastConnections {
		t.Errorf("got listen=%q strategy=%q", cfg.Listen, cfg.Strategy)
	}
	if cfg.Backends[0].Weight != 5 {
		t.Errorf("weight = %d, want 5", cfg.Backends[0].Weight)
	}
	if cfg.HealthCheck.Interval != 500*time.Millisecond || cfg.HealthCheck.Fall != 1 {
		t.Errorf("health_check = %+v", cfg.HealthCheck)
	}
	// Fields not mentioned keep their defaults.
	if cfg.HealthCheck.Rise != 2 {
		t.Errorf("rise = %d, want default 2", cfg.HealthCheck.Rise)
	}
}

func TestParseRejectsBadConfigs(t *testing.T) {
	cases := map[string]struct{ yaml, wantErr string }{
		"no backends":     {`listen: ":1"`, "at least one backend"},
		"bad strategy":    {"strategy: random\nbackends: [{url: 'http://a:1'}]", `unknown strategy "random"`},
		"bad url":         {"backends: [{url: 'localhost:9001'}]", "must be http(s)"},
		"duplicate":       {"backends: [{url: 'http://a:1'}, {url: 'http://a:1'}]", "duplicate backend"},
		"negative weight": {"backends: [{url: 'http://a:1', weight: -1}]", "weight must be >= 0"},
		"unknown key":     {"backend: [{url: 'http://a:1'}]", "field backend not found"},
		"zero rise":       {"backends: [{url: 'http://a:1'}]\nhealth_check: {rise: 0}", "rise and fall"},
		"negative retry":  {"backends: [{url: 'http://a:1'}]\nretries: -1", "retries must be"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}
