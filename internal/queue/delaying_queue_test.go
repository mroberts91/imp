// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Test cases forked from
// k8s.io/client-go/util/workqueue/delaying_queue_test.go (Copyright The
// Kubernetes Authors, Apache-2.0), driven by internal/clock.Fake.

import (
	"testing"
	"time"

	"github.com/mroberts91/imp/internal/clock"
)

// newTestDelaying returns the queue plus its concrete type for the test
// helpers that need channel visibility.
func newTestDelaying(fc *clock.Fake) (DelayingInterface, *delayingType) {
	q := NewDelayingWithClock(fc)
	return q, q.(*delayingType)
}

// waitForChannelDrained blocks until the waiting loop has consumed
// everything AddAfter pushed, so the pending entries are in its heap.
func waitForChannelDrained(t *testing.T, dq *delayingType) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(dq.waitingForAddCh) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the waiting loop to drain its channel")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForLen polls (in real time) until the queue holds n keys.
func waitForLen(t *testing.T, q Interface, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for q.Len() != n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Len() == %d, have %d", n, q.Len())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDelayingAddAfterWaits(t *testing.T) {
	fc := clock.NewFake(time.Now())
	q, dq := newTestDelaying(fc)
	defer q.ShutDown()

	q.AddAfter("daemon/web", 50*time.Millisecond)
	waitForChannelDrained(t, dq)
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d before the delay elapsed, want 0", n)
	}

	fc.Step(60 * time.Millisecond)
	waitForLen(t, q, 1)

	key, shutdown := q.Get()
	if shutdown || key != "daemon/web" {
		t.Fatalf("Get() = (%q, %v), want (daemon/web, false)", key, shutdown)
	}
	q.Done(key)
}

func TestDelayingNoDelayIsImmediate(t *testing.T) {
	fc := clock.NewFake(time.Now())
	q, _ := newTestDelaying(fc)
	defer q.ShutDown()

	q.AddAfter("daemon/web", 0)
	q.AddAfter("daemon/db", -time.Second)
	// No clock step: zero/negative delays bypass the waiting loop.
	waitForLen(t, q, 2)
}

func TestDelayingOrdering(t *testing.T) {
	fc := clock.NewFake(time.Now())
	q, dq := newTestDelaying(fc)
	defer q.ShutDown()

	q.AddAfter("proc/slow", time.Second)
	q.AddAfter("proc/fast", 30*time.Millisecond)
	q.AddAfter("proc/medium", 500*time.Millisecond)
	waitForChannelDrained(t, dq)

	fc.Step(40 * time.Millisecond)
	waitForLen(t, q, 1)
	if key, _ := q.Get(); key != "proc/fast" {
		t.Fatalf("first ready key = %q, want proc/fast", key)
	}

	fc.Step(500 * time.Millisecond)
	waitForLen(t, q, 1)
	if key, _ := q.Get(); key != "proc/medium" {
		t.Fatalf("second ready key = %q, want proc/medium", key)
	}

	fc.Step(time.Second)
	waitForLen(t, q, 1)
	if key, _ := q.Get(); key != "proc/slow" {
		t.Fatalf("third ready key = %q, want proc/slow", key)
	}
}

// TestDelayingEarlierWins: re-delaying a pending key with a shorter delay
// makes it ready at the earlier deadline. Note the delivery-count guarantee
// is deliberately weak (same as upstream workqueue): a superseded entry MAY
// fire again later, and the base queue's dedup absorbs it - so the key must
// never be queued twice, but we don't assert it fires only once.
func TestDelayingEarlierWins(t *testing.T) {
	fc := clock.NewFake(time.Now())
	q, dq := newTestDelaying(fc)
	defer q.ShutDown()

	q.AddAfter("daemon/web", time.Second)
	waitForChannelDrained(t, dq)
	q.AddAfter("daemon/web", 30*time.Millisecond)
	waitForChannelDrained(t, dq)

	// Ready well before the original 1s deadline.
	fc.Step(40 * time.Millisecond)
	waitForLen(t, q, 1)

	// If the superseded 1s entry fires anyway, dedup must absorb it.
	// (Real-time pause gives the waiting loop a chance to misbehave.)
	fc.Step(2 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := q.Len(); n != 1 {
		t.Fatalf("Len() = %d after superseded delay elapsed, want 1", n)
	}
}

// TestDelayingLaterIgnored is the inverse: a later re-delay of a pending
// key does not push its readyAt back.
func TestDelayingLaterIgnored(t *testing.T) {
	fc := clock.NewFake(time.Now())
	q, dq := newTestDelaying(fc)
	defer q.ShutDown()

	q.AddAfter("daemon/web", 30*time.Millisecond)
	waitForChannelDrained(t, dq)
	q.AddAfter("daemon/web", time.Hour)
	waitForChannelDrained(t, dq)

	fc.Step(40 * time.Millisecond)
	waitForLen(t, q, 1)
}

func TestDelayingAddAfterShutDownIgnored(t *testing.T) {
	fc := clock.NewFake(time.Now())
	q, _ := newTestDelaying(fc)

	q.ShutDown()
	q.AddAfter("daemon/web", time.Millisecond)
	fc.Step(time.Second)
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d, want 0 (AddAfter after ShutDown must be ignored)", n)
	}
}
