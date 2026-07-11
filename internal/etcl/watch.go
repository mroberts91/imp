// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package etcl

// Watch fan-out, forked in from the k8s cacher
// (staging/src/k8s.io/apiserver/pkg/storage/cacher/, Copyright The
// Kubernetes Authors, Apache-2.0)

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/mroberts91/imp/api/v1alpha1"
)

const watcherBufferSize = 100

type changeEvent struct {
	rv    int64
	kind  string
	event v1alpha1.WatchEvent
}

type watcher struct {
	id       int64
	kind     string
	in       chan changeEvent
	out      chan v1alpha1.WatchEvent
	stop     chan struct{}
	overflow sync.Once
	evicted  chan struct{}
	stopOnce sync.Once
}

func (w *watcher) signalStop() {
	w.stopOnce.Do(func() { close(w.stop) })
}

func (s *Store) Watch(kind string, sinceRV int64) (<-chan v1alpha1.WatchEvent, func(), error) {
	if kind == "" {
		return nil, nil, errors.New("etcl: watch requires a kind")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errClosed
	}
	if sinceRV < s.compactedRV {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("etcl: watch %s from resourceVersion %d, compacted through %d: %w",
			kind, sinceRV, s.compactedRV, v1alpha1.ErrCompacted)
	}
	s.nextWatcherID++
	w := &watcher{
		id:      s.nextWatcherID,
		kind:    kind,
		in:      make(chan changeEvent, watcherBufferSize),
		out:     make(chan v1alpha1.WatchEvent),
		stop:    make(chan struct{}),
		evicted: make(chan struct{}),
	}
	// startRV splits replay from live: replays (sinceRV, startRV]
	// from the changelog, and every live event has rv > startRV because the
	// watcher is registered before this section ends.
	startRV := s.rv
	s.watchers[w.id] = w
	s.pumps.Add(1)
	s.mu.Unlock()

	go s.runWatcher(w, sinceRV, startRV)

	cancel := func() {
		w.signalStop()
		s.removeWatcher(w.id)
	}
	return w.out, cancel, nil
}

func (s *Store) removeWatcher(id int64) {
	s.mu.Lock()
	delete(s.watchers, id)
	s.mu.Unlock()
}

// dispatchLocked delivers a committed change to every watcher of its kind.
func (s *Store) dispatchLocked(ce changeEvent) {
	for id, w := range s.watchers {
		if w.kind != ce.kind {
			continue
		}
		select {
		case w.in <- ce:
		default:
			delete(s.watchers, id)
			w.overflow.Do(func() { close(w.evicted) })
		}
	}
}

func (s *Store) runWatcher(w *watcher, sinceRV, startRV int64) {
	defer s.pumps.Done()
	defer close(w.out)
	defer s.removeWatcher(w.id)

	replay, err := s.replayEvents(w.kind, sinceRV, startRV)
	if err != nil {
		w.deliver(errorEvent(err.Error()))
		return
	}
	for _, ev := range replay {
		if !w.deliver(ev) {
			return
		}
	}
	for {
		select {
		case <-w.stop:
			return
		case ce := <-w.in:
			if !w.deliver(ce.event) {
				return
			}
		case <-w.evicted:
			// The dispatcher stopped sending. Forward what is already
			// buffered, then tell the client to relist.
			for {
				select {
				case ce := <-w.in:
					if !w.deliver(ce.event) {
						return
					}
				default:
					w.deliver(errorEvent("watch buffer overflowed; relist required"))
					return
				}
			}
		}
	}
}

func (s *Store) replayEvents(kind string, sinceRV, startRV int64) ([]v1alpha1.WatchEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errClosed
	}
	if sinceRV < s.compactedRV {
		return nil, fmt.Errorf("resourceVersion %d has been compacted (floor %d); relist required", sinceRV, s.compactedRV)
	}
	rows, err := s.db.Query(
		`SELECT event_type, body FROM changelog WHERE kind = ? AND rv > ? AND rv <= ? ORDER BY rv`,
		kind, sinceRV, startRV)
	if err != nil {
		return nil, fmt.Errorf("replaying changelog: %w", err)
	}
	defer rows.Close()
	var events []v1alpha1.WatchEvent
	for rows.Next() {
		var eventType string
		var body []byte
		if err := rows.Scan(&eventType, &body); err != nil {
			return nil, fmt.Errorf("replaying changelog: %w", err)
		}
		events = append(events, v1alpha1.WatchEvent{Type: v1alpha1.WatchEventType(eventType), Object: body})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replaying changelog: %w", err)
	}
	return events, nil
}

func (w *watcher) deliver(ev v1alpha1.WatchEvent) bool {
	select {
	case w.out <- ev:
		return true
	case <-w.stop:
		return false
	}
}

func errorEvent(message string) v1alpha1.WatchEvent {
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil { // can't happen for map[string]string
		body = []byte(`{"message":"watch error"}`)
	}
	return v1alpha1.WatchEvent{Type: v1alpha1.WatchError, Object: body}
}
