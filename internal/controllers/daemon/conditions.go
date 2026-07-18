// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package daemon

// Condition helpers forked from GetDeploymentCondition /
// SetDeploymentCondition / filterOutCondition in kubernetes
// pkg/controller/deployment/util/deployment_util.go (Copyright The
// Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/). Deltas: they
// operate on v1alpha1.DaemonStatus with imp's shared Condition type
// (string-typed Type), and there is no RemoveDeploymentCondition - imp
// never retires a condition type. The semantics are kept exactly: setting
// a condition whose Status AND Reason are unchanged is a no-op (prevents
// timestamp churn and status-write hot loops), and LastTransitionTime is
// preserved whenever Status is unchanged.

import "github.com/mroberts91/imp/api/v1alpha1"

// getCondition returns a pointer to the condition of the given type, or
// nil if the status has none.
func getCondition(status v1alpha1.DaemonStatus, condType string) *v1alpha1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

// setCondition updates status with cond: no-op when the existing condition
// of the same type already has cond's Status and Reason; otherwise the old
// condition is replaced, carrying its LastTransitionTime forward when the
// Status did not change.
func setCondition(status *v1alpha1.DaemonStatus, cond v1alpha1.Condition) {
	current := getCondition(*status, cond.Type)
	if current != nil && current.Status == cond.Status && current.Reason == cond.Reason {
		return
	}
	// Do not update lastTransitionTime if the status of the condition
	// doesn't change.
	if current != nil && current.Status == cond.Status {
		cond.LastTransitionTime = current.LastTransitionTime
	}
	status.Conditions = append(filterOutCondition(status.Conditions, cond.Type), cond)
}

// filterOutCondition returns conditions without any condition of the given
// type.
func filterOutCondition(conditions []v1alpha1.Condition, condType string) []v1alpha1.Condition {
	var out []v1alpha1.Condition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		out = append(out, c)
	}
	return out
}
