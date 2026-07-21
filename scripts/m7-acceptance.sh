#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M7 acceptance (rootless, fake cgroup root):
#   1. noNewPrivileges observable: NoNewPrivs 1 in the proc's status, 0 in a
#      control daemon without the field
#   2. Validation rejects a bad capability name and the ambient-outside-
#      bounding contradiction, naming the exact field
#   3. 126-honesty: privateTmp under a rootless impd crash-loops with the
#      child-setup reason one `impctl logs` away, never a silent no-op
#   4. impctl restart --rolling: high→low walk, availability-aware
#      (minReadySeconds), both PIDs replaced, rollout status green after
#   5. Clean SIGTERM with --kill-procs-on-shutdown
#
# The capability/privateTmp *enforcement* paths need root and live in the
# root-gated contributor tests (IMP_ROOT_SANDBOX_TESTS=1), not this gate.
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
need curl

if [[ "$(id -u)" == "0" ]]; then
  die "run rootless: this gate proves the *unprivileged* honesty paths"
fi

log "building binaries"
"$TASK" build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m7-acceptance-$$"
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

# ordinal_pid DAEMON N: PID of the daemon's Running proc at ordinal N
# ("" when none). Table columns: NAME DAEMON PHASE RESTARTS PID AGE; the
# ordinal is baked into the proc name (DAEMON-N-hash).
ordinal_pid() {
  "${IMPCTL[@]}" get procs 2>/dev/null |
    awk -v d="$1" -v pfx="$1-$2-" '$2 == d && $3 == "Running" && index($1, pfx) == 1 { print $5; exit }'
}

log "starting impd"
bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" \
  --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= \
  --log-level info >"$IMPD_LOG" 2>&1 &
IMPD_PID=$!
sleep 0.3
kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
wait_healthz || { cat "$IMPD_LOG"; die "healthz never ready"; }

# ---- 1. noNewPrivileges observable --------------------------------------
log "1. noNewPrivileges sets NoNewPrivs (control daemon stays 0)"
cat >"$MANIFESTS/nnp.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: nnp
spec:
  template:
    spec:
      command: ["sh", "-c", "grep NoNewPrivs /proc/self/status; exec sleep 300"]
      noNewPrivileges: true
---
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: nnp-control
spec:
  template:
    spec:
      command: ["sh", "-c", "grep NoNewPrivs /proc/self/status; exec sleep 300"]
EOF

wait_for "nnp log output" 20 sh -c 'bin/impctl logs nnp 2>/dev/null | grep -q NoNewPrivs'
"${IMPCTL[@]}" logs nnp | grep -q $'NoNewPrivs:\t1' || die "NoNewPrivs not 1 for nnp daemon"
wait_for "nnp-control log output" 20 sh -c 'bin/impctl logs nnp-control 2>/dev/null | grep -q NoNewPrivs'
"${IMPCTL[@]}" logs nnp-control | grep -q $'NoNewPrivs:\t0' || die "NoNewPrivs not 0 for control daemon"
pass "noNewPrivileges observable via /proc/self/status; control unaffected"

# ---- 2. validation rejects ----------------------------------------------
log "2. validation rejects bad capability names and contradictions"
cat >"$WORKDIR/bad-cap.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: bad-cap
spec:
  template:
    spec:
      command: ["sleep", "300"]
      capabilities:
        bounding: [not_a_cap]
EOF
if OUT="$("${IMPCTL[@]}" apply -f "$WORKDIR/bad-cap.yaml" 2>&1)"; then
  die "apply accepted an unknown capability name"
fi
grep -q 'spec.template.spec.capabilities.bounding\[0\]' <<<"$OUT" || die "rejection does not name the field: $OUT"

cat >"$WORKDIR/contradiction.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: contradiction
spec:
  template:
    spec:
      command: ["sleep", "300"]
      capabilities:
        bounding: [net_bind_service]
        ambient: [chown]
EOF
if OUT="$("${IMPCTL[@]}" apply -f "$WORKDIR/contradiction.yaml" 2>&1)"; then
  die "apply accepted ambient outside bounding"
fi
grep -q 'spec.template.spec.capabilities.ambient\[0\]' <<<"$OUT" || die "contradiction rejection does not name the field: $OUT"
pass "unknown name and ambient-outside-bounding rejected at apply"

# ---- 3. 126-honesty for privileged knobs --------------------------------
log "3. privateTmp under a rootless impd fails loudly (exit 126 crash loop)"
cat >"$MANIFESTS/wants-root.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: wants-root
spec:
  template:
    spec:
      command: ["sleep", "300"]
      privateTmp: true
EOF

wait_for "wants-root crash loop" 40 sh -c 'bin/impctl get procs -o json 2>/dev/null | grep -q CrashLoopBackOff'
wait_for "child-setup reason in logs" 10 sh -c 'bin/impctl logs wants-root 2>/dev/null | grep -q "imp child-setup:"'
"${IMPCTL[@]}" logs wants-root | grep -q 'mount namespace' || die "logs missing the mount-namespace EPERM detail"
wait_for "BackOff visible in describe" 20 sh -c 'bin/impctl describe daemon wants-root 2>/dev/null | grep -q BackOff'
rm "$MANIFESTS/wants-root.yaml"
pass "privileged knob failed loudly, reason one 'impctl logs' away"

# ---- 4. rolling restart -------------------------------------------------
log "4. impctl restart --rolling (high→low, availability-aware)"
cat >"$MANIFESTS/web.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  replicas: 2
  minReadySeconds: 2
  template:
    spec:
      command: ["sh", "-c", "exec sleep 300"]
      readinessProbe:
        exec:
          command: ["/bin/true"]
        periodSeconds: 1
EOF

wait_for "web daemon registered" 10 sh -c 'bin/impctl get daemons 2>/dev/null | grep -q "^web"'
timeout 60 "${IMPCTL[@]}" rollout status web >/dev/null || die "initial rollout did not complete"
PID0_OLD="$(ordinal_pid web 0)"
PID1_OLD="$(ordinal_pid web 1)"
[[ -n "$PID0_OLD" && -n "$PID1_OLD" ]] || die "missing PIDs before rolling restart (0: '$PID0_OLD', 1: '$PID1_OLD')"

ROLL_OUT="$(timeout 120 "${IMPCTL[@]}" restart --rolling web)" || die "restart --rolling failed: $ROLL_OUT"
grep -q 'rolling restart complete (2 procs replaced)' <<<"$ROLL_OUT" || die "rolling summary missing: $ROLL_OUT"
HIGH_LINE="$(grep -n '(ordinal 1)' <<<"$ROLL_OUT" | cut -d: -f1)"
LOW_LINE="$(grep -n '(ordinal 0)' <<<"$ROLL_OUT" | cut -d: -f1)"
[[ -n "$HIGH_LINE" && -n "$LOW_LINE" && "$HIGH_LINE" -lt "$LOW_LINE" ]] || die "walk not high→low: $ROLL_OUT"

PID0_NEW="$(ordinal_pid web 0)"
PID1_NEW="$(ordinal_pid web 1)"
[[ -n "$PID0_NEW" && "$PID0_NEW" != "$PID0_OLD" ]] || die "ordinal 0 PID unchanged ($PID0_OLD)"
[[ -n "$PID1_NEW" && "$PID1_NEW" != "$PID1_OLD" ]] || die "ordinal 1 PID unchanged ($PID1_OLD)"
timeout 60 "${IMPCTL[@]}" rollout status web >/dev/null || die "rollout status not green after rolling restart"
pass "rolling restart: ordinal 1 before 0, both PIDs replaced, rollout green"

# ---- 5. clean shutdown --------------------------------------------------
log "5. clean SIGTERM"
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

printf '\nACCEPT PASS: m7\n'
