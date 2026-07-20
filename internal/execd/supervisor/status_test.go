// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// TestReadyConditionStartupGate pins the M6 startup-probe Ready projection:
// nothing is Ready until the startup probe succeeds, regardless of the
// readiness configuration; afterwards the M3 readiness rules apply.
func TestReadyConditionStartupGate(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	running := v1alpha1.ProcState{Running: &v1alpha1.ProcStateRunning{PID: 1}}

	cases := []struct {
		name       string
		rt         RuntimeRecord
		wantStatus v1alpha1.ConditionStatus
		wantReason string
	}{
		{
			name:       "startup pending gates ready",
			rt:         RuntimeRecord{HasStartupProbe: true},
			wantStatus: v1alpha1.ConditionFalse,
			wantReason: v1alpha1.ReadyReasonProbePending,
		},
		{
			name:       "startup pending gates even with readiness ok",
			rt:         RuntimeRecord{HasStartupProbe: true, HasReadinessProbe: true, ReadinessOK: true},
			wantStatus: v1alpha1.ConditionFalse,
			wantReason: v1alpha1.ReadyReasonProbePending,
		},
		{
			name:       "startup done, no readiness probe: running means ready",
			rt:         RuntimeRecord{HasStartupProbe: true, StartupDone: true},
			wantStatus: v1alpha1.ConditionTrue,
			wantReason: "Running",
		},
		{
			name:       "startup done defers to readiness",
			rt:         RuntimeRecord{HasStartupProbe: true, StartupDone: true, HasReadinessProbe: true},
			wantStatus: v1alpha1.ConditionFalse,
			wantReason: v1alpha1.ReadyReasonProbePending,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cond := readyCondition(v1alpha1.ProcPhaseRunning, running, 1, now, &tc.rt)
			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
				t.Errorf("got %s/%s, want %s/%s", cond.Status, cond.Reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// TestStatusLastTerminated pins the M10-e projection: once an exit has been
// observed, its detail survives into status alongside the Waiting (backoff)
// and Running (restarted) states instead of living only in this worker's
// memory. The Terminated branch deliberately stays nil-LastTerminated (the
// exit detail is already the state itself), and a Proc with no observed
// exit — including a D1-adopted one — carries nothing.
func TestStatusLastTerminated(t *testing.T) {
	exit := &ExitInfo{
		ExitCode:   new(7),
		FinishedAt: time.Unix(2000, 0).UTC(),
		Message:    "exit status 7",
		Nonzero:    true,
	}

	t.Run("backoff carries last exit", func(t *testing.T) {
		rt := RuntimeRecord{
			StartedOnce:  true,
			LastExit:     exit,
			BackoffUntil: time.Unix(3000, 0).UTC(),
			RestartCount: 4,
		}
		_, state := statusFromRuntime(&rt, v1alpha1.RestartPolicyAlways)
		if state.Waiting == nil || state.Waiting.Reason != v1alpha1.WaitingReasonCrashLoopBackOff {
			t.Fatalf("state = %+v, want CrashLoopBackOff waiting", state)
		}
		lt := state.LastTerminated
		if lt == nil || lt.ExitCode == nil || *lt.ExitCode != 7 {
			t.Fatalf("LastTerminated = %+v, want exit code 7", lt)
		}
		if lt.Message != "exited with code 7" {
			t.Errorf("Message = %q, want %q", lt.Message, "exited with code 7")
		}
	})

	t.Run("running after restart carries last exit", func(t *testing.T) {
		rt := RuntimeRecord{
			Running:     true,
			PID:         42,
			StartedOnce: true,
			LastExit:    exit,
		}
		phase, state := statusFromRuntime(&rt, v1alpha1.RestartPolicyAlways)
		if phase != v1alpha1.ProcPhaseRunning || state.Running == nil {
			t.Fatalf("phase/state = %s/%+v, want Running", phase, state)
		}
		if state.LastTerminated == nil || *state.LastTerminated.ExitCode != 7 {
			t.Fatalf("LastTerminated = %+v, want exit code 7", state.LastTerminated)
		}
	})

	t.Run("no exit observed carries nothing", func(t *testing.T) {
		rt := RuntimeRecord{Running: true, PID: 42}
		_, state := statusFromRuntime(&rt, v1alpha1.RestartPolicyAlways)
		if state.LastTerminated != nil {
			t.Errorf("LastTerminated = %+v, want nil (D1-adopted / first run)", state.LastTerminated)
		}
	})

	t.Run("terminated branch leaves lastTerminated nil", func(t *testing.T) {
		rt := RuntimeRecord{StartedOnce: true, LastExit: exit}
		phase, state := statusFromRuntime(&rt, v1alpha1.RestartPolicyNever)
		if phase != v1alpha1.ProcPhaseFailed || state.Terminated == nil {
			t.Fatalf("phase/state = %s/%+v, want Failed/Terminated", phase, state)
		}
		if state.LastTerminated != nil {
			t.Errorf("LastTerminated duplicated alongside Terminated: %+v", state.LastTerminated)
		}
	})
}
