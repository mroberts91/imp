// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Notifier declares the operator-notification hook (M10-a): when a watched
// object holds a failure state — a crash-looping Proc, a failed
// run-to-completion Proc, a stuck rollout — the NotifierController creates
// one owned Proc from Template (the notification run) and execd runs it like
// any other process. The failure facts reach the run as IMP_NOTIFY_*
// environment variables, so a notifier is any executable: ft's `ft alert`,
// a three-line curl script to ntfy, sendmail. impd itself never carries a
// notification transport.
//
// Zero Notifiers means no notifications (the pre-M10 posture); one is the
// host-wide default; several partition targets via Selector.
type Notifier struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta     `json:"metadata"`
	Spec     NotifierSpec   `json:"spec"`
	Status   NotifierStatus `json:"status,omitzero"`
}

// NotifierSpec is the desired state of a Notifier.
type NotifierSpec struct {
	// Template describes the notification Proc created per firing signal.
	// RestartPolicy must be Never (the default): a notification is one
	// shot — if the failure level persists, the cooldown expiring is the
	// retry, and a failed notification run never re-fires itself (no
	// meta-alerting).
	Template ProcTemplate `json:"template"`
	// CooldownSeconds is the minimum age of the newest notification run for
	// the same (target, reason) before another may fire — the alert-fatigue
	// guard, and the boot-race damper. A still-running notification counts
	// as newest. Default 1800 (30 minutes).
	CooldownSeconds *int32 `json:"cooldownSeconds,omitempty"`
	// MinRestarts gates the crash-loop signal: a Proc in CrashLoopBackOff
	// fires only once its restartCount reaches this. Default 3 — a loop,
	// not a blip.
	MinRestarts *int32 `json:"minRestarts,omitempty"`
	// HistoryLimit caps how many finished notification runs are kept for
	// inspection, oldest pruned first. Default 20.
	HistoryLimit *int32 `json:"historyLimit,omitempty"`
	// Selector scopes which targets this Notifier watches, as comma-joined
	// equality terms over the target object's labels (k=v, k==v, k!=v —
	// the list labelSelector syntax, M9-i). Empty = every target.
	Selector string `json:"selector,omitempty"`
}

// NotifierStatus is the observed state of a Notifier, written only by the
// NotifierController.
type NotifierStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastNotificationTime is when the newest notification run was created.
	LastNotificationTime Time        `json:"lastNotificationTime,omitzero"`
	Conditions           []Condition `json:"conditions,omitempty"`
}

// Notification reasons — the values of the impd.sh/notified-reason
// annotation and the IMP_NOTIFY_REASON environment variable. Exactly three
// signals fire (M10-a3); Daemon Available=False deliberately does not — its
// causes already surface per-Proc, and double-paging one incident is the
// alert-fatigue failure mode.
const (
	// NotifyReasonCrashLoop: a Proc is in CrashLoopBackOff with
	// restartCount ≥ minRestarts.
	NotifyReasonCrashLoop = WaitingReasonCrashLoopBackOff
	// NotifyReasonRunFailed: a run-to-completion Proc (a Timer run, a
	// restartPolicy: Never one-off) terminated Failed.
	NotifyReasonRunFailed = "RunFailed"
	// NotifyReasonRolloutStuck: a Daemon's Progressing condition is False
	// with reason ProgressDeadlineExceeded.
	NotifyReasonRolloutStuck = "RolloutStuck"
)
