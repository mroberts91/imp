// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import "github.com/mroberts91/imp/api/v1alpha1"

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
