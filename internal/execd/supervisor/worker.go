// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/execd/probes"
	"github.com/mroberts91/imp/internal/metrics"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

type worker struct {
	m        *Manager
	key      string
	name     string
	client   *client.Client
	store    *cache.Store
	logs     LogCapture
	clock    clock.Clock
	recorder *recorder.Recorder
	log      *slog.Logger

	wakeCh   chan struct{}
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// detach is set when Manager.Stop leaves children alive (D1 default).
	detach atomic.Bool

	rt             RuntimeRecord
	cmd            *exec.Cmd
	exitCh         chan exitResult
	probeCh        chan probes.ResultEvent
	adoptWatchStop chan struct{}
	// pidfd watch state (linux; zero when the legacy poll watch is used).
	adoptPidfd     int
	adoptPollW     int
	adoptPidfdDone chan struct{}
}

func newWorker(m *Manager, key string) *worker {
	name := key
	if parts := strings.SplitN(key, "/", 2); len(parts) == 2 {
		name = parts[1]
	}
	return &worker{
		m:        m,
		key:      key,
		name:     name,
		client:   m.client,
		store:    m.store,
		logs:     m.logs,
		clock:    m.clock,
		recorder: m.recorder,
		log:      m.log.With("key", key),
		wakeCh:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		probeCh:  make(chan probes.ResultEvent, 16),
	}
}

func (w *worker) wake() {
	select {
	case w.wakeCh <- struct{}{}:
	default:
	}
}

func (w *worker) requestStop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.wake()
}

// requestDetach shuts the worker down without stopping the child process.
func (w *worker) requestDetach() {
	w.stopOnce.Do(func() {
		w.detach.Store(true)
		close(w.stopCh)
	})
	w.wake()
}

func (w *worker) run() {
	defer close(w.done)
	defer w.m.removeWorker(w.key)
	defer func() {
		w.stopProbes()
		w.stopAdoptWatch()
		if !w.detach.Load() {
			// Proc-object deletion (not detach): tear down its logs and its
			// per-proc config dir. Config files are reproducible from the
			// store; on detach they are left in place for the adopted proc.
			_ = w.logs.Remove(w.name)
			_ = w.m.configs.Remove(w.name)
		}
	}()

	for {
		w.syncOnce()

		_, exists := w.store.GetByKey(w.key)
		stopping := false
		select {
		case <-w.stopCh:
			stopping = true
		default:
		}
		if w.detach.Load() {
			return
		}
		if (!exists || stopping) && !w.rt.Running {
			return
		}

		delay := w.nextWait()
		var timer clock.Timer
		var timerC <-chan time.Time
		if delay > 0 {
			timer = w.clock.NewTimer(delay)
			timerC = timer.C()
		}

		select {
		case <-w.wakeCh:
		case <-w.stopCh:
		case ev := <-w.probeCh:
			w.applyProbe(ev)
		case res, ok := <-w.exitChOrNil():
			if ok {
				w.onExit(res)
			}
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (w *worker) exitChOrNil() <-chan exitResult {
	if w.exitCh == nil {
		return nil
	}
	return w.exitCh
}

func (w *worker) nextWait() time.Duration {
	if w.rt.Running || w.rt.BackoffUntil.IsZero() {
		return 0
	}
	d := w.rt.BackoffUntil.Sub(w.clock.Now())
	if d < 0 {
		return 0
	}
	return d
}

func (w *worker) syncOnce() {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("worker panicked",
				"error", fmt.Sprintf("panic: %v", r),
				"stack", string(debug.Stack()))
		}
	}()

	raw, exists := w.store.GetByKey(w.key)
	var p *v1alpha1.Proc
	if exists {
		var obj v1alpha1.Proc
		if err := json.Unmarshal(raw, &obj); err != nil {
			w.log.Warn("skipping unreadable proc", "error", err)
			return
		}
		p = &obj
		w.name = obj.Metadata.Name
	}

	select {
	case <-w.stopCh:
		if w.detach.Load() {
			return
		}
		exists = false
	default:
	}

	policy := v1alpha1.RestartPolicyAlways
	if p != nil && p.Spec.RestartPolicy != "" {
		policy = p.Spec.RestartPolicy
	}

	action := computeProcAction(exists, policy, &w.rt, w.clock.Now())
	ctx := context.Background()

	switch action.Kind {
	case ActionNone:
		if exists && p != nil {
			_ = w.projectStatus(ctx, p, policy)
		}
	case ActionWait:
		if p != nil {
			_ = w.projectStatus(ctx, p, policy)
			w.emit(ctx, p, v1alpha1.EventTypeWarning, v1alpha1.ReasonBackOff,
				fmt.Sprintf("Back-off restarting failed proc: next at %s", action.Until.UTC().Format(time.RFC3339)))
		}
	case ActionStart:
		if p == nil {
			return
		}
		if err := w.doStart(ctx, p); err != nil {
			w.log.Error("start failed", "error", err)
			finished := w.clock.Now()
			noteExit(&w.rt, ExitInfo{Nonzero: true, FinishedAt: finished, Message: err.Error()}, 0)
			_ = w.projectStatus(ctx, p, policy)
		}
	case ActionStop:
		w.doStop(ctx, p)
	}
}

func (w *worker) doStart(ctx context.Context, p *v1alpha1.Proc) error {
	if p.Metadata.UID == "" {
		return fmt.Errorf("proc %s: empty UID (needed for cgroup path)", p.Metadata.Name)
	}

	cgPath, err := w.m.cgroups.Ensure(p.Metadata.UID)
	if err != nil {
		return fmt.Errorf("cgroup ensure: %w", err)
	}
	if err := w.m.cgroups.ApplyLimits(cgPath, p.Spec.Resources.Limits); err != nil {
		return fmt.Errorf("cgroup limits: %w", err)
	}

	// Materialize referenced Config files before opening logs, so a failure
	// needs no log/cmd cleanup. A missing Config (or a write failure) is an
	// honest start failure: a Warning event plus the Waiting status message,
	// one `impctl logs`/`describe` away (M6-h spirit without the shim).
	if err := w.m.configs.Materialize(ctx, p); err != nil {
		reason := v1alpha1.ReasonConfigMaterializeFailed
		switch {
		case errors.Is(err, v1alpha1.ErrNotFound):
			reason = v1alpha1.ReasonConfigMissing
		case errors.Is(err, v1alpha1.ErrConfigPathConflict):
			reason = v1alpha1.ReasonConfigPathConflict
		}
		w.emit(ctx, p, v1alpha1.EventTypeWarning, reason, fmt.Sprintf("Config materialization failed: %v", err))
		return fmt.Errorf("materializing configs: %w", err)
	}

	stdout, stderr, err := w.logs.Open(p.Metadata.Name, p.Spec.LogRetention)
	if err != nil {
		return err
	}

	configDir := ""
	if len(p.Spec.Configs) > 0 {
		configDir = w.m.configs.Dir(p.Metadata.Name)
	}
	cmd, err := buildCmd(p, configDir, stdout, stderr)
	if err != nil {
		w.logs.CloseCapture(p.Metadata.Name)
		return err
	}
	if err := cmd.Start(); err != nil {
		w.logs.CloseCapture(p.Metadata.Name)
		return fmt.Errorf("starting: %w", err)
	}

	pid := cmd.Process.Pid
	if err := w.m.cgroups.Add(cgPath, pid); err != nil {
		w.log.Warn("cgroup add failed; killing started process", "pid", pid, "error", err)
		_ = killGroup(pid, unix.SIGKILL)
		w.logs.CloseCapture(p.Metadata.Name)
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("cgroup add: %w", err)
	}

	ticks, tErr := readProcStartTicks(pid)
	if tErr != nil {
		w.log.Warn("reading procStartTicks", "pid", pid, "error", tErr)
	}
	started := w.clock.Now()
	noteStart(&w.rt, pid, ticks, started)
	w.rt.CgroupPath = cgPath
	w.rt.Adopted = false
	w.cmd = cmd
	w.exitCh = make(chan exitResult, 1)
	go func(c *exec.Cmd, ch chan exitResult) {
		err := c.Wait()
		ch <- exitResult{state: c.ProcessState, err: err}
	}(cmd, w.exitCh)

	w.emit(ctx, p, v1alpha1.EventTypeNormal, v1alpha1.ReasonStarted,
		fmt.Sprintf("Started proc %s pid=%d", p.Metadata.Name, pid))
	w.publishCgroupSnap(p)
	w.startProbes(p)
	return w.projectStatus(ctx, p, p.Spec.RestartPolicy)
}

func (w *worker) doStop(ctx context.Context, p *v1alpha1.Proc) {
	w.stopProbes()
	// Read the probe-kill latch before clearing it: a stop triggered by a
	// liveness/startup probe failure uses that probe's grace when it set one
	// (M9-l). Deletion/rollout stops leave LivenessFailed false and are
	// unaffected.
	probeKill := w.rt.LivenessFailed
	probeGrace := w.rt.ProbeKillGrace
	w.rt.LivenessFailed = false
	w.rt.ProbeKillGrace = nil
	w.clearCgroupSnap()
	cgPath := w.rt.CgroupPath
	if !w.rt.Running {
		w.cleanupCgroup(cgPath)
		return
	}
	sig := v1alpha1.DefaultStopSignal
	grace := int64(30)
	if p != nil {
		sig = stopSignalOf(p)
		grace = gracePeriod(p)
		if probeKill && probeGrace != nil {
			grace = *probeGrace
		}
		w.emit(ctx, p, v1alpha1.EventTypeNormal, v1alpha1.ReasonKilling,
			fmt.Sprintf("Stopping proc %s", p.Metadata.Name))
	}

	if w.cmd != nil && w.exitCh != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Duration(grace+5)*time.Second)
		defer cancel()
		res := stopProcess(stopCtx, w.clock, w.cmd, sig, grace, w.exitCh, w.log)
		if res.state != nil || res.err != nil {
			w.onExit(res)
		} else if w.rt.Running {
			finished := w.clock.Now()
			noteExit(&w.rt, ExitInfo{Nonzero: true, Signal: "KILL", FinishedAt: finished}, finished.Sub(w.rt.StartedAt))
			w.cmd = nil
			w.exitCh = nil
			w.logs.CloseCapture(w.name)
		}
	} else {
		// Adopted process: signal it, then rely on cgroup Kill for the
		// group. The pidfd path cannot mis-target a recycled pid; the
		// group kill remains the fallback for legacy watches.
		if w.rt.PID > 0 && !w.signalAdopted(mustSignal(sig)) {
			_ = killGroup(w.rt.PID, mustSignal(sig))
		}
		w.stopAdoptWatch()
		if w.rt.Running {
			finished := w.clock.Now()
			noteExit(&w.rt, ExitInfo{Nonzero: true, Signal: "KILL", FinishedAt: finished}, finished.Sub(w.rt.StartedAt))
			w.exitCh = nil
			w.logs.CloseCapture(w.name)
		}
	}

	if cgPath != "" {
		if err := w.m.cgroups.Kill(cgPath); err != nil {
			w.log.Warn("cgroup kill failed", "cgroup", cgPath, "error", err)
		}
	}
	w.cleanupCgroup(cgPath)

	if p != nil {
		_ = w.projectStatus(ctx, p, p.Spec.RestartPolicy)
	}
}

func mustSignal(name string) unix.Signal {
	sig, err := parseSignal(name)
	if err != nil {
		return unix.SIGTERM
	}
	return sig
}

func (w *worker) cleanupCgroup(path string) {
	if path == "" {
		path = w.rt.CgroupPath
	}
	if path == "" {
		return
	}
	if err := w.m.cgroups.Remove(path); err != nil {
		w.log.Warn("cgroup remove failed", "cgroup", path, "error", err)
	}
	w.rt.CgroupPath = ""
}

func (w *worker) onExit(res exitResult) {
	w.stopProbes()
	w.stopAdoptWatch()
	w.clearCgroupSnap()
	finished := w.clock.Now()
	info := dissectExit(res.err, res.state, finished)
	healthy := time.Duration(0)
	if !w.rt.StartedAt.IsZero() {
		healthy = finished.Sub(w.rt.StartedAt)
	}
	noteExit(&w.rt, info, healthy)
	w.rt.Adopted = false
	w.rt.LivenessFailed = false
	w.rt.ProbeKillGrace = nil
	w.rt.HasReadinessProbe = false
	w.rt.ReadinessOK = false
	w.rt.ReadinessFailed = false
	w.cmd = nil
	w.exitCh = nil
	w.logs.CloseCapture(w.name)

	ctx := context.Background()
	raw, ok := w.store.GetByKey(w.key)
	if !ok {
		return
	}
	var p v1alpha1.Proc
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	w.emit(ctx, &p, v1alpha1.EventTypeNormal, v1alpha1.ReasonExited, fmtExitMessage(info))
	_ = w.projectStatus(ctx, &p, p.Spec.RestartPolicy)
}

func (w *worker) startProbes(p *v1alpha1.Proc) {
	w.stopProbes()
	w.rt.HasReadinessProbe = p.Spec.ReadinessProbe != nil
	w.rt.ReadinessOK = false
	w.rt.ReadinessFailed = false
	w.rt.LivenessFailed = false
	w.rt.ProbeKillGrace = nil
	w.rt.HasStartupProbe = p.Spec.StartupProbe != nil
	w.rt.StartupDone = false
	if p.Spec.StartupProbe == nil && p.Spec.LivenessProbe == nil && p.Spec.ReadinessProbe == nil {
		return
	}
	w.m.probes.Start(w.key, w.rt.CgroupPath, w.rt.StartedAt, p.Spec.StartupProbe, p.Spec.LivenessProbe, p.Spec.ReadinessProbe)
}

func (w *worker) publishCgroupSnap(p *v1alpha1.Proc) {
	daemon, timer := "", ""
	if p != nil {
		daemon = p.Metadata.Labels[v1alpha1.LabelDaemonName]
		timer = p.Metadata.Labels[v1alpha1.LabelTimerName]
	}
	w.m.setCgroupSnap(w.key, metrics.ProcCgroup{
		Proc:   w.name,
		Daemon: daemon,
		Timer:  timer,
		Path:   w.rt.CgroupPath,
	})
}

func (w *worker) clearCgroupSnap() {
	w.m.clearCgroupSnap(w.key)
}

func (w *worker) stopProbes() {
	w.m.probes.Stop(w.key)
}

func (w *worker) applyProbe(ev probes.ResultEvent) {
	ctx := context.Background()
	raw, ok := w.store.GetByKey(w.key)
	var p *v1alpha1.Proc
	if ok {
		var obj v1alpha1.Proc
		if err := json.Unmarshal(raw, &obj); err == nil {
			p = &obj
		}
	}

	switch ev.ProbeType {
	case probes.ProbeStartup:
		if ev.Result == probes.ResultSuccess {
			// Startup complete: release the held liveness/readiness workers.
			w.rt.StartupDone = true
			w.m.probes.Release(w.key)
		} else if ev.Message != "initial probe state" {
			// Effective failure (threshold crossed): the process never came
			// up — restart it through the liveness latch.
			w.rt.LivenessFailed = true
			if p != nil {
				w.rt.ProbeKillGrace = startupProbeGrace(p)
				w.emit(ctx, p, v1alpha1.EventTypeWarning, v1alpha1.ReasonProbeFailed,
					fmt.Sprintf("Startup probe failed: %s", ev.Message))
				w.emit(ctx, p, v1alpha1.EventTypeWarning, v1alpha1.ReasonUnhealthy,
					"Startup probe failed; killing and restarting")
			}
		}
	case probes.ProbeReadiness:
		if ev.Result == probes.ResultSuccess {
			w.rt.ReadinessOK = true
			w.rt.ReadinessFailed = false
		} else {
			w.rt.ReadinessOK = false
			if ev.Message != "initial probe state" {
				w.rt.ReadinessFailed = true
				if p != nil {
					w.emit(ctx, p, v1alpha1.EventTypeWarning, v1alpha1.ReasonProbeFailed,
						fmt.Sprintf("Readiness probe failed: %s", ev.Message))
				}
			}
		}
	case probes.ProbeLiveness:
		if ev.Result == probes.ResultFailure {
			w.rt.LivenessFailed = true
			if p != nil {
				w.rt.ProbeKillGrace = livenessProbeGrace(p)
				w.emit(ctx, p, v1alpha1.EventTypeWarning, v1alpha1.ReasonProbeFailed,
					fmt.Sprintf("Liveness probe failed: %s", ev.Message))
				w.emit(ctx, p, v1alpha1.EventTypeWarning, v1alpha1.ReasonUnhealthy,
					"Liveness probe failed; killing and restarting")
			}
		}
	}

	if w.m.metrics != nil && ev.Message != "initial probe state" {
		w.m.metrics.IncProbeResult(w.name, string(ev.ProbeType), string(ev.Result))
	}

	if p != nil {
		_ = w.projectStatus(ctx, p, p.Spec.RestartPolicy)
	}
}

func (w *worker) projectStatus(ctx context.Context, p *v1alpha1.Proc, policy v1alpha1.RestartPolicy) error {
	if policy == "" {
		policy = p.Spec.RestartPolicy
	}
	phase, state := statusFromRuntime(&w.rt, policy)
	ready := readyCondition(phase, state, p.Metadata.Generation, w.clock.Now(), &w.rt)
	rc := w.rt.RestartCount
	daemon := p.Metadata.Labels[v1alpha1.LabelDaemonName]
	if w.m.metrics != nil {
		w.m.metrics.SetProcPhase(p.Metadata.Name, daemon, string(phase))
		w.m.metrics.ObserveRestarts(p.Metadata.Name, daemon, rc)
	}
	err := client.RetryOnConflict(func() error {
		fresh, err := w.client.GetProc(ctx, p.Metadata.Name)
		if err != nil {
			return err
		}
		fresh.Status.Phase = phase
		fresh.Status.State = state
		fresh.Status.RestartCount = rc
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, ready)
		_, err = w.client.UpdateProcStatus(ctx, fresh)
		return err
	})
	if errors.Is(err, v1alpha1.ErrNotFound) {
		return nil
	}
	if err != nil {
		w.log.Warn("status update failed", "error", err)
	}
	return err
}

func (w *worker) emit(ctx context.Context, p *v1alpha1.Proc, typ v1alpha1.EventType, reason, message string) {
	w.recorder.Eventf(ctx, v1alpha1.ObjectRef{
		Kind: v1alpha1.KindProc,
		Name: p.Metadata.Name,
		UID:  p.Metadata.UID,
	}, typ, reason, "%s", message)
}
