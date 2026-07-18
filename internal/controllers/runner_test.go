// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mroberts91/imp/internal/queue"
)

// Timing strategy (decision of record for these tests): the real clock with
// millisecond delays and eventually-style polling, not a fake clock - the
// queue's delaying goroutine consumes AddAfter entries asynchronously, so a
// fake clock needs step-then-poll loops anyway and buys no determinism
// here. Every wait has a hard deadline; the happy path completes in
// milliseconds.

const waitDeadline = 2 * time.Second

// errPanicMarker scripts a panic: when a step yields it, Reconcile panics
// instead of returning.
var errPanicMarker = errors.New("scripted panic")

// call records one Reconcile invocation.
type call struct {
	key string
	at  time.Time
}

// scriptedReconciler returns the scripted errors for a key in order; an
// exhausted (or absent) script returns nil. Every invocation is sent on
// calls before the scripted panic/return happens.
type scriptedReconciler struct {
	mu      sync.Mutex
	scripts map[string][]error
	calls   chan call
}

func newScripted(scripts map[string][]error) *scriptedReconciler {
	return &scriptedReconciler{scripts: scripts, calls: make(chan call, 128)}
}

func (s *scriptedReconciler) Reconcile(_ context.Context, key string) error {
	s.mu.Lock()
	var err error
	if steps := s.scripts[key]; len(steps) > 0 {
		err = steps[0]
		s.scripts[key] = steps[1:]
	}
	s.mu.Unlock()

	s.calls <- call{key: key, at: time.Now()}
	if errors.Is(err, errPanicMarker) {
		panic("scripted panic")
	}
	return err
}

// waitCall waits for the next Reconcile invocation and asserts its key.
func waitCall(t *testing.T, rec *scriptedReconciler, want string) call {
	t.Helper()
	select {
	case c := <-rec.calls:
		if c.key != want {
			t.Fatalf("reconciled key = %q, want %q", c.key, want)
		}
		return c
	case <-time.After(waitDeadline):
		t.Fatalf("timed out waiting for reconcile of %q", want)
		return call{}
	}
}

// assertNoCall asserts no Reconcile happens within d.
func assertNoCall(t *testing.T, rec *scriptedReconciler, d time.Duration) {
	t.Helper()
	select {
	case c := <-rec.calls:
		t.Fatalf("unexpected reconcile of %q", c.key)
	case <-time.After(d):
	}
}

// startRunner runs a single-worker Runner on q until stop is called; stop
// cancels the context and waits for Run to return.
func startRunner(t *testing.T, q queue.RateLimitingInterface, rec Reconciler, gates ...Syncer) (stop func() error) {
	t.Helper()
	r := NewRunner("test", q, rec, 1, gates...)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return func() error {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(waitDeadline):
			t.Fatal("Run did not return after context cancel")
			return nil
		}
	}
}

func TestRunnerErrorIsRetriedWithBackoff(t *testing.T) {
	rec := newScripted(map[string][]error{
		"Daemon/a": {errors.New("boom")},
	})
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	stop := startRunner(t, q, rec)
	defer stop() //nolint:errcheck // shutdown path; error checked in dedicated test

	q.Add("Daemon/a")
	waitCall(t, rec, "Daemon/a")
	// The failure must come back via AddRateLimited: a second call
	// arrives without anyone re-adding the key.
	waitCall(t, rec, "Daemon/a")
}

func TestRunnerSuccessForgets(t *testing.T) {
	rec := newScripted(map[string][]error{
		"Daemon/a": {errors.New("boom")},
	})
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	stop := startRunner(t, q, rec)
	defer stop() //nolint:errcheck

	q.Add("Daemon/a")
	waitCall(t, rec, "Daemon/a") // fails: NumRequeues becomes 1
	waitCall(t, rec, "Daemon/a") // succeeds: Forget resets backoff

	// Forget happens after the second call returns; poll for it.
	deadline := time.Now().Add(waitDeadline)
	for q.NumRequeues("Daemon/a") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("NumRequeues = %d after success, want 0", q.NumRequeues("Daemon/a"))
		}
		time.Sleep(time.Millisecond)
	}
	// And success must not re-deliver.
	assertNoCall(t, rec, 50*time.Millisecond)
}

func TestRunnerPanicIsRecoveredAndRequeued(t *testing.T) {
	rec := newScripted(map[string][]error{
		"Daemon/p": {errPanicMarker},
	})
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	stop := startRunner(t, q, rec)

	q.Add("Daemon/p")
	waitCall(t, rec, "Daemon/p") // panics inside Reconcile
	waitCall(t, rec, "Daemon/p") // requeued via AddRateLimited, then succeeds

	// The (only) worker survived the panic: a fresh key still gets
	// processed.
	q.Add("Daemon/q")
	waitCall(t, rec, "Daemon/q")

	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after panic recovery, want nil", err)
	}
}

func TestRunnerRequeueAfterDoesNotEscalateBackoff(t *testing.T) {
	const delay = 20 * time.Millisecond
	rec := newScripted(map[string][]error{
		// Wrapped, to prove the runner matches with errors.As.
		"Daemon/r": {fmt.Errorf("scheduling: %w", RequeueAfter{After: delay})},
	})
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	stop := startRunner(t, q, rec)
	defer stop() //nolint:errcheck

	q.Add("Daemon/r")
	first := waitCall(t, rec, "Daemon/r")
	second := waitCall(t, rec, "Daemon/r")

	if got := second.at.Sub(first.at); got < delay {
		t.Errorf("key came back after %v, want >= %v", got, delay)
	}
	if got := q.NumRequeues("Daemon/r"); got != 0 {
		t.Errorf("NumRequeues = %d after RequeueAfter, want 0 (must not escalate)", got)
	}
}

func TestRunnerContextCancelShutsDown(t *testing.T) {
	rec := newScripted(nil)
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	stop := startRunner(t, q, rec)

	q.Add("Daemon/a")
	waitCall(t, rec, "Daemon/a")

	if err := stop(); err != nil {
		t.Fatalf("Run returned %v on clean shutdown, want nil", err)
	}
	if !q.ShuttingDown() {
		t.Error("queue not shutting down after Run returned")
	}
}

// chanGate releases WaitForSync when its channel closes.
type chanGate struct{ ch chan struct{} }

func (g chanGate) WaitForSync(ctx context.Context) error {
	select {
	case <-g.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRunnerWaitsForSyncGates(t *testing.T) {
	rec := newScripted(nil)
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	gate := chanGate{ch: make(chan struct{})}
	stop := startRunner(t, q, rec, gate)
	defer stop() //nolint:errcheck

	q.Add("Daemon/a")
	assertNoCall(t, rec, 30*time.Millisecond) // gate closed: no workers yet
	close(gate.ch)
	waitCall(t, rec, "Daemon/a")
}

func TestRunnerGateFailureShutsDown(t *testing.T) {
	rec := newScripted(nil)
	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	gate := chanGate{ch: make(chan struct{})} // never released: fails on cancel
	stop := startRunner(t, q, rec, gate)

	q.Add("Daemon/a")
	err := stop()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if !q.ShuttingDown() {
		t.Error("queue not shut down after gate failure")
	}
	assertNoCall(t, rec, 30*time.Millisecond)
}
