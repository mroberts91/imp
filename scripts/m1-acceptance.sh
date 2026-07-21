#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M1 acceptance: manifest dir → live restarting Daemon → get/logs →
# kill -9 backoff → sweep → clean SIGTERM. Rootless; exits non-zero on failure.
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

log "building binaries"
"$TASK" build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m1-acceptance-$$"
SOCKET="$WORKDIR/impd.sock"
DATA="$WORKDIR/data"
MANIFESTS="$WORKDIR/manifests"
CGROUP_ROOT="$WORKDIR/cgroup"
IMPD_LOG="$WORKDIR/impd.log"
mkdir -p "$DATA" "$MANIFESTS" "$CGROUP_ROOT"
printf 'cpu memory pids\n' >"$CGROUP_ROOT/cgroup.controllers"
: >"$CGROUP_ROOT/cgroup.subtree_control"
cleanup() {
  if [[ -n "${IMPD_PID:-}" ]] && kill -0 "$IMPD_PID" 2>/dev/null; then
    kill -TERM "$IMPD_PID" 2>/dev/null || true
    wait "$IMPD_PID" 2>/dev/null || true
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

export IMP_SOCKET="$SOCKET"
IMPD=(bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= --log-level info)
IMPCTL=(bin/impctl)

log "starting impd in $WORKDIR"
"${IMPD[@]}" >"$IMPD_LOG" 2>&1 &
IMPD_PID=$!
sleep 0.5
kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
for i in $(seq 1 50); do
  if curl -sf --unix-socket "$SOCKET" http://impd/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -sf --unix-socket "$SOCKET" http://impd/healthz >/dev/null || die "healthz never ready"
pass "impd up (pid $IMPD_PID)"

# --- 1–2: GitOps drop → Running ---
log "dropping examples/01-hello-daemon.yaml into manifest-dir"
cp examples/01-hello-daemon.yaml "$MANIFESTS/hello.yaml"

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

proc_running_with_pid() {
  local table
  table="$("${IMPCTL[@]}" get procs 2>/dev/null || true)"
  echo "$table" | awk 'NR>1 && $3=="Running" && $5 ~ /^[0-9]+$/ {found=1} END{exit !found}'
}

daemon_table_ok() {
  local table
  table="$("${IMPCTL[@]}" get daemons 2>/dev/null || true)"
  echo "$table" | awk 'NR>1 && $1=="hello" {found=1} END{exit !found}'
}

wait_for "hello proc Running with PID" 20 proc_running_with_pid
wait_for "hello daemon in get daemons" 5 daemon_table_ok
pass "GitOps expand → Running"

PROC="$("${IMPCTL[@]}" get procs | awk 'NR==2{print $1}')"
PID="$("${IMPCTL[@]}" get procs | awk 'NR==2{print $5}')"
[[ -n "$PROC" && "$PID" =~ ^[0-9]+$ ]] || die "could not parse proc/pid from get procs"
pass "proc=$PROC pid=$PID"

# --- 3: logs via Daemon name ---
log "checking impctl logs hello"
LOGS=""
deadline=$((SECONDS + 15))
while (( SECONDS < deadline )); do
  LOGS="$("${IMPCTL[@]}" logs hello --tail 50 2>/dev/null || true)"
  if grep -q "hello from imp" <<<"$LOGS"; then
    break
  fi
  sleep 0.5
done
grep -q "hello from imp" <<<"$LOGS" || die "logs missing 'hello from imp'; got: $LOGS"
pass "impctl logs hello"

# --- 4: kill -9 → CrashLoopBackOff + restartCount ---
log "kill -9 $PID and wait for CrashLoopBackOff"
kill -9 "$PID" || true

crashloop_seen() {
  local yaml
  yaml="$("${IMPCTL[@]}" get procs -o yaml 2>/dev/null || true)"
  grep -q CrashLoopBackOff <<<"$yaml" || return 1
  # restartCount >= 1
  echo "$yaml" | python3 -c '
import sys, re
text = sys.stdin.read()
m = re.search(r"restartCount:\s*(\d+)", text)
sys.exit(0 if m and int(m.group(1)) >= 1 else 1)
'
}

wait_for "CrashLoopBackOff and restartCount>=1" 25 crashloop_seen
pass "kill -9 → CrashLoopBackOff / restartCount"

# --- 5: remove manifest → sweep ---
log "removing manifest file (GitOps delete)"
rm -f "$MANIFESTS/hello.yaml"

no_hello() {
  local d p
  d="$("${IMPCTL[@]}" get daemons 2>/dev/null || true)"
  p="$("${IMPCTL[@]}" get procs 2>/dev/null || true)"
  ! grep -qE '^hello([[:space:]]|$)' <<<"$d" && ! grep -qE '^hello-' <<<"$p"
}

wait_for "hello Daemon/Procs swept" 20 no_hello
pass "manifest removal swept objects"

# --- 6: clean SIGTERM ---
log "SIGTERM impd"
kill -TERM "$IMPD_PID"
deadline=$((SECONDS + 35))
while kill -0 "$IMPD_PID" 2>/dev/null; do
  (( SECONDS < deadline )) || die "impd did not exit after SIGTERM"
  sleep 0.1
done
IMPD_PID=""
grep -q 'controllers stopped' "$IMPD_LOG" || die "missing 'controllers stopped' in log"
grep -q 'execd stopped' "$IMPD_LOG" || die "missing 'execd stopped' in log"
grep -q 'impd stopped' "$IMPD_LOG" || die "missing 'impd stopped' in log"
pass "clean SIGTERM shutdown"

printf '\nM1 ACCEPTANCE PASSED\n'
