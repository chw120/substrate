#!/usr/bin/env bash
#
# collect.sh — drive N suspend/resume cycles and append one test case to
# timings.csv / resources.csv, in the same schema as the `baseline` case.
#
#   TEST_CASE=after-teardown-async ./collect.sh
#   TEST_CASE=drop-caches CYCLES=20 ACTOR=counter ./collect.sh
#
# Then:  ./compare.py baseline after-teardown-async
#
# Means are computed by DIFFING Prometheus counters across the test window
# (delta(_sum) / delta(_count)), so each number is the mean over exactly the
# cycles this script drove — not a rate() over an arbitrary window.
#
# Remember to add a row to testcases.csv describing what changed. A timing with
# no test case describing it is not comparable to anything.
#
# STATUS: metric names, labels and CLI flags are all grounded in the source, but
# this has not been run end to end (the cluster it was derived from is gone).
# Do a CYCLES=1 run and eyeball the CSV before trusting a long one.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

TEST_CASE=${TEST_CASE:-run-$(date +%Y%m%d-%H%M%S)}
ACTOR=${ACTOR:-counter}
ATESPACE=${ATESPACE:-ate-demo-counter}
CYCLES=${CYCLES:-10}

PROM_NS=${PROM_NS:-otel-system}
PROM_SVC=${PROM_SVC:-prometheus}
PROM_PORT=${PROM_PORT:-9090}
LOCAL_PORT=${LOCAL_PORT:-19090}

# The kind overlay sets OTEL_METRIC_EXPORT_INTERVAL=10000. Wait past one export
# interval plus a scrape so the final cycle is definitely counted.
SETTLE_SECONDS=${SETTLE_SECONDS:-35}

PROM="http://localhost:${LOCAL_PORT}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; [[ -n "${PF_PID:-}" ]] && kill "$PF_PID" 2>/dev/null || true' EXIT

need() { command -v "$1" >/dev/null || { echo "missing dependency: $1" >&2; exit 1; }; }
need kubectl; need curl; need python3

log() { printf '\033[1;36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }

grep -q "^${TEST_CASE}," "$HERE/timings.csv" 2>/dev/null &&
  { echo "test_case '${TEST_CASE}' already in timings.csv — pick another name" >&2; exit 1; }

# ---------------------------------------------------------------- prometheus

kubectl -n "$PROM_NS" port-forward "svc/${PROM_SVC}" "${LOCAL_PORT}:${PROM_PORT}" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 30); do curl -sf "${PROM}/-/ready" >/dev/null 2>&1 && break; sleep 1; done
curl -sf "${PROM}/-/ready" >/dev/null || { echo "prometheus unreachable on ${PROM}" >&2; exit 1; }
log "prometheus up on ${PROM}"

pq() { curl -sG "${PROM}/api/v1/query" --data-urlencode "query=$1"; }

capture() {
  {
    echo '{'
    echo '"checkpoint_sum":'   "$(pq 'ate_actor_checkpoint_duration_seconds_sum{ate_sandbox_class="microvm"}')" ','
    echo '"checkpoint_count":' "$(pq 'ate_actor_checkpoint_duration_seconds_count{ate_sandbox_class="microvm"}')" ','
    echo '"restore_sum":'      "$(pq 'ate_actor_restore_duration_seconds_sum{ate_sandbox_class="microvm"}')" ','
    echo '"restore_count":'    "$(pq 'ate_actor_restore_duration_seconds_count{ate_sandbox_class="microvm"}')" ','
    echo '"size_sum":'         "$(pq 'atelet_snapshot_size_bytes_sum')" ','
    echo '"size_count":'       "$(pq 'atelet_snapshot_size_bytes_count')"
    echo '}'
  } > "$1"
}

# ------------------------------------------------------------------- cycles

log "capturing counters BEFORE"
capture "$TMP/before.json"

log "driving ${CYCLES} suspend/resume cycles on ${ATESPACE}/${ACTOR}"
for i in $(seq 1 "$CYCLES"); do
  log "  ${i}/${CYCLES} suspend"; kubectl ate suspend actor "$ACTOR" -a "$ATESPACE" >/dev/null
  log "  ${i}/${CYCLES} resume";  kubectl ate resume  actor "$ACTOR" -a "$ATESPACE" >/dev/null
done

log "waiting ${SETTLE_SECONDS}s for the export interval + scrape"
sleep "$SETTLE_SECONDS"

log "capturing counters AFTER"
capture "$TMP/after.json"
pq 'ate_microvm_guest_memory_bytes' > "$TMP/guestmem.json"

# The four ateom checkpoint segments are log-only (observability gap #16).
{
  for pod in $(kubectl -n "$ATESPACE" get pods -o name 2>/dev/null); do
    kubectl -n "$ATESPACE" logs "$pod" --all-containers --since="$((SETTLE_SECONDS + 120))s" 2>/dev/null || true
  done
} | grep -E '"msg":"(Actor checkpointed|CH API shutdown done)"' > "$TMP/ateom.log" || true
log "ateom log lines: $(wc -l < "$TMP/ateom.log" | tr -d ' ')"

# ------------------------------------------------------------------ emit CSV

TEST_CASE="$TEST_CASE" TMP="$TMP" HERE="$HERE" python3 "$HERE/_emit.py"

log "done — now add a row to ${HERE}/testcases.csv describing this case"
