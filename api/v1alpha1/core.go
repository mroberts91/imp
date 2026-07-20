// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 defines the imp API
package v1alpha1

import "slices"

// API group identity.
const (
	Group      = "impd.sh"
	Version    = "v1alpha1"
	APIVersion = Group + "/" + Version
)

const (
	KindDaemon = "Daemon"
	KindProc   = "Proc"
	KindEvent  = "Event"
	KindTimer  = "Timer"
	KindConfig = "Config"
)

var allowedKids = map[string]struct{}{
	KindDaemon: {},
	KindProc:   {},
	KindEvent:  {},
	KindTimer:  {},
	KindConfig: {},
}

// AllKinds returns every registered kind, sorted. Use it where code must act
// on "every kind" (e.g. the manifest sweep) rather than hardcoding a list that
// silently drifts from the set the apiserver accepts when a new kind is added.
func AllKinds() []string {
	kinds := make([]string, 0, len(allowedKids))
	for k := range allowedKids {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}

const (
	LabelDaemonName   = "impd.sh/daemon-name"
	LabelTemplateHash = "impd.sh/template-hash"
	// LabelConfigHash is the combined revision hash of a Proc's referenced
	// Config content (M8-g). Present only on Procs whose Daemon template names
	// Configs; absent (reads "") for no-config daemons and pre-M8 Procs, so a
	// missing label compares equal to a missing label (the M7 upgrade
	// guarantee — no roll on upgrade).
	LabelConfigHash       = "impd.sh/config-hash"
	LabelReplicaIndex     = "impd.sh/replica-index"
	LabelTimerName        = "impd.sh/timer-name"
	AnnotationManagedBy   = "impd.sh/managed-by"
	AnnotationSourcePath  = "impd.sh/source-path"
	AnnotationScheduledAt = "impd.sh/scheduled-at"
	// AnnotationManual marks a Timer run created by `impctl run` rather than
	// the schedule. Manual runs never advance status.lastScheduleTime but do
	// count as active for concurrencyPolicy.
	AnnotationManual  = "impd.sh/manual"
	ManagedByManifest = "manifest"
)

const (
	ConditionTypeReady       = "Ready"
	ConditionTypeAvailable   = "Available"
	ConditionTypeProgressing = "Progressing"
	ConditionTypeActive      = "Active"
)

// ConditionStatus is the status of a condition: True, False, or Unknown.
type ConditionStatus string

// Valid condition statuses
const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// TypeMeta identifies the schema of an object representation.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// OwnerReference identifies the owning object. Deleting the owner cascades to
// the owned object via the GarbageCollector.
type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// ObjectMeta is the metadata every persisted object carries.
type ObjectMeta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	CreationTimestamp Time              `json:"creationTimestamp,omitzero"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
}

type Condition struct {
	Type               string          `json:"type"`
	Status             ConditionStatus `json:"status"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	LastTransitionTime Time            `json:"lastTransitionTime,omitzero"`
	// LastUpdateTime moves whenever SetStatusCondition records any change
	// (flip or in-place), unlike LastTransitionTime which moves only on a
	// status flip. Mini-fork of apps/v1 DeploymentCondition.LastUpdateTime;
	// the Daemon progress deadline anchors on it. Callers stamp it alongside
	// LastTransitionTime; conditions written before M6 simply lack it.
	LastUpdateTime Time   `json:"lastUpdateTime,omitzero"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
}
