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

## Shutdown behavior

By default, stopping impd **leaves Procs running** in their cgroups so a
restart can re-attach (D1). Ad-hoc/`task run` and acceptance scripts pass
`--kill-procs-on-shutdown` so Ctrl-C / SIGTERM still tears workloads down.
Operators who want production tear-down on stop can add that flag to the unit
or delete objects with `impctl`.

## Metrics

Default: Prometheus text on `http://127.0.0.1:9090/metrics`
(`--metrics-addr`, empty disables). See [operations.md](operations.md).
