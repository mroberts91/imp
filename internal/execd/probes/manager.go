// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package probes

import (
	"sync"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
)

// Manager owns one worker per (procKey, probeType). Results are delivered
// via the OnResult callback (never empty).
type Manager struct {
	runner   Runner
	clk      clock.Clock
	onResult func(ResultEvent)

	mu      sync.Mutex
	workers map[string]*procWorkers // key = procKey
}

type procWorkers struct {
	liveness  *worker
	readiness *worker
}

// NewManager builds a probe manager. onResult must be non-nil.
func NewManager(runner Runner, clk clock.Clock, onResult func(ResultEvent)) *Manager {
	if clk == nil {
		clk = clock.Real{}
	}
	if onResult == nil {
		onResult = func(ResultEvent) {}
	}
	return &Manager{
		runner:   runner,
		clk:      clk,
		onResult: onResult,
		workers:  make(map[string]*procWorkers),
	}
}

// Start begins probe workers for a running Proc. Replaces any existing
// workers for the same key (restart). startedAt is process start time for
// InitialDelaySeconds. Nil probes are skipped.
func (m *Manager) Start(procKey string, cgroupPath string, startedAt time.Time, liveness, readiness *v1alpha1.Probe) {
	m.Stop(procKey)

	pw := &procWorkers{}
	if liveness != nil {
		pw.liveness = m.newWorker(procKey, ProbeLiveness, *liveness, cgroupPath, startedAt)
		pw.liveness.start()
	}
	if readiness != nil {
		pw.readiness = m.newWorker(procKey, ProbeReadiness, *readiness, cgroupPath, startedAt)
		pw.readiness.start()
	}
	if pw.liveness == nil && pw.readiness == nil {
		return
	}
	m.mu.Lock()
	m.workers[procKey] = pw
	m.mu.Unlock()
}

func (m *Manager) newWorker(procKey string, pt ProbeType, spec v1alpha1.Probe, cgroupPath string, startedAt time.Time) *worker {
	return &worker{
		procKey:    procKey,
		probeType:  pt,
		spec:       spec,
		cgroupPath: cgroupPath,
		startedAt:  startedAt,
		runner:     m.runner,
		clk:        m.clk,
		emit:       m.onResult,
	}
}

// Stop cancels all workers for procKey and waits for them to exit.
func (m *Manager) Stop(procKey string) {
	m.mu.Lock()
	pw := m.workers[procKey]
	delete(m.workers, procKey)
	m.mu.Unlock()
	if pw == nil {
		return
	}
	if pw.liveness != nil {
		pw.liveness.stop()
	}
	if pw.readiness != nil {
		pw.readiness.stop()
	}
}

// StopAll stops every worker (impd shutdown / detach).
func (m *Manager) StopAll() {
	m.mu.Lock()
	keys := make([]string, 0, len(m.workers))
	for k := range m.workers {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	for _, k := range keys {
		m.Stop(k)
	}
}
