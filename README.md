# go-load-balancer

[![ci](https://github.com/RahulBruh/go-load-balancer/actions/workflows/ci.yml/badge.svg)](https://github.com/RahulBruh/go-load-balancer/actions/workflows/ci.yml)

A layer-7 HTTP load balancer and reverse proxy in Go. It sits in front of a pool of backend servers, spreads requests across them, and takes unhealthy servers out of rotation.

- **Three strategies:** round-robin, smooth weighted round-robin (the nginx algorithm), and weighted least-connections
- **Active health checks:** rise/fall thresholds, so one slow probe doesn't flap a backend
- **Passive health checks:** real request failures take a backend out within milliseconds
- **Retries:** idempotent requests go to a *different* healthy backend
- **Metrics:** Prometheus `/metrics` and a JSON `/admin/status` on a separate admin port
- **Graceful shutdown:** in-flight requests drain before exit
- **Small footprint:** standard library only, plus `yaml.v3`

## Results

From one run on a 12-thread laptop under WSL2, with load generator, balancer and backends sharing the CPU. Full method and raw data are in [`bench/RESULTS.md`](bench/RESULTS.md).

| Scenario | Result |
|---|---|
| Kill a backend mid-run: active checks only | 4,327 failed requests; 2,979 ms to detect |
| … + passive checks | 8 failed requests; **20 ms** to detect |
| … + passive checks + retries | **0 failed requests** |
| One 50 ms backend among two 1 ms ones | least-connections: **2.7× the throughput** of round-robin (7,182 vs 2,621 req/s); p90 from 51.7 ms to 8.5 ms |
| Proxy throughput (1 ms backends, 50 clients) | 8,562 req/s through the balancer vs 23,177 req/s direct to one backend |

Tests: 44 test cases, 92.7% statement coverage of `internal/`. CI runs them with the race detector, plus a Docker Compose smoke test that stops a backend and asserts clients see zero errors.

## Quick start

```bash
docker compose up --build
curl localhost:8080/                 # hello from backend1 / backend2 / backend3 in turn
curl localhost:9090/admin/status     # backend state as JSON
docker compose stop backend2         # drops out of rotation; clients see no errors
curl -s localhost:9090/metrics | grep lb_backend_up
```

Without Docker:

```bash
go run ./cmd/demo-backend -addr :9001 -name b1 &
go run ./cmd/demo-backend -addr :9002 -name b2 &
go run ./cmd/demo-backend -addr :9003 -name b3 &
go run ./cmd/lb -config config.example.yaml
```

To take a backend out of rotation without killing it, run `curl -X POST localhost:9002/toggle-health`. That flips its `/health` endpoint.

## How it works

```
                      ┌──────────────── lb ─────────────────────────────┐
 client ──HTTP──▶ :8080 metrics middleware                              │
                      │   └▶ httputil.ReverseProxy                     │
                      │        (X-Forwarded-*, hop-by-hop headers,      │
                      │         streaming, trailers)                    │
                      │          └▶ balancing RoundTripper ─────────────┼──▶ backend 1
                      │               1. candidates = alive ∖ tried     │──▶ backend 2
                      │               2. strategy.Next(candidates)      │──▶ backend 3
                      │               3. send; on conn error / 502-504: │       ▲
                      │                  count passive failure, retry   │       │
                      │                  if idempotent and no body      │       │
                      │                                                 │       │
                      │   health.Checker: GET /health every interval ───┼───────┘
                      │     out after `fall` failures, back after `rise`│
 Prometheus ───▶ :9090 /metrics  /admin/status  /healthz               │
                      └─────────────────────────────────────────────────┘
```

**Backend selection happens in the transport, not in `Rewrite`.** `httputil.ReverseProxy` still handles headers, streaming, flushing and upgrades. The custom `RoundTripper` owns which backend gets each attempt, so a retry is just another loop iteration with the failed backend excluded.

**In-flight counts last until the response body closes**, not just until the headers arrive. That way least-connections sees requests that are still streaming.

### Strategies

| `strategy` | Picks | Notes |
|---|---|---|
| `round_robin` | each backend in turn | atomic counter, lock-free |
| `weighted_round_robin` | by `weight`, smoothly | nginx's smooth WRR: 5:1:1 gives `a a b a c a a`, not `a a a a a b c`. State resets when the candidate set changes; otherwise debt from the old pool sends a burst to one backend. |
| `least_connections` | fewest in-flight requests per unit of weight | compares by cross-multiplying instead of dividing; ties rotate, so an idle pool still spreads evenly |

### Health

**Active checks.** Each interval, every backend's health path gets a `GET`, probed concurrently with a timeout per probe. A 2xx passes; anything else, or a timeout, fails.
- A backend leaves rotation after `fall` consecutive failures and returns after `rise` consecutive passes.
- On startup, `lb` runs one round of probes before it starts listening, so a backend that is already down never gets traffic.

**Passive checks.** Connection errors and 502/503/504 responses on real traffic count toward `passive.max_fails`; reaching it takes the backend out at once.
- A success resets the streak.
- **Only the active checker can bring a backend back.** Otherwise requests still finishing on a just-removed backend could restore it.

**Retries.** These apply only to idempotent methods with no body: GET, HEAD, OPTIONS, TRACE, PUT, DELETE.
- Each retry goes to a backend that hasn't been tried for that request.
- A 500 is passed through, not retried: another backend would most likely repeat an application error.
- When every attempt fails, the client gets the last backend's response.
- Requests the client cancelled don't count against a backend.

| Situation | Client gets |
|---|---|
| no backend in rotation | `503` |
| every attempt got a connection error | `502` |
| every attempt got a 502/503/504 | the last of those responses |

Every proxied response carries an `X-Upstream: host:port` header naming the backend that answered.

### Metrics

| Series | Type |
|---|---|
| `lb_http_requests_total{code}` | counter: responses sent to clients |
| `lb_http_request_duration_seconds` | histogram: client-facing latency, retries included |
| `lb_retries_total` | counter |
| `lb_upstream_responses_total{backend,code}` | counter |
| `lb_upstream_connection_errors_total{backend}` | counter |
| `lb_backend_up{backend}` | gauge |
| `lb_backend_active_requests{backend}` | gauge |
| `lb_backend_state_changes_total{backend,to}` | counter: `to="down"` / `to="up"` |

These are hand-rolled on atomics instead of `client_golang`: the series set is small and fixed, and recording takes no lock.

## Configuration

Every field except `backends` is optional; defaults are shown. Unknown keys are rejected, so a typo doesn't silently fall back to a default.

```yaml
listen: ":8080"
admin_listen: ":9090"       # /metrics, /admin/status, /healthz; "" disables
strategy: round_robin       # round_robin | weighted_round_robin | least_connections

backends:
  - url: http://10.0.0.1:8080
    weight: 3               # default 1
  - url: http://10.0.0.2:8080

health_check:
  path: /health
  interval: 2s
  timeout: 1s
  rise: 2                   # consecutive passes to rejoin
  fall: 3                   # consecutive failures to leave

passive:
  max_fails: 3              # 0 disables passive checks

retries: 2                  # extra backends to try for idempotent requests

timeouts:
  dial: 2s
  response_header: 10s
```

## Layout

| Path | Contents |
|---|---|
| `cmd/lb` | the balancer: config, wiring, graceful shutdown |
| `cmd/demo-backend` | test upstream: `-delay`, `-fail-rate`, `POST /toggle-health` |
| `cmd/loadgen` | closed-loop load generator with JSON output |
| `internal/proxy` | ReverseProxy + balancing RoundTripper, retries, passive checks |
| `internal/balancer` | the three strategies |
| `internal/health` | active checker |
| `internal/backend` | per-backend state: alive flag, in-flight count, failure streaks |
| `internal/metrics` | Prometheus exposition, status JSON, middleware |
| `internal/config` | YAML loading and validation |
| `bench/` | `run.sh` and results |

## Development

```bash
go test -race ./...
bench/run.sh        # Linux; about 2 minutes
```

## Not implemented

TLS termination (put it in front, or use `https://` backends), sticky sessions, rate limiting, hot config reload, and service discovery.

## License

MIT
