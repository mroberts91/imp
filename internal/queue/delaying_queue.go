// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package queue

// Fork of k8s.io/client-go/util/workqueue/delaying_queue.go (Copyright The
// Kubernetes Authors, Apache-2.0): AddAfter feeds a single goroutine that
// keeps pending keys in a readyAt-ordered heap and re-arms one timer for
// the earliest entry. Deviations from the source: no metrics, no logger,
// and no HandleCrash recover in the loop - a panic there kills impd on
// purpose (crash-only policy) rather than leaving delayed requeues
// silently dead.

import (
	"container/heap"
	"sync"
	"time"

	"github.com/mroberts91/imp/internal/clock"
)

// DelayingInterface is an Interface that can Add a key at a later time.
// This makes it easier to requeue keys after failures without hot-looping.
type DelayingInterface interface {
	Interface
	// AddAfter adds a key to the queue after the given duration. The
	// earliest wins when the same key is delayed twice.
	AddAfter(key string, d time.Duration)
}

// NewDelaying constructs a DelayingInterface on the real clock.
func NewDelaying() DelayingInterface {
	return NewDelayingWithClock(clock.Real{})
}

// NewDelayingWithClock constructs a DelayingInterface with an injected
// clock, for tests.
func NewDelayingWithClock(c clock.Clock) DelayingInterface {
	q := &delayingType{
		Interface:       New(),
		clock:           c,
		heartbeat:       c.NewTicker(maxWait),
		stopCh:          make(chan struct{}),
		waitingForAddCh: make(chan *waitFor, 1000),
	}
	go q.waitingLoop()
	return q
}

// delayingType wraps an Interface and provides delayed re-enquing.
type delayingType struct {
	Interface

	// clock tracks time for delayed firing
	clock clock.Clock

	// stopCh signals shutdown to the waiting loop
	stopCh chan struct{}
	// stopOnce guarantees we only signal shutdown a single time
	stopOnce sync.Once

	// heartbeat ensures we wait no more than maxWait before firing
	heartbeat clock.Ticker

	// waitingForAddCh feeds waitingLoop
	waitingForAddCh chan *waitFor
}

// waitFor holds a key and the time it should be added.
type waitFor struct {
	key     string
	readyAt time.Time
	// index in the priority queue (heap)
	index int
}

// waitForPriorityQueue implements heap.Interface over waitFor items; the
// entry with the smallest readyAt is at the root.
type waitForPriorityQueue []*waitFor

func (pq waitForPriorityQueue) Len() int { return len(pq) }
func (pq waitForPriorityQueue) Less(i, j int) bool {
	return pq[i].readyAt.Before(pq[j].readyAt)
}
func (pq waitForPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push should only be called through heap.Push.
func (pq *waitForPriorityQueue) Push(x any) {
	item := x.(*waitFor)
	item.index = len(*pq)
	*pq = append(*pq, item)
}

// Pop should only be called through heap.Pop.
func (pq *waitForPriorityQueue) Pop() any {
	n := len(*pq)
	item := (*pq)[n-1]
	item.index = -1
	(*pq)[n-1] = nil
	*pq = (*pq)[:n-1]
	return item
}

// Peek returns the earliest entry without mutating the queue. Safe to call
// directly.
func (pq waitForPriorityQueue) Peek() *waitFor { return pq[0] }

// ShutDown stops the queue; after the drained items are processed, Get
// reports shutdown. Safe to call more than once.
func (q *delayingType) ShutDown() {
	q.stopOnce.Do(func() {
		q.Interface.ShutDown()
		close(q.stopCh)
		q.heartbeat.Stop()
	})
}

// AddAfter adds the given key to the work queue after the given delay.
func (q *delayingType) AddAfter(key string, d time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	// immediately add things with no delay
	if d <= 0 {
		q.Add(key)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
	case q.waitingForAddCh <- &waitFor{key: key, readyAt: q.clock.Now().Add(d)}:
	}
}

// maxWait keeps a max bound on the wait time; it's insurance against weird
// things happening. An expired entry never sits for more than 10s.
const maxWait = 10 * time.Second

// waitingLoop runs until the workqueue is shut down and keeps a check on
// the list of keys to be added.
func (q *delayingType) waitingLoop() {
	// placeholder channel used when there is nothing to wait for
	never := make(<-chan time.Time)

	// timer for the readyAt of the heap's earliest entry
	var nextReadyAtTimer clock.Timer

	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue)

	waitingEntryByKey := map[string]*waitFor{}

	for {
		if q.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek()
			if entry.readyAt.After(now) {
				break
			}

			entry = heap.Pop(waitingForQueue).(*waitFor)
			q.Add(entry.key)
			delete(waitingEntryByKey, entry.key)
		}

		// Set up a wait for the first entry's readyAt (if one exists)
		nextReadyAt := never
		if waitingForQueue.Len() > 0 {
			if nextReadyAtTimer != nil {
				nextReadyAtTimer.Stop()
			}
			entry := waitingForQueue.Peek()
			nextReadyAtTimer = q.clock.NewTimer(entry.readyAt.Sub(now))
			nextReadyAt = nextReadyAtTimer.C()
		}

		select {
		case <-q.stopCh:
			return

		case <-q.heartbeat.C():
			// continue the loop, which will add ready entries

		case <-nextReadyAt:
			// continue the loop, which will add ready entries

		case waitEntry := <-q.waitingForAddCh:
			if waitEntry.readyAt.After(q.clock.Now()) {
				insert(waitingForQueue, waitingEntryByKey, waitEntry)
			} else {
				q.Add(waitEntry.key)
			}

			drained := false
			for !drained {
				select {
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(q.clock.Now()) {
						insert(waitingForQueue, waitingEntryByKey, waitEntry)
					} else {
						q.Add(waitEntry.key)
					}
				default:
					drained = true
				}
			}
		}
	}
}

// insert adds the entry to the priority queue, or, if the key is already
// pending, keeps whichever readyAt is sooner.
func insert(q *waitForPriorityQueue, knownEntries map[string]*waitFor, entry *waitFor) {
	existing, exists := knownEntries[entry.key]
	if exists {
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			heap.Fix(q, existing.index)
		}
		return
	}

	heap.Push(q, entry)
	knownEntries[entry.key] = entry
}
