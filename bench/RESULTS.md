# Benchmark results

All numbers come from one run of `bench/run.sh` (raw JSON in [`results/`](results/)).

**Machine** ([`results/env.txt`](results/env.txt)):
- Intel i7-9750H, 6 cores / 12 threads, WSL2 (kernel 6.6.87.2)
- Go 1.27.3
- The load generator, the balancer and every backend share that one CPU. Treat the absolute numbers as a property of that laptop; the comparisons are the point.

**Method:**
- `cmd/loadgen` runs a closed loop: N workers each send a request, wait for the answer, then send the next.
- 50 workers for 10 s, unless a scenario says otherwise.
- A failure is a transport error or a 5xx.
- Demo backends add the stated delay to every response.

## 1. Proxy cost

Backends add 1 ms.

| Target | req/s | p50 | p90 | p99 | failures |
|---|---:|---:|---:|---:|---:|
| One backend, direct | 23,177 | 1.84 ms | 3.32 ms | 5.92 ms | 0 |
| Through `lb` (round-robin, 3 backends) | 8,562 | 5.31 ms | 9.25 ms | 14.61 ms | 0 |

The balancer is the bottleneck on this box. Every proxied request costs two TCP round trips and the extra work of `httputil.ReverseProxy`, and it competes for the same 12 threads as 50 load workers and 3 backends. Under that contention the proxy adds about 3.5 ms at the median. On separate machines the cost would be lower; this run doesn't measure that.

## 2. One slow backend: round-robin vs least-connections

Backends add 1 ms, 1 ms and **50 ms**.

| Strategy | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|
| round_robin | 2,621 | 2.69 ms | 51.71 ms | 54.45 ms |
| weighted_round_robin (equal weights, control) | 2,604 | 2.81 ms | 51.83 ms | 56.33 ms |
| **least_connections** | **7,182** | 4.51 ms | **8.46 ms** | 54.92 ms |

Round-robin keeps sending a third of the traffic to the slow backend, so a third of the requests wait about 50 ms (p90 = 51.7 ms). Workers stuck there drag down total throughput.

Least-connections sees that requests pile up on the slow backend and steers new ones to the fast pair:
- **2.7× the throughput**
- **p90 from 51.7 ms to 8.5 ms**

p99 doesn't change: the slow backend still gets some traffic whenever the fast pair is busy, and those requests still take 50 ms.

With equal weights, weighted round-robin behaves like plain round-robin, as it should.

## 3. Failover: killing a backend mid-run

- 20 workers for 12 s, 3 backends at 1 ms. `b2` is killed with SIGTERM 4 s in.
- Active checks: 1 s interval, `fall: 3`.
- Detection time is the gap between the kill and the balancer's `backend down` log line.

| Config | Failed requests | Detected after | Detected by |
|---|---:|---:|---|
| Active checks only (`retries: 0`, `passive.max_fails: 0`) | 4,327 of 50,633 | 2,979 ms | active check |
| + passive checks (`max_fails: 3`) | 8 of 41,197 | 20 ms | passive |
| + passive checks + retries (`retries: 2`) | **0** of 41,495 | 18 ms | passive |

Failures per second ([`failover-*.json`](results/)):

```
active-only      [0 0 0 0 1290 1593 1444 0 0 0 0 0 0]
passive          [0 0 0 0 8 0 0 0 0 0 0 0 0]
passive+retries  [0 0 0 0 0 0 0 0 0 0 0 0 0]
```

What each layer adds:
- **Active checks only:** the balancer needs 3 failed probes (about 3 s at a 1 s interval) to notice. Meanwhile a third of all requests hit the dead backend and fail.
- **Passive checks:** the 3 real requests that fail count toward `max_fails`, and the backend is out about 20 ms after the kill. Only the requests already in flight to it fail.
- **Retries:** those last requests get re-sent to a live backend, so no client sees an error.

## Reproduce

```bash
bench/run.sh                  # Linux; ~2 minutes
DUR=30s CONC=100 bench/run.sh # longer / heavier
```
