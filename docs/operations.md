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

## Events and describe

`impctl describe` and `impctl events --for kind/name` remain the primary
operator story for crash-loops and probe failures. Event retention defaults
to 72h (`--event-ttl`).

## Contributor: real cgroup tests

Set `IMP_CGROUP_ROOT` to a writable cgroup v2 path when running
`internal/execd/cgroups` Linux integration tests against a real hierarchy.
