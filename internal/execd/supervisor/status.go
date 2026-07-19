// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

const componentName = "execd"

func statusFromRuntime(rt *RuntimeRecord, policy v1alpha1.RestartPolicy) (phase v1alpha1.ProcPhase, state v1alpha1.ProcState) {
	if rt.Running {
		return v1alpha1.ProcPhaseRunning, v1alpha1.ProcState{
			Running: &v1alpha1.ProcStateRunning{
				PID:            rt.PID,
				StartedAt:      v1alpha1.NewTime(rt.StartedAt),
				ProcStartTicks: rt.ProcStartTicks,
			},
		}
	}

	willRestart := shouldRestart(policy, rt.LastExit)
	if !rt.BackoffUntil.IsZero() && (willRestart || !rt.StartedOnce) {
		msg := "backing off before restart"
		reason := v1alpha1.WaitingReasonCrashLoopBackOff
		if !rt.StartedOnce {
			msg = "waiting to start"
		}
		return v1alpha1.ProcPhasePending, v1alpha1.ProcState{
			Waiting: &v1alpha1.ProcStateWaiting{
				Reason:       reason,
				Message:      msg,
				BackoffUntil: v1alpha1.NewTime(rt.BackoffUntil),
			},
		}
	}

	if rt.LastExit != nil {
		term := &v1alpha1.ProcStateTerminated{
			ExitCode:   rt.LastExit.ExitCode,
			Signal:     rt.LastExit.Signal,
			FinishedAt: v1alpha1.NewTime(rt.LastExit.FinishedAt),
			Message:    fmtExitMessage(*rt.LastExit),
		}
		if willRestart {
			return v1alpha1.ProcPhasePending, v1alpha1.ProcState{Terminated: term}
		}
		if !rt.LastExit.Nonzero {
			return v1alpha1.ProcPhaseSucceeded, v1alpha1.ProcState{Terminated: term}
		}
		return v1alpha1.ProcPhaseFailed, v1alpha1.ProcState{Terminated: term}
	}

	return v1alpha1.ProcPhasePending, v1alpha1.ProcState{}
}

func shouldRestart(policy v1alpha1.RestartPolicy, exit *ExitInfo) bool {
	if exit == nil {
		return true
	}
	if !exit.Nonzero {
		return policy == v1alpha1.RestartPolicyAlways
	}
	return policy != v1alpha1.RestartPolicyNever
}

// readyCondition projects Ready from phase and optional readiness-probe state.
// Without a readinessProbe, M2 behavior: Running ⇒ Ready=True.
func readyCondition(phase v1alpha1.ProcPhase, state v1alpha1.ProcState, gen int64, now time.Time, rt *RuntimeRecord) v1alpha1.Condition {
	cond := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeReady,
		Status:             v1alpha1.ConditionFalse,
		ObservedGeneration: gen,
		LastTransitionTime: v1alpha1.NewTime(now),
		Reason:             "NotReady",
		Message:            "proc is not running",
	}
	if phase == v1alpha1.ProcPhaseRunning {
		if rt != nil && rt.HasReadinessProbe {
			if rt.ReadinessOK {
				cond.Status = v1alpha1.ConditionTrue
				cond.Reason = "Running"
				cond.Message = "readiness probe succeeded"
				return cond
			}
			if rt.ReadinessFailed {
				cond.Reason = v1alpha1.ReadyReasonProbeFailed
				cond.Message = "readiness probe failed"
				return cond
			}
			cond.Reason = v1alpha1.ReadyReasonProbePending
			cond.Message = "waiting for readiness probe"
			return cond
		}
		cond.Status = v1alpha1.ConditionTrue
		cond.Reason = "Running"
		cond.Message = "proc is running"
		return cond
	}
	if state.Waiting != nil && state.Waiting.Reason == v1alpha1.WaitingReasonCrashLoopBackOff {
		cond.Reason = v1alpha1.WaitingReasonCrashLoopBackOff
		cond.Message = state.Waiting.Message
		return cond
	}
	switch phase {
	case v1alpha1.ProcPhaseSucceeded:
		cond.Reason = "ProcCompleted"
		cond.Message = "proc completed successfully"
	case v1alpha1.ProcPhaseFailed:
		cond.Reason = "ProcFailed"
		cond.Message = "proc failed"
	}
	return cond
}
