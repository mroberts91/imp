// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
)

type fakeEvents struct {
	mu     sync.Mutex
	events map[string]*v1alpha1.Event
	rv     int64
	calls  int
}

func newFakeEvents() *fakeEvents {
	return &fakeEvents{events: make(map[string]*v1alpha1.Event)}
}

func (f *fakeEvents) GetEvent(_ context.Context, name string) (*v1alpha1.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.events[name]
	if !ok {
		return nil, v1alpha1.ErrNotFound
	}
	cp := *ev
	cp.Metadata = ev.Metadata
	return &cp, nil
}

func (f *fakeEvents) ApplyEvent(_ context.Context, e *v1alpha1.Event) (*v1alpha1.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	cp := *e
	cp.Metadata = e.Metadata
	if existing, ok := f.events[e.Metadata.Name]; ok {
		if e.Metadata.ResourceVersion != "" && e.Metadata.ResourceVersion != existing.Metadata.ResourceVersion {
			return nil, v1alpha1.ErrConflict
		}
	}
	f.rv++
	cp.Metadata.ResourceVersion = strconv.FormatInt(f.rv, 10)
	stored := cp
	f.events[e.Metadata.Name] = &stored
	out := cp
	return &out, nil
}

func (f *fakeEvents) get(name string) *v1alpha1.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := f.events[name]
	if ev == nil {
		return nil
	}
	cp := *ev
	return &cp
}

func (f *fakeEvents) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func regarding() v1alpha1.ObjectRef {
	return v1alpha1.ObjectRef{Kind: v1alpha1.KindProc, Name: "web-0", UID: "uid-1"}
}

func TestEventfCreateThenAggregate(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	fake := newFakeEvents()
	r := New(fake, "execd", clk)
	ctx := context.Background()

	r.Eventf(ctx, regarding(), v1alpha1.EventTypeWarning, v1alpha1.ReasonBackOff, "backing off")
	if fake.n() != 1 {
		t.Fatalf("events = %d, want 1 after create", fake.n())
	}
	var name string
	for n := range fake.events {
		name = n
	}
	first := fake.get(name)
	if first.Count != 1 {
		t.Fatalf("count = %d, want 1", first.Count)
	}

	clk.Step(time.Second)
	r.Eventf(ctx, regarding(), v1alpha1.EventTypeWarning, v1alpha1.ReasonBackOff, "backing off")
	got := fake.get(name)
	if got == nil {
		t.Fatal("event vanished")
	}
	if got.Count != 2 {
		t.Errorf("count = %d, want 2 after aggregate", got.Count)
	}
	if !got.LastTimestamp.After(first.LastTimestamp.Time) {
		t.Errorf("lastTimestamp did not advance: %v -> %v", first.LastTimestamp, got.LastTimestamp)
	}
	if fake.n() != 1 {
		t.Errorf("events = %d, want 1 (same object)", fake.n())
	}
}

func TestEventfDifferentMessageIsNewEvent(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	fake := newFakeEvents()
	r := New(fake, "execd", clk)
	ctx := context.Background()

	r.Eventf(ctx, regarding(), v1alpha1.EventTypeNormal, v1alpha1.ReasonStarted, "started pid 1")
	r.Eventf(ctx, regarding(), v1alpha1.EventTypeNormal, v1alpha1.ReasonStarted, "started pid 2")
	if fake.n() != 2 {
		t.Fatalf("events = %d, want 2 (message is part of dedup key)", fake.n())
	}
}

func TestEventfLRUTTLExpiryCreatesAgain(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	fake := newFakeEvents()
	r := New(fake, "execd", clk)
	r.keyTTL = time.Minute
	ctx := context.Background()

	r.Eventf(ctx, regarding(), v1alpha1.EventTypeWarning, v1alpha1.ReasonBackOff, "msg")
	name1 := ""
	for n := range fake.events {
		name1 = n
	}
	// Expire the correlator entry, but leave the store Event so Apply would
	// replace if we naively re-create — we still remember via apply of the
	// same deterministic name and count stays 1 on a fresh create path...
	// After TTL miss we Create (Apply) with count=1, overwriting.
	clk.Step(2 * time.Minute)
	r.Eventf(ctx, regarding(), v1alpha1.EventTypeWarning, v1alpha1.ReasonBackOff, "msg")
	got := fake.get(name1)
	if got == nil {
		t.Fatal("expected deterministic name reused")
	}
	if got.Count != 1 {
		t.Errorf("after correlator TTL expiry, create path resets count; got %d", got.Count)
	}
}

func TestEventfSpamFilter(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	fake := newFakeEvents()
	r := New(fake, "execd", clk)
	r.spamBurst = 2
	r.tokens = 2
	ctx := context.Background()

	refA := regarding()
	refB := v1alpha1.ObjectRef{Kind: v1alpha1.KindProc, Name: "web-1", UID: "uid-2"}
	refC := v1alpha1.ObjectRef{Kind: v1alpha1.KindProc, Name: "web-2", UID: "uid-3"}

	r.Eventf(ctx, refA, v1alpha1.EventTypeNormal, "A", "a")
	r.Eventf(ctx, refB, v1alpha1.EventTypeNormal, "B", "b")
	r.Eventf(ctx, refC, v1alpha1.EventTypeNormal, "C", "c") // should drop
	if fake.n() != 2 {
		t.Fatalf("events = %d, want 2 (third spam-filtered)", fake.n())
	}

	clk.Step(time.Second) // refill 1 token at 1 qps
	r.Eventf(ctx, refC, v1alpha1.EventTypeNormal, "C", "c")
	if fake.n() != 3 {
		t.Fatalf("events = %d, want 3 after refill", fake.n())
	}
}

func TestEventfSwallowsApplyError(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	failing := &failClient{err: errors.New("boom")}
	r := New(failing, "execd", clk)
	// Must not panic.
	r.Eventf(context.Background(), regarding(), v1alpha1.EventTypeNormal, "X", "y")
}

type failClient struct{ err error }

func (f *failClient) GetEvent(context.Context, string) (*v1alpha1.Event, error) {
	return nil, f.err
}
func (f *failClient) ApplyEvent(context.Context, *v1alpha1.Event) (*v1alpha1.Event, error) {
	return nil, f.err
}
