#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# M8 acceptance (rootless, fake cgroup root) — "it carries config":
#   1. Materialize + consume: a Config's file lands under IMP_CONFIG_DIR and the
#      process reads it; the Proc carries impd.sh/config-hash; describe shows it
#   2. Roll on change: editing the Config content rolls the Daemon, emits
#      ConfigChanged (not TemplateChanged), new content in logs, rollout green
#   3. Validation: a "../evil" filename and a duplicate config ref are 422s
#      naming the exact field
#   4. Missing-config hold: a Daemon referencing an absent Config creates no
#      Procs, warns ConfigMissing, reports Progressing/ConfigMissing — then
#      self-heals when the Config appears
#   5. Delete-in-use honesty: deleting an in-use Config leaves the running Proc
#      alone; a restart cannot bring it back (held) until the Config returns
#   6. Cleanup: deleting the Daemon removes its per-proc config dir
#   7. Clean SIGTERM
#
# No root-gated tests — the whole story is rootless (a first since M2): file
# writes under --data-dir, no privilege anywhere in M8.
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

if [[ "$(id -u)" == "0" ]]; then
  die "run rootless: M8 is a fully unprivileged story"
fi

log "building binaries"
"$TASK" build >/dev/null

WORKDIR="${TMPDIR:-/tmp}/imp-m8-acceptance-$$"
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
    if "$@"; then return 0; fi
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

# proc_name DAEMON: the name of the daemon's (single) proc, "" when none.
# Table columns: NAME DAEMON PHASE RESTARTS PID AGE.
proc_name() {
  "${IMPCTL[@]}" get procs 2>/dev/null | awk -v d="$1" '$2 == d { print $1; exit }'
}

# proc_phase DAEMON: the phase of the daemon's proc ("" when none).
proc_phase() {
  "${IMPCTL[@]}" get procs 2>/dev/null | awk -v d="$1" '$2 == d { print $3; exit }'
}

log "starting impd"
bin/impd --socket "$SOCKET" --data-dir "$DATA" --manifest-dir "$MANIFESTS" \
  --cgroup-root "$CGROUP_ROOT" --kill-procs-on-shutdown --metrics-addr= \
  --log-level info >"$IMPD_LOG" 2>&1 &
IMPD_PID=$!
sleep 0.3
kill -0 "$IMPD_PID" 2>/dev/null || { cat "$IMPD_LOG"; die "impd failed to start"; }
wait_healthz || { cat "$IMPD_LOG"; die "healthz never ready"; }

write_app_config() { # $1 = greeting content
  cat >"$MANIFESTS/app.yaml" <<EOF
apiVersion: impd.sh/v1alpha1
kind: Config
metadata:
  name: app
spec:
  data:
    greeting.conf: "$1"
EOF
}

write_web_daemon() {
  cat >"$MANIFESTS/web.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  replicas: 1
  minReadySeconds: 1
  template:
    spec:
      command: ["sh", "-c", "cat \"$IMP_CONFIG_DIR/app/greeting.conf\"; echo; exec sleep 300"]
      configs: [app]
      readinessProbe:
        exec:
          command: ["/bin/true"]
        periodSeconds: 1
EOF
}

# ---- 1. materialize + consume -------------------------------------------
log "1. Config materialized under IMP_CONFIG_DIR and read by the process"
write_app_config "hello-from-config-v1"
write_web_daemon

wait_for "web proc Running" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="web"{print $3}'"'"')" == Running ]]'
wait_for "config content in web logs" 15 sh -c 'bin/impctl logs web 2>/dev/null | grep -q hello-from-config-v1'
# The Proc carries the config-hash label (revision identity, M8-g).
"${IMPCTL[@]}" get procs -o json | grep -q 'impd.sh/config-hash' || die "proc missing impd.sh/config-hash label"
# describe surfaces the reference and the current revision.
"${IMPCTL[@]}" describe daemon web | grep -qE '^Configs:[[:space:]]+app' || die "describe daemon does not show Configs: app"
"${IMPCTL[@]}" describe daemon web | grep -qE '^Revision:' || die "describe daemon does not show a Revision"
WEB_PROC_V1="$(proc_name web)"
pass "materialized + consumed; config-hash label present; describe shows the ref"

# ---- 2. roll on change ---------------------------------------------------
log "2. editing the Config rolls the Daemon (ConfigChanged, new content, green)"
write_app_config "hello-from-config-v2"
wait_for "web proc name to change (roll)" 30 sh -c '[[ -n "'"$WEB_PROC_V1"'" ]] && [[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="web"{print $1}'"'"')" != "'"$WEB_PROC_V1"'" ]]'
wait_for "new config content in logs" 20 sh -c 'bin/impctl logs web 2>/dev/null | grep -q hello-from-config-v2'
wait_for "ConfigChanged event" 20 sh -c 'bin/impctl describe daemon web 2>/dev/null | grep -q ConfigChanged'
"${IMPCTL[@]}" describe daemon web | grep -q TemplateChanged && die "config-only change emitted TemplateChanged"
timeout 60 "${IMPCTL[@]}" rollout status web >/dev/null || die "rollout not green after config roll"
pass "config edit rolled the Daemon: new proc, new content, ConfigChanged, rollout green"

# ---- 3. validation -------------------------------------------------------
log "3. validation rejects a traversal filename and duplicate refs"
cat >"$WORKDIR/bad-file.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Config
metadata:
  name: bad-file
spec:
  data:
    "../evil": "x"
EOF
if OUT="$("${IMPCTL[@]}" apply -f "$WORKDIR/bad-file.yaml" 2>&1)"; then
  die "apply accepted a traversal filename"
fi
grep -q 'spec.data\[../evil\]' <<<"$OUT" || die "rejection does not name spec.data[../evil]: $OUT"

cat >"$WORKDIR/dup-ref.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: dup-ref
spec:
  template:
    spec:
      command: ["sleep", "300"]
      configs: [app, app]
EOF
if OUT="$("${IMPCTL[@]}" apply -f "$WORKDIR/dup-ref.yaml" 2>&1)"; then
  die "apply accepted duplicate config refs"
fi
grep -q 'spec.template.spec.configs\[1\]' <<<"$OUT" || die "duplicate rejection does not name configs[1]: $OUT"
pass "traversal filename and duplicate refs rejected, fields named"

# ---- 4. missing-config hold + self-heal ---------------------------------
log "4. a Daemon referencing an absent Config holds, then self-heals"
cat >"$MANIFESTS/held.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: held
spec:
  replicas: 1
  template:
    spec:
      command: ["sh", "-c", "exec sleep 300"]
      configs: [missing-cfg]
EOF
wait_for "ConfigMissing warning for held" 20 sh -c 'bin/impctl describe daemon held 2>/dev/null | grep -q ConfigMissing'
wait_for "held Progressing reason ConfigMissing" 20 \
  sh -c '[[ "$(bin/impctl describe daemon held 2>/dev/null | awk "/^Conditions:/{c=1} c&&/Progressing/{print \$3; exit}")" == ConfigMissing ]]'
[[ -z "$(proc_name held)" ]] || die "held created procs while its Config was missing"

cat >"$MANIFESTS/missing-cfg.yaml" <<'EOF'
apiVersion: impd.sh/v1alpha1
kind: Config
metadata:
  name: missing-cfg
spec:
  data:
    noop.conf: "present now"
EOF
wait_for "held proc Running after Config appears" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="held"{print $3}'"'"')" == Running ]]'
timeout 60 "${IMPCTL[@]}" rollout status held >/dev/null || die "held rollout not green after self-heal"
rm "$MANIFESTS/held.yaml" "$MANIFESTS/missing-cfg.yaml"
wait_for "held swept" 20 sh -c '! bin/impctl get daemons 2>/dev/null | grep -q "^held"'
pass "missing config held (no procs, Progressing/ConfigMissing), self-healed on appearance"

# ---- 5. delete-in-use honesty -------------------------------------------
log "5. deleting an in-use Config leaves the running proc, blocks respawn"
RUNNING_BEFORE="$(proc_name web)"
[[ -n "$RUNNING_BEFORE" ]] || die "web has no running proc before delete-in-use"
rm "$MANIFESTS/app.yaml"
wait_for "app Config swept" 20 sh -c '! bin/impctl get configs 2>/dev/null | grep -q "^app"'
sleep 3
# The already-running proc is untouched — its files are already on disk.
[[ "$(proc_name web)" == "$RUNNING_BEFORE" ]] || die "running proc was disturbed by config deletion"
[[ "$(proc_phase web)" == Running ]] || die "running proc left Running state after config deletion"
# A restart cannot bring web back until the Config returns: the controller holds.
"${IMPCTL[@]}" restart web >/dev/null 2>&1 || true
wait_for "web held after restart with Config gone" 25 sh -c '[[ "$(bin/impctl describe daemon web 2>/dev/null | awk "/^Conditions:/{c=1} c&&/Progressing/{print \$3; exit}")" == ConfigMissing ]]'
[[ -z "$(proc_name web)" ]] || die "web recreated a proc while its Config was gone"
# Re-apply the Config: web recovers.
write_app_config "hello-from-config-v3"
wait_for "web proc Running after Config restored" 25 sh -c '[[ "$(bin/impctl get procs 2>/dev/null | awk '"'"'$2=="web"{print $3}'"'"')" == Running ]]'
timeout 60 "${IMPCTL[@]}" rollout status web >/dev/null || die "web did not recover after Config restore"
pass "delete-in-use: running proc untouched, respawn held, recovered on restore"

# ---- 6. cleanup removes the per-proc config dir -------------------------
log "6. deleting the Daemon removes its per-proc config dir"
WEB_PROC_FINAL="$(proc_name web)"
[[ -n "$WEB_PROC_FINAL" ]] || die "no web proc to track for cleanup"
[[ -d "$DATA/configs/$WEB_PROC_FINAL" ]] || die "config dir was never materialized at $DATA/configs/$WEB_PROC_FINAL"
rm "$MANIFESTS/web.yaml"
wait_for "web swept" 20 sh -c '! bin/impctl get daemons 2>/dev/null | grep -q "^web"'
wait_for "per-proc config dir removed" 20 sh -c '[[ ! -d "'"$DATA/configs/$WEB_PROC_FINAL"'" ]]'
pass "per-proc config dir removed on Daemon deletion"

# ---- 7. clean shutdown --------------------------------------------------
log "7. clean SIGTERM"
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

printf '\nACCEPT PASS: m8\n'
