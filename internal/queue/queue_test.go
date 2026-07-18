// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Test cases forked from k8s.io/client-go/util/workqueue/queue_test.go
// (Copyright The Kubernetes Authors, Apache-2.0).

import (
	"fmt"
	"sync"
	"testing"
)

func TestBasic(t *testing.T) {
	const producers, consumers, itemsPerProducer = 10, 10, 100

	q := New()

	var producerWG sync.WaitGroup
	for i := range producers {
		producerWG.Go(func() {
			for j := range itemsPerProducer {
				q.Add(fmt.Sprintf("daemon/p%d-i%d", i, j))
			}
		})
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var consumerWG sync.WaitGroup
	for range consumers {
		consumerWG.Go(func() {
			for {
				key, shutdown := q.Get()
				if shutdown {
					return
				}
				mu.Lock()
				seen[key]++
				mu.Unlock()
				q.Done(key)
			}
		})
	}

	producerWG.Wait()
	q.ShutDown()
	consumerWG.Wait()

	if len(seen) != producers*itemsPerProducer {
		t.Errorf("processed %d distinct keys, want %d", len(seen), producers*itemsPerProducer)
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("key %q processed %d times, want 1", key, count)
		}
	}
}

// TestAddWhileProcessing is the load-bearing dedup property: a key
// re-added (any number of times) while it is being processed runs exactly
// once more, after Done.
func TestAddWhileProcessing(t *testing.T) {
	q := New()

	q.Add("daemon/web")
	key, shutdown := q.Get()
	if shutdown || key != "daemon/web" {
		t.Fatalf("Get() = (%q, %v), want (daemon/web, false)", key, shutdown)
	}

	// Re-add while processing: goes to dirty, not the queue.
	q.Add("daemon/web")
	q.Add("daemon/web")
	q.Add("daemon/web")
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d while key is processing, want 0", n)
	}

	// Done requeues the dirtied key exactly once.
	q.Done(key)
	if n := q.Len(); n != 1 {
		t.Fatalf("Len() after Done = %d, want 1", n)
	}

	key, _ = q.Get()
	q.Done(key)
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() after final Done = %d, want 0", n)
	}
}

func TestAddDedup(t *testing.T) {
	q := New()
	q.Add("proc/a")
	q.Add("proc/a")
	q.Add("proc/b")
	if n := q.Len(); n != 2 {
		t.Fatalf("Len() = %d, want 2 (duplicate Add must be absorbed)", n)
	}
}

func TestGetOrder(t *testing.T) {
	q := New()
	q.Add("proc/a")
	q.Add("proc/b")
	q.Add("proc/c")
	for _, want := range []string{"proc/a", "proc/b", "proc/c"} {
		key, shutdown := q.Get()
		if shutdown || key != want {
			t.Fatalf("Get() = (%q, %v), want (%q, false)", key, shutdown, want)
		}
		q.Done(key)
	}
}

func TestShutDownDrains(t *testing.T) {
	q := New()
	q.Add("proc/a")
	q.Add("proc/b")
	q.ShutDown()

	if !q.ShuttingDown() {
		t.Fatal("ShuttingDown() = false after ShutDown")
	}

	// Queued keys drain before Get reports shutdown.
	for _, want := range []string{"proc/a", "proc/b"} {
		key, shutdown := q.Get()
		if shutdown || key != want {
			t.Fatalf("Get() = (%q, %v), want (%q, false)", key, shutdown, want)
		}
		q.Done(key)
	}
	if _, shutdown := q.Get(); !shutdown {
		t.Fatal("Get() on drained shut-down queue must report shutdown")
	}
}

func TestAddAfterShutDownIgnored(t *testing.T) {
	q := New()
	q.ShutDown()
	q.Add("proc/a")
	if n := q.Len(); n != 0 {
		t.Fatalf("Len() = %d, want 0 (Add after ShutDown must be ignored)", n)
	}
}
