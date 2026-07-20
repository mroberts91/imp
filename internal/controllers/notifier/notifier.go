// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package notifier implements the NotifierController, the generic half of
// imp's failure notification (M10-a): it watches for held failure states —
// crash-looping Procs, failed run-to-completion Procs, stuck Daemon
// rollouts — and answers each with one owned notification run built from
// the Notifier's template. The run is an ordinary Proc: execd spawns it
// (this controller never touches the OS), its logs are retained, and its
// history is listable and GC-cascaded.
//
// Design rules (doc 18 §2):
//   - Signals are levels read from status/conditions, never Event content;
//     re-reconciling recomputes everything from the caches and the clock.
//   - Cooldown/dedup state is the notification runs themselves — the
//     impd.sh/notified-* annotations plus creationTimestamp — so a restart
//     loses nothing and `impctl get procs -l impd.sh/notifier-name=X` is
//     the audit trail.
//   - Procs labeled impd.sh/notifier-name never produce signals: a failing
//     notification is a Failed Proc in the store for a human, never fuel
//     for another notification (no meta-alerting, M10-a6).
package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/controllers"
	"github.com/mroberts91/imp/internal/queue"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

const componentName = "notifier-controller"

// ReportingComponent is stamped on Events emitted by this controller.
const ReportingComponent = "controllers/notifier"

// Controller reconciles Notifiers: gather firing failure signals, create a
// notification run per signal outside its cooldown, prune run history, and
// roll observed state into Notifier status (sole writer). It reads only
// the informer stores and writes only through the client; wiring owns
// informers, queue, and runner.
type Controller struct {
	client    *client.Client
	notifiers *cache.Store
	daemons   *cache.Store
	timers    *cache.Store
	procs     *cache.Store
	clock     clock.Clock
	recorder  *recorder.Recorder
	log       *slog.Logger
}

var _ controllers.Reconciler = (*Controller)(nil)

// New builds a Controller over cl and the informer stores. A nil clk means
// the real clock; a nil rec builds a recorder.
func New(cl *client.Client, notifiers, daemons, timers, procs *cache.Store, clk clock.Clock, rec *recorder.Recorder) *Controller {
	if clk == nil {
		clk = clock.Real{}
	}
	if rec == nil {
		rec = recorder.New(cl, ReportingComponent, clk)
	}
	return &Controller{
		client:    cl,
		notifiers: notifiers,
		daemons:   daemons,
		timers:    timers,
		procs:     procs,
		clock:     clk,
		recorder:  rec,
		log:       slog.With("component", componentName),
	}
}

// signal is one firing failure level, resolved to the target the operator
// thinks in: the owning Daemon or Timer when there is one, the Proc itself
// otherwise (M10-a3).
type signal struct {
	targetKind   string
	targetName   string
	targetUID    string
	targetLabels map[string]string
	reason       string
	message      string
	procName     string // the concrete Proc behind the signal ("" for RolloutStuck)
	exitCode     *int
	signalName   string
	restarts     int32
	since        v1alpha1.Time
}

// dedupKey is the cooldown identity (M10-a4): one notification per
// (target, reason) per cooldown window, however many replicas misbehave.
func (s *signal) dedupKey() string {
	return s.targetUID + "\x00" + s.reason
}

// Reconcile converges the Notifier named by key ("Notifier/<name>"): prune
// history, evaluate the failure levels against the cooldown ledger, create
// what is due, and roll up status. Level-triggered throughout.
func (c *Controller) Reconcile(ctx context.Context, key string) error {
	raw, ok := c.notifiers.GetByKey(key)
	if !ok {
		// Notifier deleted: GC owns the cascade to its runs.
		return nil
	}
	var n v1alpha1.Notifier
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("decoding %s from cache: %w", key, err)
	}

	terms, err := v1alpha1.ParseLabelSelector(n.Spec.Selector)
	if err != nil {
		// Validation prevents this; a stored unparseable selector is
		// permanent — retrying cannot fix it. Surface and stop.
		c.emit(ctx, &n, v1alpha1.EventTypeWarning, v1alpha1.ReasonFailedValidation,
			fmt.Sprintf("unparseable selector %q: %v", n.Spec.Selector, err))
		return c.rollupStatus(ctx, &n, c.ownRuns(n.Metadata.Name))
	}

	runs := c.ownRuns(n.Metadata.Name)
	if err := c.pruneHistory(ctx, &n, runs); err != nil {
		return err
	}

	now := c.clock.Now()
	cooldown := time.Duration(*n.Spec.CooldownSeconds) * time.Second
	var requeueIn time.Duration
	for _, sig := range c.gatherSignals(*n.Spec.MinRestarts) {
		if !v1alpha1.LabelsMatch(sig.targetLabels, terms) {
			continue
		}
		if newest := newestRunFor(runs, &sig); newest != nil {
			age := now.Sub(newest.Metadata.CreationTimestamp.Time)
			if age < cooldown {
				// Within cooldown (a still-running notification counts):
				// wake when it expires, in case the level still holds.
				if remaining := cooldown - age; requeueIn == 0 || remaining < requeueIn {
					requeueIn = remaining
				}
				continue
			}
		}
		run := BuildNotification(&n, &sig, now)
		created, err := c.client.ApplyProc(ctx, run)
		if err != nil {
			return fmt.Errorf("creating notification run %s: %w", run.Metadata.Name, err)
		}
		runs = append(runs, *created)
		c.emit(ctx, &n, v1alpha1.EventTypeNormal, v1alpha1.ReasonNotified,
			fmt.Sprintf("created run %s for %s of %s %s",
				created.Metadata.Name, sig.reason, sig.targetKind, sig.targetName))
	}

	if err := c.rollupStatus(ctx, &n, runs); err != nil {
		return err
	}
	if requeueIn > 0 {
		return controllers.RequeueAfter{After: requeueIn}
	}
	return nil
}

// gatherSignals scans the caches for held failure levels (M10-a3): exactly
// three kinds fire — CrashLoopBackOff past minRestarts, terminal RunFailed,
// and RolloutStuck. Multiple Procs collapsing to one dedup key (several
// replicas of one Daemon crash-looping) yield the worst single signal.
func (c *Controller) gatherSignals(minRestarts int32) []signal {
	byKey := map[string]signal{}
	for _, raw := range c.procs.List() {
		var p v1alpha1.Proc
		if err := json.Unmarshal(raw, &p); err != nil {
			c.log.Warn("skipping unreadable proc in cache", "kind", v1alpha1.KindProc, "error", err)
			continue
		}
		// No meta-alerting (M10-a6): notification runs never signal.
		if p.Metadata.Labels[v1alpha1.LabelNotifierName] != "" {
			continue
		}
		if sig, ok := c.procSignal(&p, minRestarts); ok {
			key := sig.dedupKey()
			if cur, dup := byKey[key]; !dup || sig.restarts > cur.restarts {
				byKey[key] = sig
			}
		}
	}
	for _, raw := range c.daemons.List() {
		var d v1alpha1.Daemon
		if err := json.Unmarshal(raw, &d); err != nil {
			c.log.Warn("skipping unreadable daemon in cache", "kind", v1alpha1.KindDaemon, "error", err)
			continue
		}
		cond := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
		if cond == nil || cond.Status != v1alpha1.ConditionFalse || cond.Reason != v1alpha1.ReasonProgressDeadlineExceeded {
			continue
		}
		sig := signal{
			targetKind:   v1alpha1.KindDaemon,
			targetName:   d.Metadata.Name,
			targetUID:    d.Metadata.UID,
			targetLabels: d.Metadata.Labels,
			reason:       v1alpha1.NotifyReasonRolloutStuck,
			message:      fmt.Sprintf("rollout of Daemon %s is stuck: %s", d.Metadata.Name, cond.Message),
			since:        cond.LastTransitionTime,
		}
		byKey[sig.dedupKey()] = sig
	}
	sigs := slices.Collect(maps.Values(byKey))
	// Deterministic order for tests and stable event streams.
	slices.SortFunc(sigs, func(a, b signal) int { return strings.Compare(a.dedupKey(), b.dedupKey()) })
	return sigs
}

// procSignal projects one Proc onto the two Proc-level failure levels.
func (c *Controller) procSignal(p *v1alpha1.Proc, minRestarts int32) (signal, bool) {
	if w := p.Status.State.Waiting; w != nil &&
		w.Reason == v1alpha1.WaitingReasonCrashLoopBackOff &&
		p.Status.RestartCount >= minRestarts {
		sig := c.targetOf(p)
		sig.reason = v1alpha1.NotifyReasonCrashLoop
		sig.restarts = p.Status.RestartCount
		sig.message = fmt.Sprintf("proc %s is crash-looping (%d restarts)", p.Metadata.Name, p.Status.RestartCount)
		if lt := p.Status.State.LastTerminated; lt != nil {
			sig.exitCode = lt.ExitCode
			sig.signalName = lt.Signal
			sig.since = lt.FinishedAt
		}
		return sig, true
	}
	if p.Status.Phase == v1alpha1.ProcPhaseFailed {
		sig := c.targetOf(p)
		sig.reason = v1alpha1.NotifyReasonRunFailed
		sig.message = fmt.Sprintf("run %s failed", p.Metadata.Name)
		if t := p.Status.State.Terminated; t != nil {
			sig.exitCode = t.ExitCode
			sig.signalName = t.Signal
			sig.since = t.FinishedAt
			if t.Message != "" {
				sig.message = fmt.Sprintf("run %s failed: %s", p.Metadata.Name, t.Message)
			}
		}
		return sig, true
	}
	return signal{}, false
}

// targetOf resolves the object the operator thinks in: the owning Daemon or
// Timer (identity from the ownerReference — authoritative even when the
// cache is momentarily stale; labels from the owner object when present,
// else the Proc's own), the Proc itself otherwise.
func (c *Controller) targetOf(p *v1alpha1.Proc) signal {
	sig := signal{
		targetKind:   v1alpha1.KindProc,
		targetName:   p.Metadata.Name,
		targetUID:    p.Metadata.UID,
		targetLabels: p.Metadata.Labels,
		procName:     p.Metadata.Name,
	}
	for _, ref := range p.Metadata.OwnerReferences {
		if ref.Kind != v1alpha1.KindDaemon && ref.Kind != v1alpha1.KindTimer {
			continue
		}
		sig.targetKind = ref.Kind
		sig.targetName = ref.Name
		sig.targetUID = ref.UID
		owners := c.daemons
		if ref.Kind == v1alpha1.KindTimer {
			owners = c.timers
		}
		if raw, ok := owners.GetByKey(ref.Kind + "/" + ref.Name); ok {
			var meta struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &meta); err == nil {
				sig.targetLabels = meta.Metadata.Labels
			}
		}
		break
	}
	return sig
}

// ownRuns returns this Notifier's notification runs, sorted oldest-first by
// creation time.
func (c *Controller) ownRuns(notifierName string) []v1alpha1.Proc {
	var runs []v1alpha1.Proc
	for _, raw := range c.procs.List() {
		var p v1alpha1.Proc
		if err := json.Unmarshal(raw, &p); err != nil {
			c.log.Warn("skipping unreadable proc in cache", "kind", v1alpha1.KindProc, "error", err)
			continue
		}
		if p.Metadata.Labels[v1alpha1.LabelNotifierName] != notifierName {
			continue
		}
		runs = append(runs, p)
	}
	slices.SortFunc(runs, func(a, b v1alpha1.Proc) int {
		return a.Metadata.CreationTimestamp.Compare(b.Metadata.CreationTimestamp.Time)
	})
	return runs
}

// newestRunFor returns the newest run (any phase — a still-running
// notification holds the cooldown) recording the signal's dedup key, nil
// when none.
func newestRunFor(runs []v1alpha1.Proc, sig *signal) *v1alpha1.Proc {
	for i, run := range slices.Backward(runs) {
		ann := run.Metadata.Annotations
		if ann[v1alpha1.AnnotationNotifiedUID] == sig.targetUID &&
			ann[v1alpha1.AnnotationNotifiedReason] == sig.reason {
			return &runs[i]
		}
	}
	return nil
}

// pruneHistory deletes finished runs beyond spec.historyLimit, oldest
// first. One combined limit — failures stay visible inside the same window
// (M10-a1). runs is sorted oldest-first.
func (c *Controller) pruneHistory(ctx context.Context, n *v1alpha1.Notifier, runs []v1alpha1.Proc) error {
	var finished []v1alpha1.Proc
	for _, r := range runs {
		if r.Status.Phase == v1alpha1.ProcPhaseSucceeded || r.Status.Phase == v1alpha1.ProcPhaseFailed {
			finished = append(finished, r)
		}
	}
	over := len(finished) - int(*n.Spec.HistoryLimit)
	var errs []error
	for i := range over {
		name := finished[i].Metadata.Name
		if err := c.client.DeleteProc(ctx, name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
			errs = append(errs, fmt.Errorf("pruning run %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// BuildNotification builds the Proc for one notification: template metadata
// first, the notifier-name label and notified-* annotations layered on top
// (the dedup ledger), an ownerReference back to the Notifier, and the
// failure facts prepended to env as IMP_NOTIFY_* (prepended so they win
// over a colliding template variable — first occurrence wins at getenv).
// The name embeds a hash of (target, reason) plus the second, so a retried
// pass re-applies idempotently and distinct targets never collide.
func BuildNotification(n *v1alpha1.Notifier, sig *signal, at time.Time) *v1alpha1.Proc {
	labels := map[string]string{}
	maps.Copy(labels, n.Spec.Template.Metadata.Labels)
	labels[v1alpha1.LabelNotifierName] = n.Metadata.Name

	annotations := map[string]string{}
	maps.Copy(annotations, n.Spec.Template.Metadata.Annotations)
	annotations[v1alpha1.AnnotationNotifiedKind] = sig.targetKind
	annotations[v1alpha1.AnnotationNotifiedName] = sig.targetName
	annotations[v1alpha1.AnnotationNotifiedUID] = sig.targetUID
	annotations[v1alpha1.AnnotationNotifiedReason] = sig.reason

	spec := n.Spec.Template.Spec.DeepCopy()
	// One shot, structurally: validation already forbids anything else, but
	// the invariant is cheap to hard-code where the run is born.
	spec.RestartPolicy = v1alpha1.RestartPolicyNever
	spec.Env = append(notifyEnv(sig), spec.Env...)

	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%s-%d", n.Metadata.Name, signalHash(sig), at.Unix()),
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindNotifier,
				Name:       n.Metadata.Name,
				UID:        n.Metadata.UID,
			}},
		},
		Spec: *spec,
	}
}

// signalHash is a short stable identity for (target, reason) in run names —
// fixed length regardless of target-name length, no cross-target collisions
// within a second.
func signalHash(sig *signal) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", sig.targetKind, sig.targetName, sig.targetUID, sig.reason)
	return fmt.Sprintf("%08x", h.Sum32())
}

// notifyEnv renders the failure facts as the IMP_NOTIFY_* contract
// (M10-a5). Every variable is always present — empty when not applicable —
// so notifier scripts never need existence checks.
func notifyEnv(sig *signal) []v1alpha1.EnvVar {
	exitCode := ""
	if sig.exitCode != nil {
		exitCode = strconv.Itoa(*sig.exitCode)
	}
	since := ""
	if !sig.since.IsZero() {
		since = sig.since.Format(time.RFC3339)
	}
	return []v1alpha1.EnvVar{
		{Name: "IMP_NOTIFY_KIND", Value: sig.targetKind},
		{Name: "IMP_NOTIFY_NAME", Value: sig.targetName},
		{Name: "IMP_NOTIFY_PROC", Value: sig.procName},
		{Name: "IMP_NOTIFY_REASON", Value: sig.reason},
		{Name: "IMP_NOTIFY_MESSAGE", Value: sig.message},
		{Name: "IMP_NOTIFY_EXIT_CODE", Value: exitCode},
		{Name: "IMP_NOTIFY_SIGNAL", Value: sig.signalName},
		{Name: "IMP_NOTIFY_RESTARTS", Value: strconv.Itoa(int(sig.restarts))},
		{Name: "IMP_NOTIFY_SINCE", Value: since},
	}
}

// rollupStatus writes the Notifier's status: observedGeneration, the newest
// notification time, and the Active condition. Sole-writer duty of this
// controller; guarded against same-name recreation by UID.
func (c *Controller) rollupStatus(ctx context.Context, n *v1alpha1.Notifier, runs []v1alpha1.Proc) error {
	name := n.Metadata.Name
	gen := n.Metadata.Generation

	var lastNotification v1alpha1.Time
	inFlight := ""
	for i := range runs {
		if ct := runs[i].Metadata.CreationTimestamp; ct.After(lastNotification.Time) {
			lastNotification = ct
		}
		switch runs[i].Status.Phase {
		case v1alpha1.ProcPhaseSucceeded, v1alpha1.ProcPhaseFailed:
		default:
			inFlight = runs[i].Metadata.Name
		}
	}

	cond := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeActive,
		Status:             v1alpha1.ConditionFalse,
		Reason:             "Idle",
		Message:            "no notification in flight",
		ObservedGeneration: gen,
		LastTransitionTime: v1alpha1.NewTime(c.clock.Now()),
	}
	if inFlight != "" {
		cond.Status = v1alpha1.ConditionTrue
		cond.Reason = "Notifying"
		cond.Message = "notification run " + inFlight + " in flight"
	}

	err := client.RetryOnConflict(func() error {
		fresh, err := c.client.GetNotifier(ctx, name)
		if err != nil {
			return err
		}
		if fresh.Metadata.UID != n.Metadata.UID {
			// Deleted and recreated under the same name mid-pass: this
			// observation belongs to the dead incarnation.
			return nil
		}
		fresh.Status.ObservedGeneration = gen
		// lastNotificationTime only moves forward.
		if lastNotification.After(fresh.Status.LastNotificationTime.Time) {
			fresh.Status.LastNotificationTime = lastNotification
		}
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, cond)
		_, err = c.client.UpdateNotifierStatus(ctx, fresh)
		return err
	})
	if errors.Is(err, v1alpha1.ErrNotFound) {
		// The notifier vanished mid-pass; GC owns what remains.
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating status of %s/%s: %w", v1alpha1.KindNotifier, name, err)
	}
	return nil
}

// emit records an event about n via the recorder. Best-effort.
func (c *Controller) emit(ctx context.Context, n *v1alpha1.Notifier, typ v1alpha1.EventType, reason, message string) {
	c.recorder.Eventf(ctx, v1alpha1.ObjectRef{
		Kind: v1alpha1.KindNotifier,
		Name: n.Metadata.Name,
		UID:  n.Metadata.UID,
	}, typ, reason, "%s", strings.TrimSpace(message))
}

// EnqueueAllNotifiers returns an informer handler that enqueues every
// Notifier on any fired key: failure signals are levels on *other* kinds,
// so any Daemon/Timer/Proc change may flip a level somewhere. Single-host
// object counts make the fan-out cheap; the periodic resync is the safety
// net as everywhere.
func EnqueueAllNotifiers(notifiers *cache.Store, q queue.RateLimitingInterface) func(string) {
	return func(string) {
		for _, key := range notifiers.Keys() {
			q.Add(key)
		}
	}
}
