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
	// MinReadySeconds is how long a Proc must be Ready before it counts as
	// available (and before a rolling update proceeds past it). 0 = available
	// as soon as Ready.
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`
	// ProgressDeadlineSeconds flips Progressing to False with reason
	// ProgressDeadlineExceeded when a rollout makes no progress for this
	// long. Defaulted to 600 (materialized — outside the template, so no
	// hash impact). The deadline is a report, not a brake: reconciliation
	// continues.
	ProgressDeadlineSeconds *int32       `json:"progressDeadlineSeconds,omitempty"`
	Template                ProcTemplate `json:"template"`
}

// UpdateStrategyType names a Daemon update strategy.
type UpdateStrategyType string

const (
	UpdateStrategyRecreate      UpdateStrategyType = "Recreate"
	UpdateStrategyRollingUpdate UpdateStrategyType = "RollingUpdate"
)

// UpdateStrategy declares how a Daemon replaces Procs when the template
// hash changes. Discriminated union: Type selects the strategy; per-type
// options live in the matching field (only RollingUpdate today).
type UpdateStrategy struct {
	Type          UpdateStrategyType           `json:"type,omitempty"`
	RollingUpdate *RollingUpdateDaemonStrategy `json:"rollingUpdate,omitempty"`
}

// RollingUpdateDaemonStrategy is the StatefulSet-shaped rolling options:
// replace ordinals highest first, waiting for each replacement to become
// available. Partition is the minimum ordinal of the update target sequence
// (ordinals below it are left on the old hash).
type RollingUpdateDaemonStrategy struct {
	Partition *int32 `json:"partition,omitempty"`
	// MaxUnavailable is how many Procs (in [partition, replicas)) may be
	// unavailable at once during a roll — an absolute count, ≥ 1 (no
	// percentages on a single host, M9-e). Nil = 1: one ordinal at a time,
	// byte-for-byte today's behavior. maxSurge is deliberately absent — a
	// surge replica has no honest IMP_REPLICA_INDEX (D2).
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`
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
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	Replicas           int32 `json:"replicas,omitempty"`
	ReadyReplicas      int32 `json:"readyReplicas,omitempty"`
	// AvailableReplicas counts Procs Ready for at least minReadySeconds.
	AvailableReplicas int32       `json:"availableReplicas,omitempty"`
	UpdatedReplicas   int32       `json:"updatedReplicas,omitempty"`
	Conditions        []Condition `json:"conditions,omitempty"`
}
