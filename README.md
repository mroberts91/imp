# imp

A declarative, single-host process orchestrator: Kubernetes-style resources
for managing daemons — a middle ground between systemd and Nomad. One daemon
binary (`impd`) hosts the API, object store, controllers, and process
supervisor; a CLI (`impctl`) talks to it over a Unix domain socket.

**M1 (“it runs things”) is complete:** drop a Daemon manifest → Procs start →
`impctl get` / `impctl logs` work; crash loops back off; deletes cascade.

## Quick start (rootless)

```sh
task build
task run                    # foreground impd as you (XDG dirs + socket)
# in another shell:
task ctl -- apply -f examples/01-hello-daemon.yaml
task ctl -- get daemons
task ctl -- get procs
task ctl -- logs hello
task ctl -- delete daemon hello
```

Or point both binaries at an explicit stack:

```sh
task build
mkdir -p data manifests
bin/impd --socket ./imp.sock --data-dir ./data --manifest-dir ./manifests &
export IMP_SOCKET=./imp.sock
cp examples/01-hello-daemon.yaml manifests/   # GitOps: watcher applies it
bin/impctl get procs
bin/impctl logs hello
```

Regression gate for the M1 surface: `task accept:m1`.

Go version is pinned in `.go-version`. See `task --list` for build/check/cross
targets. Tutorial manifests live in [`examples/`](examples/).

## Bootstrap (install on a host)

Three ways to run imp. All converge on the same invariant: **impd runs as some
user `U`, and `U` owns the data/config dirs and the socket.** Modes differ only
in who creates `U`/dirs and who supervises impd.

```
bootstrap/
├── install.sh          # install.sh <ad-hoc|systemd|openrc>
├── uninstall.sh        # uninstall.sh <systemd|openrc> [--purge]
├── lib/common.sh
├── systemd/imp.service
└── openrc/imp
```

| | ad-hoc | systemd | openrc |
|---|---|---|---|
| Root needed | no | yes | yes |
| Runs as | you | `imp` (system user) | `imp` (system user) |
| Paths | XDG under `$HOME` | FHS | FHS |
| Supervised by | nothing (you) | systemd | OpenRC |
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

- **`impd` is flags-only.** It reads `--socket`, `--data-dir`, `--manifest-dir`
  (and `--log-level`). It does **not** read `IMP_*` env vars for paths — the
  install scripts resolve dirs and pass flags. There is no `impd serve` subcommand.
- **`impctl`** uses `--socket` / `IMP_SOCKET` to find the API.
- On SIGTERM, impd drains Procs (stopSignal → grace → SIGKILL) then exits.
  Init systems should signal impd only (`KillMode=process` / equivalent).

## Uninstall

```sh
sudo ./bootstrap/uninstall.sh systemd            # keep data/user
sudo ./bootstrap/uninstall.sh systemd --purge    # also remove data, config, user
sudo ./bootstrap/uninstall.sh openrc [--purge]
```
