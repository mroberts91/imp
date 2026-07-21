#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M5 acceptance (rootless, fake cgroup root):
#   1. Timer via manifest GitOps: @every 10s runs fire, complete, and are
#      pruned to successfulHistoryLimit
#   2. concurrencyPolicy Forbid: one active run, SkippedRun events
#   3. logRetention: maxSizeMB 1 forces rotation; impctl logs still serves
#   4. impctl top returns rows for running Procs
#   5. impctl completion emits a shell script
#   6. Clean SIGTERM with --kill-procs-on-shutdown
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

WORKDIR="${TMPDIR:-/tmp}/imp-m5-acceptance-$$"
SOCKET="$WORKDIR/impd.sock"
DATA="$WORKDIR/data"
MANIFESTS="$WORKDIR/manifests"
CGROUP_ROOT="$WORKDIR/cgroup"
IMPD_LOG="$WORKDIR/impd.log"
mkdir -p "$DATA" "$MANIFESTS" "$CGROUP_ROOT"
printf 'cpu memory pids\n' >"$CGROUP_ROOT/cgroup.controllers"
: >"$CGROUP_ROOT/cgroup.subtree_control"

IMPD_PID=""
cleanup() {
  if [[ -n "${IMPD_PID:-}" ]] && kill -0 "$IMPD_PID" 2>/dev/null; then
    kill -TERM "$IMPD_PID" 2>/dev/null || true
    wait "$IMPD_PID" 2>/dev/null || true
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

log "starting impd"
bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" \
  --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= \
  --log-level info >"$IMPD_LOG" 2>&1 &
IMPD_PID=$!
sleep 0.3
kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
wait_healthz || { cat "$IMPD_LOG"; die "healthz never ready"; }

# ---- 1. Timer through manifest GitOps ----------------------------------
log "1. Timer fires, completes, prunes history"
cat >"$MANIFESTS/stamp.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: stamp
spec:
  schedule: "@every 10s"
  successfulHistoryLimit: 2
  template:
    spec:
      command: ["sh", "-c", "date '+ran %s'"]
EOF

succeeded_runs() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c '
import json, sys
raw = sys.stdin.read().strip()
count = 0
if raw:
    dec = json.JSONDecoder(); i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace(): i += 1
        if i >= len(raw): break
        obj, i = dec.raw_decode(raw, i)
        labels = (obj.get("metadata") or {}).get("labels") or {}
        if labels.get("impd.sh/timer-name") == "stamp" and \
           (obj.get("status") or {}).get("phase") == "Succeeded":
            count += 1
print(count)
'
}

scheduled_events() {
  "${IMPCTL[@]}" get events -o json 2>/dev/null | grep -c '"reason": "ScheduledRun"' || true
}

wait_for "first timer run to succeed" 30 sh -c '[ "$(bin/impctl get procs -o json 2>/dev/null | grep -c "\"phase\": \"Succeeded\"")" -ge 1 ]'
wait_for "three scheduled runs" 45 sh -c "[ \"\$(bin/impctl get events -o json 2>/dev/null | grep -c '\"reason\": \"ScheduledRun\"')\" -ge 3 ]"
wait_for "history pruned to limit" 30 sh -c '[ "$('"${IMPCTL[*]}"' get procs -o json 2>/dev/null | grep -c "\"phase\": \"Succeeded\"")" -le 2 ]'
[[ "$("${IMPCTL[@]}" get timers -o json | grep -c '"lastScheduleTime"')" -ge 1 ]] || die "lastScheduleTime not set"
"${IMPCTL[@]}" describe timer stamp | grep -q "Next Runs:" || die "describe timer missing next runs"
pass "timer fired, completed, pruned (limit 2)"

# ---- 2. Forbid concurrency ---------------------------------------------
log "2. Forbid keeps one active run"
cat >"$MANIFESTS/sleeper.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: sleeper
spec:
  schedule: "@every 5s"
  template:
    spec:
      command: ["sh", "-c", "sleep 60"]
EOF

active_sleepers() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c '
import json, sys
raw = sys.stdin.read().strip()
count = 0
if raw:
    dec = json.JSONDecoder(); i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace(): i += 1
        if i >= len(raw): break
        obj, i = dec.raw_decode(raw, i)
        labels = (obj.get("metadata") or {}).get("labels") or {}
        if labels.get("impd.sh/timer-name") == "sleeper" and \
           (obj.get("status") or {}).get("phase") not in ("Succeeded", "Failed"):
            count += 1
print(count)
'
}

wait_for "sleeper run to start" 20 sh -c '[ "$('"${IMPCTL[*]}"' get procs -o json 2>/dev/null | grep -c sleeper-)" -ge 1 ]'
wait_for "a SkippedRun event" 30 sh -c "bin/impctl get events -o json 2>/dev/null | grep -q '\"reason\": \"SkippedRun\"'"
[[ "$(active_sleepers)" == "1" ]] || die "Forbid allowed $(active_sleepers) concurrent runs"
pass "Forbid: one active run, SkippedRun recorded"

# ---- 3. Log retention ---------------------------------------------------
log "3. logRetention rotates at 1 MiB"
cat >"$MANIFESTS/chatty.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: chatty
spec:
  template:
    spec:
      command: ["sh", "-c", "while true; do head -c 8192 /dev/zero | tr '\\0' x; echo; done"]
      logRetention:
        maxSizeMB: 1
        maxBackups: 2
EOF

has_rotated_log() {
  local dir
  for dir in "$DATA"/logs/chatty-*; do
    [[ -d "$dir" ]] || continue
    if ls "$dir"/current-*.log >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}

wait_for "chatty proc Running" 20 sh -c "bin/impctl get procs -o json 2>/dev/null | grep -q chatty-"
wait_for "a rotated log backup" 60 has_rotated_log
"${IMPCTL[@]}" logs chatty --tail 1 >/dev/null || die "impctl logs failed after rotation"
pass "rotation produced a backup; logs still served"

# ---- 4. top -------------------------------------------------------------
log "4. impctl top"
TOP_OUT="$("${IMPCTL[@]}" top)"
grep -q "NAME" <<<"$TOP_OUT" || die "top missing header: $TOP_OUT"
grep -q "chatty-" <<<"$TOP_OUT" || die "top missing chatty proc: $TOP_OUT"
grep -q "daemon/chatty" <<<"$TOP_OUT" || die "top missing owner column: $TOP_OUT"
pass "top lists running procs with owners"

# ---- 5. completion ------------------------------------------------------
log "5. shell completion"
"${IMPCTL[@]}" completion bash | grep -q "__start_impctl" || die "bash completion script malformed"
"${IMPCTL[@]}" completion zsh >/dev/null || die "zsh completion failed"
pass "completion scripts generated"

# ---- 6. clean shutdown --------------------------------------------------
log "6. clean SIGTERM"
kill -TERM "$IMPD_PID"
deadline=$((SECONDS + 35))
while kill -0 "$IMPD_PID" 2>/dev/null; do
  (( SECONDS < deadline )) || die "impd did not exit after SIGTERM"
  sleep 0.1
done
wait "$IMPD_PID" 2>/dev/null || true
IMPD_PID=""
grep -q '"msg":"controllers stopped"' "$IMPD_LOG" || die "shutdown missing controllers stopped"
grep -q '"msg":"impd stopped"' "$IMPD_LOG" || die "shutdown missing impd stopped"
if grep -qi panic "$IMPD_LOG"; then
  die "panic in impd log"
fi
pass "clean shutdown"

printf '\nACCEPT PASS: m5\n'
