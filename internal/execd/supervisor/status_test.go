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
