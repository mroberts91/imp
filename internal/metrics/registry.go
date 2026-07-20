// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus instrumentation for impd: Proc phase /
// restarts / probes, controller reconcile timings, and cgroup resource
// gauges. Naming follows sig-instrumentation conventions (imp_ prefix,
// _total counters, seconds for durations, low-cardinality labels).
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "imp"

	ControllerDaemon = "daemon"
	ControllerGC     = "gc"
	ControllerTimer  = "timer"
)

// Registry owns the Prometheus registry and metric vectors.
type Registry struct {
	reg *prometheus.Registry

	procPhase            *prometheus.GaugeVec
	procRestarts         *prometheus.CounterVec
	probeResults         *prometheus.CounterVec
	reconcileDur         *prometheus.HistogramVec
	reconcileErrors      *prometheus.CounterVec
	procMemory           *prometheus.GaugeVec
	procCPU              *prometheus.CounterVec
	procThrottledPeriods *prometheus.GaugeVec
	procThrottledUsec    *prometheus.GaugeVec

	mu          sync.Mutex
	lastPhase   map[string]phaseKey // proc name → last published phase labels
	lastRestart map[string]int32
	lastCPU     map[string]float64 // cumulative cpu seconds last reported
}

type phaseKey struct {
	daemon, phase string
}

// New builds a Registry with all M3 metrics registered.
func New() *Registry {
	r := &Registry{
		reg:         prometheus.NewRegistry(),
		lastPhase:   map[string]phaseKey{},
		lastRestart: map[string]int32{},
		lastCPU:     map[string]float64{},
	}

	r.procPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "proc_phase",
		Help:      "Proc phase as a 1/0 gauge (one series per phase label; current phase is 1).",
	}, []string{"proc", "daemon", "phase"})

	r.procRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "proc_restarts_total",
		Help:      "Cumulative Proc restart count.",
	}, []string{"proc", "daemon"})

	r.probeResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "probe_results_total",
		Help:      "Probe result events after threshold crossing.",
	}, []string{"proc", "probe_type", "result"})

	r.reconcileDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "reconcile_duration_seconds",
		Help:      "Controller reconcile duration in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"controller"})

	r.reconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_errors_total",
		Help:      "Controller reconcile errors (excluding RequeueAfter).",
	}, []string{"controller"})

	r.procMemory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "proc_memory_bytes",
		Help:      "Proc memory.current from cgroup (bytes).",
	}, []string{"proc", "daemon"})

	r.procCPU = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "proc_cpu_seconds_total",
		Help:      "Proc cumulative CPU time from cgroup cpu.stat usage_usec.",
	}, []string{"proc", "daemon"})

	// Gauges, not counters: they mirror the kernel's own monotonic cpu.stat
	// counters, Set from each scrape (the registry's snapshot-Set pattern
	// cannot drive a true Prometheus counter's delta accounting, M9-k).
	r.procThrottledPeriods = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "proc_cpu_throttled_periods",
		Help:      "Proc cumulative CFS throttled periods from cgroup cpu.stat nr_throttled (kernel-monotonic).",
	}, []string{"proc", "daemon"})

	r.procThrottledUsec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "proc_cpu_throttled_usec",
		Help:      "Proc cumulative CFS throttled time from cgroup cpu.stat throttled_usec (kernel-monotonic).",
	}, []string{"proc", "daemon"})

	r.reg.MustRegister(
		r.procPhase,
		r.procRestarts,
		r.probeResults,
		r.reconcileDur,
		r.reconcileErrors,
		r.procMemory,
		r.procCPU,
		r.procThrottledPeriods,
		r.procThrottledUsec,
	)
	return r
}

// Gatherer returns the Prometheus gatherer for HTTP exposition.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// Registerer is exposed for tests that need extra collectors.
func (r *Registry) Registerer() prometheus.Registerer { return r.reg }

// SetProcPhase publishes the current Proc phase (zeros the previous phase series).
func (r *Registry) SetProcPhase(proc, daemon, phase string) {
	if r == nil || proc == "" || phase == "" {
		return
	}
	if daemon == "" {
		daemon = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.lastPhase[proc]; ok {
		if prev.daemon == daemon && prev.phase == phase {
			return
		}
		r.procPhase.WithLabelValues(proc, prev.daemon, prev.phase).Set(0)
	}
	r.lastPhase[proc] = phaseKey{daemon: daemon, phase: phase}
	r.procPhase.WithLabelValues(proc, daemon, phase).Set(1)
}

// ObserveRestarts advances the restart counter when restartCount increases.
func (r *Registry) ObserveRestarts(proc, daemon string, restartCount int32) {
	if r == nil || proc == "" {
		return
	}
	if daemon == "" {
		daemon = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.lastRestart[proc]
	if restartCount <= prev {
		r.lastRestart[proc] = restartCount
		return
	}
	delta := float64(restartCount - prev)
	r.lastRestart[proc] = restartCount
	r.procRestarts.WithLabelValues(proc, daemon).Add(delta)
}

// IncProbeResult increments the probe result counter.
func (r *Registry) IncProbeResult(proc, probeType, result string) {
	if r == nil || proc == "" {
		return
	}
	r.probeResults.WithLabelValues(proc, probeType, result).Inc()
}

// ObserveReconcile records reconcile latency; failed increments the error counter.
func (r *Registry) ObserveReconcile(controller string, d time.Duration, failed bool) {
	if r == nil {
		return
	}
	r.reconcileDur.WithLabelValues(controller).Observe(d.Seconds())
	if failed {
		r.reconcileErrors.WithLabelValues(controller).Inc()
	}
}

// SetProcMemory sets the memory gauge from a cgroup scrape.
func (r *Registry) SetProcMemory(proc, daemon string, bytes uint64) {
	if r == nil || proc == "" {
		return
	}
	if daemon == "" {
		daemon = "unknown"
	}
	r.procMemory.WithLabelValues(proc, daemon).Set(float64(bytes))
}

// SetProcThrottling publishes the cumulative CFS throttling counters from a
// cgroup scrape (M9-k). Gauges tracking kernel-monotonic values.
func (r *Registry) SetProcThrottling(proc, daemon string, periods, usec uint64) {
	if r == nil || proc == "" {
		return
	}
	if daemon == "" {
		daemon = "unknown"
	}
	r.procThrottledPeriods.WithLabelValues(proc, daemon).Set(float64(periods))
	r.procThrottledUsec.WithLabelValues(proc, daemon).Set(float64(usec))
}

// ObserveProcCPU advances the CPU counter from an absolute cumulative seconds reading.
func (r *Registry) ObserveProcCPU(proc, daemon string, cpuSeconds float64) {
	if r == nil || proc == "" {
		return
	}
	if daemon == "" {
		daemon = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.lastCPU[proc]
	if cpuSeconds < prev {
		// Counter reset (cgroup recreated) — start fresh.
		prev = 0
	}
	delta := cpuSeconds - prev
	r.lastCPU[proc] = cpuSeconds
	if delta > 0 {
		r.procCPU.WithLabelValues(proc, daemon).Add(delta)
	}
}

// ClearProc drops tracked state for a Proc that is gone (phase series zeroed).
func (r *Registry) ClearProc(proc string) {
	if r == nil || proc == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.lastPhase[proc]; ok {
		r.procPhase.WithLabelValues(proc, prev.daemon, prev.phase).Set(0)
		delete(r.lastPhase, proc)
	}
	delete(r.lastRestart, proc)
	delete(r.lastCPU, proc)
}
