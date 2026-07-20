# Installing imp

Three install modes share one rule: **impd runs as user `U`, and `U` owns the
data/config dirs and the API socket.** Modes differ only in who creates `U`
and who supervises impd.

```sh
./bootstrap/install.sh ad-hoc            # rootless XDG dirs (dev)
sudo ./bootstrap/install.sh systemd --start
sudo ./bootstrap/install.sh openrc --start
```

See the top-level [README](../README.md) for the mode table and uninstall.

## Cgroup root (required for real limits)

impd places every Proc in a child of a writable **cgroup v2** directory
(`--cgroup-root`). Resolution order:

1. Explicit `--cgroup-root PATH`
2. This process’s cgroup from `/proc/self/cgroup` (works with systemd `Delegate=yes`)
3. `/sys/fs/cgroup/system.slice/imp.service` if present and writable
4. Otherwise startup fails with a message pointing here

### systemd

The unit ships with `Delegate=yes` and **omits** `--cgroup-root` so auto-detect
uses the service cgroup. Do **not** enable `ProtectControlGroups=true`.
`KillMode=process` is required so workload cgroups survive an impd restart
(D1 re-attach).

### OpenRC

OpenRC has no `Delegate=`. Prepare a subtree and pass it:

```sh
# as root, once (adjust controllers to match your hierarchy)
mkdir -p /sys/fs/cgroup/imp
# ensure the parent allows memory, cpu, pids in cgroup.subtree_control
chown imp:imp /sys/fs/cgroup/imp

# /etc/conf.d/imp (install.sh may write this):
IMP_CGROUP_ROOT=/sys/fs/cgroup/imp
```

The OpenRC init script appends `--cgroup-root ${IMP_CGROUP_ROOT}` when set.
`install.sh openrc` tries to create `/sys/fs/cgroup/imp` owned by `imp`.

### Ad-hoc / `task run` (fake root)

Rootless bootstrap creates a **synthetic** tree under
`$XDG_STATE_HOME/imp/cgroup` and passes `--cgroup-root`. Spawn and kill-tree
code paths work; **`resources.limits` are not enforced by the kernel** until
you point `--cgroup-root` at a real delegated cgroup v2 directory.

> Rootless `task run` creates a synthetic `--cgroup-root` so impd can start
> without privileges; set `--cgroup-root` to a delegated cgroup v2 directory
> (or install with systemd `Delegate=yes`) before relying on memory/cpu/pids
> limits or restart re-attachment.

## Privileged mode (M10-c)

The default system install runs impd as the unprivileged `imp` user — the
single-user model: every Proc runs as `imp`, and the privileged sandbox
knobs (`user:`, `capabilities`, `privateTmp`, `filesystem`) fail honestly
(exit 126) rather than silently not applying. For hosts that want per-Proc
users and full sandbox enforcement:

```sh
sudo ./bootstrap/install.sh systemd --privileged   # or: openrc --privileged
```

What changes:

- impd runs as **root**. systemd: a `privileged.conf` drop-in resets
  `User=`/`Group=` and the unit-level confinement lines (they are inherited
  by every Proc and would shadow imp's own per-Proc sandbox — which is the
  point of this mode). OpenRC: `IMP_PRIVILEGED=1` in `/etc/conf.d/imp`
  drops `command_user`.
- The socket stays operator-friendly: impd gets `--socket-group imp`, so
  members of the `imp` group keep running `impctl` without sudo.
- Every Proc should now declare `user:` — a template without one runs as
  root, which is presumably not what you meant.
- Re-running the installer **without** `--privileged` downgrades back to
  the single-user model.

Rootless dev (`ad-hoc`, `task run`) is unaffected.

## Alpine Linux / OpenRC notes (M10-c)

Alpine is a first-class OpenRC target: the bootstrap already uses busybox
`adduser -S`/`addgroup -S` when shadow's tools are absent, and impd/impctl
are static binaries (musl is a non-issue). Two Alpine specifics:

- **cgroup v2 unified mode:** older Alpine boots OpenRC in hybrid mode.
  Set `rc_cgroup_mode="unified"` in `/etc/rc.conf` (and reboot) before
  expecting memory/cpu/pids limits to apply; then follow the prepared-
  subtree recipe above (`prepare_openrc_cgroup` attempts it at install).
- The full verification runbook for a fresh Alpine VPS (privileged
  install, sandbox enforcement, respawn behavior) runs once per host
  class as part of the M10 close-out; findings land in this section.

## Shutdown behavior

By default, stopping impd **leaves Procs running** in their cgroups so a
restart can re-attach (D1). Ad-hoc/`task run` and acceptance scripts pass
`--kill-procs-on-shutdown` so Ctrl-C / SIGTERM still tears workloads down.
Operators who want production tear-down on stop can add that flag to the unit
or delete objects with `impctl`.

## Metrics

Default: Prometheus text on `http://127.0.0.1:9090/metrics`
(`--metrics-addr`, empty disables). See [operations.md](operations.md).
