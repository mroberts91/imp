// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Test cases forked from
// k8s.io/client-go/util/workqueue/default_rate_limiters_test.go (Copyright
// The Kubernetes Authors, Apache-2.0).

import (
	"testing"
	"time"
)

func TestItemExponentialFailureRateLimiter(t *testing.T) {
	limiter := NewItemExponentialFailureRateLimiter(time.Millisecond, 1*time.Second)

	for i, want := range []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
		8 * time.Millisecond,
		16 * time.Millisecond,
	} {
		if got := limiter.When("one"); got != want {
			t.Errorf("When #%d = %v, want %v", i+1, got, want)
		}
	}
	if got := limiter.NumRequeues("one"); got != 5 {
		t.Errorf("NumRequeues(one) = %d, want 5", got)
	}

	// Each key backs off independently.
	if got := limiter.When("two"); got != 1*time.Millisecond {
		t.Errorf("When(two) = %v, want 1ms", got)
	}

	// Forget resets the key's failure count.
	limiter.Forget("one")
	if got := limiter.NumRequeues("one"); got != 0 {
		t.Errorf("NumRequeues after Forget = %d, want 0", got)
	}
	if got := limiter.When("one"); got != 1*time.Millisecond {
		t.Errorf("When after Forget = %v, want 1ms", got)
	}
}

func TestItemExponentialFailureRateLimiterCap(t *testing.T) {
	limiter := NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second)

	// Enough failures to pass both the cap and the float overflow guard.
	var last time.Duration
	for range 100 {
		last = limiter.When("overflowing")
	}
	if last != 1000*time.Second {
		t.Errorf("When after 100 failures = %v, want the 1000s cap", last)
	}
}

func TestDefaultRateLimiterConstants(t *testing.T) {
	limiter := DefaultRateLimiter()
	if got := limiter.When("k"); got != 5*time.Millisecond {
		t.Errorf("first When = %v, want the 5ms base", got)
	}
	for range 99 {
		limiter.When("k")
	}
	if got := limiter.When("k"); got != 1000*time.Second {
		t.Errorf("When after many failures = %v, want the 1000s cap", got)
	}
}
