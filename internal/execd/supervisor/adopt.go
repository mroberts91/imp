// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// Bootstrap walks cgroup dirs and either adopts matching live processes (D1)
// or kills orphans. Lists Procs from the API (not the informer store) so it
// can run before Handle is unblocked. Call once after the API is up; then
// pending informer keys are drained.
func (m *Manager) Bootstrap(ctx context.Context) {
	defer m.finishBootstrap()

	uids, err := m.cgroups.ListProcDirs()
	if err != nil {
		m.log.Warn("cgroup list for adopt failed", "error", err)
		return
	}
	procs, _, err := m.client.ListProcs(ctx)
	if err != nil {
		m.log.Warn("list procs for adopt failed", "error", err)
		return
	}
	byUID := map[string]*v1alpha1.Proc{}
	for i := range procs {
		p := &procs[i]
		if p.Metadata.UID != "" {
			byUID[p.Metadata.UID] = p
		}
	}

	for _, uid := range uids {
		path := m.cgroups.Path(uid)
		p := byUID[uid]
		if p == nil {
			m.log.Info("killing orphan cgroup", "uid", uid, "cgroup", path)
			_ = m.cgroups.Kill(path)
			_ = m.cgroups.Remove(path)
			continue
		}
		pid, ticks, ok := matchCgroupIdentity(m, path, p)
		if !ok {
			m.log.Info("cgroup identity mismatch; clearing for fresh start",
				"uid", uid, "proc", p.Metadata.Name, "cgroup", path)
			_ = m.cgroups.Kill(path)
			continue
		}
		key := "Proc/" + p.Metadata.Name
		m.mu.Lock()
		if m.stopping {
			m.mu.Unlock()
			return
		}
		if _, exists := m.workers[key]; exists {
			m.mu.Unlock()
			continue
		}
		w := newWorker(m, key)
		m.workers[key] = w
		m.mu.Unlock()

		w.bindAdopted(p, pid, ticks, path)
		go w.run()
		w.wake()
		w.emit(ctx, p, v1alpha1.EventTypeNormal, v1alpha1.ReasonAdopted,
			fmt.Sprintf("Adopted proc %s pid=%d", p.Metadata.Name, pid))
		_ = w.projectStatus(ctx, p, p.Spec.RestartPolicy)
		m.log.Info("adopted proc", "key", key, "pid", pid, "cgroup", path)
	}
}

func (m *Manager) finishBootstrap() {
	m.mu.Lock()
	m.bootstrapped = true
	pending := append([]string(nil), m.pendingKeys...)
	m.pendingKeys = nil
	m.mu.Unlock()
	for _, key := range pending {
		m.Handle(key)
	}
}

// matchCgroupIdentity finds a live pid in the cgroup whose start ticks match
// Proc.status.running.procStartTicks (and prefers the recorded PID).
func matchCgroupIdentity(m *Manager, path string, p *v1alpha1.Proc) (pid int, ticks int64, ok bool) {
	if p.Status.State.Running == nil {
		return 0, 0, false
	}
	wantTicks := p.Status.State.Running.ProcStartTicks
	wantPID := p.Status.State.Running.PID
	if wantTicks == 0 {
		return 0, 0, false
	}
	pids, err := m.cgroups.PIDs(path)
	if err != nil {
		return 0, 0, false
	}
	var candidate int
	var candidateTicks int64
	for _, id := range pids {
		if !pidAlive(id) {
			continue
		}
		t, err := readProcStartTicks(id)
		if err != nil {
			continue
		}
		if t != wantTicks {
			continue
		}
		if id == wantPID {
			return id, t, true
		}
		if candidate == 0 {
			candidate = id
			candidateTicks = t
		}
	}
	if candidate != 0 {
		return candidate, candidateTicks, true
	}
	return 0, 0, false
}

func pidAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// bindAdopted seeds the worker as Running without an exec.Cmd (D1).
func (w *worker) bindAdopted(p *v1alpha1.Proc, pid int, ticks int64, cgPath string) {
	started := w.clock.Now()
	if p.Status.State.Running != nil && !p.Status.State.Running.StartedAt.IsZero() {
		started = p.Status.State.Running.StartedAt.Time
	}
	noteStart(&w.rt, pid, ticks, started)
	w.rt.CgroupPath = cgPath
	w.rt.Adopted = true
	w.cmd = nil
	w.startAdoptWatch(pid)
	w.publishCgroupSnap(p)
	w.startProbes(p)
}

func (w *worker) startAdoptWatch(pid int) {
	w.exitCh = make(chan exitResult, 1)
	stop := make(chan struct{})
	w.adoptWatchStop = stop
	clk := w.clock
	go func() {
		for {
			timer := clk.NewTimer(200 * time.Millisecond)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C():
				timer.Stop()
				if !pidAlive(pid) {
					select {
					case w.exitCh <- exitResult{err: errAdoptedExited}:
					default:
					}
					return
				}
			}
		}
	}()
}

func (w *worker) stopAdoptWatch() {
	if w.adoptWatchStop != nil {
		select {
		case <-w.adoptWatchStop:
		default:
			close(w.adoptWatchStop)
		}
		w.adoptWatchStop = nil
	}
}

var errAdoptedExited = fmt.Errorf("adopted process exited")
