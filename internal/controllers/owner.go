// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package controllers

// Owner mapping: an informer handler for a child kind that enqueues the
// owning object's key, so a Proc change wakes its Daemon. The idea is
// controller-runtime's EnqueueRequestForOwner; the on-disk ancestor is
// handleObject in kubernetes
// staging/src/k8s.io/sample-controller/controller.go (Copyright The
// Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/). Deltas: imp
// handlers receive keys only, so the object is re-read from the informer
// store; there are no tombstones, so deletions are handled by remembering
// each child's last-enqueued owners; owners are matched on Kind plus the
// impd.sh API group prefix (not a full GVK), and every match is enqueued
// (imp's OwnerReference has no Controller field).

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/queue"
)

// KeyGetter is the slice of the informer store EnqueueOwner reads.
// *cache.Store satisfies it; tests inject a map-backed fake.
type KeyGetter interface {
	GetByKey(key string) (json.RawMessage, bool)
}

// EnqueueOwner returns an informer handler for a child kind: on every child
// key it looks the child up in store, and for each ownerReference whose
// Kind is ownerKind and whose apiVersion is in the impd.sh group (group
// prefix only - a version bump must not break mapping), it adds
// "ownerKind/name" to q.
//
// Deletion: the store entry is already gone when the handler fires, so the
// closure remembers, per child key, the owner keys it last enqueued; a
// missing child enqueues those remembered owners and forgets the entry.
// Without this, deleting a child would never wake its owner.
//
// Handlers are called sequentially from the informer goroutine today, but
// the internal map is mutex-guarded anyway - the contract does not promise
// single-threadedness forever.
func EnqueueOwner(store KeyGetter, ownerKind string, q queue.Interface) func(key string) {
	log := slog.With("component", "controllers", "kind", ownerKind)
	var mu sync.Mutex
	// lastOwners maps child key -> owner keys enqueued for it last time it
	// was seen in the store.
	lastOwners := map[string][]string{}

	return func(key string) {
		raw, ok := store.GetByKey(key)
		if !ok {
			// Child deleted: wake the owners we last saw for it.
			mu.Lock()
			owners := lastOwners[key]
			delete(lastOwners, key)
			mu.Unlock()
			for _, owner := range owners {
				q.Add(owner)
			}
			return
		}

		var envelope struct {
			Metadata struct {
				OwnerReferences []v1alpha1.OwnerReference `json:"ownerReferences"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			// The apiserver validated what it stored; this guards
			// against wire corruption, not user input.
			log.Warn("skipping child with unreadable metadata", "key", key, "error", err)
			return
		}

		var owners []string
		for _, ref := range envelope.Metadata.OwnerReferences {
			if ref.Kind != ownerKind || !inGroup(ref.APIVersion) {
				continue
			}
			owners = append(owners, ownerKind+"/"+ref.Name)
		}

		mu.Lock()
		if len(owners) == 0 {
			delete(lastOwners, key)
		} else {
			lastOwners[key] = owners
		}
		mu.Unlock()

		for _, owner := range owners {
			q.Add(owner)
		}
	}
}

// inGroup reports whether apiVersion ("group/version") is in the impd.sh
// API group. Only the group is compared, so version bumps keep matching.
func inGroup(apiVersion string) bool {
	group, _, _ := strings.Cut(apiVersion, "/")
	return group == v1alpha1.Group
}
