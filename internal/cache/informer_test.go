// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
)

// fakeLW is a scripted ListWatcher: tests mutate its object set, inject
// one-shot errors, and push watch events by hand.
type fakeLW struct {
	mu       sync.Mutex
	objects  map[string]json.RawMessage // name → object
	listRV   int
	listErr  error // returned (and cleared) by the next ListRaw
	watchErr error // returned (and cleared) by the next Watch
	stream   chan v1alpha1.WatchEvent
	lists    int
	watches  int
}

func newFakeLW(names ...string) *fakeLW {
	f := &fakeLW{objects: map[string]json.RawMessage{}, listRV: 1}
	for _, name := range names {
		f.objects[name] = obj(name, "v1")
	}
	return f
}

// obj builds a minimal raw object; marker makes versions distinguishable.
func obj(name, marker string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"kind":"Daemon","metadata":{"name":%q},"marker":%q}`, name, marker))
}

func (f *fakeLW) ListRaw(_ context.Context, _ string) (*v1alpha1.ObjectList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		err := f.listErr
		f.listErr = nil
		return nil, err
	}
	f.listRV++
	list := &v1alpha1.ObjectList{ResourceVersion: fmt.Sprint(f.listRV)}
	for _, o := range f.objects {
		list.Items = append(list.Items, bytes.Clone(o))
	}
	return list, nil
}

func (f *fakeLW) Watch(_ context.Context, _, _ string) (<-chan v1alpha1.WatchEvent, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchErr != nil {
		err := f.watchErr
		f.watchErr = nil
		return nil, nil, err
	}
	f.watches++
	f.stream = make(chan v1alpha1.WatchEvent, 100)
	return f.stream, func() {}, nil
}

func (f *fakeLW) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeLW) watchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watches
}

func (f *fakeLW) setObjects(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects = map[string]json.RawMessage{}
	for _, name := range names {
		f.objects[name] = obj(name, "v1")
	}
}

func (f *fakeLW) emit(typ v1alpha1.WatchEventType, o json.RawMessage) {
	f.mu.Lock()
	ch := f.stream
	f.mu.Unlock()
	ch <- v1alpha1.WatchEvent{Type: typ, Object: o}
}

func (f *fakeLW) endStream() {
	f.mu.Lock()
	close(f.stream)
	f.stream = nil
	f.mu.Unlock()
}

// startInformer runs an informer over lw and returns it plus a channel of
// handler firings.
func startInformer(t *testing.T, lw cache.ListWatcher, opts *cache.Options) (*cache.Informer, <-chan string) {
	t.Helper()
	keys := make(chan string, 1000)
	inf := cache.NewInformer(lw, "Daemon", func(key string) { keys <- key }, opts)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go inf.Run(ctx)
	return inf, keys
}

// collectKeys receives exactly n handler firings and returns them counted.
func collectKeys(t *testing.T, keys <-chan string, n int) map[string]int {
	t.Helper()
	got := map[string]int{}
	deadline := time.After(5 * time.Second)
	for range n {
		select {
		case k := <-keys:
			got[k]++
		case <-deadline:
			t.Fatalf("timed out collecting keys; have %v, want %d firings", got, n)
		}
	}
	return got
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInformerInitialListFiresAllKeys(t *testing.T) {
	lw := newFakeLW("a", "b")
	inf, keys := startInformer(t, lw, nil)

	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	got := collectKeys(t, keys, 2)
	if got["Daemon/a"] != 1 || got["Daemon/b"] != 1 {
		t.Errorf("initial firings = %v, want Daemon/a and Daemon/b once each", got)
	}

	o, ok := inf.Store().GetByKey("Daemon/a")
	if !ok {
		t.Fatal("Daemon/a missing from store after sync")
	}
	if !bytes.Contains(o, []byte(`"name":"a"`)) {
		t.Errorf("stored object = %s", o)
	}
	if n := len(inf.Store().List()); n != 2 {
		t.Errorf("store List() has %d items, want 2", n)
	}
}

func TestInformerWatchEventsUpdateStore(t *testing.T) {
	lw := newFakeLW("a")
	inf, keys := startInformer(t, lw, nil)
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	collectKeys(t, keys, 1)
	waitFor(t, "watch to be established", func() bool { return lw.watchCount() == 1 })

	// ADDED puts the object in the store and fires its key.
	lw.emit(v1alpha1.WatchAdded, obj("b", "v1"))
	if got := collectKeys(t, keys, 1); got["Daemon/b"] != 1 {
		t.Errorf("after ADDED, firings = %v, want Daemon/b", got)
	}
	if _, ok := inf.Store().GetByKey("Daemon/b"); !ok {
		t.Fatal("Daemon/b missing from store after ADDED")
	}

	// MODIFIED replaces the stored bytes.
	lw.emit(v1alpha1.WatchModified, obj("b", "v2"))
	collectKeys(t, keys, 1)
	waitFor(t, "store to hold v2", func() bool {
		o, _ := inf.Store().GetByKey("Daemon/b")
		return bytes.Contains(o, []byte(`"marker":"v2"`))
	})

	// DELETED drops it.
	lw.emit(v1alpha1.WatchDeleted, obj("b", "v2"))
	if got := collectKeys(t, keys, 1); got["Daemon/b"] != 1 {
		t.Errorf("after DELETED, firings = %v, want Daemon/b", got)
	}
	waitFor(t, "store to drop Daemon/b", func() bool {
		_, ok := inf.Store().GetByKey("Daemon/b")
		return !ok
	})
}

// TestInformerRelistOnStreamClose: a dying watch stream forces a relist,
// which fires every surviving key plus the keys that vanished meanwhile.
func TestInformerRelistOnStreamClose(t *testing.T) {
	lw := newFakeLW("a", "b")
	inf, keys := startInformer(t, lw, nil)
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	collectKeys(t, keys, 2)
	waitFor(t, "watch to be established", func() bool { return lw.watchCount() == 1 })

	// While the informer is "disconnected": b vanishes, d appears.
	lw.setObjects("a", "d")
	lw.endStream()

	got := collectKeys(t, keys, 3)
	for _, want := range []string{"Daemon/a", "Daemon/b", "Daemon/d"} {
		if got[want] != 1 {
			t.Errorf("after relist, firings = %v, want %s exactly once", got, want)
		}
	}
	if _, ok := inf.Store().GetByKey("Daemon/b"); ok {
		t.Error("Daemon/b still in store after relist dropped it")
	}
	if _, ok := inf.Store().GetByKey("Daemon/d"); !ok {
		t.Error("Daemon/d missing from store after relist")
	}
}

// TestInformerCompactedRelistsImmediately: ErrCompacted on watch start is
// normal operation and must relist with no backoff. The fake clock never
// ticks, so any backoff would hang the test.
func TestInformerCompactedRelistsImmediately(t *testing.T) {
	fc := clock.NewFake(time.Now())
	lw := newFakeLW("a")
	lw.watchErr = v1alpha1.ErrCompacted
	_, keys := startInformer(t, lw, &cache.Options{Clock: fc})

	waitFor(t, "second list after ErrCompacted", func() bool { return lw.listCount() == 2 })
	waitFor(t, "watch after relist", func() bool { return lw.watchCount() == 1 })
	// Two lists of the same object → two firings.
	collectKeys(t, keys, 2)
}

// TestInformerListErrorBacksOff: a failed list waits out the backoff timer
// before retrying.
func TestInformerListErrorBacksOff(t *testing.T) {
	fc := clock.NewFake(time.Now())
	lw := newFakeLW("a")
	lw.listErr = fmt.Errorf("boom")
	inf, _ := startInformer(t, lw, &cache.Options{Clock: fc})

	// Resync ticker + backoff timer = 2 waiters.
	waitFor(t, "backoff timer to be armed", func() bool { return fc.Waiters() == 2 })
	if n := lw.listCount(); n != 1 {
		t.Fatalf("list retried without backoff: %d lists", n)
	}
	if inf.HasSynced() {
		t.Fatal("HasSynced() = true before any successful list")
	}

	fc.Step(2 * time.Second)
	waitFor(t, "list retry after backoff", func() bool { return lw.listCount() == 2 })
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
}

func TestInformerResyncFiresAllKeys(t *testing.T) {
	fc := clock.NewFake(time.Now())
	lw := newFakeLW("a", "b")
	inf, keys := startInformer(t, lw, &cache.Options{Clock: fc, ResyncEvery: time.Minute})
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	collectKeys(t, keys, 2)
	waitFor(t, "watch to be established", func() bool { return lw.watchCount() == 1 })

	fc.Step(time.Minute)
	got := collectKeys(t, keys, 2)
	if got["Daemon/a"] != 1 || got["Daemon/b"] != 1 {
		t.Errorf("resync firings = %v, want Daemon/a and Daemon/b once each", got)
	}
}

// TestStoreReturnsCopies: mutating what the store hands out must not
// corrupt the store.
func TestStoreReturnsCopies(t *testing.T) {
	lw := newFakeLW("a")
	inf, keys := startInformer(t, lw, nil)
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	collectKeys(t, keys, 1)

	o, _ := inf.Store().GetByKey("Daemon/a")
	for i := range o {
		o[i] = 'X'
	}
	again, _ := inf.Store().GetByKey("Daemon/a")
	if !bytes.Contains(again, []byte(`"name":"a"`)) {
		t.Errorf("store content corrupted by caller mutation: %s", again)
	}

	list := inf.Store().List()
	for i := range list[0] {
		list[0][i] = 'X'
	}
	again, _ = inf.Store().GetByKey("Daemon/a")
	if !bytes.Contains(again, []byte(`"name":"a"`)) {
		t.Errorf("store content corrupted via List mutation: %s", again)
	}
}
