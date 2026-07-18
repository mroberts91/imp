// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package clock is the time seam for anything time-driven: components take
// a Clock so tests can drive timers deterministically with Fake. Real is
// the production implementation over the time package.
//
// Minimal by design (Now, NewTimer, NewTicker) - grow it only when a
// consumer actually needs more.
package clock

import "time"

// Clock tells time and makes timers/tickers.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// Timer fires once on its channel. Stop reports whether it stopped the
// timer before it fired, like time.Timer.Stop.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Ticker fires periodically on its channel until stopped.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real implements Clock with the time package.
type Real struct{}

var _ Clock = Real{}

func (Real) Now() time.Time { return time.Now() }

func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

func (Real) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
