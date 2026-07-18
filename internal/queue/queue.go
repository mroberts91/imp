// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package queue is the controllers' work queue. Keys go in (possibly many
// times), come out one at a time, and the load-bearing property is the
// dirty/processing two-set dedup: a key re-added while it is being
// processed runs exactly once more, never concurrently and never N times.
//
// Near-verbatim fork of k8s.io/client-go/util/workqueue (Copyright The
// Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/), specialized:
// items are string keys ("kind/name"), no metrics, no generics or
// deprecated shims, clock injected via internal/clock, and no recover in
// the delaying goroutine (impd's panic policy is crash-only).
package queue

import "sync"

// Interface is the base queue: Add with dedup, blocking Get, Done to
// release. Fork of workqueue's TypedInterface.
type Interface interface {
	// Add marks key as needing processing. Adds are absorbed while the
	// key is dirty; a shut-down queue ignores new keys.
	Add(key string)
	// Len is the number of keys waiting (informational only).
	Len() int
	// Get blocks until a key is available or the queue shuts down.
	// The caller must call Done(key) when finished processing.
	Get() (key string, shutdown bool)
	// Done marks key as done processing; if it was re-added while being
	// processed, it is requeued.
	Done(key string)
	// ShutDown ignores all further Adds and wakes blocked Gets; workers
	// drain what is already queued, then Get reports shutdown.
	ShutDown()
	// ShuttingDown reports whether ShutDown was called.
	ShuttingDown() bool
}

// New constructs a base work queue.
func New() Interface {
	return &queueType{
		dirty:      set{},
		processing: set{},
		cond:       sync.NewCond(&sync.Mutex{}),
	}
}

// queueType is workqueue's Typed[T]. All fields are guarded by cond.L.
type queueType struct {
	// queue defines the order keys are worked on. Every element is in
	// dirty and not in processing.
	queue []string

	// dirty holds all keys that need processing.
	dirty set

	// processing holds keys currently being processed. A key may be in
	// dirty at the same time: when Done removes it from processing, it
	// is requeued.
	processing set

	cond *sync.Cond

	shuttingDown bool
}

type set map[string]struct{}

func (s set) has(key string) bool { _, ok := s[key]; return ok }
func (s set) insert(key string)   { s[key] = struct{}{} }
func (s set) delete(key string)   { delete(s, key) }

func (q *queueType) Add(key string) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	if q.shuttingDown {
		return
	}
	if q.dirty.has(key) {
		return
	}

	q.dirty.insert(key)
	if q.processing.has(key) {
		return
	}

	q.queue = append(q.queue, key)
	q.cond.Signal()
}

func (q *queueType) Len() int {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return len(q.queue)
}

func (q *queueType) Get() (key string, shutdown bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	for len(q.queue) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		// We must be shutting down.
		return "", true
	}

	key = q.queue[0]
	q.queue[0] = "" // drop the backing-array reference
	q.queue = q.queue[1:]

	q.processing.insert(key)
	q.dirty.delete(key)

	return key, false
}

func (q *queueType) Done(key string) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	q.processing.delete(key)
	if q.dirty.has(key) {
		q.queue = append(q.queue, key)
		q.cond.Signal()
	}
}

func (q *queueType) ShutDown() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	q.shuttingDown = true
	q.cond.Broadcast()
}

func (q *queueType) ShuttingDown() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	return q.shuttingDown
}
