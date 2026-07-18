// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cache

// Store is a miniature of client-go's tools/cache/thread_safe_store.go
// (Copyright The Kubernetes Authors, Apache-2.0) minus the indexing
// machinery: exact-key and list-all are the only queries imp's controllers
// need; anything fancier is a linear scan over dozens of objects.

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Store is the informer's local mirror of one kind's objects, keyed
// "kind/name". Readers always get copies: the caller may not mutate what
// the store returns, and the store never hands out its own backing bytes.
//
// Objects are raw JSON, same as the wire: controllers unmarshal at
// reconcile time, which is itself the deep copy.
type Store struct {
	mu    sync.RWMutex
	items map[string]json.RawMessage
}

func newStore() *Store {
	return &Store{items: map[string]json.RawMessage{}}
}

// GetByKey returns a copy of the object for key, and whether it exists.
func (s *Store) GetByKey(key string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.items[key]
	if !ok {
		return nil, false
	}
	return bytes.Clone(obj), true
}

// List returns copies of every object in the store, in no particular
// order.
func (s *Store) List() []json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]json.RawMessage, 0, len(s.items))
	for _, obj := range s.items {
		out = append(out, bytes.Clone(obj))
	}
	return out
}

// Keys returns every key in the store, in no particular order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.items))
	for key := range s.items {
		out = append(out, key)
	}
	return out
}

// replace swaps the entire contents for items (which the store takes
// ownership of) and returns the keys that vanished, so the informer can
// fire deletion hints after a relist.
func (s *Store) replace(items map[string]json.RawMessage) (removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.items {
		if _, still := items[key]; !still {
			removed = append(removed, key)
		}
	}
	s.items = items
	return removed
}

func (s *Store) set(key string, obj json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = obj
}

func (s *Store) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
}
