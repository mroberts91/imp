#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M3 acceptance (rootless, fake cgroup root):
#   1. memory limit + readinessProbe → Ready after probe; cgroup memory.max set
#   2. livenessProbe fail → restart + ProbeFailed/Unhealthy Events
#   3. curl /metrics shows phase (+ memory when scraped)
#   4. default shutdown: kill -9 impd → child survives → restart → Adopted
#   5. --kill-procs-on-shutdown: SIGTERM → no leftover cgroup procs
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

log() { printf '+ %s\n' "$*"; }
die() { printf 'ACCEPT FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"
}

# Alpine packages Task as go-task (name clash with taskwarrior).
TASK="$(command -v task || command -v go-task || true)"
[ -n "$TASK" ] || die "need task (or go-task) on PATH"
need python3
need curl

log "building binaries"
"$TASK" build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m3-acceptance-$$"
SOCKET="$WORKDIR/impd.sock"
DATA="$WORKDIR/data"
MANIFESTS="$WORKDIR/manifests"
CGROUP_ROOT="$WORKDIR/cgroup"
IMPD_LOG="$WORKDIR/impd.log"
METRICS_PORT=$((19000 + $$ % 1000))
METRICS_ADDR="127.0.0.1:${METRICS_PORT}"
mkdir -p "$DATA" "$MANIFESTS" "$CGROUP_ROOT"
printf 'cpu memory pids\n' >"$CGROUP_ROOT/cgroup.controllers"
: >"$CGROUP_ROOT/cgroup.subtree_control"

IMPD_PID=""
CHILD_PID=""

cleanup() {
  if [[ -n "${IMPD_PID:-}" ]] && kill -0 "$IMPD_PID" 2>/dev/null; then
    kill -TERM "$IMPD_PID" 2>/dev/null || true
    wait "$IMPD_PID" 2>/dev/null || true
  fi
  if [[ -n "${CHILD_PID:-}" ]] && kill -0 "$CHILD_PID" 2>/dev/null; then
    kill -KILL "$CHILD_PID" 2>/dev/null || true
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

export IMP_SOCKET="$SOCKET"
IMPCTL=(bin/impctl)

wait_for() {
  local what="$1" secs="$2"
  shift 2
  local deadline=$((SECONDS + secs))
  while (( SECONDS < deadline )); do
    if "$@"; then
      return 0
    fi
    sleep 0.25
  done
  die "timed out waiting for $what (${secs}s)"
}

wait_healthz() {
  local i
  for i in $(seq 1 50); do
    if curl -sf --unix-socket "$SOCKET" http://impd/healthz >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

# start_impd MODE METRICS
# MODE: kill | survive
# METRICS: on | off
start_impd() {
  local mode="$1" metrics_mode="${2:-off}"
  local -a args
  args=(bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS"
    --cgroup-root "$CGROUP_ROOT" --log-level info)
  if [[ "$mode" == "kill" ]]; then
    args+=(--kill-procs-on-shutdown)
  fi
  if [[ "$metrics_mode" == "on" ]]; then
    args+=(--metrics-addr "$METRICS_ADDR")
  else
    args+=(--metrics-addr=)
  fi
  : >"$IMPD_LOG"
  "${args[@]}" >"$IMPD_LOG" 2>&1 &
  IMPD_PID=$!
  sleep 0.3
  kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
  wait_healthz || { cat "$IMPD_LOG"; die "healthz never ready"; }
}

stop_impd_term() {
  [[ -n "${IMPD_PID:-}" ]] || return 0
  kill -TERM "$IMPD_PID" 2>/dev/null || true
  local deadline=$((SECONDS + 35))
  while kill -0 "$IMPD_PID" 2>/dev/null; do
    (( SECONDS < deadline )) || die "impd did not exit after SIGTERM"
    sleep 0.1
  done
  wait "$IMPD_PID" 2>/dev/null || true
  IMPD_PID=""
}

daemon_absent() {
  local name="$1"
  ! "${IMPCTL[@]}" get daemons 2>/dev/null | grep -qE "^${name}([[:space:]]|$)"
}

proc_yaml() {
  "${IMPCTL[@]}" get procs -o yaml 2>/dev/null || true
}

proc_running() {
  grep -q 'phase: Running' <<<"$(proc_yaml)"
}

proc_uid() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c '
import sys, json
raw = sys.stdin.read().strip()
if not raw:
    raise SystemExit(0)
docs = json.loads(raw)
if isinstance(docs, list):
    docs = docs[0] if docs else {}
print(docs.get("metadata", {}).get("uid", ""))
'
}

proc_pid() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c '
import sys, json
raw = sys.stdin.read().strip()
if not raw:
    raise SystemExit(0)
docs = json.loads(raw)
if isinstance(docs, list):
    docs = docs[0] if docs else {}
running = ((docs.get("status") or {}).get("state") or {}).get("running") or {}
pid = running.get("pid")
print(pid if pid is not None else "")
'
}

ready_status_reason() {
  proc_yaml | python3 -c '
import sys, re
text = sys.stdin.read()
# Conditions list items; type may appear after status/reason in YAML.
for block in re.split(r"\n\s*-\s+", text):
    if "type: Ready" not in block and "type: \"Ready\"" not in block:
        continue
    sm = re.search(r"status:\s*\"?(True|False)\"?", block)
    rm = re.search(r"reason:\s*\"?(\w+)\"?", block)
    status = sm.group(1) if sm else ""
    reason = rm.group(1) if rm else ""
    print(f"{status} {reason}")
    raise SystemExit(0)
print(" ")
'
}

ready_false_pending() {
  local sr
  sr="$(ready_status_reason)"
  [[ "$sr" == "False ProbePending" || "$sr" == "False ProbeFailed" ]]
}

ready_true() {
  local sr
  sr="$(ready_status_reason)"
  [[ "$sr" == True* ]]
}

no_live_cgroup_procs() {
  local d pid
  shopt -s nullglob
  for d in "$CGROUP_ROOT"/proc-*; do
    [[ -f "$d/cgroup.procs" ]] || continue
    while read -r pid; do
      [[ -z "$pid" ]] && continue
      if kill -0 "$pid" 2>/dev/null; then
        return 1
      fi
    done <"$d/cgroup.procs"
  done
  return 0
}

# --- Phase A: readiness + memory limit + metrics ---
log "starting impd (kill-on-shutdown + metrics) in $WORKDIR"
start_impd kill on
pass "impd up (pid $IMPD_PID) metrics=$METRICS_ADDR"

READY_YAML="$WORKDIR/ready.yaml"
cat >"$READY_YAML" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: ready
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sleep", "120"]
      restartPolicy: Always
      stopSignal: TERM
      terminationGracePeriodSeconds: 2
      resources:
        limits:
          memory: 32Mi
          cpuWeight: 50
          pids: 32
      readinessProbe:
        exec:
          command: ["/bin/true"]
        initialDelaySeconds: 2
        periodSeconds: 1
        timeoutSeconds: 1
        successThreshold: 1
        failureThreshold: 3
EOF

log "applying ready Daemon (limits + readinessProbe)"
"${IMPCTL[@]}" apply -f "$READY_YAML"

wait_for "ready proc Running" 20 proc_running
if ready_false_pending; then
  pass "Ready=False ProbePending before probe success"
else
  log "skipped ProbePending observation (raced past delay)"
fi
wait_for "Ready=True after readiness probe" 20 ready_true
pass "readiness probe gated Ready → True"

PROC_UID="$(proc_uid)"
[[ -n "$PROC_UID" ]] || die "could not parse proc uid"
MEM_FILE="$CGROUP_ROOT/proc-$PROC_UID/memory.max"
[[ -f "$MEM_FILE" ]] || die "missing $MEM_FILE"
grep -q 33554432 "$MEM_FILE" || die "memory.max want 33554432 (32Mi), got: $(cat "$MEM_FILE")"
pass "cgroup memory.max=32Mi"

log "scraping metrics at $METRICS_ADDR"
metrics_has_phase() {
  local body
  body="$(curl -sf "http://${METRICS_ADDR}/metrics" 2>/dev/null || true)"
  grep -q 'imp_proc_phase' <<<"$body" && grep -q 'phase="Running"' <<<"$body"
}
wait_for "imp_proc_phase Running in /metrics" 10 metrics_has_phase
pass "metrics show imp_proc_phase Running"

metrics_has_memory() {
  local body
  body="$(curl -sf "http://${METRICS_ADDR}/metrics" 2>/dev/null || true)"
  grep -q 'imp_proc_memory_bytes' <<<"$body"
}
wait_for "imp_proc_memory_bytes in /metrics" 25 metrics_has_memory
pass "metrics show imp_proc_memory_bytes"

# --- Phase B: liveness restart ---
log "replacing with liveness-fail Daemon"
"${IMPCTL[@]}" delete daemon ready >/dev/null 2>&1 || true
wait_for "ready Daemon gone" 20 daemon_absent ready

LIVE_YAML="$WORKDIR/live.yaml"
cat >"$LIVE_YAML" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: live
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sleep", "120"]
      restartPolicy: Always
      stopSignal: TERM
      terminationGracePeriodSeconds: 1
      livenessProbe:
        exec:
          command: ["/bin/false"]
        initialDelaySeconds: 0
        periodSeconds: 1
        timeoutSeconds: 1
        successThreshold: 1
        failureThreshold: 1
EOF
"${IMPCTL[@]}" apply -f "$LIVE_YAML"

liveness_restarted() {
  local yaml events rc
  yaml="$(proc_yaml)"
  echo "$yaml" | python3 -c '
import sys, re
text = sys.stdin.read()
m = re.search(r"restartCount:\s*(\d+)", text)
sys.exit(0 if m and int(m.group(1)) >= 1 else 1)
' || return 1
  events="$("${IMPCTL[@]}" get events -o yaml 2>/dev/null || true)"
  grep -q ProbeFailed <<<"$events" || return 1
  grep -q Unhealthy <<<"$events" || return 1
  return 0
}

wait_for "liveness restart + ProbeFailed/Unhealthy" 30 liveness_restarted
pass "liveness failure → restart + ProbeFailed/Unhealthy Events"

# --- Phase C: kill-on-shutdown leaves no cgroup procs ---
log "deleting live Daemon then SIGTERM with kill-on-shutdown"
"${IMPCTL[@]}" delete daemon live >/dev/null 2>&1 || true
wait_for "live Daemon gone" 20 daemon_absent live
stop_impd_term
grep -q 'impd stopped' "$IMPD_LOG" || die "missing impd stopped after kill-on-shutdown SIGTERM"
no_live_cgroup_procs || die "leftover live processes in cgroups after kill-on-shutdown"
pass "kill-on-shutdown SIGTERM → no leftover cgroup procs"

# --- Phase D: D1 re-attach (survive shutdown) ---
log "starting impd without kill-on-shutdown for D1 adopt"
start_impd survive off

KEEP_YAML="$WORKDIR/keep.yaml"
cat >"$KEEP_YAML" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: keep
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sleep", "300"]
      restartPolicy: Always
      stopSignal: TERM
      terminationGracePeriodSeconds: 2
EOF
"${IMPCTL[@]}" apply -f "$KEEP_YAML"
wait_for "keep proc Running" 20 proc_running

CHILD_PID="$(proc_pid)"
[[ "$CHILD_PID" =~ ^[0-9]+$ ]] || die "could not parse child pid"
pass "keep proc pid=$CHILD_PID"

log "kill -9 impd (children should survive)"
kill -9 "$IMPD_PID"
wait "$IMPD_PID" 2>/dev/null || true
IMPD_PID=""
sleep 0.3
kill -0 "$CHILD_PID" 2>/dev/null || die "child $CHILD_PID died after impd kill -9"
pass "child survived impd kill -9"

log "restarting impd (same data+cgroup) for adopt"
start_impd survive off

adopted() {
  local events yaml
  events="$("${IMPCTL[@]}" get events -o yaml 2>/dev/null || true)"
  grep -q Adopted <<<"$events" || return 1
  yaml="$(proc_yaml)"
  echo "$yaml" | WANT_PID="$CHILD_PID" python3 -c '
import sys, re, os
want = os.environ["WANT_PID"]
text = sys.stdin.read()
m = re.search(r"pid:\s*(\d+)", text)
sys.exit(0 if m and m.group(1) == want else 1)
'
}

wait_for "Adopted event + same pid" 25 adopted
pass "restart → Adopted with pid=$CHILD_PID"

# Tear down: switch to kill-on-shutdown and delete.
log "tearing down keep"
stop_impd_term
start_impd kill off
"${IMPCTL[@]}" delete daemon keep >/dev/null 2>&1 || true
wait_for "keep Daemon gone" 25 daemon_absent keep
# If the adopted child is still around, wait for stop; else force.
deadline=$((SECONDS + 15))
while kill -0 "$CHILD_PID" 2>/dev/null && (( SECONDS < deadline )); do
  sleep 0.25
done
if kill -0 "$CHILD_PID" 2>/dev/null; then
  kill -KILL "$CHILD_PID" 2>/dev/null || true
fi
CHILD_PID=""
stop_impd_term
pass "D1 adopt path exercised; teardown complete"

printf '\nM3 ACCEPTANCE PASSED\n'
