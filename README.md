# imp
A declarative, single-host process orchestrator. Kubernetes-style resources for managing daemons. A middle ground between systemd and Nomad.

# imp - Bootstrap

Three ways to run imp on a host. All three converge on the same runtime
invariant: **impd runs as some user `U`, and `U` owns the data/config/log dirs
and the socket.** The modes differ only in *who creates `U` and the dirs* and
*who supervises impd*.

```
bootstrap/
├── install.sh          # entrypoint: install.sh <ad-hoc|systemd|openrc>
├── uninstall.sh        # uninstall.sh <systemd|openrc> [--purge]
├── lib/common.sh       # shared, idempotent helpers (sourced, not run)
├── systemd/imp.service # systemd unit (single-user model)
└── openrc/imp          # OpenRC init script
```

## Modes

| | ad-hoc | systemd | openrc |
|---|---|---|---|
| Root needed | no | yes | yes |
| Runs as | you | `imp` (system user) | `imp` (system user) |
| Paths | XDG under `$HOME` | FHS (`/var/lib/imp`, …) | FHS |
| Supervised by | nothing (you) | systemd | OpenRC |
| Purpose | dev / testing | production | production |

### ad-hoc

```sh
./install.sh ad-hoc            # create XDG dirs, print the run command
./install.sh ad-hoc --run      # …and exec impd in the foreground
./install.sh ad-hoc --detach   # …and start impd in the background
```

Everything lands under `$HOME` (`~/.local/share/imp`, `~/.config/imp`,
`~/.local/state/imp`, `$XDG_RUNTIME_DIR/imp.sock`). No user creation, no
`chown`, nothing to clean up but a `rm -rf`.

### systemd

```sh
sudo ./install.sh systemd          # install user, dirs, unit
sudo ./install.sh systemd --start  # …and enable --now
```

### openrc

```sh
sudo ./install.sh openrc
sudo ./install.sh openrc --start
```

## Using impctl after a system install

The api-server socket is `/run/imp/impd.sock`, owned `imp:imp` mode 0660. Add
human operators to the `imp` group so they can talk to it:

```sh
sudo usermod -aG imp alice     # alice re-logs in, then `impctl` works
```

`impctl` resolves the socket as `--socket` flag -> `IMP_SOCK` env ->
`/run/imp/impd.sock` -> `$XDG_RUNTIME_DIR/imp.sock` (so it also finds an ad-hoc
instance with no configuration).

## Single-user vs multi-user supervision

These scripts ship the **single-user** model: impd runs as `imp` and launches
every Proc as `imp`. Running Procs as *other* users is privileged and is left
as a documented, commented **hook**:

- **systemd** - uncomment the `AmbientCapabilities` / `CapabilityBoundingSet`
  block in `systemd/imp.service` and remove `NoNewPrivileges=true`.
- **openrc** - either run impd as root (drop `command_user`) or
  `setcap cap_setuid,cap_setgid+ep /usr/local/bin/impd` (re-apply after every
  binary upgrade).

Until then, impd should validate that a Proc's `user`/`group` equals the
service user (reject otherwise) - the API field exists but is constrained.

## Contract these scripts assume of the binaries

The scripts define the interface impd/impctl must honor:

- **`impd serve`** - start the daemon (api-server + controllers + execd).
- **Path resolution** (both binaries): `--data-dir/--config-dir/--log-dir/`
  `--socket` flag -> `IMP_DATA_DIR` / `IMP_CONFIG_DIR` / `IMP_LOG_DIR` /
  `IMP_SOCK` env -> XDG -> built-in default. Manifests live under
  `$IMP_CONFIG_DIR/manifests`.
- **Socket** - impd creates `$IMP_SOCK` at mode 0660, owned by its own
  user/group, and removes it on clean shutdown.
- **Shutdown** - on SIGTERM, impd drains its Procs itself (per-Proc SIGTERM ->
  grace -> SIGKILL) and exits within the stop timeout (90s). This is why both
  supervisors signal impd only rather than the whole process group.
- **Build output** - `install.sh` finds `impd`/`impctl` via `IMP_BIN_SRC`,
  then `./bin`, `../bin`, then `PATH`.

## Uninstall

```sh
sudo ./uninstall.sh systemd            # remove unit + binaries, KEEP data/user
sudo ./uninstall.sh systemd --purge    # also remove data, config, logs, user
sudo ./uninstall.sh openrc  [--purge]
```

