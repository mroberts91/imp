// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// ObjectRef points at the object an Event is about.
type ObjectRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type EventType string

const (
	EventTypeNormal  EventType = "Normal"
	EventTypeWarning EventType = "Warning"
)

const (
	ReasonApplied          = "Applied"
	ReasonFailedValidation = "FailedValidation"
	ReasonCreated          = "Created"
	ReasonStarted          = "Started"
	ReasonExited           = "Exited"
	ReasonBackOff          = "BackOff"
	ReasonKilling          = "Killing"
	ReasonDeleted          = "Deleted"
	ReasonScalingReplicas  = "ScalingReplicas"
	ReasonTemplateChanged  = "TemplateChanged"
	// ReasonConfigChanged (M8): a Daemon rolled because a referenced Config's
	// content changed (distinct from a template change).
	ReasonConfigChanged = "ConfigChanged"
	// ReasonConfigMissing (M8): a referenced Config does not exist. The
	// DaemonController emits it while holding rollout; execd emits it when a
	// Config is gone at spawn time.
	ReasonConfigMissing = "ConfigMissing"
	// ReasonConfigMaterializeFailed (M8): execd could not write a referenced
	// Config's files before spawn.
	ReasonConfigMaterializeFailed = "ConfigMaterializeFailed"
	// ReasonConfigPathConflict (M9-b): a path: ref would overwrite a file imp
	// did not write. execd refuses and fails the spawn honestly.
	ReasonConfigPathConflict = "ConfigPathConflict"
	ReasonProbeFailed        = "ProbeFailed"
	ReasonUnhealthy          = "Unhealthy"
	ReasonAdopted            = "Adopted"
	ReasonOrphanKilled       = "OrphanKilled"
	ReasonScheduledRun       = "ScheduledRun"
	ReasonSkippedRun         = "SkippedRun"
	ReasonMissedRun          = "MissedRun"
	// ReasonProgressDeadlineExceeded (M6): a Daemon rollout made no progress
	// for spec.progressDeadlineSeconds.
	ReasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
	// ReasonNotified (M10-a): a Notifier created a notification run for a
	// firing failure signal.
	ReasonNotified = "Notified"
)

// Event is an append-only, TTL-bounded record of something notable happening
// to an object.
type Event struct {
	TypeMeta           `json:",inline"`
	Metadata           ObjectMeta `json:"metadata"`
	Regarding          ObjectRef  `json:"regarding"`
	Type               EventType  `json:"type"`
	Reason             string     `json:"reason"`
	Message            string     `json:"message,omitempty"`
	Count              int32      `json:"count,omitempty"`
	FirstTimestamp     Time       `json:"firstTimestamp,omitzero"`
	LastTimestamp      Time       `json:"lastTimestamp,omitzero"`
	ReportingComponent string     `json:"reportingComponent,omitempty"`
}
