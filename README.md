# imp

A declarative, single-host process orchestrator: Kubernetes-style resources
for managing daemons — a middle ground between systemd and Nomad. One daemon
binary (`impd`) hosts the API, object store, controllers, and process
supervisor; a CLI (`impctl`) talks to it over a Unix domain socket.

**M1–M6 complete:** drop a Daemon → Procs start → conditions/Events explain
failures → cgroup limits (memory/cpu/pids), probes (startup/liveness/
readiness), `/metrics`, restart re-attach → replicas + RollingUpdate →
Timer (cron replacement), log retention, `impctl top` → unit-file-grade
hardening (rlimits, nice, oomScoreAdjust, umask), rollout safety
(minReadySeconds, progressDeadlineSeconds), and operator verbs
(`impctl restart` / `run` / `rollout status`). Gates: `task accept:m1` …
`task accept:m6`.

## Quick start (rootless)

```sh
task build
task run                    # foreground impd as you (XDG dirs + socket)
# in another shell:
task ctl -- apply -f examples/01-hello-daemon.yaml
task ctl -- get daemons
task ctl -- get procs
task ctl -- logs hello
task ctl -- describe daemon hello
task ctl -- delete daemon hello
```

Rootless `task run` creates a synthetic `--cgroup-root` so impd can start
without privileges; set `--cgroup-root` to a delegated cgroup v2 directory
(or install with systemd `Delegate=yes`) before relying on memory/cpu/pids
limits or restart re-attachment.

Or point both binaries at an explicit stack:

```sh
task build
mkdir -p data manifests
# still need a cgroup root (fake tree or real delegated path):
mkdir -p data/cgroup && printf 'cpu memory pids\n' >data/cgroup/cgroup.controllers
: >data/cgroup/cgroup.subtree_control
bin/impd --socket ./imp.sock --data-dir ./data --manifest-dir ./manifests \
  --cgroup-root ./data/cgroup --kill-procs-on-shutdown &
export IMP_SOCKET=./imp.sock
cp examples/01-hello-daemon.yaml manifests/   # GitOps: watcher applies it
bin/impctl get procs
bin/impctl logs hello
```

Regression gates: `task accept:m1` through `task accept:m6`. Metrics
(when enabled): `curl -s http://127.0.0.1:9090/metrics`.

Go version is pinned in `.go-version`. See `task --list` for build/check/cross
targets. Tutorial manifests live in [`examples/`](examples/). Install and
cgroup details: [`docs/install.md`](docs/install.md),
[`docs/operations.md`](docs/operations.md).

## Bootstrap (install on a host)

Three ways to run imp. All converge on the same invariant: **impd runs as some
user `U`, and `U` owns the data/config dirs and the socket.** Modes differ only
in who creates `U`/dirs and who supervises impd.

```
bootstrap/
├── install.sh          # install.sh <ad-hoc|systemd|openrc>
├── uninstall.sh        # uninstall.sh <systemd|openrc> [--purge]
├── lib/common.sh
├── systemd/imp.service # Delegate=yes; KillMode=process
└── openrc/imp          # set IMP_CGROUP_ROOT for real limits
```

| | ad-hoc | systemd | openrc |
|---|---|---|---|
| Root needed | no | yes | yes |
| Runs as | you | `imp` (system user) | `imp` (system user) |
| Paths | XDG under `$HOME` | FHS | FHS |
| Supervised by | nothing (you) | systemd | OpenRC |
| Cgroups | fake tree (no kernel limits) | `Delegate=yes` auto-detect | prepared `--cgroup-root` |
| Purpose | dev / testing | production | production |

### ad-hoc

```sh
./bootstrap/install.sh ad-hoc            # create XDG dirs, print the run command
./bootstrap/install.sh ad-hoc --run      # …and exec impd in the foreground
./bootstrap/install.sh ad-hoc --detach   # …and start impd in the background
```

### systemd / openrc

```sh
sudo ./bootstrap/install.sh systemd --start
sudo ./bootstrap/install.sh openrc --start
```

After a system install the socket is `/run/imp/impd.sock` (`imp:imp` 0660).
Add operators to group `imp`. Socket resolution for `impctl`: `--socket` flag →
`IMP_SOCKET` env → `/run/imp/impd.sock` → `$XDG_RUNTIME_DIR/imp/impd.sock`.

## Contract: binaries vs install scripts

- **`impd` is flags-only.** Notable flags: `--socket`, `--data-dir`,
  `--manifest-dir`, `--cgroup-root` (empty → auto-detect), `--metrics-addr`,
  `--kill-procs-on-shutdown`, `--event-ttl`, `--log-level`. It does **not**
  read `IMP_*` env vars for paths — the install scripts resolve dirs and pass
  flags.
- **`impctl`** uses `--socket` / `IMP_SOCKET` to find the API.
- Default stop **leaves Procs running** for re-attach; init systems should
  signal impd only (`KillMode=process` / equivalent). Ad-hoc passes
  `--kill-procs-on-shutdown` so Ctrl-C tears workloads down.

## Uninstall

```sh
sudo ./bootstrap/uninstall.sh systemd            # keep data/user
sudo ./bootstrap/uninstall.sh systemd --purge    # also remove data, config, user
sudo ./bootstrap/uninstall.sh openrc [--purge]
```
