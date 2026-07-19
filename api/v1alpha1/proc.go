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
	Command                       []string             `json:"command"`
	Env                           []EnvVar             `json:"env,omitempty"`
	WorkingDir                    string               `json:"workingDir,omitempty"`
	User                          string               `json:"user,omitempty"`
	Group                         string               `json:"group,omitempty"`
	RestartPolicy                 RestartPolicy        `json:"restartPolicy,omitempty"`
	StopSignal                    string               `json:"stopSignal,omitempty"`
	TerminationGracePeriodSeconds *int64               `json:"terminationGracePeriodSeconds,omitempty"`
	Resources                     ResourceRequirements `json:"resources,omitzero"`
	LivenessProbe                 *Probe               `json:"livenessProbe,omitempty"`
	ReadinessProbe                *Probe               `json:"readinessProbe,omitempty"`
	// StartupProbe, while present and not yet succeeded, holds liveness and
	// readiness probing; its failure threshold restarts the process (kubelet
	// semantics). Nil means no startup gate — M3 behavior. Like every M6
	// field below it is never defaulted when nil, so pre-M6 template hashes
	// stay stable and existing Daemons do not roll on upgrade.
	StartupProbe *Probe `json:"startupProbe,omitempty"`
	// Rlimits sets process resource limits (setrlimit) before exec, while
	// still privileged — so hard limits may be raised (LimitNOFILE= parity).
	// Like env, entry order is part of the template hash: reordering rolls
	// the Daemon.
	Rlimits []Rlimit `json:"rlimits,omitempty"`
	// Nice is the scheduling priority, -20..19. Negative values need
	// privilege (CAP_SYS_NICE). Nil = inherit (0).
	Nice *int32 `json:"nice,omitempty"`
	// OOMScoreAdjust is written to /proc/<pid>/oom_score_adj, -1000..1000.
	// Negative values need privilege. Nil = inherit.
	OOMScoreAdjust *int32 `json:"oomScoreAdjust,omitempty"`
	// Umask is the file-mode creation mask as an octal string, e.g. "0022".
	// Nil = inherit impd's umask.
	Umask *string `json:"umask,omitempty"`
	// LogRetention tunes per-proc log rotation. Nil means the built-in
	// defaults (10 MiB, 3 backups, no age pruning), resolved by execd at
	// consumption time — never defaulted here, so pre-M5 template hashes
	// stay stable and existing Daemons do not roll on upgrade.
	LogRetention *LogRetention `json:"logRetention,omitempty"`
}

// Rlimit sets one resource limit for the process. When only one of
// soft/hard is given, both are set to that value (systemd LimitNOFILE=
// semantics); -1 means unlimited (RLIM_INFINITY).
type Rlimit struct {
	// Resource is the lowercase RLIMIT_* name, e.g. "nofile", "core".
	Resource string `json:"resource"`
	Soft     *int64 `json:"soft,omitempty"`
	Hard     *int64 `json:"hard,omitempty"`
}

// RlimitInfinity is the sentinel Soft/Hard value meaning RLIM_INFINITY.
const RlimitInfinity = int64(-1)

// Values resolves the M6-g one-implies-both rule into the pair to set.
// Callers guarantee validity (at least one side present, checked by
// validation).
func (in Rlimit) Values() (soft, hard int64) {
	switch {
	case in.Soft != nil && in.Hard != nil:
		return *in.Soft, *in.Hard
	case in.Soft != nil:
		return *in.Soft, *in.Soft
	default:
		return *in.Hard, *in.Hard
	}
}

// LogRetention is the per-proc log rotation policy (doc 08 M5-d). It lives
// inside the hashed template: changing it rolls the Daemon, like any other
// spec change. Nil inner fields mean the built-in default for that field.
type LogRetention struct {
	// MaxSizeMB is the size a log file may reach before rotation.
	MaxSizeMB *int32 `json:"maxSizeMB,omitempty"`
	// MaxBackups is how many rotated files are kept. 0 keeps them all
	// (bounded only by MaxAgeDays) — 0 consistently means "no limit on
	// this axis".
	MaxBackups *int32 `json:"maxBackups,omitempty"`
	// MaxAgeDays prunes rotated files older than this. 0 keeps them until
	// MaxBackups retires them.
	MaxAgeDays *int32 `json:"maxAgeDays,omitempty"`
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
