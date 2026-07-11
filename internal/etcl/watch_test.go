// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package etcl

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func recvEvent(t *testing.T, ch <-chan v1alpha1.WatchEvent) v1alpha1.WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch event")
		panic("unreachable")
	}
}

func expectClosed(t *testing.T, ch <-chan v1alpha1.WatchEvent) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("expected closed channel, got event %s %s", ev.Type, ev.Object)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch channel to close")
	}
}

func TestWatchReplayThenLive(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))
	mustCreate(t, s, "Widget", obj("b", "", ""))

	events, cancel, err := s.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	// Replay of history before the watch started.
	for _, want := range []string{"a", "b"} {
		ev := recvEvent(t, events)
		if ev.Type != v1alpha1.WatchAdded || metaOf(t, ev.Object)["name"] != want {
			t.Fatalf("replay = %s %v, want ADDED %s", ev.Type, metaOf(t, ev.Object)["name"], want)
		}
	}

	// Live events after.
	if _, err := s.Update("Widget", "a", obj("a", `{"n":2}`, ""), 0); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if ev := recvEvent(t, events); ev.Type != v1alpha1.WatchModified {
		t.Errorf("live event = %s, want MODIFIED", ev.Type)
	}
	if err := s.Delete("Widget", "b", 0); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ev := recvEvent(t, events)
	if ev.Type != v1alpha1.WatchDeleted || metaOf(t, ev.Object)["name"] != "b" {
		t.Errorf("live event = %s %v, want DELETED b", ev.Type, metaOf(t, ev.Object)["name"])
	}
	// The DELETED body carries the deletion's rv, not the last write's.
	if rvOf(t, ev.Object) != 4 {
		t.Errorf("DELETED body rv = %d, want 4", rvOf(t, ev.Object))
	}
}

func TestWatchFromListRV(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("a", "", ""))
	mustCreate(t, s, "Widget", obj("b", "", ""))

	// The reflector contract: list at RV, watch from RV, see only what
	// happened after the snapshot.
	_, listRV, err := s.List("Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	events, cancel, err := s.Watch("Widget", listRV)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	mustCreate(t, s, "Widget", obj("c", "", ""))
	ev := recvEvent(t, events)
	if ev.Type != v1alpha1.WatchAdded || metaOf(t, ev.Object)["name"] != "c" {
		t.Errorf("event = %s %v, want ADDED c only", ev.Type, metaOf(t, ev.Object)["name"])
	}
	select {
	case extra := <-events:
		t.Errorf("unexpected extra event: %s %s", extra.Type, extra.Object)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchKindFilter(t *testing.T) {
	s := openTest(t, nil)
	events, cancel, err := s.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	mustCreate(t, s, "Gadget", obj("noise", "", ""))
	mustCreate(t, s, "Widget", obj("signal", "", ""))

	ev := recvEvent(t, events)
	if metaOf(t, ev.Object)["name"] != "signal" {
		t.Errorf("kind filter leaked: got %v", metaOf(t, ev.Object)["name"])
	}
}

func TestWatchCancelClosesChannel(t *testing.T) {
	s := openTest(t, nil)
	events, cancel, err := s.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	expectClosed(t, events)

	// Cancel is idempotent and writes after cancel don't panic.
	cancel()
	mustCreate(t, s, "Widget", obj("a", "", ""))
}

func TestWatchCloseStoreClosesChannel(t *testing.T) {
	s := openTest(t, nil)
	events, _, err := s.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	expectClosed(t, events)
}

// TestWatchSlowConsumerEvicted is the cacher rule: a consumer that stops
// draining is evicted with a terminal ERROR event, and writers are never
// blocked by it.
func TestWatchSlowConsumerEvicted(t *testing.T) {
	s := openTest(t, nil)
	events, cancel, err := s.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	// Nobody reads `events`. Write far past the buffer; every write must
	// complete promptly (a blocked writer would hang the test).
	const writes = watcherBufferSize + 50
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range writes {
			mustCreate(t, s, "Widget", obj(fmt.Sprintf("w-%03d", i), "", ""))
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("writers blocked by a slow watcher")
	}

	// Now drain: some prefix of normal events, then exactly one ERROR, then
	// closed. No gaps in the prefix.
	var normal int
	var sawError bool
	for {
		ev, ok := <-events
		if !ok {
			break
		}
		if ev.Type == v1alpha1.WatchError {
			sawError = true
			continue
		}
		if sawError {
			t.Fatalf("event after terminal ERROR: %s %s", ev.Type, ev.Object)
		}
		if want := fmt.Sprintf("w-%03d", normal); metaOf(t, ev.Object)["name"] != want {
			t.Fatalf("gap in delivered prefix: got %v, want %s", metaOf(t, ev.Object)["name"], want)
		}
		normal++
	}
	if !sawError {
		t.Error("no terminal ERROR event delivered")
	}
	if normal == 0 || normal > watcherBufferSize+1 {
		t.Errorf("delivered %d events before ERROR, want 1..%d", normal, watcherBufferSize+1)
	}
}

// TestWatchOrderingUnderConcurrentWriters drains a watch while writers race,
// asserting the stream is gap-free and in resourceVersion order.
func TestWatchOrderingUnderConcurrentWriters(t *testing.T) {
	s := openTest(t, nil)
	events, cancel, err := s.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	const writers, perWriter = 4, 50
	seen := make(map[string]bool)
	var lastRV int64
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for range writers * perWriter {
			ev := recvEvent(t, events)
			if ev.Type != v1alpha1.WatchAdded {
				t.Errorf("event type = %s, want ADDED", ev.Type)
				return
			}
			rv := rvOf(t, ev.Object)
			if rv <= lastRV {
				t.Errorf("out-of-order rv: %d after %d", rv, lastRV)
				return
			}
			lastRV = rv
			seen[metaOf(t, ev.Object)["name"].(string)] = true
		}
	}()

	var wg sync.WaitGroup
	for wr := range writers {
		wg.Go(func() {
			for i := range perWriter {
				mustCreate(t, s, "Widget", obj("w"+strconv.Itoa(wr)+"-"+strconv.Itoa(i), "", ""))
			}
		})
	}
	wg.Wait()

	select {
	case <-consumed:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out consuming events")
	}
	if len(seen) != writers*perWriter {
		t.Errorf("saw %d distinct objects, want %d", len(seen), writers*perWriter)
	}
}
