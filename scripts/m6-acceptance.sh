#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M6 acceptance (rootless, fake cgroup root):
#   1. Hardening fields through the child-setup shim: rlimits (nofile),
#      umask observed by the process; nice + oomScoreAdjust visible in /proc
#   2. limits.cpu → cpu.max written into the (fake) cgroup
#   3. startupProbe gates liveness: a slow starter is not killed
#   4. minReadySeconds + RollingUpdate roll; impctl rollout status exits 0
#   5. progressDeadlineSeconds: stuck roll flips Progressing, rollout status
#      exits nonzero, Warning event recorded; recovery after a fix
#   6. impctl restart: procs replaced (new PID), spec untouched
#   7. impctl run: manual timer run completes; lastScheduleTime untouched
#   8. Clean SIGTERM with --kill-procs-on-shutdown
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
need curl

log "building binaries"
task build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m6-acceptance-$$"
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

# proc_pid DAEMON: the PID of the daemon's first Running proc ("" when none).
# Table columns: NAME DAEMON PHASE RESTARTS PID AGE.
proc_pid() {
  "${IMPCTL[@]}" get procs 2>/dev/null |
    awk -v d="$1" '$2 == d && $3 == "Running" { print $5; exit }'
}

# proc_restarts DAEMON: RESTARTS of the daemon's first proc.
proc_restarts() {
  "${IMPCTL[@]}" get procs 2>/dev/null |
    awk -v d="$1" '$2 == d { print $4; exit }'
}

# daemon_col NAME N: column N of the daemon's row (READY=2, UP-TO-DATE=3,
# AVAILABLE=4).
daemon_col() {
  "${IMPCTL[@]}" get daemons 2>/dev/null |
    awk -v d="$1" -v n="$2" '$1 == d { print $n; exit }'
}

log "starting impd"
bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" \
  --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= \
  --log-level info >"$IMPD_LOG" 2>&1 &
IMPD_PID=$!
sleep 0.3
kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
wait_healthz || { cat "$IMPD_LOG"; die "healthz never ready"; }

# ---- 1 + 2. hardening fields + cpu.max ---------------------------------
log "1. rlimits/umask/nice/oomScoreAdjust through the shim (+ cpu.max)"
cat >"$MANIFESTS/hardened.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: hardened
spec:
  template:
    spec:
      command: ["sh", "-c", "ulimit -Sn; ulimit -Hn; umask; exec sleep 300"]
      rlimits:
        - resource: nofile
          soft: 256
          hard: 512
      umask: "0077"
      nice: 5
      oomScoreAdjust: 100
      resources:
        limits:
          cpu: 500m
EOF

wait_for "hardened proc Running" 20 sh -c '[ -n "$(bin/impctl get procs 2>/dev/null | awk '\''$2 == "hardened" && $3 == "Running" { print $5 }'\'')" ]'
wait_for "hardened log output" 10 sh -c 'bin/impctl logs hardened 2>/dev/null | grep -q 0077'
LOGS="$("${IMPCTL[@]}" logs hardened)"
grep -qx '256' <<<"$LOGS" || die "soft nofile not 256: $LOGS"
grep -qx '512' <<<"$LOGS" || die "hard nofile not 512: $LOGS"
grep -qx '0077' <<<"$LOGS" || die "umask not 0077: $LOGS"

PID="$(proc_pid hardened)"
[[ -n "$PID" ]] || die "no PID for hardened proc"
NICE="$(cut -d' ' -f19 "/proc/$PID/stat")"
[[ "$NICE" == "5" ]] || die "nice = $NICE, want 5"
ADJ="$(cat "/proc/$PID/oom_score_adj")"
[[ "$ADJ" == "100" ]] || die "oom_score_adj = $ADJ, want 100"
pass "shim applied rlimits, umask, nice, oomScoreAdjust"

log "2. cpu.max in the (fake) cgroup"
grep -Rqx '50000 100000' "$CGROUP_ROOT" --include=cpu.max || die "cpu.max not written (want '50000 100000')"
pass "limits.cpu 500m -> cpu.max 50000 100000"

# ---- 3. startupProbe gates liveness ------------------------------------
log "3. startupProbe holds liveness for a slow starter"
STARTED="$WORKDIR/slow-started"
cat >"$MANIFESTS/slow.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: slow
spec:
  template:
    spec:
      command: ["sh", "-c", "sleep 3; touch $STARTED; exec sleep 300"]
      startupProbe:
        exec:
          command: ["test", "-f", "$STARTED"]
        periodSeconds: 1
        failureThreshold: 30
      livenessProbe:
        exec:
          command: ["test", "-f", "$STARTED"]
        initialDelaySeconds: 0
        periodSeconds: 1
        failureThreshold: 2
EOF

# Without the startup gate, the liveness probe (2 failures at 1s) would
# kill the process long before its 3s warm-up finishes.
wait_for "slow daemon Ready 1/1" 30 sh -c '[ "$(bin/impctl get daemons 2>/dev/null | awk '\''$1 == "slow" { print $2 }'\'')" = "1/1" ]'
RESTARTS="$(proc_restarts slow)"
[[ "${RESTARTS:-0}" == "0" ]] || die "slow starter was restarted $RESTARTS time(s) — startup gate failed"
pass "slow starter survived its warm-up (restartCount 0)"

# ---- 4. minReadySeconds + rollout status --------------------------------
log "4. RollingUpdate with minReadySeconds; rollout status"
cat >"$MANIFESTS/web.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  replicas: 2
  minReadySeconds: 2
  updateStrategy:
    type: RollingUpdate
  template:
    spec:
      command: ["sh", "-c", "exec sleep 300"]
      env:
        - name: REV
          value: "1"
      readinessProbe:
        exec:
          command: ["/bin/true"]
        periodSeconds: 1
EOF

wait_for "web daemon registered" 10 sh -c 'bin/impctl get daemons 2>/dev/null | grep -q "^web"'
timeout 60 "${IMPCTL[@]}" rollout status web >/dev/null || die "initial rollout did not complete"
sed -i 's/value: "1"/value: "2"/' "$MANIFESTS/web.yaml"
OUT="$(timeout 90 "${IMPCTL[@]}" rollout status web)" || die "rolling update did not complete: $OUT"
grep -q 'successfully rolled out' <<<"$OUT" || die "rollout status output: $OUT"
AVAILABLE="$(daemon_col web 4)"
[[ "$AVAILABLE" == "2" ]] || die "availableReplicas = $AVAILABLE, want 2"
pass "rolling update completed; availableReplicas honest under minReadySeconds"

# ---- 5. progressDeadlineSeconds -----------------------------------------
log "5. progress deadline flips a stuck roll, then recovers"
cat >"$MANIFESTS/stuck.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: stuck
spec:
  progressDeadlineSeconds: 10
  template:
    spec:
      command: ["sh", "-c", "exec sleep 300"]
      readinessProbe:
        exec:
          command: ["/bin/false"]
        periodSeconds: 3
        failureThreshold: 1
EOF

wait_for "ProgressDeadlineExceeded condition" 40 sh -c 'bin/impctl describe daemon stuck 2>/dev/null | grep -q ProgressDeadlineExceeded'
if timeout 20 "${IMPCTL[@]}" rollout status stuck >/dev/null 2>&1; then
  die "rollout status exited 0 for a deadline-exceeded daemon"
fi
wait_for "ProgressDeadlineExceeded event" 20 sh -c 'bin/impctl get events -o json 2>/dev/null | grep -q "\"reason\": \"ProgressDeadlineExceeded\""'
# Fix the daemon: a new generation resets the deadline and the roll
# converges. Wait for the manifest re-scan to land the fix before asking
# rollout status (it reports the still-exceeded state until then).
sed -i '/readinessProbe:/,$d' "$MANIFESTS/stuck.yaml"
# (Scope the check to the Conditions block — the ProgressDeadlineExceeded
# *event* stays in describe long after the condition recovers.)
wait_for "fixed spec applied and exceeded state cleared" 30 sh -c '! bin/impctl describe daemon stuck 2>/dev/null | grep -A5 "^Conditions:" | grep -q ProgressDeadlineExceeded'
timeout 60 "${IMPCTL[@]}" rollout status stuck >/dev/null || die "fixed daemon did not recover"
pass "deadline flip, warning event, nonzero exit, recovery"

# ---- 6. impctl restart --------------------------------------------------
log "6. restart replaces procs"
OLDPID="$(proc_pid hardened)"
[[ -n "$OLDPID" ]] || die "no PID before restart"
RESTART_OUT="$("${IMPCTL[@]}" restart hardened)"
grep -q 'deleted' <<<"$RESTART_OUT" || die "restart output: $RESTART_OUT"
wait_for "replacement proc Running with a new PID" 30 sh -c '
  pid="$(bin/impctl get procs 2>/dev/null | awk '\''$2 == "hardened" && $3 == "Running" { print $5; exit }'\'')"
  [ -n "$pid" ] && [ "$pid" != "'"$OLDPID"'" ]'
pass "restart: old pid $OLDPID replaced"

# ---- 7. impctl run ------------------------------------------------------
log "7. manual timer run"
cat >"$MANIFESTS/backup.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: backup
spec:
  schedule: "0 5 * * *"
  template:
    spec:
      command: ["sh", "-c", "echo backed-up"]
EOF

wait_for "timer registered" 10 sh -c 'bin/impctl get timers 2>/dev/null | grep -q backup'
RUN_OUT="$("${IMPCTL[@]}" run backup)"
grep -q 'backup-manual-' <<<"$RUN_OUT" || die "run output: $RUN_OUT"
# (JSON, not the table: a timer run has no DAEMON column value, which
# collapses under awk's whitespace splitting. No other proc in this script
# ever reaches Succeeded.)
wait_for "manual run to succeed" 30 sh -c 'bin/impctl get procs -o json 2>/dev/null | grep -q "\"phase\": \"Succeeded\""'
if "${IMPCTL[@]}" get timers -o json | grep -q lastScheduleTime; then
  die "manual run advanced lastScheduleTime"
fi
pass "manual run completed; schedule bookkeeping untouched"

# ---- 8. clean shutdown --------------------------------------------------
log "8. clean SIGTERM"
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

printf '\nACCEPT PASS: m6\n'
