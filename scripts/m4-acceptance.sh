#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M4 acceptance (rootless, fake cgroup root):
#   1. replicas=3 RollingUpdate → three Running Procs
#   2. D2: each Proc sees a distinct IMP_REPLICA_INDEX in logs
#   3. Template change rolls high→low (lower ordinal stays old until its turn)
#   4. Clean SIGTERM with --kill-procs-on-shutdown
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

WORKDIR="${TMPDIR:-/tmp}/imp-m4-acceptance-$$"
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

start_impd() {
  : >"$IMPD_LOG"
  bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" \
    --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= \
    --log-level info >"$IMPD_LOG" 2>&1 &
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

procs_json() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null || echo '[]'
}

# Print "index hash phase" lines sorted by index.
proc_summary() {
  procs_json | python3 -c '
import json, sys
raw = sys.stdin.read().strip()
docs = []
if raw:
    dec = json.JSONDecoder()
    i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace():
            i += 1
        if i >= len(raw):
            break
        obj, end = dec.raw_decode(raw, i)
        docs.append(obj)
        i = end
rows = []
for p in docs:
    labels = (p.get("metadata") or {}).get("labels") or {}
    idx = labels.get("impd.sh/replica-index", "?")
    h = labels.get("impd.sh/template-hash", "?")
    phase = (p.get("status") or {}).get("phase") or ""
    rows.append((int(idx) if str(idx).isdigit() else 99, str(idx), h, phase))
for _, idx, h, phase in sorted(rows):
    print(f"{idx} {h} {phase}")
'
}

three_running() {
  local n
  n="$(proc_summary | awk '$3=="Running"{c++} END{print c+0}')"
  [[ "$n" == "3" ]]
}

all_hash() {
  local want="$1"
  proc_summary | awk -v w="$want" '$2!=w{bad=1} END{exit bad+0}'
}

# True when ordinal 2 has hash NEW and ordinal 0 still has hash OLD.
mid_roll_high_first() {
  local old="$1" new="$2"
  proc_summary | python3 -c '
import sys
old, new = sys.argv[1], sys.argv[2]
by = {}
for line in sys.stdin:
    parts = line.split()
    if len(parts) >= 2:
        by[parts[0]] = parts[1]
sys.exit(0 if by.get("2") == new and by.get("0") == old else 1)
' "$old" "$new"
}

write_manifest() {
  local version="$1"
  cat >"$MANIFESTS/roll.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: rollme
spec:
  replicas: 3
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        app: rollme
    spec:
      command:
        - sh
        - -c
        - while true; do echo "rollme ${version} index=\$IMP_REPLICA_INDEX"; sleep 2; done
      restartPolicy: Always
      # Slow Ready so the mid-roll window is observable (Running alone
      # would Ready immediately and the roll can finish in one poll gap).
      readinessProbe:
        exec:
          command: ["true"]
        initialDelaySeconds: 3
        periodSeconds: 1
EOF
}

log "starting impd"
start_impd
pass "impd up"

three_ready() {
  procs_json | python3 -c '
import json, sys
raw = sys.stdin.read().strip()
docs = []
if raw:
    dec = json.JSONDecoder()
    i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace():
            i += 1
        if i >= len(raw):
            break
        obj, end = dec.raw_decode(raw, i)
        docs.append(obj)
        i = end
ready = 0
for p in docs:
    for c in (p.get("status") or {}).get("conditions") or []:
        if c.get("type") == "Ready" and c.get("status") == "True":
            ready += 1
            break
sys.exit(0 if ready >= 3 else 1)
'
}

log "1. deploy replicas=3 RollingUpdate"
write_manifest v1
wait_for "3 Running procs" 30 three_running
wait_for "3 Ready procs" 30 three_ready
OLD_HASH="$(proc_summary | awk 'NR==1{print $2; exit}')"
[[ -n "$OLD_HASH" ]] || die "no template hash"
pass "3 procs Running+Ready (hash $OLD_HASH)"

log "2. D2: distinct IMP_REPLICA_INDEX in logs"
have_index_log() {
  local idx="$1"
  local name="rollme-${idx}-${OLD_HASH}"
  "${IMPCTL[@]}" logs "$name" 2>/dev/null | grep -q "index=${idx}"
}
wait_for "index=0 in logs" 20 have_index_log 0
wait_for "index=1 in logs" 20 have_index_log 1
wait_for "index=2 in logs" 20 have_index_log 2
pass "IMP_REPLICA_INDEX distinct per ordinal"

log "3. template change rolls high→low"
write_manifest v2
NEW_HASH=""
saw_mid_roll() {
  local summary new old0
  summary="$(proc_summary)" || return 1
  new="$(awk '$1=="2"{print $2; exit}' <<<"$summary")"
  old0="$(awk '$1=="0"{print $2; exit}' <<<"$summary")"
  [[ -n "$new" && -n "$old0" && "$new" != "$old0" && "$old0" == "$OLD_HASH" ]]
}
wait_for "ordinal 2 on new hash while 0 still old" 60 saw_mid_roll
NEW_HASH="$(proc_summary | awk '$1=="2"{print $2; exit}')"
[[ -n "$NEW_HASH" && "$NEW_HASH" != "$OLD_HASH" ]] || die "failed to observe new hash on ordinal 2"
mid_roll_high_first "$OLD_HASH" "$NEW_HASH" || die "expected ordinal 2=$NEW_HASH and ordinal 0=$OLD_HASH mid-roll; got: $(proc_summary | tr '\n' ';')"
pass "rolling high→low (2 updated, 0 still $OLD_HASH)"

roll_complete() {
  all_hash "$NEW_HASH" && three_running
}
wait_for "all procs on new hash" 90 roll_complete
pass "roll complete (hash $NEW_HASH)"

# describe shows RollingUpdate
DESC="$("${IMPCTL[@]}" describe daemon rollme)"
grep -q 'UpdateStrategy:[[:space:]]*RollingUpdate' <<<"$DESC" || die "describe missing RollingUpdate"
grep -q 'Partition:[[:space:]]*0' <<<"$DESC" || die "describe missing Partition"
pass "describe shows RollingUpdate + Partition"

log "4. clean SIGTERM"
stop_impd_term
grep -q 'impd stopped' "$IMPD_LOG" || die "missing 'impd stopped' in log"
pass "clean shutdown"

printf '\nACCEPT PASS: m4\n'
