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

`impctl info` shows the root a running impd resolved and whether it is
kernel-enforced. If the host's cgroup v2 mount itself is missing or
hybrid, see [Cgroups v2: detect, enable, recover](#cgroups-v2-detect-enable-recover).

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

## Alpine Linux / OpenRC notes (M10-c — verified on a fresh Alpine VPS, 2026-07-21)

Alpine is a verified OpenRC target: the full M10 runbook — cold
`--privileged` install, per-Proc user drops, kernel-enforced
memory/pids limits, `filesystem:` read-only-root with carve-outs,
Notifier paging, `kill -9` respawn with D1 re-attach, hands-free reboot,
and the m1/m10 acceptance gates on musl — passed end to end. The bootstrap
uses busybox `adduser -S`/`addgroup -S` when shadow's tools are absent,
and impd/impctl are static binaries (musl is a non-issue). What a fresh
minimal image needs before `install.sh openrc`:

1. **Enable the cgroups service.** Minimal cloud images often ship
   without it, leaving `/sys/fs/cgroup` empty (nothing mounted):

   ```sh
   rc-update add cgroups sysinit && rc-service cgroups start
   ```

2. **cgroup v2 unified mode.** Alpine's OpenRC defaults to hybrid; set
   `rc_cgroup_mode="unified"` in `/etc/rc.conf` first — with the service
   already running you can restart it instead of rebooting.

That's all: the init script's `start_pre` recreates the imp cgroup
subtree and re-enables its controllers on every boot (cgroupfs is
virtual — install-time preparation alone does not survive a reboot).

Incidentals, verified live: Task is packaged as `go-task` (the
acceptance scripts and Taskfile accept either; a
`ln -s "$(command -v go-task)" /usr/local/bin/task` makes docs match
verbatim); busybox `adduser <user> <group>` replaces `usermod -aG`, and
group membership applies at next login (`su - <user>` picks it up
immediately); building on the box wants Go at the `.go-version` pin —
if apk's Go lags, the official golang.org tarball runs fine on musl.

## Cgroups v2: detect, enable, recover

The runbook for a host where the cgroup v2 mount is missing, hybrid, or
broke after a reboot. Two symptoms point here:

- impd refuses to start: `no usable cgroup root: set --cgroup-root …`
  (deliberate — imp does not run half-enforced; see resolution order above).
- Limits silently do nothing: `impctl info` shows the cgroup root marked
  `FAKE — limits not kernel-enforced`.

**Start with `impctl info`** (daemon running) — it reports the cgroup root
impd is actually using and whether it sits on a real cgroup2 filesystem.
If impd is down, go straight to detection.

### Detect

```sh
cat /sys/fs/cgroup/cgroup.controllers
```

- Prints a list including `cpu`, `memory`, `pids` → **v2 unified is
  mounted** and the controllers imp needs exist. The mount is fine; if
  imp still fails, the problem is the imp subtree (see below).
- File missing but `/sys/fs/cgroup` contains controller-named
  subdirectories (`cpu/`, `memory/`, …) → **v1/hybrid layout**; imp
  requires unified.
- `/sys/fs/cgroup` empty or absent → **nothing is mounted** (minimal
  images that ship without a cgroups boot service).

### Enable — OpenRC (Alpine)

As root:

```sh
# 1. unified mode (Alpine defaults to hybrid) — edit, don't append a duplicate:
#    /etc/rc.conf:  rc_cgroup_mode="unified"
# 2. mount at every boot:
rc-update add cgroups sysinit
# 3. apply now (restart if it was already running in hybrid mode):
rc-service cgroups restart
cat /sys/fs/cgroup/cgroup.controllers   # should now list cpu memory pids
```

No reboot is strictly needed, but reboot once at the end to prove the
configuration persists.

### Enable — systemd

v2 unified has been the default since systemd 243 (all current distros).
A host still on legacy/hybrid was pinned by a kernel argument — remove
`systemd.unified_cgroup_hierarchy=0` / `systemd.legacy_systemd_cgroup_controller`
from the bootloader config, or force unified with
`systemd.unified_cgroup_hierarchy=1`, then reboot.

### The imp subtree heals itself

Once the mount exists, you never rebuild `/sys/fs/cgroup/imp` by hand:
the OpenRC init script's `start_pre` recreates the subtree, chowns it,
and re-enables its controllers on **every** service start (cgroupfs is
virtual — nothing done at install time survives a reboot). Under systemd,
`Delegate=yes` hands the service its own subtree the same way.

### Recovery sequence (proven on a fresh Alpine VPS, 2026-07-21)

```sh
cat /sys/fs/cgroup/cgroup.controllers    # empty dir → nothing mounted
vi /etc/rc.conf                          # rc_cgroup_mode="unified"
rc-update add cgroups sysinit
rc-service cgroups restart
cat /sys/fs/cgroup/cgroup.controllers    # cpuset cpu io memory … pids
rc-service imp restart                   # start_pre rebuilds /sys/fs/cgroup/imp
impctl info                              # Cgroup root: … (kernel-enforced)
```

## Shutdown behavior

By default, stopping impd **leaves Procs running** in their cgroups so a
restart can re-attach (D1). Ad-hoc/`task run` and acceptance scripts pass
`--kill-procs-on-shutdown` so Ctrl-C / SIGTERM still tears workloads down.
Operators who want production tear-down on stop can add that flag to the unit
or delete objects with `impctl`.

## Metrics

Default: Prometheus text on `http://127.0.0.1:9090/metrics`
(`--metrics-addr`, empty disables). See [operations.md](operations.md).
