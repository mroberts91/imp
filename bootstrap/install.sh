#!/usr/bin/env bash
# install.sh - bootstrap imp in one of three modes.
#
#   ./install.sh ad-hoc            run as your user, XDG paths, no init system
#   sudo ./install.sh systemd      install a systemd unit + system user/dirs
#   sudo ./install.sh openrc       install an OpenRC service + system user/dirs
#
# The two system modes are identical except for which supervisor file they
# drop in; both create the same 'imp' system user and directories via the
# shared library. Ad-hoc shares none of that: it needs no root and no user.
#
# impd itself is configured by flags only: --socket, --data-dir,
# --manifest-dir, --log-level. This script's job is to create directories the
# invoking user can actually write, then hand impd the matching flags.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "$HERE/lib/common.sh"

usage() {
    cat >&2 <<EOF
${_c_bold}imp bootstrap${_c_reset}

Usage: $0 <mode> [options]

Modes:
  ad-hoc     Run as the current user under XDG dirs. For dev/testing.
             No root, no system user, no init integration.
  systemd    Install /etc/systemd/system/imp.service + system user/dirs.
  openrc     Install /etc/init.d/imp + system user/dirs.

Options (system modes):
  --start        enable and start the service after installing
  --bin-dir DIR  where to install impd/impctl (default: $IMP_BIN_DIR)

Options (ad-hoc):
  --run          exec impd in the foreground after creating dirs
  --detach       start impd in the background after creating dirs

Environment overrides (system modes): IMP_USER, IMP_GROUP, IMP_DATA_DIR,
  IMP_CONFIG_DIR, IMP_MANIFEST_DIR, IMP_LOG_DIR, IMP_RUN_DIR, IMP_SOCKET,
  IMP_BIN_SRC (dir holding built binaries).
  Example - run impd as an existing service account:
      sudo IMP_USER=svcacct IMP_GROUP=svcacct $0 systemd
  (sudo strips exported vars; pass them on sudo's command line as shown.)
  Pass the same overrides to uninstall.sh later.
EOF
    exit "${1:-2}"
}

# ---------------------------------------------------------------------------
# Mode: ad-hoc
# ---------------------------------------------------------------------------
cmd_adhoc() {
    refuse_root

    local run_mode="print"
    while [ $# -gt 0 ]; do
        case "$1" in
            --run)    run_mode="foreground" ;;
            --detach) run_mode="detach" ;;
            -h|--help) usage 0 ;;
            *) die "unknown ad-hoc option: $1" ;;
        esac
        shift
    done

    # XDG defaults - everything lands under $HOME (or your runtime dir),
    # owned by you, so there is nothing to chown and no permissions to grant.
    # data-dir holds the object store and, later, captured Proc logs; state
    # only holds impd's own stderr when detached.
    local data manifests state run sock
    data="${XDG_DATA_HOME:-$HOME/.local/share}/imp"
    manifests="${XDG_CONFIG_HOME:-$HOME/.config}/imp/manifests"
    state="${XDG_STATE_HOME:-$HOME/.local/state}/imp"
    run="${XDG_RUNTIME_DIR:-/tmp/imp-$(id -u)}/imp"
    sock="$run/impd.sock"
    # Unix socket paths are capped at ~108 bytes (sun_path); past that, bind
    # fails with a baffling "invalid argument".
    if [ "${#sock}" -gt 100 ]; then
        die "socket path is ${#sock} chars, over the unix-socket limit: $sock (set XDG_RUNTIME_DIR to something shorter)"
    fi

    mkdir -p "$data" "$manifests" "$state" "$run"
    log "data      $data"
    log "manifests $manifests"
    log "socket    $sock"

    local impd; impd="$(find_binary impd)"

    case "$run_mode" in
        foreground)
            log "starting impd in the foreground (Ctrl-C to stop)"
            log "in another shell: export IMP_SOCKET=$sock && impctl get daemons"
            exec "$impd" --socket "$sock" --data-dir "$data" --manifest-dir "$manifests"
            ;;
        detach)
            "$impd" --socket "$sock" --data-dir "$data" --manifest-dir "$manifests" \
                >>"$state/impd.log" 2>&1 &
            log "impd started (pid $!); logs -> $state/impd.log"
            log "point impctl at it with: export IMP_SOCKET=$sock"
            ;;
        print)
            cat >&2 <<EOF

Dirs are ready. Start impd with:

    $impd \\
        --socket $sock \\
        --data-dir $data \\
        --manifest-dir $manifests

and point impctl at it:

    export IMP_SOCKET=$sock
    impctl get daemons

(or re-run: $0 ad-hoc --run)
EOF
            ;;
    esac
}

# ---------------------------------------------------------------------------
# System-mode override plumbing
# ---------------------------------------------------------------------------
# The checked-in service files carry impd's built-in defaults: paths AND the
# imp:imp identity. When the invoker overrides any IMP_* value (or --bin-dir),
# the service must change too - via each init system's native override
# mechanism, so the installed service file itself stays pristine.
config_customized() {
    [ "$IMP_USER" != imp ] ||
    [ "$IMP_GROUP" != imp ] ||
    [ "$IMP_BIN_DIR" != /usr/local/bin ] ||
    [ "$IMP_DATA_DIR" != /var/lib/imp ] ||
    [ "$IMP_MANIFEST_DIR" != /etc/imp/manifests ] ||
    [ "$IMP_RUN_DIR" != /run/imp ] ||
    [ "$IMP_SOCKET" != /run/imp/impd.sock ] ||
    [ "$IMP_LOG_DIR" != /var/log/imp ]
}

write_systemd_dropin() {
    # RuntimeDirectory= only manages paths under /run.
    case "$IMP_RUN_DIR" in
        /run/?*) ;;
        *) die "IMP_RUN_DIR must live under /run for the systemd install (got: $IMP_RUN_DIR)" ;;
    esac
    mkdir -p /etc/systemd/system/imp.service.d
    cat > /etc/systemd/system/imp.service.d/override.conf <<EOF
# Written by bootstrap/install.sh from IMP_* overrides. Re-running the
# installer regenerates this file.
[Service]
User=${IMP_USER}
Group=${IMP_GROUP}
ExecStart=
ExecStart=${IMP_BIN_DIR}/impd --socket ${IMP_SOCKET} --data-dir ${IMP_DATA_DIR} --manifest-dir ${IMP_MANIFEST_DIR}
RuntimeDirectory=${IMP_RUN_DIR#/run/}
EOF
    log "drop-in /etc/systemd/system/imp.service.d/override.conf (custom config)"
}

write_openrc_confd() {
    mkdir -p /etc/conf.d
    cat > /etc/conf.d/imp <<EOF
# Written by bootstrap/install.sh from IMP_* overrides. Re-running the
# installer regenerates this file.
IMP_USER="${IMP_USER}"
IMP_GROUP="${IMP_GROUP}"
IMP_BIN_DIR="${IMP_BIN_DIR}"
IMP_DATA_DIR="${IMP_DATA_DIR}"
IMP_MANIFEST_DIR="${IMP_MANIFEST_DIR}"
IMP_RUN_DIR="${IMP_RUN_DIR}"
IMP_SOCKET="${IMP_SOCKET}"
IMP_LOG_FILE="${IMP_LOG_DIR}/impd.log"
EOF
    log "config /etc/conf.d/imp (custom config)"
}

# ---------------------------------------------------------------------------
# Mode: systemd
# ---------------------------------------------------------------------------
cmd_systemd() {
    need_root
    command -v systemctl >/dev/null 2>&1 || die "systemctl not found; is this a systemd host?"

    local do_start=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --start)   do_start=1 ;;
            --bin-dir) IMP_BIN_DIR="$2"; shift ;;
            -h|--help) usage 0 ;;
            *) die "unknown systemd option: $1" ;;
        esac
        shift
    done

    ensure_user
    ensure_dirs
    install_binaries

    install -m 0644 "$HERE/systemd/imp.service" /etc/systemd/system/imp.service
    log "unit /etc/systemd/system/imp.service"
    if config_customized; then
        write_systemd_dropin
    fi
    systemctl daemon-reload

    if [ "$do_start" -eq 1 ]; then
        systemctl enable --now imp
        log "service enabled and started"
    else
        log "installed. start with: systemctl enable --now imp"
    fi
    print_socket_access_note
}

# ---------------------------------------------------------------------------
# Mode: openrc
# ---------------------------------------------------------------------------
cmd_openrc() {
    need_root
    command -v rc-update >/dev/null 2>&1 || die "rc-update not found; is this an OpenRC host?"

    local do_start=0
    while [ $# -gt 0 ]; do
        case "$1" in
            --start)   do_start=1 ;;
            --bin-dir) IMP_BIN_DIR="$2"; shift ;;
            -h|--help) usage 0 ;;
            *) die "unknown openrc option: $1" ;;
        esac
        shift
    done

    ensure_user
    ensure_dirs
    install_binaries

    install -m 0755 "$HERE/openrc/imp" /etc/init.d/imp
    log "init script /etc/init.d/imp"
    if config_customized; then
        write_openrc_confd
    fi

    if [ "$do_start" -eq 1 ]; then
        rc-update add imp default
        rc-service imp start
        log "service added to default runlevel and started"
    else
        log "installed. enable with: rc-update add imp default && rc-service imp start"
    fi
    print_socket_access_note
}

# ---------------------------------------------------------------------------
main() {
    [ $# -ge 1 ] || usage 2
    local mode="$1"; shift
    case "$mode" in
        ad-hoc|adhoc) cmd_adhoc   "$@" ;;
        systemd)      cmd_systemd "$@" ;;
        openrc)       cmd_openrc  "$@" ;;
        -h|--help)    usage 0 ;;
        *) die "unknown mode: $mode (expected ad-hoc | systemd | openrc)" ;;
    esac
}

main "$@"
