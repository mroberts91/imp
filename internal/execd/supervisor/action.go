// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package supervisor makes Proc objects true against the OS: it is imp's
// kubelet analog. The pure plan function computeProcAction is a logical
// fork of computePodActions in
// pkg/kubelet/kuberuntime/kuberuntime_manager.go (Copyright The Kubernetes
// Authors, Apache-2.0; see LICENSES/kubernetes/). Deltas: one process per
// Proc (no containers/sandboxes), Proc spec is immutable so there is no
// "spec changed" kill branch, and restart/backoff bookkeeping lives on the
// in-memory runtime record rather than a separate status manager.
package supervisor

import (
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// ActionKind is what the worker should do next for one Proc.
type ActionKind int

const (
	// ActionNone means the observed state already matches desire.
	ActionNone ActionKind = iota
	// ActionStart means fork/exec a new child.
	ActionStart
	// ActionStop means deliver stopSignal → grace → SIGKILL to the group.
	ActionStop
	// ActionWait means do not start yet; sit until Until (CrashLoopBackOff).
	ActionWait
)

// Action is the plan returned by computeProcAction. Until is set only for
// ActionWait.
type Action struct {
	Kind  ActionKind
	Until time.Time
}

// ExitInfo records how the last child finished.
type ExitInfo struct {
	ExitCode   *int
	Signal     string
	FinishedAt time.Time
	Message    string
	// Nonzero is true when the process failed (nonzero exit or signal).
	Nonzero bool
}

// RuntimeRecord is the worker's in-memory view of one Proc's OS state. It is
// the "observed" half of computeProcAction; Proc.status is a projection of
// it written after each action.
type RuntimeRecord struct {
	Running        bool
	PID            int
	ProcStartTicks int64
	StartedAt      time.Time

	RestartCount int32
	LastExit     *ExitInfo

	// BackoffUntil is when the next Start is allowed after a failure/restart.
	BackoffUntil time.Time
	// NextBackoff is the delay that will be used on the *next* failure.
	NextBackoff time.Duration
	// LastStableAt is when the process last entered Running; used to reset
	// backoff after sustained healthy run.
	LastStableAt time.Time

	// StartedOnce is true after the first successful Start for this Proc.
	StartedOnce bool
}

// computeProcAction decides the next action from desired (exists + policy)
// and observed (runtime). Pure: no I/O, no mutation of rt.
func computeProcAction(exists bool, policy v1alpha1.RestartPolicy, rt *RuntimeRecord, now time.Time) Action {
	if rt == nil {
		rt = &RuntimeRecord{}
	}

	if rt.Running {
		if !exists {
			return Action{Kind: ActionStop}
		}
		return Action{Kind: ActionNone}
	}

	// Not running.
	if !exists {
		return Action{Kind: ActionNone}
	}

	// Backoff pending before any (re)start.
	if !rt.BackoffUntil.IsZero() && now.Before(rt.BackoffUntil) {
		return Action{Kind: ActionWait, Until: rt.BackoffUntil}
	}

	if !rt.StartedOnce {
		// Never started: first Start. No last exit yet.
		return Action{Kind: ActionStart}
	}

	// Has exited at least once.
	exit := rt.LastExit
	if exit == nil {
		// StartedOnce but no exit recorded shouldn't happen; start again.
		return Action{Kind: ActionStart}
	}

	if !exit.Nonzero {
		// Clean exit.
		switch policy {
		case v1alpha1.RestartPolicyAlways:
			return Action{Kind: ActionStart}
		default: // OnFailure, Never → Succeeded terminal
			return Action{Kind: ActionNone}
		}
	}

	// Nonzero / signalled.
	switch policy {
	case v1alpha1.RestartPolicyNever:
		return Action{Kind: ActionNone}
	default: // Always, OnFailure
		return Action{Kind: ActionStart}
	}
}
