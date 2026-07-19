# Operations notes

## Resource limits

Daemon / Proc templates may set `resources.limits` (`memory`, `cpuWeight`,
`pids`). Enforcement requires a **real** cgroup v2 `--cgroup-root` (systemd
`Delegate=yes`, or an OpenRC prepared subtree). A fake ad-hoc root only
exercises the code path — see [install.md](install.md).

## Probes

`livenessProbe` / `readinessProbe` (exec, httpGet, tcpSocket) gate Proc
`Ready` and trigger restarts. Exec probes run **inside** the Proc cgroup;
http/tcp dial from execd. Failures surface as Ready reasons (`ProbePending`,
`ProbeFailed`) and Events (`ProbeFailed`, `Unhealthy`).

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
