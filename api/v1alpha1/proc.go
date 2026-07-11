// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Proc is one supervised process instance.
type Proc struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     ProcSpec   `json:"spec"`
	Status   ProcStatus `json:"status,omitzero"`
}

// ProcSpec is the fully-resolved process definition: identical in shape to
// ProcTemplateSpec, with all defaults applied at creation by the controller
// so execd never defaults anything. A Proc's spec is immutable after
// creation.
type ProcSpec = ProcTemplateSpec

// EnvVar is a single environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// RestartPolicy dictates when execd restarts an exited process.
type RestartPolicy string

const (
	RestartPolicyAlways    RestartPolicy = "Always"
	RestartPolicyOnFailure RestartPolicy = "OnFailure"
	RestartPolicyNever     RestartPolicy = "Never"
)

// ProcTemplateSpec is the process definition, shared verbatim between a
// Daemon's template and a Proc's spec
type ProcTemplateSpec struct {
	Command                       []string      `json:"command"`
	Env                           []EnvVar      `json:"env,omitempty"`
	WorkingDir                    string        `json:"workingDir,omitempty"`
	User                          string        `json:"user,omitempty"`
	Group                         string        `json:"group,omitempty"`
	RestartPolicy                 RestartPolicy `json:"restartPolicy,omitempty"`
	StopSignal                    string        `json:"stopSignal,omitempty"`
	TerminationGracePeriodSeconds *int64        `json:"terminationGracePeriodSeconds,omitempty"`
}

type ProcPhase string

const (
	ProcPhasePending   ProcPhase = "Pending"
	ProcPhaseRunning   ProcPhase = "Running"
	ProcPhaseSucceeded ProcPhase = "Succeeded"
	ProcPhaseFailed    ProcPhase = "Failed"
	ProcPhaseUnknown   ProcPhase = "Unknown"
)

const WaitingReasonCrashLoopBackOff = "CrashLoopBackOff"

type ProcState struct {
	Waiting    *ProcStateWaiting    `json:"waiting,omitempty"`
	Running    *ProcStateRunning    `json:"running,omitempty"`
	Terminated *ProcStateTerminated `json:"terminated,omitempty"`
}

// ProcStateWaiting means no process is running and execd is deciding or
// delaying.
type ProcStateWaiting struct {
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	BackoffUntil Time   `json:"backoffUntil,omitzero"`
}

// ProcStateRunning means the process is alive.
type ProcStateRunning struct {
	PID            int   `json:"pid"`
	StartedAt      Time  `json:"startedAt,omitzero"`
	ProcStartTicks int64 `json:"procStartTicks,omitempty"`
}

// ProcStateTerminated means the process exited.
type ProcStateTerminated struct {
	ExitCode   *int   `json:"exitCode,omitempty"`
	Signal     string `json:"signal,omitempty"`
	FinishedAt Time   `json:"finishedAt,omitzero"`
	Message    string `json:"message,omitempty"`
}

// ProcStatus is the observed state of a Proc. Written only by execd.
type ProcStatus struct {
	Phase        ProcPhase   `json:"phase,omitempty"`
	State        ProcState   `json:"state,omitzero"`
	RestartCount int32       `json:"restartCount,omitempty"`
	Conditions   []Condition `json:"conditions,omitempty"`
}
