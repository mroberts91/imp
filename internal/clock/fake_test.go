// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"testing"
	"time"
)

func TestFakeTimerFiresOnStep(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	timer := fc.NewTimer(50 * time.Millisecond)

	fc.Step(49 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("timer fired before its deadline")
	default:
	}

	fc.Step(1 * time.Millisecond)
	select {
	case <-timer.C():
	default:
		t.Fatal("timer did not fire at its deadline")
	}

	if timer.Stop() {
		t.Error("Stop after firing should report false")
	}
}

func TestFakeTimerStop(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	timer := fc.NewTimer(time.Second)

	if !timer.Stop() {
		t.Fatal("Stop before firing should report true")
	}
	fc.Step(2 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("stopped timer fired")
	default:
	}
}

func TestFakeTimerZeroDurationFiresImmediately(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	timer := fc.NewTimer(0)
	select {
	case <-timer.C():
	default:
		t.Fatal("zero-duration timer did not fire immediately")
	}
}

func TestFakeTickerRepeatsAndCoalesces(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	ticker := fc.NewTicker(10 * time.Millisecond)

	fc.Step(10 * time.Millisecond)
	select {
	case <-ticker.C():
	default:
		t.Fatal("ticker did not fire on first period")
	}

	// A step spanning several periods coalesces into one pending fire,
	// like time.Ticker.
	fc.Step(35 * time.Millisecond)
	select {
	case <-ticker.C():
	default:
		t.Fatal("ticker did not fire after multi-period step")
	}
	select {
	case <-ticker.C():
		t.Fatal("coalesced ticker delivered more than one pending fire")
	default:
	}

	ticker.Stop()
	fc.Step(50 * time.Millisecond)
	select {
	case <-ticker.C():
		t.Fatal("stopped ticker fired")
	default:
	}
}

func TestFakeWaiters(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	if got := fc.Waiters(); got != 0 {
		t.Fatalf("Waiters() = %d, want 0", got)
	}

	timer := fc.NewTimer(time.Second)
	fc.NewTicker(time.Second)
	if got := fc.Waiters(); got != 2 {
		t.Fatalf("Waiters() = %d, want 2", got)
	}

	timer.Stop()
	if got := fc.Waiters(); got != 1 {
		t.Fatalf("Waiters() after timer stop = %d, want 1", got)
	}

	// A fired one-shot timer is no longer waiting; the ticker re-arms.
	fc.Step(2 * time.Second)
	if got := fc.Waiters(); got != 1 {
		t.Fatalf("Waiters() after step = %d, want 1", got)
	}
}

func TestFakeNow(t *testing.T) {
	start := time.Unix(100, 0)
	fc := NewFake(start)
	if !fc.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", fc.Now(), start)
	}
	fc.Step(time.Minute)
	if want := start.Add(time.Minute); !fc.Now().Equal(want) {
		t.Fatalf("Now() after step = %v, want %v", fc.Now(), want)
	}
}
