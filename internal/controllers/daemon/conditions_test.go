// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func condAt(status v1alpha1.ConditionStatus, reason string, at time.Time) v1alpha1.Condition {
	return v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeAvailable,
		Status:             status,
		Reason:             reason,
		Message:            "m-" + reason,
		LastTransitionTime: v1alpha1.NewTime(at),
	}
}

func TestSetConditionAdds(t *testing.T) {
	var status v1alpha1.DaemonStatus
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	setCondition(&status, condAt(v1alpha1.ConditionFalse, "A", t0))
	got := getCondition(status, v1alpha1.ConditionTypeAvailable)
	if got == nil {
		t.Fatal("condition not added")
	}
	if got.Status != v1alpha1.ConditionFalse || got.Reason != "A" {
		t.Errorf("got %s/%s, want False/A", got.Status, got.Reason)
	}
	if !got.LastTransitionTime.Equal(v1alpha1.NewTime(t0)) {
		t.Errorf("lastTransitionTime = %v, want %v", got.LastTransitionTime, t0)
	}
}

func TestSetConditionNoOpOnSameStatusAndReason(t *testing.T) {
	var status v1alpha1.DaemonStatus
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	setCondition(&status, condAt(v1alpha1.ConditionFalse, "A", t0))
	later := condAt(v1alpha1.ConditionFalse, "A", t0.Add(time.Hour))
	later.Message = "a different message that must not be written"
	setCondition(&status, later)

	got := getCondition(status, v1alpha1.ConditionTypeAvailable)
	if got.Message != "m-A" {
		t.Errorf("message = %q; same status+reason must be a full no-op", got.Message)
	}
	if !got.LastTransitionTime.Equal(v1alpha1.NewTime(t0)) {
		t.Errorf("lastTransitionTime moved to %v on a no-op set", got.LastTransitionTime)
	}
	if len(status.Conditions) != 1 {
		t.Errorf("len(conditions) = %d, want 1", len(status.Conditions))
	}
}

func TestSetConditionPreservesTransitionTimeWhenStatusUnchanged(t *testing.T) {
	var status v1alpha1.DaemonStatus
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	setCondition(&status, condAt(v1alpha1.ConditionTrue, "A", t0))
	setCondition(&status, condAt(v1alpha1.ConditionTrue, "B", t0.Add(time.Hour)))

	got := getCondition(status, v1alpha1.ConditionTypeAvailable)
	if got.Reason != "B" {
		t.Errorf("reason = %q, want B (reason change must update)", got.Reason)
	}
	if !got.LastTransitionTime.Equal(v1alpha1.NewTime(t0)) {
		t.Errorf("lastTransitionTime = %v, want original %v (status unchanged)", got.LastTransitionTime, t0)
	}
}

func TestSetConditionNewTransitionUpdatesTime(t *testing.T) {
	var status v1alpha1.DaemonStatus
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	setCondition(&status, condAt(v1alpha1.ConditionFalse, "A", t0))
	setCondition(&status, condAt(v1alpha1.ConditionTrue, "B", t1))

	got := getCondition(status, v1alpha1.ConditionTypeAvailable)
	if got.Status != v1alpha1.ConditionTrue || got.Reason != "B" {
		t.Errorf("got %s/%s, want True/B", got.Status, got.Reason)
	}
	if !got.LastTransitionTime.Equal(v1alpha1.NewTime(t1)) {
		t.Errorf("lastTransitionTime = %v, want %v (genuine transition)", got.LastTransitionTime, t1)
	}
	if len(status.Conditions) != 1 {
		t.Errorf("len(conditions) = %d, want 1 (replace by type)", len(status.Conditions))
	}
}

func TestSetConditionLeavesOtherTypesAlone(t *testing.T) {
	var status v1alpha1.DaemonStatus
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	prog := condAt(v1alpha1.ConditionTrue, "P", t0)
	prog.Type = v1alpha1.ConditionTypeProgressing
	setCondition(&status, prog)
	setCondition(&status, condAt(v1alpha1.ConditionFalse, "A", t0))

	if len(status.Conditions) != 2 {
		t.Fatalf("len(conditions) = %d, want 2", len(status.Conditions))
	}
	if getCondition(status, v1alpha1.ConditionTypeProgressing) == nil {
		t.Error("Progressing condition lost when setting Available")
	}
}
