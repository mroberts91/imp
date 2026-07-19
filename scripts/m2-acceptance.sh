#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M2 acceptance: crash-looping Daemon → describe tells the story
# (conditions honest, events aggregated, observedGeneration current)
# plus events --for and get -w smoke. Rootless; exits non-zero on failure.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

log() { printf '+ %s\n' "$*"; }
die() { printf 'ACCEPT FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"
}

need task
need python3
need timeout || need gtimeout || true

log "building binaries"
task build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m2-acceptance-$$"
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

# --- 1: apply crash-loop Daemon ---
CRASH_YAML="$WORKDIR/crash.yaml"
cat >"$CRASH_YAML" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: crash
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/false"]
      restartPolicy: Always
EOF

log "applying crash-loop Daemon"
"${IMPCTL[@]}" apply -f "$CRASH_YAML"

crashloop_ready() {
  local yaml events
  yaml="$("${IMPCTL[@]}" get procs -o yaml 2>/dev/null || true)"
  grep -q CrashLoopBackOff <<<"$yaml" || return 1
  echo "$yaml" | python3 -c '
import sys, re
text = sys.stdin.read()
m = re.search(r"restartCount:\s*(\d+)", text)
sys.exit(0 if m and int(m.group(1)) >= 1 else 1)
' || return 1
  events="$("${IMPCTL[@]}" get events -o yaml 2>/dev/null || true)"
  echo "$events" | python3 -c '
import sys, re
text = sys.stdin.read()
# Need a BackOff event with count >= 2
for block in text.split("---"):
    if "reason: BackOff" not in block and "reason: \"BackOff\"" not in block:
        # yaml may omit quotes
        if "BackOff" not in block or "reason:" not in block:
            continue
    m = re.search(r"count:\s*(\d+)", block)
    if m and int(m.group(1)) >= 2:
        sys.exit(0)
# Also accept reason on its own line near count in same object via looser scan
counts = re.findall(r"count:\s*(\d+)", text)
reasons = re.findall(r"reason:\s*(\S+)", text)
for r, c in zip(reasons, counts):
    if "BackOff" in r and int(c) >= 2:
        sys.exit(0)
sys.exit(1)
'
}

wait_for "CrashLoopBackOff + BackOff event count>=2" 45 crashloop_ready
pass "crash-loop with aggregated BackOff"

# --- 2: describe tells the story ---
log "impctl describe daemon crash"
DESC="$("${IMPCTL[@]}" describe daemon crash)"
echo "$DESC" | grep -q 'Name:[[:space:]]*crash' || die "describe missing Name:\n$DESC"
echo "$DESC" | grep -q 'ObservedGeneration:' || die "describe missing ObservedGeneration:\n$DESC"
echo "$DESC" | grep -Eq 'Available[[:space:]]+False' || die "describe missing Available False:\n$DESC"
echo "$DESC" | grep -q 'BackOff' || die "describe missing BackOff event:\n$DESC"
echo "$DESC" | grep -Eq 'x[0-9]+' || die "describe missing aggregated count (xN):\n$DESC"
# Proc Ready=False (CrashLoopBackOff) surfaces via Daemon Available and/or events;
# also check describe proc if a proc exists.
PROC="$("${IMPCTL[@]}" get procs | awk 'NR==2{print $1}')"
if [[ -n "$PROC" && "$PROC" != "NAME" ]]; then
  PDESC="$("${IMPCTL[@]}" describe proc "$PROC")"
  echo "$PDESC" | grep -Eq 'Ready[[:space:]]+False' || die "proc describe missing Ready False:\n$PDESC"
  echo "$PDESC" | grep -q CrashLoopBackOff || die "proc describe missing CrashLoopBackOff:\n$PDESC"
  pass "describe proc $PROC shows Ready False / CrashLoopBackOff"
fi
pass "describe daemon crash tells the crash-loop story"

# --- 3: events --for ---
log "impctl events --for daemon/crash and proc"
EVFOR="$("${IMPCTL[@]}" events --for daemon/crash)"
echo "$EVFOR" | grep -q Created || die "events --for daemon/crash missing Created:\n$EVFOR"
if [[ -n "${PROC:-}" && "$PROC" != "NAME" ]]; then
  EVPROC="$("${IMPCTL[@]}" events --for "proc/$PROC")"
  echo "$EVPROC" | grep -q BackOff || die "events --for proc/$PROC missing BackOff:\n$EVPROC"
  pass "events --for proc/$PROC shows BackOff"
fi
pass "events --for filters correctly"

# --- 4: get -w smoke (timeout after a couple seconds) ---
log "impctl get -w events (brief)"
TIMEOUT_BIN=timeout
command -v timeout >/dev/null 2>&1 || TIMEOUT_BIN=gtimeout
set +e
WATCH_OUT="$($TIMEOUT_BIN 2 "${IMPCTL[@]}" get -w events 2>&1)"
WATCH_RC=$?
set -e
# timeout exits 124; watch may also exit early — either is fine if we saw the header.
echo "$WATCH_OUT" | grep -q 'LAST SEEN' || die "get -w events missing table header:\n$WATCH_OUT"
pass "get -w events streamed table header (rc=$WATCH_RC)"

# --- 5: clean SIGTERM ---
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

printf '\nM2 ACCEPTANCE PASSED\n'
