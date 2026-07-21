#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M10 acceptance (rootless, fake cgroup root) — "it earns its keep":
#   1. CrashLoopBackOff notification: a crash-looping Daemon (exit 7) fires
#      the Notifier once restartCount ≥ minRestarts; the notification run
#      receives the IMP_NOTIFY_* contract (kind/name/reason/exit/restarts —
#      exit code proves lastTerminated, M10-e, end to end)
#   2. Cooldown: exactly one notification per (target, reason) inside the
#      window; the held level re-fires after it expires
#   3. RunFailed: a Timer whose run exits 3 notifies with EXIT_CODE=3 (the
#      ft cli-fetch contract, proven generically)
#   4. filesystem: rootless honesty — the shim exits 126 into
#      CrashLoopBackOff, and THAT pages too (the M10 features prove each
#      other); validation rejects a carve-out without readOnlyRoot
#   5. Selector scoping: a non-matching target never notifies
#   6. No meta-alerting: a Notifier whose own runs fail never breeds
#      notifications about them
#   7. impctl surface: get notifiers table, describe notifier (Recent Runs)
#   8. GC cascade: deleting the Notifier removes its run history
#   9. Clean SIGTERM
#
# Fully rootless. The root-gated enforcement half of M10-b lives in
#   sudo IMP_ROOT_SANDBOX_TESTS=1 go test -run TestRootSandbox ./internal/execd/childsetup/
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

log() { printf '+ %s\n' "$*"; }
die() { printf 'ACCEPT FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

need() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }
# Alpine packages Task as go-task (name clash with taskwarrior).
TASK="$(command -v task || command -v go-task || true)"
[ -n "$TASK" ] || die "need task (or go-task) on PATH"
need curl
need python3

if [[ "$(id -u)" == "0" ]]; then
  die "run rootless: the M10 gate is the unprivileged story (root enforcement is the gated go test)"
fi

log "building binaries"
"$TASK" build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m10-acceptance-$$"
SOCKET="$WORKDIR/impd.sock"
DATA="$WORKDIR/data"
MANIFESTS="$WORKDIR/manifests"
CGROUP_ROOT="$WORKDIR/cgroup"
IMPD_LOG="$WORKDIR/impd.log"
NOTIFY_LOG="$WORKDIR/notify.log"
mkdir -p "$DATA" "$MANIFESTS" "$CGROUP_ROOT"
printf 'cpu memory pids\n' >"$CGROUP_ROOT/cgroup.controllers"
: >"$CGROUP_ROOT/cgroup.subtree_control"
: >"$NOTIFY_LOG"

export IMP_SOCKET="$SOCKET"
IMPCTL=(bin/impctl)

# Counts pager runs recording the given target name, via the notified-*
# annotations (the dedup ledger).
read -r -d '' PY_COUNT_FOR_TARGET <<'PY' || true
import json, sys
target = sys.argv[1]
raw = sys.stdin.read().strip()
count = 0
if raw:
    dec = json.JSONDecoder(); i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace(): i += 1
        if i >= len(raw): break
        obj, i = dec.raw_decode(raw, i)
        meta = obj.get("metadata") or {}
        ann = meta.get("annotations") or {}
        if (meta.get("labels") or {}).get("impd.sh/notifier-name") == "pager" \
           and ann.get("impd.sh/notified-name") == target:
            count += 1
print(count)
PY
count_for_target() { # TARGET → number of pager runs recording it
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c "$PY_COUNT_FOR_TARGET" "$1"
}

IMPD_PID=""
start_impd() {
  bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" \
    --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= \
    --log-level info >>"$IMPD_LOG" 2>&1 &
  IMPD_PID=$!
  sleep 0.3
  kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
  local i
  for i in $(seq 1 50); do
    if curl -sf --unix-socket "$SOCKET" http://impd/healthz >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  cat "$IMPD_LOG"; die "healthz never ready"
}
stop_impd() {
  if [[ -n "${IMPD_PID:-}" ]] && kill -0 "$IMPD_PID" 2>/dev/null; then
    kill -TERM "$IMPD_PID" 2>/dev/null || true
    wait "$IMPD_PID" 2>/dev/null || true
  fi
  IMPD_PID=""
}
cleanup() {
  stop_impd
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

wait_for() {
  local what="$1" secs="$2"
  shift 2
  local deadline=$((SECONDS + secs))
  while (( SECONDS < deadline )); do
    if "$@"; then return 0; fi
    sleep 0.25
  done
  die "timed out waiting for $what (${secs}s)"
}

log "starting impd"
start_impd

# ======================================================================
log "0. manifests: one Notifier (the pager), its targets, and a decoy"
# The pager: every IMP_NOTIFY_* fact appended to one file — the notifier
# pattern is "any executable"; this one is four lines of shell.
cat >"$MANIFESTS/notifier.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Notifier
metadata:
  name: pager
spec:
  cooldownSeconds: 5
  minRestarts: 2
  selector: page=yes
  template:
    spec:
      command:
        - /bin/sh
        - -c
        - >-
          printf 'REC kind=%s name=%s reason=%s exit=%s restarts=%s proc=%s\n'
          "\$IMP_NOTIFY_KIND" "\$IMP_NOTIFY_NAME" "\$IMP_NOTIFY_REASON"
          "\$IMP_NOTIFY_EXIT_CODE" "\$IMP_NOTIFY_RESTARTS" "\$IMP_NOTIFY_PROC"
          >> $NOTIFY_LOG
EOF
cat >"$MANIFESTS/crash.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: crash
  labels:
    page: "yes"
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sh", "-c", "exit 7"]
EOF
# The decoy: crash-loops just as hard, but its labels miss the selector.
cat >"$MANIFESTS/quiet.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: quiet
  labels:
    page: "no"
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sh", "-c", "exit 1"]
EOF

# ======================================================================
log "1. crash-loop pages with the IMP_NOTIFY_* contract (exit code via lastTerminated)"
wait_for "crash-loop notification" 60 grep -q 'name=crash' "$NOTIFY_LOG"
grep -Eq 'REC kind=Daemon name=crash reason=CrashLoopBackOff exit=7 restarts=([2-9]|[1-9][0-9]+) proc=crash-' "$NOTIFY_LOG" \
  || { cat "$NOTIFY_LOG"; die "crash notification missing contract fields"; }
pass "IMP_NOTIFY_KIND/NAME/REASON/EXIT_CODE/RESTARTS/PROC all correct"

# ======================================================================
log "2. cooldown: one notification inside the window, re-fire after it"
first_count="$(count_for_target crash)"
[[ "$first_count" == 1 ]] || die "expected exactly 1 crash notification inside the cooldown, got $first_count"
sleep 2
[[ "$(count_for_target crash)" == 1 ]] || die "cooldown violated: a second notification inside the window"
# The level persists (the daemon still crash-loops), so the expiring
# cooldown re-fires.
crash_refired() { [[ "$(count_for_target crash)" -ge 2 ]]; }
wait_for "post-cooldown re-fire" 30 crash_refired
pass "cooldown held, then the held level re-fired"

# ======================================================================
log "3. a Timer run exiting 3 pages RunFailed with EXIT_CODE=3"
cat >"$MANIFESTS/failjob.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: failjob
  labels:
    page: "yes"
spec:
  schedule: "@every 2s"
  template:
    spec:
      command: ["/bin/sh", "-c", "exit 3"]
EOF
wait_for "RunFailed notification" 60 grep -Eq 'kind=Timer name=failjob reason=RunFailed exit=3' "$NOTIFY_LOG"
pass "exit 3 distinguishable from transient (the ft cli-fetch contract)"
rm "$MANIFESTS/failjob.yaml"

# ======================================================================
log "4. filesystem: rootless 126-honesty — and the crash-loop it causes pages"
cat >"$MANIFESTS/sandboxed.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: sandboxed
  labels:
    page: "yes"
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sleep", "1h"]
      filesystem:
        readOnlyRoot: true
EOF
wait_for "sandboxed 126 notification" 60 grep -Eq 'name=sandboxed reason=CrashLoopBackOff exit=126' "$NOTIFY_LOG"
"${IMPCTL[@]}" logs "$("${IMPCTL[@]}" get procs 2>/dev/null | awk '$2=="sandboxed"{print $1; exit}')" 2>/dev/null \
  | grep -q 'imp child-setup' || die "126 exit did not leave the child-setup reason in the proc log"
pass "unprivileged filesystem: fails honestly (126) and the failure pages"
rm "$MANIFESTS/sandboxed.yaml"

# Validation half: a carve-out without readOnlyRoot is rejected at apply.
if bad=$(cat <<'EOF' | curl -sf --unix-socket "$SOCKET" -X PUT -d @- http://impd/apis/impd.sh/v1alpha1/daemons/badfs 2>&1
{"apiVersion":"impd.sh/v1alpha1","kind":"Daemon","metadata":{"name":"badfs"},
 "spec":{"replicas":1,"template":{"spec":{"command":["/bin/true"],
 "filesystem":{"protectHome":true,"readWritePaths":["/var/lib/x"]}}}}}
EOF
); then
  die "invalid filesystem block accepted: $bad"
fi
pass "readWritePaths without readOnlyRoot rejected at admission"

# ======================================================================
log "5. selector scoping: the decoy crash-loops but never pages"
grep -q 'name=quiet' "$NOTIFY_LOG" && die "selector ignored: decoy 'quiet' was notified about"
pass "selector scoped notifications to page=yes targets"

# ======================================================================
log "6. no meta-alerting: a Notifier whose runs fail breeds nothing"
cat >"$MANIFESTS/broken.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Notifier
metadata:
  name: broken
spec:
  cooldownSeconds: 5
  minRestarts: 2
  selector: page=yes
  template:
    spec:
      command: ["/bin/sh", "-c", "exit 1"]
EOF
# Give broken time to fire (its target set matches crash) and fail.
broken_run_exists() { "${IMPCTL[@]}" get procs 2>/dev/null | grep -q '^broken-'; }
wait_for "broken notifier run exists" 30 broken_run_exists
sleep 3
grep -Eq 'name=(broken|pager)-' "$NOTIFY_LOG" && die "meta-alerting: a notification about a notification run"
pass "failed notification runs are visible Procs, never new signals"
rm "$MANIFESTS/broken.yaml"

# ======================================================================
log "7. impctl surface: get notifiers table, describe with Recent Runs"
"${IMPCTL[@]}" get notifiers | head -1 | grep -q 'COOLDOWN' || die "get notifiers header missing COOLDOWN"
"${IMPCTL[@]}" get notifiers | grep -Eq '^pager[[:space:]]+5s' || die "pager row missing/incorrect cooldown"
"${IMPCTL[@]}" describe notifier pager | grep -q 'Recent Runs:' || die "describe notifier missing Recent Runs"
"${IMPCTL[@]}" describe notifier pager | grep -q 'Daemon crash' || die "describe run history missing the crash target"
pass "get notifiers + describe notifier read correctly"

# ======================================================================
log "8. GC cascade: deleting the Notifier removes its run history"
rm "$MANIFESTS/notifier.yaml"
pager_gone() { ! "${IMPCTL[@]}" get notifiers 2>/dev/null | grep -q '^pager'; }
history_gone() { ! "${IMPCTL[@]}" get procs 2>/dev/null | grep -q '^pager-'; }
wait_for "notifier gone" 30 pager_gone
wait_for "run history collected" 60 history_gone
pass "ownerReference cascade collected the notification runs"

# ======================================================================
log "9. clean SIGTERM"
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

printf '\nACCEPT PASS: m10\n'
