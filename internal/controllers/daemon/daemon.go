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
// pkg/controller/statefulset/stateful_set_control.go; the condition
// vocabulary and set-condition semantics are forked from
// pkg/controller/deployment/util/deployment_util.go (see api/v1alpha1
// conditions helpers). Deltas: owned Procs are matched by the
// impd.sh/daemon-name label rather than a selector; the only update
// strategy is Recreate (delete every stale-template Proc, requeue, create
// replacements on a later pass); no expectations machinery and no
// SlowStartBatch - the informer-backed cache plus level-triggered requeues
// carry convergence; events go through internal/recorder.
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

// recreateDelay spaces the passes of a Recreate rollout: pass one deletes
// the stale Procs and schedules pass two, which creates the replacements.
// Converge in passes; never block inside a reconcile.
const recreateDelay = 1 * time.Second

// Controller reconciles Daemons: it owns the Daemon -> Procs expansion and
// the Daemon status rollup. It reads only from the two informer stores and
// writes only through the client; wiring (informers, queue, runner) is the
// caller's job.
type Controller struct {
	client   *client.Client
	daemons  *cache.Store
	procs    *cache.Store
	clock    clock.Clock
	recorder *recorder.Recorder
	log      *slog.Logger
}

var _ controllers.Reconciler = (*Controller)(nil)

// New builds a Controller over cl and the two informer stores. A nil clk
// means the real clock. A nil rec builds a recorder with ReportingComponent.
func New(cl *client.Client, daemons, procs *cache.Store, clk clock.Clock, rec *recorder.Recorder) *Controller {
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
		clock:    clk,
		recorder: rec,
		log:      slog.With("component", componentName),
	}
}

// Reconcile converges the Daemon named by key ("Daemon/<name>") toward its
// spec: retire stale-template Procs (Recreate strategy), then create or
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

	hash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	current, stale := c.observedProcs(d.Metadata.Name, hash)

	// Recreate strategy: every stale-template Proc goes before any
	// replacement is created. Replacements arrive on the requeued pass.
	if len(stale) > 0 {
		var errs []error
		for i := range stale {
			name := stale[i].Metadata.Name
			if err := c.client.DeleteProc(ctx, name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
				errs = append(errs, fmt.Errorf("deleting stale proc %s: %w", name, err))
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		c.emit(ctx, &d, v1alpha1.ReasonTemplateChanged,
			fmt.Sprintf("template hash changed to %s, deleted %d stale proc(s)", hash, len(stale)))
		if err := c.rollupStatus(ctx, &d, current, stale); err != nil {
			return err
		}
		return controllers.RequeueAfter{After: recreateDelay}
	}

	// Daemons read from the server are always defaulted: Replicas is
	// non-nil.
	replicas := int(*d.Spec.Replicas)
	switch {
	case len(current) < replicas:
		if err := c.scaleUp(ctx, &d, current, hash, replicas); err != nil {
			return err
		}
	case len(current) > replicas:
		if err := c.scaleDown(ctx, &d, current, replicas); err != nil {
			return err
		}
	}

	return c.rollupStatus(ctx, &d, current, stale)
}

// observedProcs scans the proc store for Procs labeled with the daemon's
// name and partitions them into current-template and stale-template sets.
func (c *Controller) observedProcs(daemonName, hash string) (current, stale []v1alpha1.Proc) {
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
		if p.Metadata.Labels[v1alpha1.LabelTemplateHash] == hash {
			current = append(current, p)
		} else {
			stale = append(stale, p)
		}
	}
	return current, stale
}

// scaleUp creates a Proc for every free replica index in [0, replicas),
// lowest first, so identities stay monotonic and stable.
func (c *Controller) scaleUp(ctx context.Context, d *v1alpha1.Daemon, current []v1alpha1.Proc, hash string, replicas int) error {
	used := make(map[int]bool, len(current))
	for i := range current {
		if idx, ok := c.replicaIndex(&current[i]); ok {
			used[idx] = true
		}
	}

	var created []string
	for idx := range replicas {
		if used[idx] {
			continue
		}
		p := newProc(d, idx, hash)
		if _, err := c.client.ApplyProc(ctx, p); err != nil {
			// Includes ErrInvalid from a same-name-different-spec
			// collision: a real error, surfaced for backoff.
			return fmt.Errorf("creating proc %s: %w", p.Metadata.Name, err)
		}
		created = append(created, p.Metadata.Name)
	}

	if len(current) == 0 {
		// First creation: the Daemon is being expanded for the first
		// time (or after a Recreate rollout), not scaled.
		for _, name := range created {
			c.emit(ctx, d, v1alpha1.ReasonCreated, "created proc "+name)
		}
	} else if len(created) > 0 {
		c.emit(ctx, d, v1alpha1.ReasonScalingReplicas,
			fmt.Sprintf("scaled up: created %d proc(s) toward %d replicas", len(created), replicas))
	}
	return nil
}

// scaleDown deletes surplus Procs, highest ordinals first. A Proc with a
// malformed replica-index label sorts above every well-formed ordinal, so
// anomalous Procs are retired first.
func (c *Controller) scaleDown(ctx context.Context, d *v1alpha1.Daemon, current []v1alpha1.Proc, replicas int) error {
	type ordinal struct {
		name  string
		index int
	}
	ordinals := make([]ordinal, 0, len(current))
	for i := range current {
		idx, ok := c.replicaIndex(&current[i])
		if !ok {
			idx = math.MaxInt
		}
		ordinals = append(ordinals, ordinal{name: current[i].Metadata.Name, index: idx})
	}
	slices.SortFunc(ordinals, func(a, b ordinal) int { return cmp.Compare(b.index, a.index) })

	var errs []error
	deleted := 0
	for _, o := range ordinals[:len(current)-replicas] {
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
// Daemon, and a deep copy of the (already defaulted) template spec.
func newProc(d *v1alpha1.Daemon, idx int, hash string) *v1alpha1.Proc {
	labels := maps.Clone(d.Spec.Template.Metadata.Labels)
	if labels == nil {
		labels = make(map[string]string, 3)
	}
	labels[v1alpha1.LabelDaemonName] = d.Metadata.Name
	labels[v1alpha1.LabelTemplateHash] = hash
	labels[v1alpha1.LabelReplicaIndex] = strconv.Itoa(idx)

	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d-%s", d.Metadata.Name, idx, hash),
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
)

// rollupStatus writes the Daemon's status from the Proc set observed at
// the start of the pass. The write is unconditional: etcl's no-op
// short-circuit absorbs identical bodies with zero resourceVersion churn.
func (c *Controller) rollupStatus(ctx context.Context, d *v1alpha1.Daemon, current, stale []v1alpha1.Proc) error {
	name := d.Metadata.Name
	gen := d.Metadata.Generation
	replicas := *d.Spec.Replicas

	total := int32(len(current) + len(stale))
	updated := int32(len(current))
	var ready int32
	for _, set := range [][]v1alpha1.Proc{current, stale} {
		for i := range set {
			if procReady(&set[i]) {
				ready++
			}
		}
	}

	now := v1alpha1.NewTime(c.clock.Now())
	avail := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeAvailable,
		Status:             v1alpha1.ConditionFalse,
		Reason:             reasonMinReplicasUnavailable,
		Message:            "waiting for procs to become ready",
		ObservedGeneration: gen,
		LastTransitionTime: now,
	}
	if ready >= replicas {
		avail.Status = v1alpha1.ConditionTrue
		avail.Reason = reasonMinReplicasAvailable
		avail.Message = "minimum number of replicas is available"
	}

	// Progressing=False needs a progress deadline - deferred, out of M1
	// scope. True/ProcsAvailable is the terminal complete state (mirrors
	// the deployment controller's NewReplicaSetAvailable).
	prog := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeProgressing,
		Status:             v1alpha1.ConditionTrue,
		Reason:             reasonProcsUpdated,
		Message:            "rollout of the current template is in progress",
		ObservedGeneration: gen,
		LastTransitionTime: now,
	}
	if len(stale) == 0 && updated == replicas && ready == replicas {
		prog.Reason = reasonProcsAvailable
		prog.Message = "all procs are updated and available"
	}

	err := client.RetryOnConflict(func() error {
		fresh, err := c.client.GetDaemon(ctx, name)
		if err != nil {
			return err
		}
		fresh.Status.ObservedGeneration = gen
		fresh.Status.Replicas = total
		fresh.Status.UpdatedReplicas = updated
		fresh.Status.ReadyReplicas = ready
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, avail)
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, prog)
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
	return nil
}

// procReady reports whether p should count toward readyReplicas: Ready
// condition True when present, else Phase==Running (M1-era fallback).
func procReady(p *v1alpha1.Proc) bool {
	if c := v1alpha1.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionTypeReady); c != nil {
		return c.Status == v1alpha1.ConditionTrue
	}
	return p.Status.Phase == v1alpha1.ProcPhaseRunning
}
