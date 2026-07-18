// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

// Crash-loop backoff constants mirror kubelet's CrashLoopBackOff numbers
// (initial 10s, ×2, cap 5m, reset after 2×cap = 10m of healthy run). The
// algorithm is a logical fork of
// staging/src/k8s.io/client-go/util/flowcontrol/backoff.go (Copyright The
// Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/). Distinct on
// purpose from the workqueue's 5ms→1000s limiter.

import "time"

const (
	backoffInitial = 10 * time.Second
	backoffCap     = 5 * time.Minute
	// backoffResetAfter is how long a process must stay healthy before the
	// next failure starts over at backoffInitial.
	backoffResetAfter = 2 * backoffCap // 10m
)

// noteExit records an exit on rt and advances crash-loop backoff. Call once
// per reaped child, before the next computeProcAction. healthyFor is how
// long the process ran before exiting (startedAt → finishedAt); used to
// decide whether to reset the backoff ladder.
//
// RestartCount increments on every exit (the process ended and may be
// restarted). Terminal Never/OnFailure-success paths simply stop calling
// Start afterward.
func noteExit(rt *RuntimeRecord, exit ExitInfo, healthyFor time.Duration) {
	rt.Running = false
	rt.PID = 0
	rt.ProcStartTicks = 0
	rt.LastExit = &exit
	rt.RestartCount++

	if healthyFor >= backoffResetAfter {
		rt.NextBackoff = backoffInitial
	}
	if rt.NextBackoff == 0 {
		rt.NextBackoff = backoffInitial
	}

	delay := rt.NextBackoff
	rt.BackoffUntil = exit.FinishedAt.Add(delay)

	next := min(rt.NextBackoff*2, backoffCap)
	rt.NextBackoff = next
}

// noteStart marks the process as running and clears any pending backoff wait.
func noteStart(rt *RuntimeRecord, pid int, ticks int64, startedAt time.Time) {
	rt.Running = true
	rt.PID = pid
	rt.ProcStartTicks = ticks
	rt.StartedAt = startedAt
	rt.StartedOnce = true
	rt.LastStableAt = startedAt
	rt.BackoffUntil = time.Time{}
}
