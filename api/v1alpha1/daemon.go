// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Daemon declares a workload: a process template plus how many replicas of it
// should run and how updates roll out.
type Daemon struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     DaemonSpec   `json:"spec"`
	Status   DaemonStatus `json:"status,omitzero"`
}

// DaemonSpec is the desired state of a Daemon.
type DaemonSpec struct {
	Replicas       *int32         `json:"replicas,omitempty"`
	UpdateStrategy UpdateStrategy `json:"updateStrategy,omitzero"`
	Template       ProcTemplate   `json:"template"`
}

// UpdateStrategyType names a Daemon update strategy.
type UpdateStrategyType string

const (
	UpdateStrategyRecreate UpdateStrategyType = "Recreate"
)

type UpdateStrategy struct {
	Type UpdateStrategyType `json:"type,omitempty"`
}

// ProcTemplate is the part of a DaemonSpec that describes the Procs to
// create.
type ProcTemplate struct {
	Metadata TemplateMeta     `json:"metadata,omitzero"`
	Spec     ProcTemplateSpec `json:"spec"`
}

// TemplateMeta is the subset of ObjectMeta a template may set on the objects
// created from it.
type TemplateMeta struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DaemonStatus is the observed state of a Daemon,
// rolled up from its owned Procs.
type DaemonStatus struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Replicas           int32       `json:"replicas,omitempty"`
	ReadyReplicas      int32       `json:"readyReplicas,omitempty"`
	UpdatedReplicas    int32       `json:"updatedReplicas,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
}
