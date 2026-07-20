// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/mroberts91/imp/api/v1alpha1"
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
	Open(procName string, retention *v1alpha1.LogRetention) (stdout, stderr io.WriteCloser, err error)
	CloseCapture(procName string)
	Remove(procName string) error
}

// ConfigMaterializer is the slice of execd/configfiles the supervisor needs:
// write a Proc's referenced Config files before spawn, expose the per-proc
// directory for IMP_CONFIG_DIR, and remove it on teardown (M8).
type ConfigMaterializer interface {
	Materialize(ctx context.Context, p *v1alpha1.Proc) error
	Dir(proc string) string
	Remove(proc string) error
}

// noopMaterializer stands in when no ConfigMaterializer is wired (e.g. a test
// that spawns only config-less Procs). Materialize/Remove are no-ops and Dir
// is empty, so buildEnv injects no IMP_CONFIG_DIR.
type noopMaterializer struct{}

func (noopMaterializer) Materialize(context.Context, *v1alpha1.Proc) error { return nil }
func (noopMaterializer) Dir(string) string                                 { return "" }
func (noopMaterializer) Remove(string) error                               { return nil }

// Manager routes Proc informer keys to per-Proc workers. Logical fork of
// pkg/kubelet/pod_workers.go (Copyright The Kubernetes Authors, Apache-2.0;
// see LICENSES/kubernetes/): all mutations for one Proc happen on one
// goroutine; the manager only routes.
type Manager struct {
	client         *client.Client
	store          *cache.Store
	logs           LogCapture
	configs        ConfigMaterializer
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
// ("execd"). A nil configs means no Config materialization (config-less Procs
// only). When killOnShutdown is false (D1 default), Stop leaves children
// running in their cgroups; when true, Stop terminates them (acceptance /
// full teardown).
func NewManager(cl *client.Client, store *cache.Store, logs LogCapture, configs ConfigMaterializer, cg *cgroups.Manager, clk clock.Clock, rec *recorder.Recorder, killOnShutdown bool) *Manager {
	if cg == nil {
		panic("supervisor.NewManager: cgroups Manager is required")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	if rec == nil {
		rec = recorder.New(cl, componentName, clk)
	}
	if configs == nil {
		configs = noopMaterializer{}
	}
	m := &Manager{
		client:         cl,
		store:          store,
		logs:           logs,
		configs:        configs,
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

// ProcStats reads live cgroup stats for every Running Proc — the data
// behind the api-server's /stats route (impctl top). It implements
// apiserver.StatsProvider; wiring in cmd/impd is the compile-time check.
// Observations only: nothing here touches the store (doc 08 M5-e).
func (m *Manager) ProcStats() []v1alpha1.ProcStat {
	snaps := m.ListProcCgroups()
	now := v1alpha1.NewTime(m.clock.Now())
	out := make([]v1alpha1.ProcStat, 0, len(snaps))
	for _, s := range snaps {
		st, err := m.cgroups.Stats(s.Path)
		if err != nil {
			// The Proc may have exited between the snapshot and the read.
			continue
		}
		ps := v1alpha1.ProcStat{
			Proc:               s.Proc,
			CPUUsageUsec:       st.CPUUsageUsec,
			MemoryCurrentBytes: st.MemoryCurrent,
			PidsCurrent:        st.PidsCurrent,
			NrThrottled:        st.NrThrottled,
			ThrottledUsec:      st.ThrottledUsec,
			SampledAt:          now,
		}
		switch {
		case s.Daemon != "":
			ps.Owner = v1alpha1.ObjectRef{Kind: v1alpha1.KindDaemon, Name: s.Daemon}
		case s.Timer != "":
			ps.Owner = v1alpha1.ObjectRef{Kind: v1alpha1.KindTimer, Name: s.Timer}
		}
		out = append(out, ps)
	}
	slices.SortFunc(out, func(a, b v1alpha1.ProcStat) int { return strings.Compare(a.Proc, b.Proc) })
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
