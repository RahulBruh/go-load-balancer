#!/usr/bin/env bash
# Runs every benchmark scenario and writes raw JSON to bench/results/.
# Usage: bench/run.sh            (from the repo root, on Linux)
# Everything runs on one machine, so absolute numbers are a property of that
# machine; the comparisons between scenarios are the point.
set -euo pipefail
cd "$(dirname "$0")/.."

DUR=${DUR:-10s}
CONC=${CONC:-50}
OUT=bench/results
BIN=$(mktemp -d)
mkdir -p "$OUT"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$BIN"' EXIT

go build -o "$BIN/" ./cmd/lb ./cmd/demo-backend ./cmd/loadgen

{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "go: $(go version)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs)"
  echo "cores: $(nproc)"
  echo "kernel: $(uname -r)"
  echo "duration: $DUR  concurrency: $CONC"
} > "$OUT/env.txt"

# start_backends "<delay1> <delay2> <delay3>"
start_backends() {
  local i=1
  for d in $1; do
    "$BIN/demo-backend" -addr ":910$i" -name "b$i" -delay "$d" >/dev/null 2>&1 &
    i=$((i + 1))
  done
  wait_for http://localhost:9101/health http://localhost:9102/health http://localhost:9103/health
}

wait_for() {
  for u in "$@"; do
    for _ in $(seq 1 100); do curl -sf -o /dev/null "$u" && break; sleep 0.05; done
  done
}

# start_lb <strategy> <retries> <max_fails> <logfile>
start_lb() {
  cat > "$BIN/lb.yaml" <<EOF
listen: ":8180"
admin_listen: ":9190"
strategy: $1
backends:
  - url: http://localhost:9101
  - url: http://localhost:9102
  - url: http://localhost:9103
health_check: {interval: 1s, timeout: 500ms, rise: 2, fall: 3}
passive: {max_fails: $3}
retries: $2
EOF
  "$BIN/lb" -config "$BIN/lb.yaml" 2>"$4" &
  LB_PID=$!
  wait_for http://localhost:9190/healthz
}

stop_all() {
  kill $(jobs -p) 2>/dev/null || true
  wait 2>/dev/null || true
}

load() { "$BIN/loadgen" -json -c "$CONC" -d "$DUR" "$@"; }

echo "== 1. throughput and proxy overhead (1ms backends)"
start_backends "1ms 1ms 1ms"
load -url http://localhost:9101/ > "$OUT/direct.json"
start_lb round_robin 2 3 /dev/null
load -url http://localhost:8180/ > "$OUT/lb-round-robin.json"
stop_all

echo "== 2. one slow backend (1ms, 1ms, 50ms): round-robin vs least-connections"
for s in round_robin least_connections weighted_round_robin; do
  start_backends "1ms 1ms 50ms"
  start_lb "$s" 2 3 /dev/null
  load -url http://localhost:8180/ > "$OUT/slow-$s.json"
  stop_all
done

echo "== 3. failover: kill b2 at 4s into a 12s run"
failover() { # name retries max_fails
  start_backends "1ms 1ms 1ms"
  start_lb round_robin "$2" "$3" "$BIN/lb-$1.log"
  "$BIN/loadgen" -json -timeline -c 20 -d 12s -url http://localhost:8180/ > "$OUT/failover-$1.json" &
  local gen=$!
  sleep 4
  local killed_at
  killed_at=$(date +%s.%N)
  pkill -f -- "-addr :9102" || true
  wait "$gen"
  local down_at
  down_at=$(grep -m1 'msg="backend down" backend=localhost:9102' "$BIN/lb-$1.log" | sed -E 's/^time=([^ ]+).*/\1/')
  local detect_ms=null
  if [ -n "$down_at" ]; then
    detect_ms=$(echo "($(date -d "$down_at" +%s.%N) - $killed_at) * 1000" | bc | cut -d. -f1)
  fi
  echo "{\"scenario\":\"$1\",\"retries\":$2,\"passive_max_fails\":$3,\"detect_ms\":$detect_ms,\"down_reason\":\"$(grep -m1 'backend=localhost:9102' "$BIN/lb-$1.log" | grep -o 'reason=[^ ]*' | cut -d= -f2 | tr -d '\"')\"}" > "$OUT/failover-$1-detect.json"
  stop_all
}
failover active-only 0 0
failover passive 0 3
failover passive-retries 2 3

echo "done: $OUT"
