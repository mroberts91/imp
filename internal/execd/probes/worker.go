// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0
//
// Threshold / initial-delay loop is a logical fork of
// pkg/kubelet/prober/worker.go (Copyright The Kubernetes Authors, Apache-2.0).

package probes

import (
	"context"
	"sync"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
)

type worker struct {
	procKey    string
	probeType  ProbeType
	spec       v1alpha1.Probe
	cgroupPath string
	startedAt  time.Time
	runner     Runner
	clk        clock.Clock
	emit       func(ResultEvent)

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Threshold state (kubelet worker).
	lastResult Result
	resultRun  int
	onHold     bool
}

func (w *worker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Go(func() {
		w.loop(ctx)
	})
}

func (w *worker) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func (w *worker) loop(ctx context.Context) {
	// Initial published result before first successful threshold crossing:
	// readiness=Failure, liveness=Success (match kubelet).
	initial := ResultSuccess
	if w.probeType == ProbeReadiness {
		initial = ResultFailure
	}
	w.emit(ResultEvent{
		ProcKey:   w.procKey,
		ProbeType: w.probeType,
		Result:    initial,
		Message:   "initial probe state",
		At:        w.clk.Now(),
	})
	w.lastResult = initial

	period := time.Duration(w.spec.PeriodSeconds) * time.Second
	if period <= 0 {
		period = 10 * time.Second
	}
	timeout := time.Duration(w.spec.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Second
	}
	initialDelay := time.Duration(w.spec.InitialDelaySeconds) * time.Second

	ticker := w.clk.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if w.onHold {
				continue
			}
			if initialDelay > 0 && w.clk.Now().Before(w.startedAt.Add(initialDelay)) {
				continue
			}
			w.doProbe(ctx, timeout)
		}
	}
}

func (w *worker) doProbe(parent context.Context, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	result, msg := runProbe(ctx, w.runner, w.cgroupPath, &w.spec)
	if result == ResultUnknown {
		// Prober error — discard (kubelet).
		return
	}

	if w.lastResult == result {
		w.resultRun++
	} else {
		w.lastResult = result
		w.resultRun = 1
	}

	successTh := max(int(w.spec.SuccessThreshold), 1)
	failureTh := int(w.spec.FailureThreshold)
	if failureTh < 1 {
		failureTh = 3
	}

	if (result == ResultFailure && w.resultRun < failureTh) ||
		(result == ResultSuccess && w.resultRun < successTh) {
		return
	}

	w.emit(ResultEvent{
		ProcKey:   w.procKey,
		ProbeType: w.probeType,
		Result:    result,
		Message:   msg,
		At:        w.clk.Now(),
	})

	if w.probeType == ProbeLiveness && result == ResultFailure {
		// Stop probing until supervisor restarts the Proc (new Start()).
		w.onHold = true
		w.resultRun = 0
	}
}
