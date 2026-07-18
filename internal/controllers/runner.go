// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package controllers holds the shared machinery every imp controller loop
// runs on: a Runner that pumps keys from a rate-limiting queue into a
// Reconciler with a pool of workers, and an owner-mapping informer handler
// (owner.go) that wakes an owner when a child changes.
//
// The worker-loop shape (Get / deferred Done / Forget on success /
// AddRateLimited on error) is transposed from kubernetes
// staging/src/k8s.io/sample-controller/controller.go, processNextWorkItem
// (Copyright The Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/).
// Deltas: items are plain "Kind/name" string keys, no metrics or
// eventBroadcaster, sync gates are a tiny consumer-defined interface
// instead of cache.WaitForCacheSync, requeue-with-delay is a typed
// sentinel error instead of controller-runtime's Result struct, and the
// only recover is the documented per-item boundary below (impd's panic
// policy is otherwise crash-only).
package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mroberts91/imp/internal/queue"
)

// Reconciler reconciles the object named by key ("Kind/name") toward its
// desired state. The key is a hint that something may have changed; the
// implementation must read current state and converge (level-triggered).
type Reconciler interface {
	Reconcile(ctx context.Context, key string) error
}

// RequeueAfter is a scheduling instruction, not a failure: a Reconciler
// returns it (possibly wrapped) to say "this pass is done, run me again for
// this key after d". The Runner treats it as success - the rate limiter is
// reset (Forget), never escalated - and re-adds the key after After.
//
// Decision of record 2026-07-18: imp uses this typed sentinel error rather
// than adopting controller-runtime's Result struct.
type RequeueAfter struct {
	After time.Duration
}

func (r RequeueAfter) Error() string {
	return fmt.Sprintf("requeue after %s", r.After)
}

// Syncer is a startup gate: Run waits for every gate before starting
// workers, so reconcilers never see a half-filled cache. *cache.Informer
// satisfies it.
type Syncer interface {
	WaitForSync(ctx context.Context) error
}

// Runner drives one controller: it waits for its sync gates, then runs
// worker goroutines that feed queue keys to the Reconciler until the
// context ends and the queue drains.
type Runner struct {
	name       string
	queue      queue.RateLimitingInterface
	reconciler Reconciler
	workers    int
	gates      []Syncer
	log        *slog.Logger
}

// NewRunner builds a Runner named name (the slog component key) around q
// and rec. The queue is injected so tests can pass a clocked one; the
// caller keeps a reference for handlers that Add to it. workers <= 0 means
// one worker. gates are waited on, in order, before any worker starts.
func NewRunner(name string, q queue.RateLimitingInterface, rec Reconciler, workers int, gates ...Syncer) *Runner {
	if workers <= 0 {
		workers = 1
	}
	return &Runner{
		name:       name,
		queue:      q,
		reconciler: rec,
		workers:    workers,
		gates:      gates,
		log:        slog.With("component", name),
	}
}

// Run blocks until ctx ends and every worker has exited (the queue drains
// per its shutdown semantics). It first waits for every sync gate; a gate
// error (including ctx cancellation) shuts the queue down and is returned.
// A nil return means a clean context-driven shutdown.
func (r *Runner) Run(ctx context.Context) error {
	for _, g := range r.gates {
		if err := g.WaitForSync(ctx); err != nil {
			r.queue.ShutDown()
			return fmt.Errorf("%s: waiting for sync: %w", r.name, err)
		}
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		<-ctx.Done()
		r.queue.ShutDown()
	})
	for range r.workers {
		wg.Go(func() {
			for r.processNextItem(ctx) {
			}
		})
	}
	wg.Wait()
	return nil
}

// processNextItem works one key; false means the queue has shut down and
// the worker should exit. Ordering is load-bearing (see the package doc's
// sample-controller attribution): Get, deferred Done, then exactly one of
// Forget+AddAfter (RequeueAfter), AddRateLimited (error), or Forget.
func (r *Runner) processNextItem(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)

	err := r.reconcileGuarded(ctx, key)

	var requeue RequeueAfter
	switch {
	case errors.As(err, &requeue):
		// A scheduling instruction, not a failure: reset backoff and
		// come back after the requested delay.
		r.queue.Forget(key)
		r.queue.AddAfter(key, requeue.After)
	case err != nil:
		r.log.Error("reconcile failed", "key", key, "error", err)
		r.queue.AddRateLimited(key)
	default:
		r.queue.Forget(key)
	}
	return true
}

// reconcileGuarded wraps the single Reconcile call in the ONE sanctioned
// recover() in the codebase. impd's panic policy is crash-only, but a
// poison object must not crash-loop the daemon: the panic is logged with
// its stack, converted to an error (so the caller rate-limits the key),
// and the worker lives on. Everything outside this call - the worker loop,
// the queue, the informers - stays unguarded on purpose.
func (r *Runner) reconcileGuarded(ctx context.Context, key string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("reconcile panicked",
				"key", key,
				"error", fmt.Sprintf("panic: %v", p),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("reconcile panicked: %v", p)
		}
	}()
	return r.reconciler.Reconcile(ctx, key)
}
