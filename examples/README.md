# Getting started with imp manifests

imp manages processes the way Kubernetes manages containers: you declare
what should be running in YAML, apply it, and the system converges reality
toward it — restarts, replicas, updates, and cleanup included.

The examples here are a tutorial. Each file is self-contained and commented;
read them in order.

> **Status:** M1–M6 are complete. Controllers expand Daemons into Procs;
> execd starts and restarts them; RollingUpdate, Timers, hardening fields,
> and the `restart`/`run`/`rollout status` verbs all work.
> Try the “watch it run” commands below.

## Setup

```sh
task build
bin/impd --socket ./imp.sock --data-dir ./data --manifest-dir ./manifests &
export IMP_SOCKET=./imp.sock
```

(Or `./bootstrap/install.sh ad-hoc --run` for XDG paths — see the top-level
README. `impctl` finds an ad-hoc socket without configuration.)

## The two resources you write

- **Daemon** — "keep N replicas of this process running, and here's how to
  update and stop them." This is what you almost always want.
- **Proc** — one supervised process instance. Daemons create and own Procs;
  you only write one directly for an unmanaged one-shot. Proc specs are
  immutable — change means replace.

A third kind, **Event**, is system-generated audit output (`impctl get
events`); you never author it.

## The tutorial

| | File | Introduces |
|---|---|---|
| Basic | [`01-hello-daemon.yaml`](01-hello-daemon.yaml) | The minimal Daemon; what the defaults are |
| Basic | [`02-oneshot-proc.yaml`](02-oneshot-proc.yaml) | Bare Procs; `restartPolicy` for run-to-completion tasks |
| Intermediate | [`03-env-and-workdir.yaml`](03-env-and-workdir.yaml) | `env`, `workingDir`, argv vs shell |
| Intermediate | [`04-graceful-shutdown.yaml`](04-graceful-shutdown.yaml) | `stopSignal`, grace period, the TERM→wait→KILL sequence |
| Advanced | [`05-replicas-and-labels.yaml`](05-replicas-and-labels.yaml) | Replicas, labels/annotations, Recreate updates, `IMP_REPLICA_INDEX` |
| Advanced | [`06-full-stack.yaml`](06-full-stack.yaml) | Multi-document files, a multi-service app, every spec field |
| Advanced | [`07-rolling-update.yaml`](07-rolling-update.yaml) | RollingUpdate: one ordinal at a time, high→low |
| Advanced | [`08-timer.yaml`](08-timer.yaml) | Timer: scheduled run-to-completion Procs (cron replacement) |
| Advanced | [`09-log-retention.yaml`](09-log-retention.yaml) | Per-daemon log rotation policy |
| Advanced | [`10-hardened-daemon.yaml`](10-hardened-daemon.yaml) | Unit-file hardening: `rlimits`, `nice`, `oomScoreAdjust`, `umask`, `limits.cpu` |
| Advanced | [`11-startup-probe.yaml`](11-startup-probe.yaml) | `startupProbe`: protecting slow starters from liveness |

Work through one:

```sh
impctl apply -f examples/01-hello-daemon.yaml
impctl get daemons
impctl get procs           # the Proc the daemon controller created
impctl logs hello -f       # a Daemon name resolves to its Proc's logs
impctl delete daemon hello # cascades: owned Procs are stopped and removed
```

## Two ways to apply manifests

**impctl** (imperative, like `kubectl`): `impctl apply -f FILE` writes the
objects; you own their lifecycle and delete them yourself.

**The manifest directory** (declarative, like static pods / GitOps): drop
files into the directory passed as `--manifest-dir` (system installs use
`/etc/imp/manifests/`). impd watches the directory and keeps the store in
sync — edit a file and it's re-applied, delete a file (or a document from it)
and the objects it declared are deleted. Objects synced this way are
annotated `impd.sh/managed-by: manifest`; don't fight the watcher by editing
them with impctl.

## Rules worth knowing before you write your own

- Every document needs `apiVersion: impd.sh/v1alpha1`, a `kind`, and a
  `metadata.name` (lowercase DNS-style: `[a-z0-9-]`, dot-separated segments).
  Identity is (kind, name) — there are no namespaces.
- Unknown fields are rejected, not ignored — a typo is an error naming the
  field, not silent misconfiguration.
- `command` is argv: `["nginx", "-g", "daemon off;"]` execs the binary
  directly. Wrap in `sh -c` only when you need shell features.
- Defaults applied server-side: `replicas: 1`, `restartPolicy: Always`,
  `stopSignal: TERM`, `terminationGracePeriodSeconds: 30`,
  `updateStrategy.type: Recreate` (or `RollingUpdate` with `partition: 0`).
- The `impd.sh/` label prefix belongs to the system. On Procs you'll see
  `impd.sh/daemon-name`, `impd.sh/replica-index`, and `impd.sh/template-hash`
  — the hash is how updates work: editing a Daemon's template never mutates
  running Procs; replacement Procs are created from the new template
  (`Recreate`: stop all old, then start new; `RollingUpdate`: one ordinal
  at a time, highest first). execd injects `IMP_REPLICA_INDEX` for
  per-replica config (ports, etc.); imp does not rewrite args/env for you.
