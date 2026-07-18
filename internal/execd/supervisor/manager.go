// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"io"
	"log/slog"
	"sync"

	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
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
	client *client.Client
	store  *cache.Store
	logs   LogCapture
	clock  clock.Clock
	log    *slog.Logger

	mu       sync.Mutex
	workers  map[string]*worker
	stopping bool
}

// NewManager builds a Manager. A nil clk means the real clock.
func NewManager(cl *client.Client, store *cache.Store, logs LogCapture, clk clock.Clock) *Manager {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Manager{
		client:  cl,
		store:   store,
		logs:    logs,
		clock:   clk,
		log:     slog.With("component", componentName),
		workers: map[string]*worker{},
	}
}

// Handle is the Proc informer handler: ensure a worker for key and wake it.
func (m *Manager) Handle(key string) {
	m.mu.Lock()
	if m.stopping {
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

// Stop shuts every worker down (SIGTERM children) and waits for them to
// finish. Safe to call once; subsequent calls are no-ops.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	m.stopping = true
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.mu.Unlock()

	for _, w := range workers {
		w.requestStop()
	}
	for _, w := range workers {
		<-w.done
	}
}

func (m *Manager) removeWorker(key string) {
	m.mu.Lock()
	delete(m.workers, key)
	m.mu.Unlock()
}
