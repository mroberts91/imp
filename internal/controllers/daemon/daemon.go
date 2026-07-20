// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package daemon implements the DaemonController, imp's core M1 loop: it
// expands each Daemon into its owned Procs and rolls the observed Proc set
// back up into the Daemon's status (the DaemonController is the sole
// writer of Daemon status).
//
// Logical forks (Copyright The Kubernetes Authors, Apache-2.0; see
// LICENSES/kubernetes/): the reconcile skeleton - read from cache,
// converge children, write status - is transposed from syncHandler in
// staging/src/k8s.io/sample-controller/controller.go; ordinal management
// (fill the lowest free replica indices on scale-up, retire the highest
// ordinals first on scale-down) is the monotonic-identity instinct of
// pkg/controller/statefulset/stateful_set_control.go; RollingUpdate
// (one ordinal at a time, high→low, wait for Ready, optional partition)
// forks the rolling loop in that same file (~709–735); the condition
// vocabulary and set-condition semantics are forked from
// pkg/controller/deployment/util/deployment_util.go (see api/v1alpha1
// conditions helpers). Deltas: owned Procs are matched by the
// impd.sh/daemon-name label rather than a selector; Recreate deletes every
// stale-template Proc then requeues; RollingUpdate never surges (delete-
// then-create per ordinal — even though hash-suffixed names could
// coexist); no expectations machinery and no SlowStartBatch - the
// informer-backed cache plus level-triggered requeues carry convergence;
// events go through internal/recorder.
package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/controllers"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

const componentName = "daemon-controller"

// ReportingComponent is stamped on Events emitted by this controller.
const ReportingComponent = "controllers/daemon"

// recreateDelay spaces the passes of a Recreate or RollingUpdate step:
// pass one deletes stale Proc(s) and schedules the next pass, which
// creates replacements. Converge in passes; never block inside a reconcile.
const recreateDelay = 1 * time.Second

// Controller reconciles Daemons: it owns the Daemon -> Procs expansion and
// the Daemon status rollup. It reads only from the two informer stores and
// writes only through the client; wiring (informers, queue, runner) is the
// caller's job.
type Controller struct {
	client   *client.Client
	daemons  *cache.Store
	procs    *cache.Store
	configs  *cache.Store
	clock    clock.Clock
	recorder *recorder.Recorder
	log      *slog.Logger
}

var _ controllers.Reconciler = (*Controller)(nil)

// New builds a Controller over cl and the informer stores (daemons, procs, and
// — for M8 config resolution — configs). A nil clk means the real clock. A nil
// rec builds a recorder with ReportingComponent.
func New(cl *client.Client, daemons, procs, configs *cache.Store, clk clock.Clock, rec *recorder.Recorder) *Controller {
	if clk == nil {
		clk = clock.Real{}
	}
	if rec == nil {
		rec = recorder.New(cl, ReportingComponent, clk)
	}
	return &Controller{
		client:   cl,
		daemons:  daemons,
		procs:    procs,
		configs:  configs,
		clock:    clk,
		recorder: rec,
		log:      slog.With("component", componentName),
	}
}

// Reconcile converges the Daemon named by key ("Daemon/<name>") toward its
// spec: retire stale-template Procs per updateStrategy, then create or
// delete Procs to match spec.replicas, then roll observed state up into
// status. Everything is computed from the cache as observed at the start
// of the pass; convergence happens across passes as Proc events re-enqueue
// the Daemon.
func (c *Controller) Reconcile(ctx context.Context, key string) error {
	raw, ok := c.daemons.GetByKey(key)
	if !ok {
		// Daemon deleted: GC owns the cascade to its Procs.
		return nil
	}
	var d v1alpha1.Daemon
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("decoding %s from cache: %w", key, err)
	}

	// Resolve the Proc revision (template hash × referenced Config content,
	// M8). A missing Config holds the whole pass (M8-h) and self-heals when
	// the Config appears; a Daemon without config refs behaves exactly as it
	// did pre-M8 (rev.config == "").
	rev, missing := c.resolveRevision(&d)
	if missing != "" {
		c.emitWarning(ctx, &d, v1alpha1.ReasonConfigMissing,
			fmt.Sprintf("config %q not found; holding rollout until it exists", missing))
		return c.holdForMissingConfig(ctx, &d, missing)
	}

	current, stale := c.observedProcs(d.Metadata.Name, rev)
	partition := rollingPartition(&d)
	// Daemons read from the server are always defaulted: Replicas is
	// non-nil.
	replicas := int(*d.Spec.Replicas)

	switch d.Spec.UpdateStrategy.Type {
	case v1alpha1.UpdateStrategyRollingUpdate:
		// Fill holes before deleting more stale ordinals (StatefulSet
		// order: create missing at the update revision, then roll).
		created, err := c.scaleUp(ctx, &d, current, stale, rev, replicas)
		if err != nil {
			return err
		}
		if created > 0 {
			// One action per pass. The informer has not observed the new
			// Procs yet, so walking the roll now would see their ordinals
			// as empty slots and delete a second stale ordinal — two
			// replicas down at once. The created Procs' own watch events
			// re-enqueue this Daemon; the next pass sees them as current
			// and waits for Ready.
			return c.rollupStatus(ctx, &d, current, stale, partition)
		}
		done, err := c.rollingUpdate(ctx, &d, current, stale, rev, partition)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
		if len(current)+len(stale) > replicas {
			if err := c.scaleDown(ctx, &d, current, stale, replicas); err != nil {
				return err
			}
		}
		return c.rollupStatus(ctx, &d, current, stale, partition)
	default:
		// Recreate (default): every stale-template Proc goes before any
		// replacement is created. Replacements arrive on the requeued pass.
		if len(stale) > 0 {
			var errs []error
			deleted := 0
			for i := range stale {
				name := stale[i].Metadata.Name
				switch err := c.client.DeleteProc(ctx, name); {
				case err == nil:
					deleted++
				case !errors.Is(err, v1alpha1.ErrNotFound):
					// ErrNotFound is cache lag from a prior pass: the Proc
					// is already gone and must not be reported as deleted.
					errs = append(errs, fmt.Errorf("deleting stale proc %s: %w", name, err))
				}
			}
			if err := errors.Join(errs...); err != nil {
				return err
			}
			if deleted > 0 {
				c.emit(ctx, &d, rollReason(stale, rev),
					fmt.Sprintf("rolled to revision %s: deleted %d stale proc(s)", rev.suffix, deleted))
			}
			if err := c.rollupStatus(ctx, &d, current, stale, partition); err != nil {
				return err
			}
			return controllers.RequeueAfter{After: recreateDelay}
		}
	}

	switch {
	case len(current) < replicas:
		if _, err := c.scaleUp(ctx, &d, current, stale, rev, replicas); err != nil {
			return err
		}
	case len(current)+len(stale) > replicas:
		if err := c.scaleDown(ctx, &d, current, stale, replicas); err != nil {
			return err
		}
	}

	return c.rollupStatus(ctx, &d, current, stale, partition)
}

// rollingPartition returns the RollingUpdate partition (0 when unset /
// Recreate).
func rollingPartition(d *v1alpha1.Daemon) int {
	if d.Spec.UpdateStrategy.Type != v1alpha1.UpdateStrategyRollingUpdate {
		return 0
	}
	if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		return int(*ru.Partition)
	}
	return 0
}

// rollingUpdate performs one step of a StatefulSet-style roll: walk
// ordinals high→low from replicas-1 down to partition, deleting stale Procs
// within a maxUnavailable budget (M9-e). The budget starts at maxUnavailable
// (nil = 1, today's one-at-a-time behavior) and is spent by every ordinal in
// range that is already unavailable — a Proc still maturing (not available for
// minReadySeconds — M6), an empty slot whose replacement the informer hasn't
// observed, or a stale Proc this pass deletes. Stopping when the budget hits 0
// keeps at most maxUnavailable ordinals down at once. done is true when no
// rolling work remains for this pass (caller may scale); when false the status
// has already been rolled up.
func (c *Controller) rollingUpdate(ctx context.Context, d *v1alpha1.Daemon, current, stale []v1alpha1.Proc, rev revision, partition int) (done bool, err error) {
	replicas := int(*d.Spec.Replicas)
	budget := 1
	if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.MaxUnavailable != nil {
		budget = int(*ru.MaxUnavailable)
	}
	byIndex := indexProcs(current, stale)
	now := c.clock.Now()
	reason := rollReason(stale, rev)

	deleted := 0
	sawDown := false // an already-unavailable slot we can only wait on
	for idx := replicas - 1; idx >= partition && budget > 0; idx-- {
		slot := byIndex[idx]
		available := false
		if slot.current != nil {
			available, _ = procAvailable(slot.current, d.Spec.MinReadySeconds, now)
		}
		switch {
		case slot.current != nil && !available:
			// A current Proc still maturing or failed: already down, so it
			// consumes a budget unit but there is nothing to delete.
			sawDown = true
			budget--
		case slot.stale != nil:
			// Delete this stale Proc to advance the roll (its replacement is
			// created next pass). Consumes a budget unit.
			name := slot.stale.Metadata.Name
			switch err := c.client.DeleteProc(ctx, name); {
			case errors.Is(err, v1alpha1.ErrNotFound):
				// Already gone (cache lag from a prior pass): nothing deleted,
				// no event, but the slot is now down — spend the budget unit.
			case err != nil:
				return false, fmt.Errorf("deleting stale proc %s: %w", name, err)
			default:
				deleted++
				c.emit(ctx, d, reason,
					fmt.Sprintf("rolling update to revision %s: deleted stale proc %s (ordinal %d)", rev.suffix, name, idx))
			}
			budget--
		case slot.current != nil:
			// Current and available: up, costs nothing — move to the next.
		default:
			// Empty slot: a replacement created by an earlier pass the informer
			// has not observed yet, or a malformed replica-index label. Its
			// ordinal's state is unknown; treat it as down and wait.
			sawDown = true
			budget--
		}
	}

	if deleted > 0 {
		// Deleted one or more stale Procs this pass: roll up, then requeue to
		// create the replacements next pass (same shape as one-at-a-time).
		if err := c.rollupStatus(ctx, d, current, stale, partition); err != nil {
			return false, err
		}
		return false, controllers.RequeueAfter{After: recreateDelay}
	}
	if sawDown {
		// No stale was deletable (budget spent on already-down slots): wait.
		// rollupStatus's own time-driven requeue (maturation/deadline), if any,
		// propagates as the returned error.
		if err := c.rollupStatus(ctx, d, current, stale, partition); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

type procSlot struct {
	current *v1alpha1.Proc
	stale   *v1alpha1.Proc
}

func indexProcs(current, stale []v1alpha1.Proc) map[int]procSlot {
	out := make(map[int]procSlot)
	for i := range current {
		idx, err := strconv.Atoi(current[i].Metadata.Labels[v1alpha1.LabelReplicaIndex])
		if err != nil {
			continue
		}
		s := out[idx]
		s.current = &current[i]
		out[idx] = s
	}
	for i := range stale {
		idx, err := strconv.Atoi(stale[i].Metadata.Labels[v1alpha1.LabelReplicaIndex])
		if err != nil {
			continue
		}
		s := out[idx]
		s.stale = &stale[i]
		out[idx] = s
	}
	return out
}

// observedProcs scans the proc store for Procs labeled with the daemon's
// name and partitions them into current-revision and stale sets. A Proc is
// current iff BOTH its template-hash and config-hash labels match the desired
// revision (M8-g); a missing config-hash label reads "", so no-config daemons
// and pre-M8 Procs compare equal to a "" desired config hash — the upgrade
// guarantee.
func (c *Controller) observedProcs(daemonName string, rev revision) (current, stale []v1alpha1.Proc) {
	for _, raw := range c.procs.List() {
		var p v1alpha1.Proc
		if err := json.Unmarshal(raw, &p); err != nil {
			// The apiserver validated what it stored; this guards
			// against wire corruption, not user input.
			c.log.Warn("skipping unreadable proc in cache", "kind", v1alpha1.KindProc, "error", err)
			continue
		}
		if p.Metadata.Labels[v1alpha1.LabelDaemonName] != daemonName {
			continue
		}
		if p.Metadata.Labels[v1alpha1.LabelTemplateHash] == rev.template &&
			p.Metadata.Labels[v1alpha1.LabelConfigHash] == rev.config {
			current = append(current, p)
		} else {
			stale = append(stale, p)
		}
	}
	return current, stale
}

// scaleUp creates a Proc for every free replica index in [0, replicas),
// lowest first, so identities stay monotonic and stable. Indices that
// still hold a stale Proc are skipped (RollingUpdate delete-then-create).
// Returns how many Procs were created, so the RollingUpdate path can stop
// after a creation pass instead of trusting the (still-lagging) cache.
func (c *Controller) scaleUp(ctx context.Context, d *v1alpha1.Daemon, current, stale []v1alpha1.Proc, rev revision, replicas int) (int, error) {
	used := make(map[int]bool, len(current)+len(stale))
	for i := range current {
		if idx, ok := c.replicaIndex(&current[i]); ok {
			used[idx] = true
		}
	}
	for i := range stale {
		if idx, ok := c.replicaIndex(&stale[i]); ok {
			used[idx] = true
		}
	}

	var created []string
	for idx := range replicas {
		if used[idx] {
			continue
		}
		p := newProc(d, idx, rev)
		if _, err := c.client.ApplyProc(ctx, p); err != nil {
			// Includes ErrInvalid from a same-name-different-spec
			// collision: a real error, surfaced for backoff.
			return len(created), fmt.Errorf("creating proc %s: %w", p.Metadata.Name, err)
		}
		created = append(created, p.Metadata.Name)
	}

	switch {
	case len(current) == 0 && len(stale) == 0:
		// First creation: the Daemon is being expanded for the first
		// time (or after a Recreate rollout), not scaled.
		for _, name := range created {
			c.emit(ctx, d, v1alpha1.ReasonCreated, "created proc "+name)
		}
	case len(stale) > 0 && len(created) > 0:
		// Stale Procs present means a roll is in flight: these creations
		// are rolling replacements, not a scale change.
		reason := rollReason(stale, rev)
		for _, name := range created {
			c.emit(ctx, d, reason,
				fmt.Sprintf("rolling update to revision %s: created proc %s", rev.suffix, name))
		}
	case len(created) > 0:
		c.emit(ctx, d, v1alpha1.ReasonScalingReplicas,
			fmt.Sprintf("scaled up: created %d proc(s) toward %d replicas", len(created), replicas))
	}
	return len(created), nil
}

// scaleDown deletes surplus Procs, highest ordinals first. A Proc with a
// malformed replica-index label sorts above every well-formed ordinal, so
// anomalous Procs are retired first. Considers current and stale together
// so a RollingUpdate mid-roll does not leave surplus stale ordinals.
func (c *Controller) scaleDown(ctx context.Context, d *v1alpha1.Daemon, current, stale []v1alpha1.Proc, replicas int) error {
	type ordinal struct {
		name  string
		index int
	}
	all := make([]v1alpha1.Proc, 0, len(current)+len(stale))
	all = append(all, current...)
	all = append(all, stale...)
	ordinals := make([]ordinal, 0, len(all))
	for i := range all {
		idx, ok := c.replicaIndex(&all[i])
		if !ok {
			idx = math.MaxInt
		}
		ordinals = append(ordinals, ordinal{name: all[i].Metadata.Name, index: idx})
	}
	slices.SortFunc(ordinals, func(a, b ordinal) int { return cmp.Compare(b.index, a.index) })

	surplus := len(all) - replicas
	if surplus <= 0 {
		return nil
	}
	var errs []error
	deleted := 0
	for _, o := range ordinals[:surplus] {
		if err := c.client.DeleteProc(ctx, o.name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
			errs = append(errs, fmt.Errorf("deleting proc %s: %w", o.name, err))
			continue
		}
		deleted++
	}
	if deleted > 0 {
		c.emit(ctx, d, v1alpha1.ReasonScalingReplicas,
			fmt.Sprintf("scaled down: deleted %d proc(s) toward %d replicas", deleted, replicas))
	}
	return errors.Join(errs...)
}

// replicaIndex reads the impd.sh/replica-index label; false means the
// label is missing or malformed and the Proc occupies no index.
func (c *Controller) replicaIndex(p *v1alpha1.Proc) (int, bool) {
	idx, err := strconv.Atoi(p.Metadata.Labels[v1alpha1.LabelReplicaIndex])
	if err != nil {
		c.log.Warn("proc has a malformed replica-index label",
			"kind", v1alpha1.KindProc,
			"key", v1alpha1.KindProc+"/"+p.Metadata.Name,
			"error", err)
		return 0, false
	}
	return idx, true
}

// newProc builds the Proc for one replica index of d: template metadata
// first, system labels layered on top, an ownerReference back to the
// Daemon, and a deep copy of the (already defaulted) template spec. The name
// suffix and config-hash label carry the revision (M8-g); the config-hash
// label is set only when the template references Configs, so no-config
// Daemons produce byte-identical Procs to pre-M8.
func newProc(d *v1alpha1.Daemon, idx int, rev revision) *v1alpha1.Proc {
	labels := maps.Clone(d.Spec.Template.Metadata.Labels)
	if labels == nil {
		labels = make(map[string]string, 3)
	}
	labels[v1alpha1.LabelDaemonName] = d.Metadata.Name
	labels[v1alpha1.LabelTemplateHash] = rev.template
	if rev.config != "" {
		labels[v1alpha1.LabelConfigHash] = rev.config
	}
	labels[v1alpha1.LabelReplicaIndex] = strconv.Itoa(idx)

	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d-%s", d.Metadata.Name, idx, rev.suffix),
			Labels:      labels,
			Annotations: maps.Clone(d.Spec.Template.Metadata.Annotations),
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindDaemon,
				Name:       d.Metadata.Name,
				UID:        d.Metadata.UID,
			}},
		},
		Spec: *d.Spec.Template.Spec.DeepCopy(),
	}
}

// Condition reasons, forked from the deployment controller's vocabulary in
// pkg/controller/deployment/util/deployment_util.go.
const (
	reasonMinReplicasAvailable   = "MinimumReplicasAvailable"
	reasonMinReplicasUnavailable = "MinimumReplicasUnavailable"
	reasonProcsAvailable         = "ProcsAvailable"
	reasonProcsUpdated           = "ProcsUpdated"
	// reasonProgressDeadlineExceeded is Progressing=False past the deadline
	// (M6); shares its name with the Event reason.
	reasonProgressDeadlineExceeded = v1alpha1.ReasonProgressDeadlineExceeded
)

// rollupStatus writes the Daemon's status from the Proc set observed at
// the start of the pass. The write is unconditional: etcl's no-op
// short-circuit absorbs identical bodies with zero resourceVersion churn.
// partition gates Progressing completion under RollingUpdate (stale below
// partition is intentional).
//
// M6 additions: availableReplicas (Ready for minReadySeconds) drives the
// Available condition; the progress deadline flips Progressing to
// False/ProgressDeadlineExceeded when its lastUpdateTime — bumped by
// SetStatusCondition on every recorded change, frozen on stalls — falls
// progressDeadlineSeconds behind. Time-driven flips (a proc maturing to
// available, the deadline firing) get a RequeueAfter, since no watch event
// announces the passage of time.
// availableCondition builds the Available condition from the observed
// available/replica counts. Shared by rollupStatus and the missing-config hold
// so availability is reported identically on both paths.
func availableCondition(available, replicas int32, gen int64, now v1alpha1.Time) v1alpha1.Condition {
	c := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeAvailable,
		Status:             v1alpha1.ConditionFalse,
		Reason:             reasonMinReplicasUnavailable,
		Message:            fmt.Sprintf("waiting for procs to become available (%d/%d)", available, replicas),
		ObservedGeneration: gen,
		LastTransitionTime: now,
		LastUpdateTime:     now,
	}
	if available >= replicas {
		c.Status = v1alpha1.ConditionTrue
		c.Reason = reasonMinReplicasAvailable
		c.Message = "minimum number of replicas is available"
	}
	return c
}

func (c *Controller) rollupStatus(ctx context.Context, d *v1alpha1.Daemon, current, stale []v1alpha1.Proc, partition int) error {
	name := d.Metadata.Name
	gen := d.Metadata.Generation
	replicas := *d.Spec.Replicas

	total := int32(len(current) + len(stale))
	updated := int32(len(current))
	nowT := c.clock.Now()
	var ready, available int32
	var recheckIn time.Duration // soonest ready→available maturation; 0 = none pending
	for _, set := range [][]v1alpha1.Proc{current, stale} {
		for i := range set {
			if !procReady(&set[i]) {
				continue
			}
			ready++
			ok, in := procAvailable(&set[i], d.Spec.MinReadySeconds, nowT)
			if ok {
				available++
			} else if recheckIn == 0 || in < recheckIn {
				recheckIn = in
			}
		}
	}

	now := v1alpha1.NewTime(nowT)
	avail := availableCondition(available, replicas, gen, now)

	complete := rolloutComplete(current, stale, int(replicas), partition, d.Spec.MinReadySeconds, nowT)
	// The counts in the message are load-bearing: any real progress changes
	// it, which bumps the condition's lastUpdateTime; a stalled roll leaves
	// it identical, freezing the anchor the deadline measures against.
	prog := v1alpha1.Condition{
		Type:   v1alpha1.ConditionTypeProgressing,
		Status: v1alpha1.ConditionTrue,
		Reason: reasonProcsUpdated,
		Message: fmt.Sprintf("rollout in progress: %d/%d updated, %d available",
			updated, replicas, available),
		ObservedGeneration: gen,
		LastTransitionTime: now,
		LastUpdateTime:     now,
	}
	if complete {
		// True/ProcsAvailable is the terminal complete state (mirrors the
		// deployment controller's NewReplicaSetAvailable).
		prog.Reason = reasonProcsAvailable
		prog.Message = "all procs are updated and available"
		if len(stale) > 0 {
			// Complete with stale Procs only happens under a partition:
			// ordinals below it are intentionally left on the old hash.
			// Do not claim "all procs are updated" when they are not.
			prog.Message = fmt.Sprintf("ordinals >= %d are updated and available (%d proc(s) below the partition remain on the old hash)",
				partition, len(stale))
		}
	}

	exceededNow := false         // the deadline flipped Progressing this pass
	var deadlineIn time.Duration // time until the deadline would fire; 0 = not armed
	err := client.RetryOnConflict(func() error {
		exceededNow, deadlineIn = false, 0
		fresh, err := c.client.GetDaemon(ctx, name)
		if err != nil {
			return err
		}
		if fresh.Metadata.UID != d.Metadata.UID {
			// The Daemon was deleted and recreated under the same name
			// mid-pass. This pass's observation belongs to the dead
			// incarnation; writing it would stamp the new object with
			// another object's counts and an observedGeneration its own
			// generation may never have reached. The new incarnation's
			// watch events drive fresh reconciles.
			return nil
		}
		fresh.Status.ObservedGeneration = gen
		fresh.Status.Replicas = total
		fresh.Status.UpdatedReplicas = updated
		fresh.Status.ReadyReplicas = ready
		fresh.Status.AvailableReplicas = available
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, avail)

		// An exceeded state is sticky only within its generation: a spec
		// change is a new rollout and gets a fresh deadline (the Deployment
		// posture — SetStatusCondition re-anchors lastUpdateTime when the
		// True/ProcsUpdated condition below records the change).
		stored := v1alpha1.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionTypeProgressing)
		alreadyExceeded := !complete && stored != nil &&
			stored.Status == v1alpha1.ConditionFalse && stored.Reason == reasonProgressDeadlineExceeded &&
			stored.ObservedGeneration == gen
		if !alreadyExceeded {
			// Record this pass's progress (or completion) first, so real
			// progress bumps lastUpdateTime before the deadline is judged.
			v1alpha1.SetStatusCondition(&fresh.Status.Conditions, prog)
			if !complete {
				cur := v1alpha1.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionTypeProgressing)
				if pds := d.Spec.ProgressDeadlineSeconds; pds != nil && cur != nil && !cur.LastUpdateTime.IsZero() {
					deadline := cur.LastUpdateTime.Add(time.Duration(*pds) * time.Second)
					if nowT.Before(deadline) {
						deadlineIn = deadline.Sub(nowT)
					} else {
						v1alpha1.SetStatusCondition(&fresh.Status.Conditions, v1alpha1.Condition{
							Type:   v1alpha1.ConditionTypeProgressing,
							Status: v1alpha1.ConditionFalse,
							Reason: reasonProgressDeadlineExceeded,
							Message: fmt.Sprintf("rollout has made no progress for %ds: %d/%d updated, %d available",
								*pds, updated, replicas, available),
							ObservedGeneration: gen,
							LastTransitionTime: now,
							LastUpdateTime:     now,
						})
						exceededNow = true
					}
				}
			}
		}
		_, err = c.client.UpdateDaemonStatus(ctx, fresh)
		return err
	})
	if errors.Is(err, v1alpha1.ErrNotFound) {
		// The daemon vanished mid-pass; GC owns what remains.
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating status of %s/%s: %w", v1alpha1.KindDaemon, name, err)
	}
	if exceededNow {
		// The deadline is a report, not a brake: reconciliation continues,
		// and the complete path above recovers the condition if the world
		// converges later.
		c.emitWarning(ctx, d, v1alpha1.ReasonProgressDeadlineExceeded,
			fmt.Sprintf("rollout has made no progress for %ds", *d.Spec.ProgressDeadlineSeconds))
	}
	// Wake up for whichever time-driven flip comes first.
	switch {
	case recheckIn > 0 && (deadlineIn == 0 || recheckIn < deadlineIn):
		return controllers.RequeueAfter{After: recheckIn}
	case deadlineIn > 0:
		return controllers.RequeueAfter{After: deadlineIn}
	}
	return nil
}

// rolloutComplete reports whether every ordinal in [partition, replicas)
// has a current-hash available Proc, and there is no stale Proc at those
// ordinals. Stale Procs below partition are intentional under RollingUpdate.
func rolloutComplete(current, stale []v1alpha1.Proc, replicas, partition int, minReady int32, now time.Time) bool {
	if replicas == 0 {
		return len(current) == 0 && len(stale) == 0
	}
	byIndex := indexProcs(current, stale)
	for idx := partition; idx < replicas; idx++ {
		slot := byIndex[idx]
		if slot.stale != nil || slot.current == nil {
			return false
		}
		if ok, _ := procAvailable(slot.current, minReady, now); !ok {
			return false
		}
	}
	// No surplus stale at ordinals >= partition (scale should have cleared
	// ordinals >= replicas; anything still stale in the roll range fails).
	for i := range stale {
		idx, err := strconv.Atoi(stale[i].Metadata.Labels[v1alpha1.LabelReplicaIndex])
		if err != nil {
			return false
		}
		if idx >= partition {
			return false
		}
	}
	return true
}

// procReady reports whether p should count toward readyReplicas: Ready
// condition True when present, else Phase==Running (M1-era fallback).
func procReady(p *v1alpha1.Proc) bool {
	if c := v1alpha1.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionTypeReady); c != nil {
		return c.Status == v1alpha1.ConditionTrue
	}
	return p.Status.Phase == v1alpha1.ProcPhaseRunning
}

// procAvailable reports whether p counts toward availableReplicas: Ready
// for at least minReadySeconds, measured against the Ready condition's
// lastTransitionTime (the Deployment rule). When Ready but not yet mature,
// recheckIn is how long until it would become available. Procs counted via
// the M1-era phase fallback (no Ready condition) have no transition time to
// measure and count as available immediately.
func procAvailable(p *v1alpha1.Proc, minReady int32, now time.Time) (available bool, recheckIn time.Duration) {
	c := v1alpha1.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionTypeReady)
	if c == nil {
		return p.Status.Phase == v1alpha1.ProcPhaseRunning, 0
	}
	if c.Status != v1alpha1.ConditionTrue {
		return false, 0
	}
	if minReady <= 0 || c.LastTransitionTime.IsZero() {
		return true, 0
	}
	availableAt := c.LastTransitionTime.Add(time.Duration(minReady) * time.Second)
	if now.Before(availableAt) {
		return false, availableAt.Sub(now)
	}
	return true, 0
}
