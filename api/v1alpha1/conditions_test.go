// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
	"time"
)

func condAt(status ConditionStatus, reason string, at time.Time) Condition {
	return Condition{
		Type:               ConditionTypeAvailable,
		Status:             status,
		Reason:             reason,
		Message:            "m-" + reason,
		LastTransitionTime: NewTime(at),
	}
}

func TestSetStatusConditionAdds(t *testing.T) {
	var conditions []Condition
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	if !SetStatusCondition(&conditions, condAt(ConditionFalse, "A", t0)) {
		t.Fatal("expected true on add")
	}
	got := FindStatusCondition(conditions, ConditionTypeAvailable)
	if got == nil {
		t.Fatal("condition not added")
	}
	if got.Status != ConditionFalse || got.Reason != "A" {
		t.Errorf("got %s/%s, want False/A", got.Status, got.Reason)
	}
	if !got.LastTransitionTime.Equal(NewTime(t0)) {
		t.Errorf("lastTransitionTime = %v, want %v", got.LastTransitionTime, t0)
	}
}

func TestSetStatusConditionNoOpOnIdenticalCondition(t *testing.T) {
	var conditions []Condition
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	SetStatusCondition(&conditions, condAt(ConditionFalse, "A", t0))
	// Identical condition an hour later: full no-op, timestamp untouched.
	if SetStatusCondition(&conditions, condAt(ConditionFalse, "A", t0.Add(time.Hour))) {
		t.Error("identical condition must be a full no-op (false)")
	}

	got := FindStatusCondition(conditions, ConditionTypeAvailable)
	if !got.LastTransitionTime.Equal(NewTime(t0)) {
		t.Errorf("lastTransitionTime moved to %v on a no-op set", got.LastTransitionTime)
	}
	if len(conditions) != 1 {
		t.Errorf("len(conditions) = %d, want 1", len(conditions))
	}
}

func TestSetStatusConditionUpdatesMessageAndObservedGeneration(t *testing.T) {
	var conditions []Condition
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	first := condAt(ConditionFalse, "A", t0)
	first.ObservedGeneration = 1
	SetStatusCondition(&conditions, first)

	// Same Status and Reason, but the generation advanced and the message
	// changed: both must be recorded in place, transition time preserved
	// (apimachinery semantics — a frozen observedGeneration would contradict
	// status.observedGeneration after a spec bump).
	next := condAt(ConditionFalse, "A", t0.Add(time.Hour))
	next.ObservedGeneration = 2
	next.Message = "still waiting, new generation"
	if !SetStatusCondition(&conditions, next) {
		t.Fatal("observedGeneration/message change must update (true)")
	}

	got := FindStatusCondition(conditions, ConditionTypeAvailable)
	if got.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want 2", got.ObservedGeneration)
	}
	if got.Message != "still waiting, new generation" {
		t.Errorf("message = %q, want the updated message", got.Message)
	}
	if !got.LastTransitionTime.Equal(NewTime(t0)) {
		t.Errorf("lastTransitionTime = %v, want original %v (status unchanged)", got.LastTransitionTime, t0)
	}
	if len(conditions) != 1 {
		t.Errorf("len(conditions) = %d, want 1", len(conditions))
	}
}

func TestSetStatusConditionPreservesTransitionTimeWhenStatusUnchanged(t *testing.T) {
	var conditions []Condition
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	SetStatusCondition(&conditions, condAt(ConditionTrue, "A", t0))
	if !SetStatusCondition(&conditions, condAt(ConditionTrue, "B", t0.Add(time.Hour))) {
		t.Fatal("reason change must update")
	}

	got := FindStatusCondition(conditions, ConditionTypeAvailable)
	if got.Reason != "B" {
		t.Errorf("reason = %q, want B (reason change must update)", got.Reason)
	}
	if !got.LastTransitionTime.Equal(NewTime(t0)) {
		t.Errorf("lastTransitionTime = %v, want original %v (status unchanged)", got.LastTransitionTime, t0)
	}
}

func TestSetStatusConditionNewTransitionUpdatesTime(t *testing.T) {
	var conditions []Condition
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	SetStatusCondition(&conditions, condAt(ConditionFalse, "A", t0))
	SetStatusCondition(&conditions, condAt(ConditionTrue, "B", t1))

	got := FindStatusCondition(conditions, ConditionTypeAvailable)
	if got.Status != ConditionTrue || got.Reason != "B" {
		t.Errorf("got %s/%s, want True/B", got.Status, got.Reason)
	}
	if !got.LastTransitionTime.Equal(NewTime(t1)) {
		t.Errorf("lastTransitionTime = %v, want %v (genuine transition)", got.LastTransitionTime, t1)
	}
	if len(conditions) != 1 {
		t.Errorf("len(conditions) = %d, want 1 (replace by type)", len(conditions))
	}
}

func TestSetStatusConditionLeavesOtherTypesAlone(t *testing.T) {
	var conditions []Condition
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	prog := condAt(ConditionTrue, "P", t0)
	prog.Type = ConditionTypeProgressing
	SetStatusCondition(&conditions, prog)
	SetStatusCondition(&conditions, condAt(ConditionFalse, "A", t0))

	if len(conditions) != 2 {
		t.Fatalf("len(conditions) = %d, want 2", len(conditions))
	}
	if FindStatusCondition(conditions, ConditionTypeProgressing) == nil {
		t.Error("Progressing condition lost when setting Available")
	}
}
