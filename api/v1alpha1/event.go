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
	ReasonProbeFailed      = "ProbeFailed"
	ReasonUnhealthy        = "Unhealthy"
	ReasonAdopted          = "Adopted"
	ReasonOrphanKilled     = "OrphanKilled"
	ReasonScheduledRun     = "ScheduledRun"
	ReasonSkippedRun       = "SkippedRun"
	ReasonMissedRun        = "MissedRun"
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
