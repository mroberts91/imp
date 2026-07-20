#!/usr/bin/env bash
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0

# common.sh - shared helpers for imp's bootstrap scripts.
# Sourced by install.sh and uninstall.sh; not meant to be run directly.
#
# Everything here is idempotent: running it twice is safe and is a no-op the
# second time. That is what makes the installer re-runnable after an upgrade.
#
# How impd is actually configured: flags only (--socket, --data-dir,
# --manifest-dir, --log-level, --cgroup-root, --metrics-addr,
# --kill-procs-on-shutdown, --event-ttl). impd reads no environment variables
# for paths. The IMP_* variables below configure THESE SCRIPTS - they decide
# which paths get created and which flags the service files pass. The one env
# var the binaries themselves know is IMP_SOCKET, read by impctl to find the
# socket.

# ---------------------------------------------------------------------------
# Configuration (override by exporting before invoking install.sh)
# ---------------------------------------------------------------------------
: "${IMP_USER:=imp}"                       # system user impd runs as
: "${IMP_GROUP:=imp}"                      # its primary group
: "${IMP_DATA_DIR:=/var/lib/imp}"          # --data-dir: object store + Proc logs
: "${IMP_CONFIG_DIR:=/etc/imp}"            # parent of the manifest dir
: "${IMP_MANIFEST_DIR:=${IMP_CONFIG_DIR}/manifests}"  # --manifest-dir
: "${IMP_LOG_DIR:=/var/log/imp}"           # impd's own stderr log (OpenRC only;
                                           # under systemd journald captures it)
: "${IMP_RUN_DIR:=/run/imp}"               # holds the unix socket (tmpfs)
: "${IMP_SOCKET:=${IMP_RUN_DIR}/impd.sock}"  # --socket (and impctl's IMP_SOCKET)
: "${IMP_BIN_DIR:=/usr/local/bin}"         # where impd/impctl get installed
: "${IMP_PRIVILEGED:=0}"                   # 1: impd runs as root (M10-c) —
                                           # full per-Proc sandbox enforcement;
                                           # set by install.sh --privileged
# OpenRC: writable cgroup v2 subtree for --cgroup-root (systemd uses Delegate=).
: "${IMP_CGROUP_ROOT:=}"

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
_c_reset=$'\033[0m'; _c_bold=$'\033[1m'; _c_red=$'\033[31m'
_c_grn=$'\033[32m'; _c_ylw=$'\033[33m'
[ -t 2 ] || { _c_reset=; _c_bold=; _c_red=; _c_grn=; _c_ylw=; }

log()  { printf '%s==>%s %s\n'  "$_c_grn"  "$_c_reset" "$*" >&2; }
warn() { printf '%swarn:%s %s\n' "$_c_ylw" "$_c_reset" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$_c_red" "$_c_reset" "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
need_root() {
    [ "$(id -u)" -eq 0 ] || die "this mode must run as root (try: sudo $0 ...)"
}

refuse_root() {
    [ "$(id -u)" -ne 0 ] || die "ad-hoc mode must run as your normal user, not root"
}

# Locate a built binary. Search order:
#   $IMP_BIN_SRC (if exported) -> <repo>/bin (task build's output) -> PATH
# Anchored on THIS file's location (bootstrap/lib -> repo root), never the
# caller's: BASH_SOURCE[1] pointed at whichever frame called us, so going
# through install_binaries searched bootstrap/bin instead of the repo's bin
# and a fresh checkout's system install could never find its own build
# (second finding of the M10 Alpine runbook). Prints the resolved absolute
# path on stdout.
find_binary() {
    local name="$1" root candidate
    root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
    for candidate in \
        "${IMP_BIN_SRC:-}/$name" \
        "$root/bin/$name" \
        "$(command -v "$name" 2>/dev/null || true)"
    do
        [ -n "$candidate" ] && [ -x "$candidate" ] && { printf '%s\n' "$candidate"; return 0; }
    done
    die "could not find built binary '$name' (run 'task build', or export IMP_BIN_SRC=/path/to/dir)"
}

# Resolve the distro's nologin shell (path differs across distros).
nologin_shell() {
    for s in /usr/sbin/nologin /sbin/nologin /bin/false; do
        [ -x "$s" ] && { printf '%s\n' "$s"; return; }
    done
    printf '/bin/false\n'
}

# ---------------------------------------------------------------------------
# Idempotent primitives - these wrap chown/chmod so callers never touch them.
# ---------------------------------------------------------------------------

# make_dir PATH OWNER GROUP MODE
#   Creates PATH if absent, then forces ownership and permission bits.
#   chown sets who owns it (user:group); chmod sets the rwx bits (e.g. 0750 =
#   owner rwx, group r-x, others none). Re-running just re-asserts them.
make_dir() {
    local path="$1" owner="$2" group="$3" mode="$4"
    mkdir -p "$path"
    chown "$owner:$group" "$path"
    chmod "$mode" "$path"
    log "dir $path  ($owner:$group $mode)"
}

# install_bin SRC DESTDIR
#   Copies an executable into place with 0755 (owner rwx, group+others r-x).
install_bin() {
    local src="$1" destdir="$2" base
    base="$(basename "$src")"
    mkdir -p "$destdir"
    install -m 0755 "$src" "$destdir/$base"
    log "bin $destdir/$base"
}

# ---------------------------------------------------------------------------
# Shared setup for the two system modes (systemd + openrc).
# Ad-hoc mode does NOT call these - it owns everything as the invoking user.
# ---------------------------------------------------------------------------

# Creates $IMP_GROUP and $IMP_USER if they don't exist. An EXISTING account
# is left untouched (the "run impd as this pre-provisioned user" case): the
# init system forces both uid and gid at start (systemd User=/Group=, OpenRC
# command_user), so we never need to edit the account's own passwd/group
# entries - the group just has to exist.
ensure_user() {
    if ! getent group "$IMP_GROUP" >/dev/null 2>&1; then
        if command -v groupadd >/dev/null 2>&1; then
            groupadd --system "$IMP_GROUP"
        elif command -v addgroup >/dev/null 2>&1; then
            addgroup -S "$IMP_GROUP"    # busybox/alpine
        else
            die "no groupadd/addgroup found; create the '$IMP_GROUP' group manually"
        fi
        log "created system group $IMP_GROUP"
    fi
    if getent passwd "$IMP_USER" >/dev/null 2>&1; then
        log "user $IMP_USER already exists (service will run it with group $IMP_GROUP)"
        return
    fi
    local shell; shell="$(nologin_shell)"
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --no-create-home --shell "$shell" --gid "$IMP_GROUP" "$IMP_USER"
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -D -H -s "$shell" -G "$IMP_GROUP" "$IMP_USER"    # busybox/alpine
    else
        die "no useradd/adduser found; create the '$IMP_USER' system user manually"
    fi
    log "created system user $IMP_USER"
}

ensure_dirs() {
    # Data (SQLite object store, later Proc logs) and impd's own log file are
    # impd's to write -> owned by imp.
    make_dir "$IMP_DATA_DIR" "$IMP_USER" "$IMP_GROUP" 0750
    make_dir "$IMP_LOG_DIR"  "$IMP_USER" "$IMP_GROUP" 0750
    # Manifests are the admin's to write, impd's to read -> root owns, imp
    # reads via group. 0750 keeps them off-limits to everyone else.
    make_dir "$IMP_CONFIG_DIR"   root "$IMP_GROUP" 0750
    make_dir "$IMP_MANIFEST_DIR" root "$IMP_GROUP" 0750
    # NOTE: $IMP_RUN_DIR is on tmpfs and is (re)created at service start by
    # systemd's RuntimeDirectory= / OpenRC's checkpath - never at install time.
}

install_binaries() {
    # Split declaration from assignment (the cmd_adhoc pattern): die inside
    # "$( )" only exits the subshell, and inlining it in install_bin's
    # arguments would carry on with an empty path ("install: cannot stat ''"
    # — found live on the Alpine runbook). As standalone assignments under
    # set -e, a failed lookup aborts the install here instead.
    local impd impctl
    impd="$(find_binary impd)"
    impctl="$(find_binary impctl)"
    install_bin "$impd"   "$IMP_BIN_DIR"
    install_bin "$impctl" "$IMP_BIN_DIR"
}

# Print the "how to actually use it" epilogue common to system installs.
print_socket_access_note() {
    cat >&2 <<EOF

${_c_bold}Grant an admin access to impctl:${_c_reset}
  The api-server socket is ${IMP_SOCKET} (owner ${IMP_USER}:${IMP_GROUP}, mode 0660).
  Add human operators to the '${IMP_GROUP}' group so they can run impctl:

      sudo usermod -aG ${IMP_GROUP} <username>      # then re-login

  impctl looks for /run/imp/impd.sock by default; if you changed the socket
  path, point it there with the IMP_SOCKET env var or --socket flag.
EOF
}
