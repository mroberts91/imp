// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"io"
	"log/slog"
	"sync"

	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/execd/cgroups"
	"github.com/mroberts91/imp/internal/execd/probes"
	"github.com/mroberts91/imp/internal/metrics"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

// LogCapture is the slice of execd/logs the supervisor needs.
type LogCapture interface {
	Open(procName string) (stdout, stderr io.WriteCloser, err error)
	CloseCapture(procName string)
	Remove(procName string) error
}

// Manager routes Proc informer keys to per-Proc workers. Logical fork of
// pkg/kubelet/pod_workers.go (Copyright The Kubernetes Authors, Apache-2.0;
// see LICENSES/kubernetes/): all mutations for one Proc happen on one
// goroutine; the manager only routes.
type Manager struct {
	client         *client.Client
	store          *cache.Store
	logs           LogCapture
	cgroups        *cgroups.Manager
	probes         *probes.Manager
	clock          clock.Clock
	recorder       *recorder.Recorder
	metrics        *metrics.Registry
	log            *slog.Logger
	killOnShutdown bool

	mu           sync.Mutex
	workers      map[string]*worker
	cgroupSnaps  map[string]metrics.ProcCgroup // key → last Running cgroup
	stopping     bool
	bootstrapped bool
	pendingKeys  []string
}

// NewManager builds a Manager. cg is required (cgroup v2 root). A nil clk
// means the real clock. A nil rec builds a recorder stamped ReportingComponent
// ("execd"). When killOnShutdown is false (D1 default), Stop leaves children
// running in their cgroups; when true, Stop terminates them (acceptance /
// full teardown).
func NewManager(cl *client.Client, store *cache.Store, logs LogCapture, cg *cgroups.Manager, clk clock.Clock, rec *recorder.Recorder, killOnShutdown bool) *Manager {
	if cg == nil {
		panic("supervisor.NewManager: cgroups Manager is required")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	if rec == nil {
		rec = recorder.New(cl, componentName, clk)
	}
	m := &Manager{
		client:         cl,
		store:          store,
		logs:           logs,
		cgroups:        cg,
		clock:          clk,
		recorder:       rec,
		log:            slog.With("component", componentName),
		killOnShutdown: killOnShutdown,
		workers:        map[string]*worker{},
		cgroupSnaps:    map[string]metrics.ProcCgroup{},
	}
	m.probes = probes.NewManager(cg, clk, m.handleProbeResult)
	return m
}

// SetMetrics attaches a metrics registry (optional; nil disables).
func (m *Manager) SetMetrics(reg *metrics.Registry) {
	m.metrics = reg
}

// ListProcCgroups returns Running Procs with a cgroup path for metrics scrapes.
func (m *Manager) ListProcCgroups() []metrics.ProcCgroup {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]metrics.ProcCgroup, 0, len(m.cgroupSnaps))
	for _, s := range m.cgroupSnaps {
		out = append(out, s)
	}
	return out
}

func (m *Manager) setCgroupSnap(key string, snap metrics.ProcCgroup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snap.Path == "" {
		delete(m.cgroupSnaps, key)
		return
	}
	m.cgroupSnaps[key] = snap
}

func (m *Manager) clearCgroupSnap(key string) {
	m.mu.Lock()
	delete(m.cgroupSnaps, key)
	m.mu.Unlock()
}

func (m *Manager) handleProbeResult(ev probes.ResultEvent) {
	m.mu.Lock()
	w := m.workers[ev.ProcKey]
	m.mu.Unlock()
	if w == nil {
		return
	}
	select {
	case w.probeCh <- ev:
	default:
		// Drop if the worker is backed up; a later wake/resync will refresh.
	}
	w.wake()
}

// Handle is the Proc informer handler: ensure a worker for key and wake it.
// Keys arriving before Bootstrap completes are queued and drained afterward
// so we never Start a Proc that Bootstrap is about to Adopt.
func (m *Manager) Handle(key string) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	if !m.bootstrapped {
		m.pendingKeys = append(m.pendingKeys, key)
		m.mu.Unlock()
		return
	}
	w, ok := m.workers[key]
	if !ok {
		w = newWorker(m, key)
		m.workers[key] = w
		go w.run()
	}
	m.mu.Unlock()
	w.wake()
}

// Stop shuts workers down. With killOnShutdown, children are SIGTERM'd and
// cgroups removed; otherwise workers detach and leave processes running (D1).
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	m.stopping = true
	kill := m.killOnShutdown
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.mu.Unlock()

	m.probes.StopAll()

	for _, w := range workers {
		if kill {
			w.requestStop()
		} else {
			w.requestDetach()
		}
	}
	for _, w := range workers {
		<-w.done
	}
}

func (m *Manager) removeWorker(key string) {
	m.mu.Lock()
	w := m.workers[key]
	delete(m.workers, key)
	delete(m.cgroupSnaps, key)
	m.mu.Unlock()
	if w != nil && m.metrics != nil {
		m.metrics.ClearProc(w.name)
	}
}
