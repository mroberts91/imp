// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/pkg/client"
)

type worker struct {
	m      *Manager
	key    string
	name   string
	client *client.Client
	store  *cache.Store
	logs   LogCapture
	clock  clock.Clock
	log    *slog.Logger

	wakeCh   chan struct{}
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	rt     RuntimeRecord
	cmd    *exec.Cmd
	exitCh chan exitResult
}

func newWorker(m *Manager, key string) *worker {
	name := key
	if parts := strings.SplitN(key, "/", 2); len(parts) == 2 {
		name = parts[1]
	}
	return &worker{
		m:      m,
		key:    key,
		name:   name,
		client: m.client,
		store:  m.store,
		logs:   m.logs,
		clock:  m.clock,
		log:    m.log.With("key", key),
		wakeCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
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

func (w *worker) run() {
	defer close(w.done)
	defer w.m.removeWorker(w.key)
	defer func() { _ = w.logs.Remove(w.name) }()

	for {
		w.syncOnce()

		_, exists := w.store.GetByKey(w.key)
		stopping := false
		select {
		case <-w.stopCh:
			stopping = true
		default:
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
	stdout, stderr, err := w.logs.Open(p.Metadata.Name)
	if err != nil {
		return err
	}

	cmd, err := buildCmd(p, stdout, stderr)
	if err != nil {
		w.logs.CloseCapture(p.Metadata.Name)
		return err
	}
	if err := cmd.Start(); err != nil {
		w.logs.CloseCapture(p.Metadata.Name)
		return fmt.Errorf("starting: %w", err)
	}

	pid := cmd.Process.Pid
	ticks, tErr := readProcStartTicks(pid)
	if tErr != nil {
		w.log.Warn("reading procStartTicks", "pid", pid, "error", tErr)
	}
	started := w.clock.Now()
	noteStart(&w.rt, pid, ticks, started)
	w.cmd = cmd
	w.exitCh = make(chan exitResult, 1)
	go func(c *exec.Cmd, ch chan exitResult) {
		err := c.Wait()
		ch <- exitResult{state: c.ProcessState, err: err}
	}(cmd, w.exitCh)

	w.emit(ctx, p, v1alpha1.EventTypeNormal, v1alpha1.ReasonStarted,
		fmt.Sprintf("Started proc %s pid=%d", p.Metadata.Name, pid))
	return w.projectStatus(ctx, p, p.Spec.RestartPolicy)
}

func (w *worker) doStop(ctx context.Context, p *v1alpha1.Proc) {
	if !w.rt.Running || w.cmd == nil || w.exitCh == nil {
		return
	}
	sig := v1alpha1.DefaultStopSignal
	grace := int64(30)
	if p != nil {
		sig = stopSignalOf(p)
		grace = gracePeriod(p)
		w.emit(ctx, p, v1alpha1.EventTypeNormal, v1alpha1.ReasonKilling,
			fmt.Sprintf("Stopping proc %s", p.Metadata.Name))
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Duration(grace+5)*time.Second)
	defer cancel()
	res := stopProcess(stopCtx, w.clock, w.cmd, sig, grace, w.exitCh, w.log)
	if res.state != nil || res.err != nil {
		w.onExit(res)
	} else if w.rt.Running {
		// Forced clear if Wait never delivered (ctx canceled).
		finished := w.clock.Now()
		noteExit(&w.rt, ExitInfo{Nonzero: true, Signal: "KILL", FinishedAt: finished}, finished.Sub(w.rt.StartedAt))
		w.cmd = nil
		w.exitCh = nil
		w.logs.CloseCapture(w.name)
	}
	if p != nil {
		_ = w.projectStatus(ctx, p, p.Spec.RestartPolicy)
	}
}

func (w *worker) onExit(res exitResult) {
	finished := w.clock.Now()
	info := dissectExit(res.err, res.state, finished)
	healthy := time.Duration(0)
	if !w.rt.StartedAt.IsZero() {
		healthy = finished.Sub(w.rt.StartedAt)
	}
	noteExit(&w.rt, info, healthy)
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

func (w *worker) projectStatus(ctx context.Context, p *v1alpha1.Proc, policy v1alpha1.RestartPolicy) error {
	if policy == "" {
		policy = p.Spec.RestartPolicy
	}
	phase, state := statusFromRuntime(&w.rt, policy)
	rc := w.rt.RestartCount
	err := client.RetryOnConflict(func() error {
		fresh, err := w.client.GetProc(ctx, p.Metadata.Name)
		if err != nil {
			return err
		}
		fresh.Status.Phase = phase
		fresh.Status.State = state
		fresh.Status.RestartCount = rc
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
	now := v1alpha1.NewTime(w.clock.Now())
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%s", reason, message)
	ev := &v1alpha1.Event{
		Metadata: v1alpha1.ObjectMeta{
			Name: fmt.Sprintf("%s.%08x", p.Metadata.Name, h.Sum32()),
		},
		Regarding: v1alpha1.ObjectRef{
			Kind: v1alpha1.KindProc,
			Name: p.Metadata.Name,
			UID:  p.Metadata.UID,
		},
		Type:               typ,
		Reason:             reason,
		Message:            message,
		Count:              1,
		FirstTimestamp:     now,
		LastTimestamp:      now,
		ReportingComponent: componentName,
	}
	if _, err := w.client.ApplyEvent(ctx, ev); err != nil {
		w.log.Warn("dropping event", "reason", reason, "error", err)
	}
}
