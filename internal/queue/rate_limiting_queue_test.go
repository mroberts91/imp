// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Test cases forked from
// k8s.io/client-go/util/workqueue/rate_limiting_queue_test.go (Copyright
// The Kubernetes Authors, Apache-2.0).

import (
	"testing"
	"time"

	"github.com/mroberts91/imp/internal/clock"
)

func TestRateLimitingQueue(t *testing.T) {
	fc := clock.NewFake(time.Now())
	limiter := NewItemExponentialFailureRateLimiter(time.Millisecond, time.Second)
	q := NewRateLimitingWithClock(limiter, fc)
	defer q.ShutDown()
	dq := q.(*rateLimitingType).DelayingInterface.(*delayingType)

	// Backoff grows per AddRateLimited call.
	q.AddRateLimited("daemon/web")
	if got := q.NumRequeues("daemon/web"); got != 1 {
		t.Fatalf("NumRequeues = %d, want 1", got)
	}
	waitForChannelDrained(t, dq)
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d before backoff elapsed, want 0", n)
	}

	fc.Step(2 * time.Millisecond)
	waitForLen(t, q, 1)
	key, _ := q.Get()
	q.Done(key)

	// Second failure: 2ms backoff.
	q.AddRateLimited("daemon/web")
	if got := q.NumRequeues("daemon/web"); got != 2 {
		t.Fatalf("NumRequeues = %d, want 2", got)
	}
	waitForChannelDrained(t, dq)
	fc.Step(time.Millisecond)
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d after 1ms of a 2ms backoff, want 0", n)
	}
	fc.Step(2 * time.Millisecond)
	waitForLen(t, q, 1)
	key, _ = q.Get()
	q.Done(key)

	// Forget resets the backoff to base.
	q.Forget("daemon/web")
	if got := q.NumRequeues("daemon/web"); got != 0 {
		t.Fatalf("NumRequeues after Forget = %d, want 0", got)
	}
	q.AddRateLimited("daemon/web")
	if got := q.NumRequeues("daemon/web"); got != 1 {
		t.Fatalf("NumRequeues after Forget+AddRateLimited = %d, want 1", got)
	}
}
