# Operations notes

## Resource limits

Daemon / Proc templates may set `resources.limits` (`memory`, `cpu`,
`cpuWeight`, `pids`). `cpu` is a hard quota — millicores (`"500m"`) or whole
cores (`"2"`) mapped to cgroup `cpu.max`; `cpuWeight` is the proportional
share. Enforcement requires a **real** cgroup v2 `--cgroup-root` (systemd
`Delegate=yes`, or an OpenRC prepared subtree). A fake ad-hoc root only
exercises the code path — see [install.md](install.md).

## Process hardening (unit-file translation)

The template speaks systemd's process-control dialect
([examples/10](../examples/10-hardened-daemon.yaml)):

| unit-file directive | template field |
|---|---|
| `LimitNOFILE=65536` | `rlimits: [{resource: nofile, soft: 65536}]` (one value sets soft+hard; `-1` = unlimited) |
| `Nice=5` | `nice: 5` |
| `OOMScoreAdjust=-100` | `oomScoreAdjust: -100` |
| `UMask=0027` | `umask: "0027"` |
| `CPUQuota=50%` | `resources.limits.cpu: "500m"` |
| `User=`/`Group=` | `user:` / `group:` |
| `NoNewPrivileges=yes` | `noNewPrivileges: true` |
| `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` | `capabilities.bounding: [net_bind_service]` (keep ONLY the listed caps) |
| `AmbientCapabilities=CAP_NET_BIND_SERVICE` | `capabilities.ambient: [net_bind_service]` (grant to a non-root `user:`) |
| `PrivateTmp=yes` | `privateTmp: true` |

Setup runs inside the child before exec, in systemd's order: rlimits while
still privileged (hard limits can be *raised* under a root impd), then
oom_score_adj and nice, then privateTmp and the capability bounding drops
(both need capabilities the identity drop clears), then the
capability-aware identity drop (with the ambient raise when requested),
then noNewPrivileges, then umask. A setup failure exits 126 with an
`imp child-setup: …` line in the Proc's own log.

**Sandboxing needs privilege.** `capabilities` (CAP_SETPCAP) and
`privateTmp` (CAP_SYS_ADMIN) only take effect under a privileged impd;
under a rootless impd they fail *loudly* — the Proc crash-loops with the
EPERM reason one `impctl logs` away — never silently no-op (the same
honesty posture as the fake cgroup root). `noNewPrivileges` works rootless.
Capability names are lowercase without the `CAP_` prefix; ambient names
must also appear in `bounding` when both lists are set; an empty bounding
list ("drop everything") is deliberately not expressible — run as an
unprivileged `user:` with `noNewPrivileges` instead. See
[examples/12](../examples/12-sandboxed-daemon.yaml) for the "nginx as
www-data on port 80" shape.

Sandbox settings are spawn-time: a Proc re-attached after an impd restart
(D1 adoption) is trusted to be what its status says — they cannot be
re-verified or re-applied, same as rlimits and nice. Note that with
`privateTmp` a `workingDir` under `/tmp` keeps referencing the host
directory it resolved to at spawn (the chdir happens before the namespace
setup), so use absolute paths at runtime to reach the private tmpfs.

Note (M6 behavior change): when `user:` is set, the process now gets that
user's **supplementary groups** (initgroups semantics, as systemd does).
Before M6 the supplementary list was empty.

## Probes

`livenessProbe` / `readinessProbe` (exec, httpGet, tcpSocket) gate Proc
`Ready` and trigger restarts. Exec probes run **inside** the Proc cgroup;
http/tcp dial from execd. Failures surface as Ready reasons (`ProbePending`,
`ProbeFailed`) and Events (`ProbeFailed`, `Unhealthy`).

`startupProbe` protects slow starters: until it succeeds, liveness and
readiness are held and the Proc stays Ready=False/`ProbePending`; past its
failure threshold the process is restarted like a liveness failure
([examples/11](../examples/11-startup-probe.yaml)).

## Rollout safety and operator verbs

`spec.minReadySeconds` makes a Proc count as *available* only after being
Ready that long; `status.availableReplicas` and the Available condition are
computed from it, and a RollingUpdate waits for availability before moving
to the next ordinal. `spec.progressDeadlineSeconds` (default 600) flips
Progressing to False/`ProgressDeadlineExceeded` (plus a Warning event) when
a rollout makes no progress for that long — a report, not a brake: the
controller keeps reconciling, and a fixed spec (new generation) resets the
deadline.

- `impctl rollout status DAEMON` watches until the rollout completes
  (exit 0) or exceeds its deadline (exit 1). There is no `rollout undo` /
  `history`: the manifest directory owns the spec, so the manifest file is
  the revision history.
- `impctl restart DAEMON` deletes the daemon's Procs and lets the
  controller recreate them — `systemctl restart` semantics (all replicas at
  once, brief downtime). The spec is untouched, so nothing fights the
  manifest directory.
- `impctl restart --rolling DAEMON` replaces procs one ordinal at a time
  (highest first, the RollingUpdate direction), waiting for each
  replacement to be *available* — Ready, plus `minReadySeconds` when set —
  before moving on. The per-ordinal wait budget is the daemon's own
  `progressDeadlineSeconds`; on timeout the walk exits nonzero and the
  controller's level-triggered convergence owns the rest (check
  `impctl rollout status`). With `replicas: 1` it is still a full-stop
  restart with a wait.
- `impctl run TIMER` fires a timer's run immediately (`{timer}-manual-<ts>`,
  annotated `impd.sh/manual`). Manual runs count as active for
  `concurrencyPolicy` but never advance `lastScheduleTime`.

## Metrics

Scrape `GET /metrics` on `--metrics-addr` (default `127.0.0.1:9090`).

| Metric | Meaning |
|---|---|
| `imp_proc_phase` | Current phase (gauge 1 on active phase label) |
| `imp_proc_restarts_total` | Restart count |
| `imp_probe_results_total` | Probe threshold crossings |
| `imp_reconcile_duration_seconds` | Controller reconcile latency |
| `imp_reconcile_errors_total` | Controller reconcile errors |
| `imp_proc_memory_bytes` | `memory.current` (~15s scrape) |
| `imp_proc_cpu_seconds_total` | Cumulative CPU from `cpu.stat` |

On a fake cgroup root, memory/cpu series may be present but are not
kernel-backed.

## Live usage: impctl top

`impctl top` shows per-Proc CPU%, memory, and pid count read straight from
impd's cgroup accounting over the socket — no metrics endpoint needed.
CPU% comes from two samples one second apart. Under a fake cgroup root
(rootless ad-hoc mode) values read as zero; real numbers need a delegated
cgroup subtree (see `docs/install.md`).

## Timers

`Timer` objects replace cron entries: a 5-field cron expression or
descriptor (`@hourly`, `@every 30s`) in host-local time fires one
run-to-completion Proc per tick (`restartPolicy` Never or OnFailure).
`concurrencyPolicy` defaults to Forbid — an overrunning job skips ticks
with `SkippedRun` events rather than stacking. Ticks missed while impd was
down are skipped with a `MissedRun` event (systemd `Persistent=false`
posture; widen with `spec.startingDeadlineSeconds`). Finished runs are
kept per `successfulHistoryLimit`/`failedHistoryLimit` (3/1) for
`impctl logs <run>`; `impctl describe timer NAME` shows the next fire
times and recent runs.

## Log retention

Per-Proc logs under `{data-dir}/logs/{proc}/` rotate at
`spec.template.spec.logRetention` (`maxSizeMB` 10, `maxBackups` 3,
`maxAgeDays` 0 = keep until backups retire them). The policy lives inside
the hashed template, so editing it rolls the Daemon like any other spec
change.

## Configuration files

A `Config` is a named set of files — `spec.data` maps each filename to inline
content. A Daemon (or Timer) template references Configs by name via
`spec.template.spec.configs: [name, …]`. Before every spawn, execd
materializes each referenced Config under a per-proc directory and injects
`IMP_CONFIG_DIR`; the process reads its config by path:

```
{data-dir}/configs/{proc}/{configName}/{filename}
IMP_CONFIG_DIR={data-dir}/configs/{proc}
```

so a daemon that takes a `-c` argument is configured with
`command: ["nginx", "-c", "$IMP_CONFIG_DIR/web/nginx.conf"]`. Content is inline
only — imp never reads or writes outside `--data-dir` **unless a ref sets an
explicit `path:`** (below).

**Object-form refs (`path:`, absolute destinations).** A ref may be an object
`{name, path}` instead of a bare name; its files then land in that absolute
directory — for a service that reads a fixed location it cannot be told to
change:

```yaml
configs:
  - name: nginx
    path: /etc/nginx        # files land here, not under IMP_CONFIG_DIR
```

Safety rails: the directory is created `0755` only if absent (an existing
admin-managed dir is left untouched); imp **refuses to overwrite a file it did
not write** — a hand-placed `/etc/nginx/nginx.conf` makes the spawn fail with a
`ConfigPathConflict` event (one `impctl describe daemon` away), it is never
clobbered; deleting the Daemon removes only the files imp wrote, never their
directory. imp tracks each proc's written paths under
`{data-dir}/configs/.paths/`. Changing a ref's `path:` rolls the Daemon (it
changes the template).

**Binary content and per-file modes.** `spec.binaryData` carries raw bytes
(base64 in YAML), with keys disjoint from `data`; `spec.modes` overrides the
Config-wide `spec.mode` for a single file (resolution: `modes[f]` → `mode` →
`0644`). Binary and text files share the 1 MiB / 64-file caps. `impctl describe
config NAME` lists every file with its resolved mode and text/binary type
(content itself is shown only by `get config -o yaml`).

**Roll-on-change is the point.** Referenced config content joins the Proc's
revision identity: editing a Config's `data` and re-applying rolls the Daemon
exactly like a template change, through the same strategy machinery (Recreate /
RollingUpdate, `minReadySeconds`, `progressDeadlineSeconds`, `rollout status`).
Procs of a config-referencing Daemon carry an `impd.sh/config-hash` label and a
name suffix that is the combined revision (template hash × resolved config
content); a config-only change emits a `ConfigChanged` event, a template change
still emits `TemplateChanged`. Daemons without config refs behave exactly as
before — same Proc names, no `config-hash` label, no roll on upgrade.
`impctl describe daemon NAME` shows the referenced `Configs:` and the current
`Revision:`.

**Honest failure paths.** A Daemon referencing a Config that does not exist
holds — it creates no Procs, emits a `ConfigMissing` Warning, and reports
`Progressing=ConfigMissing` — and self-heals the instant the Config appears
(its watch re-enqueues referencing Daemons). Deleting a Config a running Daemon
uses leaves the running process untouched (its files are already on disk); the
controller then holds new creations, and any respawn fails at materialization
with a `ConfigMissing`/`ConfigMaterializeFailed` event, one `impctl logs` /
`describe` away, until the Config returns. There is no deletion protection (no
finalizers — consistent with the M3 decision); the honesty is the safety net.
Timer runs are the deliberate asymmetry: the TimerController does not resolve
configs, so a scheduled run against a missing Config fails at spawn (the timer
already owns failed-run semantics).

**Modes, ownership, and size caps.** Files default to mode `0644`; set
`spec.mode: "0600"` on the Config for a secret-ish file. `mode` is plain
permission bits (`000`–`777`, and must grant owner read) — special bits
(setuid/setgid/sticky) are rejected, since they are meaningless for a regular
config file. When the Daemon drops
privilege (`user:`/`group:`), the per-proc config dir and files are chowned to
that user so it can read even a `0600` file. For a privilege-dropping service,
the `--data-dir` must be traversable (`o+x`) by that user — the per-proc dir is
private (`0700`) but the process must reach it; the default system-install data
dir (`0750 imp:imp`) blocks a service user not in group `imp`, so either widen
the data dir to `0751` or add the service user to group `imp`. Per Config:
≤ 1 MiB total content, ≤ 64 files, each filename a single path component. A
Secret kind (encryption/redaction) is deferred; a `0600` Config covers the
single-host case for now.

**Content visibility.** `impctl describe config NAME` prints file names, sizes
and mode — never content, which may be large or sensitive. `impctl get config
NAME -o yaml` shows everything for those who ask; that asymmetry is deliberate.

**D1 (adopted procs).** Config materialization is a spawn-time setting, like
rlimits and the sandbox: a Proc re-attached across an impd restart is not
re-materialized, and keeps the `IMP_CONFIG_DIR` its original spawn set (the
path is deterministic from the Proc name). The per-proc config dir is removed
when the Proc object is deleted, not on process exit.

## Failure notifications (Notifier)

The `Notifier` kind is the `OnFailure=` story (M10): declare one object and
imp pages you when a failure state *holds* — a Proc in CrashLoopBackOff
past `minRestarts` (default 3), a run-to-completion Proc (timer run,
one-off) that Failed, or a Daemon rollout stuck past its progress deadline.
Each firing creates one **notification run**: an ordinary Proc built from
the Notifier's template, spawned by execd, logged, evented, and listed by
`impctl get procs -l impd.sh/notifier-name=<name>`.

The failure facts arrive as environment variables — `IMP_NOTIFY_KIND`,
`IMP_NOTIFY_NAME`, `IMP_NOTIFY_PROC`, `IMP_NOTIFY_REASON`,
`IMP_NOTIFY_MESSAGE`, `IMP_NOTIFY_EXIT_CODE`, `IMP_NOTIFY_SIGNAL`,
`IMP_NOTIFY_RESTARTS`, `IMP_NOTIFY_SINCE` (always set; empty when not
applicable) — so a notifier is any executable. A complete ntfy pager is
three lines of curl (`examples/notifiers/ntfy.sh`); imp itself never
carries a notification transport.

Semantics worth knowing:

- **Cooldown, not dedup-forever:** one notification per (target, reason)
  per `cooldownSeconds` (default 1800). If the failure still holds when
  the cooldown expires, it fires again — imp's backoff never gives up on
  a crash-looping daemon (unlike systemd's start-limit-fail), so paging
  is what makes that safe.
- **Replicas collapse:** five crash-looping replicas of one Daemon are
  one page (the dedup key is the owning object), naming the worst
  replica.
- **No meta-alerting:** a failing notification run is a Failed Proc with
  logs for a human — it never triggers another notification. If the
  paging channel is down, there is nobody left to tell.
- **Scoping:** `selector` (same `k=v,k!=v` terms as `impctl get -l`)
  partitions targets between multiple Notifiers; zero Notifiers means no
  notifications.

See `examples/15-notifier.yaml` and `impctl describe notifier <name>`
(Recent Runs is the pager history).

## Filesystem sandboxing (M10)

`filesystem:` on a template is the `ProtectSystem=strict` /
`ProtectHome=` translation — see the hardening table above for the M6/M7
knobs it composes with:

```yaml
filesystem:
  readOnlyRoot: true            # every mount ro except /dev /proc /sys /run,
                                # private tmpfs, and readWritePaths
  protectHome: true             # /home /root /run/user hidden (empty ro tmpfs)
  readWritePaths: ["/var/lib/myservice"]
```

Needs a privileged impd (`install.sh --privileged`); rootless the spawn
fails honestly — exit 126, reason in `impctl logs`, CrashLoopBackOff — and
a Notifier pages about exactly that instead of the service silently
running unconfined.

## Secrets posture (deliberate non-feature)

imp has no secret-typed kind (decision of record, M10-d). The posture:

- **Files that are secrets** (keys, certs) stay on disk outside imp,
  owner-only mode; templates carry only their *paths* in `env:`. imp
  never reads them.
- **Capability strings** (an ntfy topic, a webhook URL) ride template
  `env:` in a manifest. Manifest-directory permissions are the guard —
  `/etc/imp/manifests` root-owned — which is the same trust model as a
  systemd `EnvironmentFile=` under `/etc`. They also land in imp's store
  (`/var/lib/imp`, service-owned) and in `describe` output (socket-gated).
- Keeping such values in `env:` rather than a `Config` also keeps them
  out of revision identity: rotating a topic does not roll the daemons.

If a real secret-at-rest need appears, `Secret` is a reserved kind name
(doc: naming reference) — it becomes its own kind, never a Config flag.

## Shell completion

`impctl completion bash|zsh|fish` emits the standard cobra script (e.g.
`impctl completion bash > /etc/bash_completion.d/impctl`). Resource-name
completion queries impd live and degrades silently when the socket is
unreachable.

## Events and describe

`impctl describe` and `impctl events --for kind/name` remain the primary
operator story for crash-loops and probe failures. Event retention defaults
to 72h (`--event-ttl`).

## Contributor: real cgroup tests

Set `IMP_CGROUP_ROOT` to a writable cgroup v2 path when running
`internal/execd/cgroups` Linux integration tests against a real hierarchy.

## Contributor: root sandbox tests

The capability, privateTmp, and filesystem (M10) *enforcement* paths need
root and are gated behind an explicit opt-in (the rootless gates only
prove validation and the honest-failure paths):

```sh
sudo IMP_ROOT_SANDBOX_TESTS=1 go test -run TestRootSandbox ./internal/execd/childsetup/
```
