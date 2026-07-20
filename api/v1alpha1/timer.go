// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Timer declares a scheduled workload: a process template fired on a cron
// schedule, one run-to-completion Proc per tick (the systemd-timer / cron
// replacement). Timers create Procs directly — there is no intermediate
// Job kind (decision of record, doc 08 M5-a).
type Timer struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta  `json:"metadata"`
	Spec     TimerSpec   `json:"spec"`
	Status   TimerStatus `json:"status,omitzero"`
}

// TimerSpec is the desired state of a Timer.
type TimerSpec struct {
	// Schedule is a standard 5-field cron expression or a descriptor
	// (@hourly, @daily, @every 10s, ...), evaluated in TimeZone if set, else
	// host-local time.
	Schedule string `json:"schedule"`
	// TimeZone is an IANA zone name (e.g. "America/New_York") the schedule is
	// evaluated in (M9-f). Nil = host-local time (unchanged). @every schedules
	// are duration-based and zone-independent.
	TimeZone *string `json:"timeZone,omitempty"`
	// JitterSeconds spreads a tick's fire time by a deterministic delay in
	// [0, jitterSeconds) (M9-g), the systemd RandomizedDelaySec analog. Nil/0 =
	// no jitter. The delay is derived from (timer UID, tick), so level-triggered
	// re-evaluation computes the same fire time.
	JitterSeconds *int32 `json:"jitterSeconds,omitempty"`
	// CatchUp, when true, fires a run for a tick missed while impd was down
	// instead of skipping it — the systemd Persistent=true analog (M9-h). Nil/
	// false = today's skip posture (Persistent=false). A long downtime still
	// collapses to a single run (mostRecentDue).
	CatchUp *bool `json:"catchUp,omitempty"`
	// Suspend pauses scheduling without deleting the Timer. Runs already
	// started are left alone.
	Suspend *bool `json:"suspend,omitempty"`
	// ConcurrencyPolicy says what to do when a tick fires while a
	// previous run is still active. Default Forbid.
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`
	// StartingDeadlineSeconds bounds how late a missed tick may still
	// fire. Nil means missed ticks are skipped (systemd Persistent=false).
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
	// SuccessfulHistoryLimit / FailedHistoryLimit cap how many finished
	// Procs are kept for inspection. Defaults 3 / 1.
	SuccessfulHistoryLimit *int32 `json:"successfulHistoryLimit,omitempty"`
	FailedHistoryLimit     *int32 `json:"failedHistoryLimit,omitempty"`
	// Template describes the Proc created per tick. RestartPolicy must be
	// Never or OnFailure (defaulted to Never).
	Template ProcTemplate `json:"template"`
}

// ConcurrencyPolicy names how overlapping Timer runs are handled.
type ConcurrencyPolicy string

const (
	// ConcurrencyForbid skips a tick while a previous run is active.
	ConcurrencyForbid ConcurrencyPolicy = "Forbid"
	// ConcurrencyAllow lets runs overlap.
	ConcurrencyAllow ConcurrencyPolicy = "Allow"
	// ConcurrencyReplace deletes the active run, then starts the new one.
	ConcurrencyReplace ConcurrencyPolicy = "Replace"
)

// TimerStatus is the observed state of a Timer, written only by the
// TimerController.
type TimerStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastScheduleTime is the tick that most recently produced (or
	// deliberately skipped) a run.
	LastScheduleTime Time `json:"lastScheduleTime,omitzero"`
	// LastSuccessfulTime is the scheduled time of the newest Succeeded run.
	LastSuccessfulTime Time `json:"lastSuccessfulTime,omitzero"`
	// ActiveProc names the currently running Proc, "" when none.
	ActiveProc string      `json:"activeProc,omitempty"`
	Conditions []Condition `json:"conditions,omitempty"`
}
