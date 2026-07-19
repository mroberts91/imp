// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package timer implements the TimerController, imp's cron replacement:
// each Timer fires run-to-completion Procs on a schedule (doc 08 §4).
//
// Logical fork of the CronJob controller's scheduling ideas in kubernetes
// pkg/controller/cronjob/ (Copyright The Kubernetes Authors, Apache-2.0;
// see LICENSES/kubernetes/): most-recent-due-tick computation with a
// missed-runs cap, deterministic run names derived from the scheduled
// time (so a retried pass re-applies idempotently), concurrency policies,
// and history-limit pruning. Deltas: Timers create Procs directly (no Job
// kind — doc 08 M5-a); ticking is the shared Runner's RequeueAfter on
// internal/clock, not a dedicated goroutine; schedules are evaluated in
// host-local time; a missed tick past its deadline is skipped with a
// MissedRun event (systemd Persistent=false posture — the built-in
// tolerance when startingDeadlineSeconds is unset is small, not infinite
// as in CronJob).
package timer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/controllers"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

const componentName = "timer-controller"

// ReportingComponent is stamped on Events emitted by this controller.
const ReportingComponent = "controllers/timer"

// defaultStartingDeadline is the missed-tick tolerance when
// spec.startingDeadlineSeconds is unset: enough for scheduler jitter, small
// enough that ticks missed while impd was down are skipped, not fired late.
const defaultStartingDeadline = 10 * time.Second

// maxMissedTicks bounds the catch-up walk after long downtime (CronJob's
// "too many missed start times" guard).
const maxMissedTicks = 1000

// replaceDelay spaces the delete-then-create passes of ConcurrencyReplace.
const replaceDelay = 1 * time.Second

// Controller reconciles Timers: fire due ticks, enforce the concurrency
// policy, prune run history, and roll observed state into Timer status
// (sole writer). It reads only the informer stores and writes only through
// the client; wiring owns informers, queue, and runner.
type Controller struct {
	client   *client.Client
	timers   *cache.Store
	procs    *cache.Store
	clock    clock.Clock
	recorder *recorder.Recorder
	log      *slog.Logger
}

var _ controllers.Reconciler = (*Controller)(nil)

// New builds a Controller over cl and the two informer stores. A nil clk
// means the real clock; a nil rec builds a recorder.
func New(cl *client.Client, timers, procs *cache.Store, clk clock.Clock, rec *recorder.Recorder) *Controller {
	if clk == nil {
		clk = clock.Real{}
	}
	if rec == nil {
		rec = recorder.New(cl, ReportingComponent, clk)
	}
	return &Controller{
		client:   cl,
		timers:   timers,
		procs:    procs,
		clock:    clk,
		recorder: rec,
		log:      slog.With("component", componentName),
	}
}

// Reconcile converges the Timer named by key ("Timer/<name>"): prune
// history, evaluate the schedule, fire or skip the due tick, and roll up
// status. Level-triggered: everything is recomputed from the cache and the
// clock; RequeueAfter is the tick mechanism.
func (c *Controller) Reconcile(ctx context.Context, key string) error {
	raw, ok := c.timers.GetByKey(key)
	if !ok {
		// Timer deleted: GC owns the cascade to its Procs.
		return nil
	}
	var t v1alpha1.Timer
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("decoding %s from cache: %w", key, err)
	}

	active, finished := c.observedRuns(t.Metadata.Name)

	if err := c.pruneHistory(ctx, &t, finished); err != nil {
		return err
	}

	sched, err := cron.ParseStandard(t.Spec.Schedule)
	if err != nil {
		// Validation prevents this; a stored unparseable schedule is
		// permanent — retrying cannot fix it. Surface and stop.
		c.emit(ctx, &t, v1alpha1.EventTypeWarning, v1alpha1.ReasonFailedValidation,
			fmt.Sprintf("unparseable schedule %q: %v", t.Spec.Schedule, err))
		return c.rollupStatus(ctx, &t, t.Status.LastScheduleTime, active, finished)
	}

	if t.Spec.Suspend != nil && *t.Spec.Suspend {
		return c.rollupStatus(ctx, &t, t.Status.LastScheduleTime, active, finished)
	}

	now := c.clock.Now()
	last := t.Status.LastScheduleTime.Time
	if t.Status.LastScheduleTime.IsZero() {
		last = t.Metadata.CreationTimestamp.Time
	}

	due, overflowed := mostRecentDue(sched, last, now)
	if overflowed {
		// Pathologically many missed ticks (long downtime on a tight
		// schedule): skip them wholesale and resume from now.
		c.emit(ctx, &t, v1alpha1.EventTypeWarning, v1alpha1.ReasonMissedRun,
			fmt.Sprintf("more than %d missed runs; resuming from now", maxMissedTicks))
		if err := c.rollupStatus(ctx, &t, v1alpha1.NewTime(now), active, finished); err != nil {
			return err
		}
		return controllers.RequeueAfter{After: sched.Next(now).Sub(now)}
	}
	if due.IsZero() {
		// Nothing due yet: wake at the next tick.
		if err := c.rollupStatus(ctx, &t, t.Status.LastScheduleTime, active, finished); err != nil {
			return err
		}
		return controllers.RequeueAfter{After: sched.Next(now).Sub(now)}
	}

	// A tick is due. Too late to fire it?
	deadline := defaultStartingDeadline
	if t.Spec.StartingDeadlineSeconds != nil {
		deadline = time.Duration(*t.Spec.StartingDeadlineSeconds) * time.Second
	}
	if now.Sub(due) > deadline {
		c.emit(ctx, &t, v1alpha1.EventTypeWarning, v1alpha1.ReasonMissedRun,
			fmt.Sprintf("missed scheduled run at %s (past deadline)", due.Format(time.RFC3339)))
		if err := c.rollupStatus(ctx, &t, v1alpha1.NewTime(due), active, finished); err != nil {
			return err
		}
		return controllers.RequeueAfter{After: sched.Next(now).Sub(now)}
	}

	if len(active) > 0 {
		switch t.Spec.ConcurrencyPolicy {
		case v1alpha1.ConcurrencyForbid:
			c.emit(ctx, &t, v1alpha1.EventTypeNormal, v1alpha1.ReasonSkippedRun,
				fmt.Sprintf("skipped run at %s: previous run %s still active",
					due.Format(time.RFC3339), active[0].Metadata.Name))
			if err := c.rollupStatus(ctx, &t, v1alpha1.NewTime(due), active, finished); err != nil {
				return err
			}
			return controllers.RequeueAfter{After: sched.Next(due).Sub(now)}
		case v1alpha1.ConcurrencyReplace:
			// Delete the active run(s); the create happens next pass,
			// Recreate-style (converge in passes). Known window: if this
			// pass already ran close to the deadline, the create pass may
			// land past it and skip the tick (MissedRun) after the delete.
			// That is the strict reading of the deadline — the next tick
			// heals — and the RequeueAfter wake makes it rare in practice.
			for i := range active {
				name := active[i].Metadata.Name
				if err := c.client.DeleteProc(ctx, name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
					return fmt.Errorf("replacing active run %s: %w", name, err)
				}
			}
			if err := c.rollupStatus(ctx, &t, t.Status.LastScheduleTime, active, finished); err != nil {
				return err
			}
			return controllers.RequeueAfter{After: replaceDelay}
		}
		// ConcurrencyAllow falls through and fires alongside.
	}

	p := newRunProc(&t, due)
	if _, err := c.client.ApplyProc(ctx, p); err != nil {
		return fmt.Errorf("creating run %s: %w", p.Metadata.Name, err)
	}
	c.emit(ctx, &t, v1alpha1.EventTypeNormal, v1alpha1.ReasonScheduledRun,
		fmt.Sprintf("created run %s for tick %s", p.Metadata.Name, due.Format(time.RFC3339)))
	if err := c.rollupStatus(ctx, &t, v1alpha1.NewTime(due), active, finished); err != nil {
		return err
	}
	return controllers.RequeueAfter{After: sched.Next(due).Sub(now)}
}

// mostRecentDue walks the schedule from last and returns the newest tick
// that is <= now (zero when none), skipping intermediate missed ticks.
// overflowed reports hitting the maxMissedTicks cap.
func mostRecentDue(sched cron.Schedule, last, now time.Time) (due time.Time, overflowed bool) {
	next := sched.Next(last)
	for i := 0; !next.After(now); i++ {
		if i >= maxMissedTicks {
			return time.Time{}, true
		}
		due = next
		next = sched.Next(next)
	}
	return due, false
}

// observedRuns scans the proc store for this Timer's runs, split into
// active (not yet terminal) and finished (Succeeded/Failed), each sorted
// oldest-first by scheduled time.
func (c *Controller) observedRuns(timerName string) (active, finished []v1alpha1.Proc) {
	for _, raw := range c.procs.List() {
		var p v1alpha1.Proc
		if err := json.Unmarshal(raw, &p); err != nil {
			c.log.Warn("skipping unreadable proc in cache", "kind", v1alpha1.KindProc, "error", err)
			continue
		}
		if p.Metadata.Labels[v1alpha1.LabelTimerName] != timerName {
			continue
		}
		switch p.Status.Phase {
		case v1alpha1.ProcPhaseSucceeded, v1alpha1.ProcPhaseFailed:
			finished = append(finished, p)
		default:
			active = append(active, p)
		}
	}
	byScheduledAt := func(a, b v1alpha1.Proc) int {
		return scheduledAt(&a).Compare(scheduledAt(&b))
	}
	slices.SortFunc(active, byScheduledAt)
	slices.SortFunc(finished, byScheduledAt)
	return active, finished
}

// scheduledAt reads the run's tick from its annotation, falling back to
// creation time for anomalous Procs.
func scheduledAt(p *v1alpha1.Proc) time.Time {
	if s := p.Metadata.Annotations[v1alpha1.AnnotationScheduledAt]; s != "" {
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts
		}
	}
	return p.Metadata.CreationTimestamp.Time
}

// pruneHistory deletes finished runs beyond the per-outcome history
// limits, oldest first. finished is sorted oldest-first.
func (c *Controller) pruneHistory(ctx context.Context, t *v1alpha1.Timer, finished []v1alpha1.Proc) error {
	limits := map[v1alpha1.ProcPhase]int{
		v1alpha1.ProcPhaseSucceeded: int(*t.Spec.SuccessfulHistoryLimit),
		v1alpha1.ProcPhaseFailed:    int(*t.Spec.FailedHistoryLimit),
	}
	counts := map[v1alpha1.ProcPhase]int{}
	for i := range finished {
		counts[finished[i].Status.Phase]++
	}
	var errs []error
	for i := range finished {
		phase := finished[i].Status.Phase
		if counts[phase] <= limits[phase] {
			continue
		}
		name := finished[i].Metadata.Name
		if err := c.client.DeleteProc(ctx, name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
			errs = append(errs, fmt.Errorf("pruning run %s: %w", name, err))
			continue
		}
		counts[phase]--
	}
	return errors.Join(errs...)
}

// newRunProc builds the Proc for one tick: template metadata first, system
// labels layered on top, the tick recorded in an annotation, an
// ownerReference back to the Timer, and a deep copy of the (already
// defaulted) template spec. The name is derived from the tick so a
// retried pass re-applies the same object idempotently.
func newRunProc(t *v1alpha1.Timer, due time.Time) *v1alpha1.Proc {
	labels := map[string]string{}
	maps.Copy(labels, t.Spec.Template.Metadata.Labels)
	labels[v1alpha1.LabelTimerName] = t.Metadata.Name

	annotations := map[string]string{}
	maps.Copy(annotations, t.Spec.Template.Metadata.Annotations)
	annotations[v1alpha1.AnnotationScheduledAt] = due.Format(time.RFC3339)

	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d", t.Metadata.Name, due.Unix()),
			Labels:      labels,
			Annotations: annotations,
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindTimer,
				Name:       t.Metadata.Name,
				UID:        t.Metadata.UID,
			}},
		},
		Spec: *t.Spec.Template.Spec.DeepCopy(),
	}
}

// rollupStatus writes the Timer's status: observedGeneration, schedule
// bookkeeping, the active run, and the Active condition. Sole-writer duty
// of this controller; guarded against same-name recreation by UID.
func (c *Controller) rollupStatus(ctx context.Context, t *v1alpha1.Timer, lastSchedule v1alpha1.Time, active, finished []v1alpha1.Proc) error {
	name := t.Metadata.Name
	gen := t.Metadata.Generation

	activeProc := ""
	if len(active) > 0 {
		// Newest active run (sorted oldest-first).
		activeProc = active[len(active)-1].Metadata.Name
	}
	var lastSuccessful v1alpha1.Time
	for i := range finished {
		if finished[i].Status.Phase == v1alpha1.ProcPhaseSucceeded {
			lastSuccessful = v1alpha1.NewTime(scheduledAt(&finished[i]))
		}
	}

	now := v1alpha1.NewTime(c.clock.Now())
	cond := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeActive,
		Status:             v1alpha1.ConditionFalse,
		Reason:             "Idle",
		Message:            "no run in flight",
		ObservedGeneration: gen,
		LastTransitionTime: now,
	}
	if activeProc != "" {
		cond.Status = v1alpha1.ConditionTrue
		cond.Reason = "RunActive"
		cond.Message = "run " + activeProc + " in flight"
	}

	err := client.RetryOnConflict(func() error {
		fresh, err := c.client.GetTimer(ctx, name)
		if err != nil {
			return err
		}
		if fresh.Metadata.UID != t.Metadata.UID {
			// Deleted and recreated under the same name mid-pass: this
			// observation belongs to the dead incarnation.
			return nil
		}
		fresh.Status.ObservedGeneration = gen
		// lastScheduleTime only moves forward.
		if lastSchedule.After(fresh.Status.LastScheduleTime.Time) {
			fresh.Status.LastScheduleTime = lastSchedule
		}
		if lastSuccessful.After(fresh.Status.LastSuccessfulTime.Time) {
			fresh.Status.LastSuccessfulTime = lastSuccessful
		}
		fresh.Status.ActiveProc = activeProc
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, cond)
		_, err = c.client.UpdateTimerStatus(ctx, fresh)
		return err
	})
	if errors.Is(err, v1alpha1.ErrNotFound) {
		// The timer vanished mid-pass; GC owns what remains.
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating status of %s/%s: %w", v1alpha1.KindTimer, name, err)
	}
	return nil
}

// emit records an event about t via the recorder. Best-effort.
func (c *Controller) emit(ctx context.Context, t *v1alpha1.Timer, typ v1alpha1.EventType, reason, message string) {
	c.recorder.Eventf(ctx, v1alpha1.ObjectRef{
		Kind: v1alpha1.KindTimer,
		Name: t.Metadata.Name,
		UID:  t.Metadata.UID,
	}, typ, reason, "%s", strings.TrimSpace(message))
}
