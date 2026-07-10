#!/usr/bin/env bash
# uninstall.sh - remove an imp system install (systemd or openrc).
#
#   sudo ./uninstall.sh systemd [--purge]
#   sudo ./uninstall.sh openrc  [--purge]
#
# Without --purge, data/config/logs and the 'imp' user are LEFT IN PLACE
# (safe default - you keep your state). --purge removes them too.
#
# Ad-hoc installs live entirely under your $HOME; remove them yourself with:
#   rm -rf "${XDG_DATA_HOME:-$HOME/.local/share}/imp" \
#          "${XDG_CONFIG_HOME:-$HOME/.config}/imp" \
#          "${XDG_STATE_HOME:-$HOME/.local/state}/imp"

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$HERE/lib/common.sh"

purge_common() {
    warn "purging state, config and logs"
    rm -rf "$IMP_DATA_DIR" "$IMP_LOG_DIR" "$IMP_CONFIG_DIR" "$IMP_RUN_DIR"
    # Only remove the account when it is the default identity this installer
    # itself creates. A custom IMP_USER was (or may have been) provisioned by
    # the admin for wider purposes - never delete an account we don't own.
    if [ "$IMP_USER" = imp ] && getent passwd imp >/dev/null 2>&1; then
        userdel imp 2>/dev/null || deluser imp 2>/dev/null || \
            warn "could not remove user imp; remove it manually"
        log "removed user imp"
    elif [ "$IMP_USER" != imp ]; then
        log "leaving user '$IMP_USER' in place (custom identity; remove it yourself if unwanted)"
    fi
    rm -f "$IMP_BIN_DIR/impd" "$IMP_BIN_DIR/impctl"
}

cmd_systemd() {
    need_root
    local purge=0; [ "${1:-}" = "--purge" ] && purge=1
    systemctl disable --now imp 2>/dev/null || true
    rm -f /etc/systemd/system/imp.service
    rm -rf /etc/systemd/system/imp.service.d
    systemctl daemon-reload 2>/dev/null || true
    log "removed systemd unit"
    if [ "$purge" -eq 1 ]; then
        purge_common
    else
        log "kept state/config/logs and user (re-run with --purge to remove)"
    fi
}

cmd_openrc() {
    need_root
    local purge=0; [ "${1:-}" = "--purge" ] && purge=1
    rc-service imp stop 2>/dev/null || true
    rc-update del imp default 2>/dev/null || true
    rm -f /etc/init.d/imp /etc/conf.d/imp
    log "removed openrc service"
    if [ "$purge" -eq 1 ]; then
        purge_common
    else
        log "kept state/config/logs and user (re-run with --purge to remove)"
    fi
}

[ $# -ge 1 ] || die "usage: $0 <systemd|openrc> [--purge]"
mode="$1"; shift
case "$mode" in
    systemd) cmd_systemd "$@" ;;
    openrc)  cmd_openrc  "$@" ;;
    *) die "unknown mode: $mode (expected systemd | openrc)" ;;
esac
