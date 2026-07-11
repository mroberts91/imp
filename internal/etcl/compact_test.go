// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package etcl

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestCompaction(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
	s := openTest(t, &Options{
		RetainRows:   5,
		RetainWindow: time.Minute,
		CompactEvery: 10_000, // manual only
		Now:          clock.Now,
	})

	// 20 old rows (rv 1..20), then 3 recent ones (rv 21..23).
	for i := range 20 {
		mustCreate(t, s, "Widget", obj(fmt.Sprintf("old-%02d", i), "", ""))
	}
	clock.Advance(2 * time.Minute)
	for i := range 3 {
		mustCreate(t, s, "Widget", obj(fmt.Sprintf("new-%d", i), "", ""))
	}

	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Count rule keeps rv 19..23 (last 5); age rule keeps rv 21..23. The
	// union keeps rv 19..23, so the floor is 18.
	if _, _, err := s.Watch("Widget", 17); !errors.Is(err, v1alpha1.ErrCompacted) {
		t.Errorf("watch below floor: err = %v, want ErrCompacted", err)
	}

	// From the floor itself, everything retained must replay gap-free.
	events, cancel, err := s.Watch("Widget", 18)
	if err != nil {
		t.Fatalf("watch from floor: %v", err)
	}
	defer cancel()
	for _, wantRV := range []int64{19, 20, 21, 22, 23} {
		ev := recvEvent(t, events)
		if rvOf(t, ev.Object) != wantRV {
			t.Errorf("replay rv = %d, want %d", rvOf(t, ev.Object), wantRV)
		}
	}

	// Objects are untouched by compaction; only watch history shrinks.
	objs, listRV, err := s.List("Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 23 || listRV != 23 {
		t.Errorf("List = %d objects at rv %d, want 23 at 23", len(objs), listRV)
	}
}

func TestCompactionNeverExceedsRetention(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
	s := openTest(t, &Options{
		RetainRows:   5,
		RetainWindow: time.Minute,
		CompactEvery: 10_000,
		Now:          clock.Now,
	})

	// All rows recent: age rule protects everything despite RetainRows=5.
	for i := range 20 {
		mustCreate(t, s, "Widget", obj(fmt.Sprintf("w-%02d", i), "", ""))
	}
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if _, _, err := s.Watch("Widget", 0); err != nil {
		t.Errorf("watch from 0 after no-op compaction: %v", err)
	}

	// All rows stale: count rule still keeps the last 5 (floor 15).
	clock.Advance(time.Hour)
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if _, _, err := s.Watch("Widget", 14); !errors.Is(err, v1alpha1.ErrCompacted) {
		t.Errorf("watch below floor: err = %v, want ErrCompacted", err)
	}
	if _, _, err := s.Watch("Widget", 15); err != nil {
		t.Errorf("watch from floor: %v", err)
	}

	// Fewer rows than RetainRows is never compactable.
	s2 := openTest(t, &Options{RetainRows: 100, RetainWindow: time.Nanosecond, CompactEvery: 10_000})
	mustCreate(t, s2, "Widget", obj("only", "", ""))
	if err := s2.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if _, _, err := s2.Watch("Widget", 0); err != nil {
		t.Errorf("small changelog compacted: %v", err)
	}
}

func TestCompactionRunsAutomatically(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
	s := openTest(t, &Options{
		RetainRows:   5,
		RetainWindow: time.Minute,
		CompactEvery: 10,
		Now:          clock.Now,
	})
	for i := range 10 {
		mustCreate(t, s, "Widget", obj(fmt.Sprintf("w-%02d", i), "", ""))
	}
	clock.Advance(time.Hour)
	// Writes 11..20: the 20th triggers the second automatic compaction,
	// with rows 1..10 now stale.
	for i := 10; i < 20; i++ {
		mustCreate(t, s, "Widget", obj(fmt.Sprintf("w-%02d", i), "", ""))
	}
	if _, _, err := s.Watch("Widget", 0); !errors.Is(err, v1alpha1.ErrCompacted) {
		t.Errorf("automatic compaction did not run: err = %v, want ErrCompacted", err)
	}
}

func TestCompactedRVPersistsAcrossReopen(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "etcl.db")
	s := openTestAt(t, path, &Options{
		RetainRows:   5,
		RetainWindow: time.Minute,
		CompactEvery: 10_000,
		Now:          clock.Now,
	})
	for i := range 20 {
		mustCreate(t, s, "Widget", obj(fmt.Sprintf("w-%02d", i), "", ""))
	}
	clock.Advance(time.Hour)
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openTestAt(t, path, nil)
	if _, _, err := s2.Watch("Widget", 0); !errors.Is(err, v1alpha1.ErrCompacted) {
		t.Errorf("compaction floor lost across reopen: err = %v, want ErrCompacted", err)
	}
	if _, _, err := s2.Watch("Widget", 15); err != nil {
		t.Errorf("watch from persisted floor: %v", err)
	}
}
