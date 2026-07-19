// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Condition helpers forked from GetDeploymentCondition /
// SetDeploymentCondition / filterOutCondition in kubernetes
// pkg/controller/deployment/util/deployment_util.go and from
// SetStatusCondition in staging/src/k8s.io/apimachinery/pkg/api/meta/conditions.go
// (Copyright The Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/).
//
// Semantics: setting a condition that is identical to the stored one
// (Status, Reason, Message, ObservedGeneration) is a no-op (prevents
// timestamp churn and status-write hot loops). LastTransitionTime moves only
// on a Status flip; Reason/Message/ObservedGeneration changes are recorded
// in place under the preserved transition time — the apimachinery behavior
// (a generation bump must advance the condition's observedGeneration even
// when the condition itself hasn't flipped). Imp never retires a condition
// type.

// FindStatusCondition returns a pointer to the condition of the given type,
// or nil if none exists.
func FindStatusCondition(conditions []Condition, condType string) *Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// SetStatusCondition updates conditions with cond. Returns false when the
// existing condition of the same type already matches cond's Status, Reason,
// Message, and ObservedGeneration (full no-op). Otherwise replaces the old
// condition — carrying its LastTransitionTime forward when Status did not
// change — and returns true.
func SetStatusCondition(conditions *[]Condition, cond Condition) bool {
	current := FindStatusCondition(*conditions, cond.Type)
	if current == nil {
		*conditions = append(*conditions, cond)
		return true
	}
	// Do not update lastTransitionTime if the status of the condition
	// doesn't change.
	if current.Status == cond.Status {
		cond.LastTransitionTime = current.LastTransitionTime
	}
	if current.Status == cond.Status && current.Reason == cond.Reason &&
		current.Message == cond.Message && current.ObservedGeneration == cond.ObservedGeneration {
		return false
	}
	*conditions = append(filterOutCondition(*conditions, cond.Type), cond)
	return true
}

// filterOutCondition returns conditions without any condition of the given type.
func filterOutCondition(conditions []Condition, condType string) []Condition {
	var out []Condition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		out = append(out, c)
	}
	return out
}
