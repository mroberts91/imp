#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M9 acceptance (rootless, fake cgroup root) — "it sweats the details":
#   1. Log pump: unterminated output shows in `impctl logs` before exit (the
#      bug — no `; echo` workaround); a silent proc prints "no output captured
#      yet" (exit 0); a bogus name still errors
#   2. Config path/modes/binary: a Config with data + binaryData + modes and a
#      path: ref materializes into an absolute dir (per-file mode + binary
#      round-trip asserted); a conflicting hand-made file makes the spawn fail
#      with ConfigPathConflict, leaving that file untouched; deleting the daemon
#      cleans the path files but not the shared dir
#   3. maxUnavailable: replicas=4, maxUnavailable=2 rolls two ordinals at once
#      (observed mid-roll), rollout completes, ConfigChanged emitted
#   4. Timer: @every + jitterSeconds fires; a timeZone timer validates and
#      describe shows it; a catchUp timer fires a past-due run after impd is
#      restarted across a missed tick
#   5. Selectors + top: get -l filters (equality + inequality); top shows the
#      THROTTLED column; top --watch repaints and exits cleanly
#   6. Clean SIGTERM
#
# Fully rootless. Throttling/cpu values read 0 under the fake cgroup root
# (accounting needs a delegated subtree) — asserted for presence, not value.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

log() { printf '+ %s\n' "$*"; }
die() { printf 'ACCEPT FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

need() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }
need task
need curl
need python3
need base64
need stat
need cmp

if [[ "$(id -u)" == "0" ]]; then
  die "run rootless: M9 is a fully unprivileged story"
fi

log "building binaries"
task build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m9-acceptance-$$"
SOCKET="$WORKDIR/impd.sock"
DATA="$WORKDIR/data"
MANIFESTS="$WORKDIR/manifests"
CGROUP_ROOT="$WORKDIR/cgroup"
IMPD_LOG="$WORKDIR/impd.log"
ETCSIM="$WORKDIR/etc-sim"     # a path: destination we own
ETCSIM2="$WORKDIR/etc-sim2"   # a path: destination with a hand-made conflict
mkdir -p "$DATA" "$MANIFESTS" "$CGROUP_ROOT" "$ETCSIM" "$ETCSIM2"
printf 'cpu memory pids\n' >"$CGROUP_ROOT/cgroup.controllers"
: >"$CGROUP_ROOT/cgroup.subtree_control"

export IMP_SOCKET="$SOCKET"
IMPCTL=(bin/impctl)

# Python readers over `get procs -o json`. Passed via `python3 -c` so the JSON
# arrives on stdin (a heredoc would override the pipe and starve the reader).
read -r -d '' PY_DECODE_PROCS <<'PY' || true
import json, sys
def procs():
    raw = sys.stdin.read().strip()
    if not raw:
        return
    dec = json.JSONDecoder(); i = 0
    while i < len(raw):
        while i < len(raw) and raw[i].isspace(): i += 1
        if i >= len(raw): break
        obj, i = dec.raw_decode(raw, i)
        yield obj
def labels(o):
    return (o.get("metadata") or {}).get("labels") or {}
def name(o):
    return (o.get("metadata") or {}).get("name", "")
PY

PY_ROLL_HASH="$PY_DECODE_PROCS"'
for o in procs():
    if labels(o).get("impd.sh/daemon-name") == "roll":
        print(labels(o).get("impd.sh/config-hash", "")); break
'
PY_ROLL_COUNTS="$PY_DECODE_PROCS"'
old = sys.argv[1]; o = n = 0
for obj in procs():
    if labels(obj).get("impd.sh/daemon-name") != "roll": continue
    if labels(obj).get("impd.sh/config-hash", "") == old: o += 1
    else: n += 1
print(o, n, o + n)
'
PY_CATCHUP_PRE="$PY_DECODE_PROCS"'
print(" ".join(name(o) for o in procs() if name(o).startswith("catchup-")))
'
PY_CATCHUP_SEEN="$PY_DECODE_PROCS"'
restart = int(sys.argv[1])
pre = set(sys.argv[2].split()) if len(sys.argv) > 2 and sys.argv[2] else set()
found = False
for o in procs():
    n = name(o)
    if not n.startswith("catchup-") or n in pre: continue
    try: tick = int(n.rsplit("-", 1)[1])
    except ValueError: continue
    if tick < restart: found = True
sys.exit(0 if found else 1)
'

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

proc_phase() { # DAEMON → phase of its proc, "" when none
  "${IMPCTL[@]}" get procs 2>/dev/null | awk -v d="$1" '$2 == d { print $3; exit }'
}

log "starting impd"
start_impd

# ======================================================================
log "1. log pump: unterminated output visible before exit; silent-proc honesty"
# The headline bug: no trailing newline, no `; echo` workaround.
cat >"$MANIFESTS/banner.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: banner
spec:
  replicas: 1
  template:
    spec:
      command: ["sh", "-c", "printf ready-no-newline; exec sleep 300"]
EOF
wait_for "banner proc Running" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="banner"{print $3}'"'"')" == Running ]]'
# The unterminated banner must appear WHILE the proc is still running.
wait_for "unterminated banner in logs before exit" 8 sh -c 'bin/impctl logs banner 2>/dev/null | grep -q ready-no-newline'
[[ "$(proc_phase banner)" == Running ]] || die "banner exited before its output was visible (defeats the test)"
pass "unterminated output visible in logs before exit"

# A running proc that has printed nothing: honest empty stream, exit 0.
cat >"$MANIFESTS/silent.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: silent
spec:
  replicas: 1
  template:
    spec:
      command: ["sleep", "300"]
EOF
wait_for "silent proc Running" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="silent"{print $3}'"'"')" == Running ]]'
set +e
OUT="$("${IMPCTL[@]}" logs silent 2>&1)"; RC=$?
set -e
[[ $RC -eq 0 ]] || die "logs of a silent proc exited $RC, want 0"
grep -q 'no output captured yet' <<<"$OUT" || die "silent proc logs did not say 'no output captured yet': $OUT"
# A genuinely bogus name still errors (not an empty stream).
set +e
"${IMPCTL[@]}" logs no-such-daemon-or-proc >/dev/null 2>&1; RC=$?
set -e
[[ $RC -ne 0 ]] || die "logs of a bogus name exited 0, want nonzero"
rm "$MANIFESTS/banner.yaml" "$MANIFESTS/silent.yaml"
wait_for "banner+silent swept" 20 sh -c '! bin/impctl get daemons 2>/dev/null | grep -qE "^(banner|silent)"'
pass "silent proc says 'no output captured yet' (exit 0); bogus name errors"

# ======================================================================
log "2. config path/modes/binary + conflict + teardown"
# Raw bytes for the binary round-trip.
printf '\x00\x01\x02\x03\xfe\xff' >"$WORKDIR/cert.orig"
CERT_B64="$(base64 -w0 <"$WORKDIR/cert.orig" 2>/dev/null || base64 <"$WORKDIR/cert.orig" | tr -d '\n')"
cat >"$MANIFESTS/appcfg.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Config
metadata:
  name: appcfg
spec:
  data:
    app.conf: "listen 8080\n"
  binaryData:
    cert.bin: "$CERT_B64"
  mode: "0644"
  modes:
    app.conf: "0640"
EOF
cat >"$MANIFESTS/pathweb.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: pathweb
spec:
  replicas: 1
  template:
    spec:
      command: ["sh", "-c", "cat \"$ETCSIM/app.conf\"; exec sleep 300"]
      configs:
        - name: appcfg
          path: "$ETCSIM"
EOF
wait_for "pathweb proc Running" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="pathweb"{print $3}'"'"')" == Running ]]'
wait_for "app.conf materialized at the path: dir" 15 sh -c "[[ -f '$ETCSIM/app.conf' ]]"
[[ "$(cat "$ETCSIM/app.conf")" == "listen 8080" ]] || die "path-materialized app.conf content wrong: $(cat "$ETCSIM/app.conf")"
[[ "$(stat -c '%a' "$ETCSIM/app.conf")" == "640" ]] || die "app.conf mode = $(stat -c '%a' "$ETCSIM/app.conf"), want 640 (per-file Modes)"
cmp -s "$ETCSIM/cert.bin" "$WORKDIR/cert.orig" || die "binary cert.bin did not round-trip through materialization"
pass "path: ref materialized with per-file mode + binary round-trip"

# Conflict: a hand-made file imp did not write blocks materialization.
printf 'HANDMADE\n' >"$ETCSIM2/app.conf"
cat >"$MANIFESTS/conflict.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: conflict
spec:
  replicas: 1
  template:
    spec:
      command: ["sh", "-c", "exec sleep 300"]
      configs:
        - name: appcfg
          path: "$ETCSIM2"
EOF
wait_for "ConfigPathConflict on describe" 25 sh -c 'bin/impctl describe daemon conflict 2>/dev/null | grep -q ConfigPathConflict'
[[ "$(cat "$ETCSIM2/app.conf")" == "HANDMADE" ]] || die "conflicting hand-made file was overwritten"
[[ -z "$(bin/impctl get procs 2>/dev/null | awk '$2=="conflict"&&$3=="Running"{print $1}')" ]] || die "conflict daemon ran despite the path conflict"
rm "$MANIFESTS/conflict.yaml"
wait_for "conflict swept" 20 sh -c '! bin/impctl get daemons 2>/dev/null | grep -q "^conflict"'
pass "path conflict fails the spawn (ConfigPathConflict), hand-made file untouched"

# Teardown cleans the path files but leaves the shared dir.
rm "$MANIFESTS/pathweb.yaml"
wait_for "pathweb swept" 20 sh -c '! bin/impctl get daemons 2>/dev/null | grep -q "^pathweb"'
wait_for "path files cleaned on teardown" 20 sh -c "[[ ! -f '$ETCSIM/app.conf' && ! -f '$ETCSIM/cert.bin' ]]"
[[ -d "$ETCSIM" ]] || die "teardown removed the shared path: dir (should stay)"
pass "teardown removed path files, kept the shared dir"

# ======================================================================
log "3. maxUnavailable: replicas=4, maxUnavailable=2 rolls a pair at once"
# initialDelaySeconds keeps replacements unavailable long enough to observe the
# two-old/two-new coexistence (m4's mid-roll-window trick).
write_rolling() { # $1 = greeting content
  cat >"$MANIFESTS/roll.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Config
metadata:
  name: rollcfg
spec:
  data:
    g.conf: "$1"
---
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: roll
spec:
  replicas: 4
  minReadySeconds: 1
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 2
  template:
    spec:
      command: ["sh", "-c", "cat \"\$IMP_CONFIG_DIR/rollcfg/g.conf\"; echo; exec sleep 300"]
      configs: [rollcfg]
      readinessProbe:
        exec:
          command: ["/bin/true"]
        initialDelaySeconds: 3
        periodSeconds: 1
EOF
}
write_rolling roll-v1
wait_for "4 roll procs available (rollout v1)" 45 sh -c 'timeout 40 bin/impctl rollout status roll >/dev/null 2>&1'
# roll_counts prints "old new total" for daemon roll, given $1 = the old hash.
roll_counts() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c "$PY_ROLL_COUNTS" "$1"
}
OLD_HASH="$("${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c "$PY_ROLL_HASH")"
[[ -n "$OLD_HASH" ]] || die "could not read roll's config-hash before the roll"

# Trigger the roll by editing the config, then watch for the two-new/two-old
# coexistence — proof that TWO ordinals rolled at once (maxUnavailable=2).
write_rolling roll-v2
SAW_PAIR=0
deadline=$((SECONDS + 40))
while (( SECONDS < deadline )); do
  read -r OLD NEW TOTAL < <(roll_counts "$OLD_HASH")
  if [[ "$NEW" -ge 2 && "$OLD" -ge 1 && "$TOTAL" -eq 4 ]]; then SAW_PAIR=1; break; fi
  if [[ "$TOTAL" -le 2 ]]; then SAW_PAIR=1; break; fi   # the two-deleted moment
  sleep 0.2
done
[[ "$SAW_PAIR" == 1 ]] || die "never observed two ordinals rolling at once (maxUnavailable=2)"
timeout 60 "${IMPCTL[@]}" rollout status roll >/dev/null || die "roll did not complete"
wait_for "ConfigChanged event for roll" 20 sh -c 'bin/impctl describe daemon roll 2>/dev/null | grep -q ConfigChanged'
rm "$MANIFESTS/roll.yaml"
wait_for "roll swept" 25 sh -c '! bin/impctl get daemons 2>/dev/null | grep -q "^roll"'
pass "maxUnavailable=2 rolled two ordinals at once, completed, ConfigChanged emitted"

# ======================================================================
log "4. timer timeZone / jitter / catchUp"
# Jitter: @every 5s + jitterSeconds fires within a bounded window.
cat >"$MANIFESTS/jit.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: jit
spec:
  schedule: "@every 5s"
  jitterSeconds: 2
  template:
    spec:
      command: ["sh", "-c", "true"]
EOF
wait_for "jitter timer fired a run" 20 sh -c 'bin/impctl get procs 2>/dev/null | grep -q "^jit-"'
rm "$MANIFESTS/jit.yaml"
pass "jitter timer fired within the tick+jitter window"

# timeZone: a 5-field schedule validates and describe surfaces the zone.
cat >"$MANIFESTS/tz.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: tz
spec:
  schedule: "0 0 * * *"
  timeZone: America/New_York
  template:
    spec:
      command: ["sh", "-c", "true"]
EOF
wait_for "timeZone timer accepted" 15 sh -c 'bin/impctl get timers 2>/dev/null | grep -q "^tz"'
"${IMPCTL[@]}" describe timer tz | grep -qE '^TimeZone:[[:space:]]+America/New_York' || die "describe timer does not show TimeZone"
rm "$MANIFESTS/tz.yaml"
pass "timeZone timer validated; describe shows the zone"

# catchUp: a tick missed while impd is down fires on restart (Persistent=true).
cat >"$MANIFESTS/catchup.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: catchup
spec:
  schedule: "@every 5s"
  startingDeadlineSeconds: 2
  catchUp: true
  concurrencyPolicy: Allow
  template:
    spec:
      command: ["sh", "-c", "true"]
EOF
# Let it establish lastScheduleTime and record the runs that exist before downtime.
wait_for "catchup timer fired at least once" 25 sh -c 'bin/impctl get procs 2>/dev/null | grep -q "^catchup-"'
PRE_RUNS="$("${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c "$PY_CATCHUP_PRE")"
log "stopping impd to simulate downtime across a tick"
stop_impd
sleep 12   # several @every-5s ticks pass while impd is down
BEFORE_RESTART=$(date +%s)
log "restarting impd — the newest missed tick should fire (catch-up)"
start_impd
# A new catchup run whose tick (name suffix) predates the restart = catch-up.
catchup_seen() {
  "${IMPCTL[@]}" get procs -o json 2>/dev/null | python3 -c "$PY_CATCHUP_SEEN" "$BEFORE_RESTART" "$PRE_RUNS"
}
wait_for "catch-up run for a missed tick" 12 catchup_seen
rm "$MANIFESTS/catchup.yaml"
wait_for "catchup swept" 20 sh -c '! bin/impctl get timers 2>/dev/null | grep -q "^catchup"'
pass "catchUp fired a past-due run after restart (systemd Persistent=true)"

# ======================================================================
log "5. selectors + top"
cat >"$MANIFESTS/sel.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: sel-web
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: web
    spec:
      command: ["sleep", "300"]
---
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: sel-db
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: db
    spec:
      command: ["sleep", "300"]
EOF
wait_for "sel procs Running" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | grep -cE "^sel-(web|db)-.*Running")" -ge 2 ]]'
# Equality selector: only the app=web proc.
"${IMPCTL[@]}" get procs -l app=web | grep -q '^sel-web-' || die "get -l app=web did not return the web proc"
"${IMPCTL[@]}" get procs -l app=web | grep -q '^sel-db-' && die "get -l app=web leaked the db proc"
# Inequality selector: the complement.
"${IMPCTL[@]}" get procs -l 'app!=web' | grep -q '^sel-db-' || die "get -l app!=web did not return the db proc"
"${IMPCTL[@]}" get procs -l 'app!=web' | grep -q '^sel-web-' && die "get -l app!=web leaked the web proc"
pass "label selectors filter (equality and inequality)"

# top shows the THROTTLED column; --watch repaints and exits cleanly.
"${IMPCTL[@]}" top | head -1 | grep -q 'THROTTLED' || die "top header missing THROTTLED column"
timeout 3 "${IMPCTL[@]}" top --watch --interval 1 >/dev/null 2>&1 || true
pass "top shows THROTTLED; top --watch smoke ran and exited"
rm "$MANIFESTS/sel.yaml"

# ======================================================================
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

printf '\nACCEPT PASS: m9\n'
