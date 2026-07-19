// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package gc deletes Procs whose owning Daemon or Timer no longer exists,
// completing the ownership cascade: deleting an owner orphans its Procs,
// GC removes them, and execd observes the Proc deletions and stops the
// processes.
//
// This is the trivial special case of kubernetes
// pkg/controller/garbagecollector (Copyright The Kubernetes Authors,
// Apache-2.0; see LICENSES/kubernetes/). Deltas: no dependency graph, no
// deletion policies (foreground/orphan), and the only edges are Procs
// owned by Daemons or Timers, so reconciling a Proc key is a direct
// per-kind owner lookup; the "verify with a live read before deleting"
// rule is kept from the upstream absentOwnerCache discipline.
package gc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/queue"
	"github.com/mroberts91/imp/pkg/client"
)

// Controller implements controllers.Reconciler for Proc keys: a Proc whose
// owners (Daemons and Timers) are all confirmed gone is deleted. Unowned
// Procs are never touched.
type Controller struct {
	client *client.Client
	// owners maps each ownable kind to its informer cache; liveGone
	// confirms absence against the server per kind.
	owners map[string]*cache.Store
	procs  *cache.Store
	log    *slog.Logger
}

// New builds the GC controller around the API client and the informer
// stores (one per ownable kind, plus procs). It creates no informers or
// queues; wiring owns those.
func New(c *client.Client, daemons, timers, procs *cache.Store) *Controller {
	return &Controller{
		client: c,
		owners: map[string]*cache.Store{
			v1alpha1.KindDaemon: daemons,
			v1alpha1.KindTimer:  timers,
		},
		procs: procs,
		log:   slog.With("component", "gc", "kind", v1alpha1.KindProc),
	}
}

// liveGone confirms against the server that the referenced owner does not
// exist. A nil error means confirmed gone.
func (c *Controller) liveGone(ctx context.Context, ref v1alpha1.OwnerReference) (bool, error) {
	var err error
	switch ref.Kind {
	case v1alpha1.KindDaemon:
		_, err = c.client.GetDaemon(ctx, ref.Name)
	case v1alpha1.KindTimer:
		_, err = c.client.GetTimer(ctx, ref.Name)
	default:
		return false, fmt.Errorf("gc: no live check for owner kind %q", ref.Kind)
	}
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, v1alpha1.ErrNotFound):
		return true, nil
	default:
		return false, fmt.Errorf("gc: confirming %s %q is gone: %w", ref.Kind, ref.Name, err)
	}
}

// procMeta is the slice of a stored Proc GC reads.
type procMeta struct {
	Metadata struct {
		Name            string                    `json:"name"`
		OwnerReferences []v1alpha1.OwnerReference `json:"ownerReferences"`
	} `json:"metadata"`
}

// Reconcile handles one "Proc/<name>" key. It deletes the Proc only when
// every owner is absent from its kind's cache AND a live read confirms
// each one is gone (never delete on stale cache alone).
func (c *Controller) Reconcile(ctx context.Context, key string) error {
	raw, ok := c.procs.GetByKey(key)
	if !ok {
		// Already gone; nothing to collect.
		return nil
	}

	var proc procMeta
	if err := json.Unmarshal(raw, &proc); err != nil {
		// The apiserver validated what it stored; this guards against
		// wire corruption, not user input.
		c.log.Warn("skipping proc with unreadable metadata", "key", key, "error", err)
		return nil
	}

	var owners []v1alpha1.OwnerReference
	for _, ref := range proc.Metadata.OwnerReferences {
		if _, ownable := c.owners[ref.Kind]; ownable && inGroup(ref.APIVersion) {
			owners = append(owners, ref)
		}
	}
	if len(owners) == 0 {
		// Unowned Procs are not GC's business.
		return nil
	}

	for _, ref := range owners {
		if _, present := c.owners[ref.Kind].GetByKey(ref.Kind + "/" + ref.Name); present {
			return nil
		}
	}

	// Freshness rule: every owner looked absent in the cache, but the cache
	// may be stale. Confirm each one against the server before deleting.
	for _, ref := range owners {
		gone, err := c.liveGone(ctx, ref)
		if err != nil {
			return err
		}
		if !gone {
			// The cache was stale; the owner is alive.
			return nil
		}
	}

	if err := c.client.DeleteProc(ctx, proc.Metadata.Name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
		return fmt.Errorf("gc: deleting orphaned proc %q: %w", proc.Metadata.Name, err)
	}
	c.log.Info("deleted orphaned proc", "key", key)
	return nil
}

// EnqueueOwnedProcs returns an owner-informer handler (Daemon or Timer)
// that enqueues the key of every Proc owned by the fired object. It makes
// cascade deletion prompt: the Proc informer never fires Proc keys when an
// owner disappears, so without this hook orphans would wait for the
// periodic resync. Scanning the whole proc store per owner event is fine
// on a single host with dozens of objects.
func EnqueueOwnedProcs(procs *cache.Store, q queue.RateLimitingInterface) func(key string) {
	log := slog.With("component", "gc", "kind", v1alpha1.KindProc)
	return func(key string) {
		kind, name, ok := strings.Cut(key, "/")
		if !ok || (kind != v1alpha1.KindDaemon && kind != v1alpha1.KindTimer) {
			return
		}
		for _, raw := range procs.List() {
			var proc procMeta
			if err := json.Unmarshal(raw, &proc); err != nil {
				log.Warn("skipping proc with unreadable metadata", "error", err)
				continue
			}
			for _, ref := range proc.Metadata.OwnerReferences {
				if ref.Kind == kind && inGroup(ref.APIVersion) && ref.Name == name {
					q.Add(v1alpha1.KindProc + "/" + proc.Metadata.Name)
					break
				}
			}
		}
	}
}

// inGroup reports whether apiVersion ("group/version") is in the impd.sh
// API group. Only the group is compared, so version bumps keep matching.
func inGroup(apiVersion string) bool {
	group, _, _ := strings.Cut(apiVersion, "/")
	return group == v1alpha1.Group
}
