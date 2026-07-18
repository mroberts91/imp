// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func TestComputeProcAction(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	zero := 0
	one := 1

	tests := []struct {
		name   string
		exists bool
		policy v1alpha1.RestartPolicy
		rt     *RuntimeRecord
		want   ActionKind
		until  time.Time
	}{
		{
			name:   "first start",
			exists: true,
			policy: v1alpha1.RestartPolicyAlways,
			rt:     &RuntimeRecord{},
			want:   ActionStart,
		},
		{
			name:   "running and desired",
			exists: true,
			policy: v1alpha1.RestartPolicyAlways,
			rt:     &RuntimeRecord{Running: true, PID: 42, StartedOnce: true},
			want:   ActionNone,
		},
		{
			name:   "running but deleted",
			exists: false,
			policy: v1alpha1.RestartPolicyAlways,
			rt:     &RuntimeRecord{Running: true, PID: 42, StartedOnce: true},
			want:   ActionStop,
		},
		{
			name:   "deleted and already stopped",
			exists: false,
			policy: v1alpha1.RestartPolicyAlways,
			rt:     &RuntimeRecord{StartedOnce: true},
			want:   ActionNone,
		},
		{
			name:   "backoff pending",
			exists: true,
			policy: v1alpha1.RestartPolicyAlways,
			rt: &RuntimeRecord{
				StartedOnce:  true,
				BackoffUntil: now.Add(5 * time.Second),
				LastExit:     &ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: now.Add(-5 * time.Second)},
			},
			want:  ActionWait,
			until: now.Add(5 * time.Second),
		},
		{
			name:   "exit 0 Always after backoff",
			exists: true,
			policy: v1alpha1.RestartPolicyAlways,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{ExitCode: &zero, Nonzero: false, FinishedAt: now.Add(-time.Minute)},
			},
			want: ActionStart,
		},
		{
			name:   "exit 0 OnFailure terminal",
			exists: true,
			policy: v1alpha1.RestartPolicyOnFailure,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{ExitCode: &zero, Nonzero: false, FinishedAt: now},
			},
			want: ActionNone,
		},
		{
			name:   "exit 0 Never terminal",
			exists: true,
			policy: v1alpha1.RestartPolicyNever,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{ExitCode: &zero, Nonzero: false, FinishedAt: now},
			},
			want: ActionNone,
		},
		{
			name:   "exit nonzero Always",
			exists: true,
			policy: v1alpha1.RestartPolicyAlways,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: now.Add(-time.Minute)},
			},
			want: ActionStart,
		},
		{
			name:   "exit nonzero OnFailure",
			exists: true,
			policy: v1alpha1.RestartPolicyOnFailure,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: now.Add(-time.Minute)},
			},
			want: ActionStart,
		},
		{
			name:   "exit nonzero Never terminal Failed",
			exists: true,
			policy: v1alpha1.RestartPolicyNever,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: now},
			},
			want: ActionNone,
		},
		{
			name:   "signal exit OnFailure",
			exists: true,
			policy: v1alpha1.RestartPolicyOnFailure,
			rt: &RuntimeRecord{
				StartedOnce: true,
				LastExit:    &ExitInfo{Signal: "TERM", Nonzero: true, FinishedAt: now.Add(-time.Minute)},
			},
			want: ActionStart,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeProcAction(tt.exists, tt.policy, tt.rt, now)
			if got.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", got.Kind, tt.want)
			}
			if tt.want == ActionWait && !got.Until.Equal(tt.until) {
				t.Errorf("Until = %v, want %v", got.Until, tt.until)
			}
		})
	}
}

func TestNoteExitBackoffRampAndReset(t *testing.T) {
	rt := &RuntimeRecord{}
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	one := 1

	noteExit(rt, ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: t0}, time.Second)
	if rt.NextBackoff != 20*time.Second {
		t.Fatalf("after first exit NextBackoff = %v, want 20s", rt.NextBackoff)
	}
	if !rt.BackoffUntil.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("BackoffUntil = %v, want t0+10s", rt.BackoffUntil)
	}
	if rt.RestartCount != 1 {
		t.Fatalf("RestartCount = %d, want 1", rt.RestartCount)
	}

	noteExit(rt, ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: t0.Add(time.Minute)}, time.Second)
	if rt.NextBackoff != 40*time.Second {
		t.Fatalf("after second exit NextBackoff = %v, want 40s", rt.NextBackoff)
	}

	// Cap at 5m.
	rt.NextBackoff = backoffCap
	noteExit(rt, ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: t0}, time.Second)
	if rt.NextBackoff != backoffCap {
		t.Fatalf("capped NextBackoff = %v, want %v", rt.NextBackoff, backoffCap)
	}

	// Long healthy run resets ladder for the *next* failure's delay base.
	noteExit(rt, ExitInfo{ExitCode: &one, Nonzero: true, FinishedAt: t0}, backoffResetAfter)
	if got := rt.BackoffUntil.Sub(t0); got != backoffInitial {
		t.Fatalf("after reset delay = %v, want %v", got, backoffInitial)
	}
	if rt.NextBackoff != 20*time.Second {
		t.Fatalf("after reset NextBackoff = %v, want 20s", rt.NextBackoff)
	}
}
