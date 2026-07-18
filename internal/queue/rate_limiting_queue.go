// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Fork of k8s.io/client-go/util/workqueue/rate_limiting_queue.go
// (Copyright The Kubernetes Authors, Apache-2.0).

import "github.com/mroberts91/imp/internal/clock"

// RateLimitingInterface rate limits keys being re-added to the queue.
type RateLimitingInterface interface {
	DelayingInterface

	// AddRateLimited adds a key after the rate limiter says it's ok.
	AddRateLimited(key string)

	// Forget tells the rate limiter to stop tracking the key: call it on
	// success (or permanent failure) or backoff grows forever. This only
	// clears the rate limiter - you still have to Done the queue.
	Forget(key string)

	// NumRequeues returns how many times the key was requeued.
	NumRequeues(key string) int
}

// NewRateLimiting constructs a RateLimitingInterface on the real clock.
// Remember to call Forget! If you don't, you may end up tracking failures
// forever.
func NewRateLimiting(rateLimiter RateLimiter) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewDelaying(),
		rateLimiter:       rateLimiter,
	}
}

// NewRateLimitingWithClock is NewRateLimiting with an injected clock, for
// tests.
func NewRateLimitingWithClock(rateLimiter RateLimiter, c clock.Clock) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewDelayingWithClock(c),
		rateLimiter:       rateLimiter,
	}
}

// rateLimitingType wraps a DelayingInterface and provides rate-limited
// re-enquing.
type rateLimitingType struct {
	DelayingInterface

	rateLimiter RateLimiter
}

func (q *rateLimitingType) AddRateLimited(key string) {
	q.AddAfter(key, q.rateLimiter.When(key))
}

func (q *rateLimitingType) NumRequeues(key string) int {
	return q.rateLimiter.NumRequeues(key)
}

func (q *rateLimitingType) Forget(key string) {
	q.rateLimiter.Forget(key)
}
