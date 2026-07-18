// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package clock

// Fake is a hand-rolled miniature of k8s.io/utils/clock/testing
// (Copyright The Kubernetes Authors, Apache-2.0): time only moves when the
// test calls Step, and due timers/tickers fire during the step.

import (
	"sync"
	"time"
)

// Fake implements Clock for tests. Time starts at the given instant and
// advances only via Step. Safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

var _ Clock = (*Fake)(nil)

// fakeWaiter is one armed timer or ticker.
type fakeWaiter struct {
	target time.Time
	period time.Duration // 0 for one-shot timers
	active bool
	ch     chan time.Time
}

// NewFake returns a Fake whose current time is t.
func NewFake(t time.Time) *Fake {
	return &Fake{now: t}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{target: f.now.Add(d), active: true, ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	f.fireLocked() // a zero or negative duration fires immediately
	return &fakeTimer{f: f, w: w}
}

func (f *Fake) NewTicker(d time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{target: f.now.Add(d), period: d, active: true, ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	return &fakeTicker{f: f, w: w}
}

// Step advances the clock by d and fires every timer/ticker that comes due.
func (f *Fake) Step(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.fireLocked()
}

// Waiters returns the number of armed timers/tickers. Tests use it to wait
// until the code under test has actually set up its timer before stepping.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.waiters {
		if w.active {
			n++
		}
	}
	return n
}

// fireLocked delivers to every due waiter. Sends are non-blocking against
// the 1-buffered channels, so an unread ticker coalesces fires exactly like
// time.Ticker. Callers must hold f.mu.
func (f *Fake) fireLocked() {
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		for w.active && !w.target.After(f.now) {
			select {
			case w.ch <- w.target:
			default:
			}
			if w.period > 0 {
				w.target = w.target.Add(w.period)
			} else {
				w.active = false
			}
		}
		if w.active {
			kept = append(kept, w)
		}
	}
	f.waiters = kept
}

type fakeTimer struct {
	f *Fake
	w *fakeWaiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	was := t.w.active
	t.w.active = false
	return was
}

type fakeTicker struct {
	f *Fake
	w *fakeWaiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }

func (t *fakeTicker) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.w.active = false
}
