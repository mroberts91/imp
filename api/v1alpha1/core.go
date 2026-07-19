// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 defines the imp API
package v1alpha1

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
)

var allowedKids = map[string]struct{}{
	KindDaemon: {},
	KindProc:   {},
	KindEvent:  {},
	KindTimer:  {},
}

const (
	LabelDaemonName       = "impd.sh/daemon-name"
	LabelTemplateHash     = "impd.sh/template-hash"
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
