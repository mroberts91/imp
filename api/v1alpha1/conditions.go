// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Condition helpers forked from GetDeploymentCondition /
// SetDeploymentCondition / filterOutCondition in kubernetes
// pkg/controller/deployment/util/deployment_util.go and from
// SetStatusCondition in staging/src/k8s.io/apimachinery/pkg/api/meta/conditions.go
// (Copyright The Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/).
//
// Semantics: setting a condition whose Status AND Reason are unchanged is a
// no-op (prevents timestamp churn and status-write hot loops). LastTransitionTime
// is preserved whenever Status is unchanged. Imp never retires a condition type.

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
// existing condition of the same type already has cond's Status and Reason
// (full no-op). Otherwise replaces the old condition, carrying its
// LastTransitionTime forward when Status did not change, and returns true.
func SetStatusCondition(conditions *[]Condition, cond Condition) bool {
	current := FindStatusCondition(*conditions, cond.Type)
	if current != nil && current.Status == cond.Status && current.Reason == cond.Reason {
		return false
	}
	// Do not update lastTransitionTime if the status of the condition
	// doesn't change.
	if current != nil && current.Status == cond.Status {
		cond.LastTransitionTime = current.LastTransitionTime
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
