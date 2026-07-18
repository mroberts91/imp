// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Fork of k8s.io/client-go/util/workqueue/default_rate_limiters.go
// (Copyright The Kubernetes Authors, Apache-2.0), keeping only the
// per-item exponential limiter - imp has no need for the overall token
// bucket (one host, a handful of controllers), and dropping it drops the
// x/time dependency.

import (
	"math"
	"sync"
	"time"
)

// RateLimiter decides how long a key must wait before it may be requeued.
type RateLimiter interface {
	// When returns how long the key should wait; each call counts as a
	// failure.
	When(key string) time.Duration
	// Forget stops tracking the key (resets its failure count).
	Forget(key string)
	// NumRequeues returns how many failures the key has had.
	NumRequeues(key string) int
}

// DefaultRateLimiter is the controllers' default: per-key exponential
// backoff, base 5ms capped at 1000s (workqueue's defaults). Distinct on
// purpose from execd's process crash backoff, which has its own constants.
func DefaultRateLimiter() RateLimiter {
	return NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second)
}

// itemExponentialFailureRateLimiter does a simple baseDelay*2^<failures>
// limit; dealing with max failures and expiration is up to the caller.
type itemExponentialFailureRateLimiter struct {
	failuresLock sync.Mutex
	failures     map[string]int

	baseDelay time.Duration
	maxDelay  time.Duration
}

// NewItemExponentialFailureRateLimiter constructs a per-key exponential
// backoff limiter: baseDelay·2^failures, capped at maxDelay.
func NewItemExponentialFailureRateLimiter(baseDelay, maxDelay time.Duration) RateLimiter {
	return &itemExponentialFailureRateLimiter{
		failures:  map[string]int{},
		baseDelay: baseDelay,
		maxDelay:  maxDelay,
	}
}

func (r *itemExponentialFailureRateLimiter) When(key string) time.Duration {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	exp := r.failures[key]
	r.failures[key] = exp + 1

	// The backoff is computed in float so the doubling can never
	// overflow, then capped.
	backoff := float64(r.baseDelay.Nanoseconds()) * math.Pow(2, float64(exp))
	if backoff > math.MaxInt64 {
		return r.maxDelay
	}

	calculated := time.Duration(backoff)
	if calculated > r.maxDelay {
		return r.maxDelay
	}

	return calculated
}

func (r *itemExponentialFailureRateLimiter) NumRequeues(key string) int {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	return r.failures[key]
}

func (r *itemExponentialFailureRateLimiter) Forget(key string) {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	delete(r.failures, key)
}
