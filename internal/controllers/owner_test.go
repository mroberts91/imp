// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// mapStore is a mutable KeyGetter for tests (the real *cache.Store is
// read-only outside the informer).
type mapStore struct {
	mu    sync.Mutex
	items map[string]json.RawMessage
}

func newMapStore() *mapStore {
	return &mapStore{items: map[string]json.RawMessage{}}
}

func (s *mapStore) GetByKey(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.items[key]
	return obj, ok
}

func (s *mapStore) set(key string, obj json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = obj
}

func (s *mapStore) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, key)
}

// recordingQueue implements queue.Interface and records Adds.
type recordingQueue struct {
	mu    sync.Mutex
	added []string
}

func (q *recordingQueue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.added = append(q.added, key)
}

func (q *recordingQueue) take() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.added
	q.added = nil
	return out
}

func (q *recordingQueue) Len() int            { q.mu.Lock(); defer q.mu.Unlock(); return len(q.added) }
func (q *recordingQueue) Get() (string, bool) { return "", true }
func (q *recordingQueue) Done(string)         {}
func (q *recordingQueue) ShutDown()           {}
func (q *recordingQueue) ShuttingDown() bool  { return false }

// procJSON builds a child object with the given ownerReferences.
func procJSON(t *testing.T, name string, owners []v1alpha1.OwnerReference) json.RawMessage {
	t.Helper()
	obj := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       v1alpha1.KindProc,
		"metadata": map[string]any{
			"name":            name,
			"ownerReferences": owners,
		},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal child: %v", err)
	}
	return raw
}

func TestEnqueueOwner(t *testing.T) {
	tests := []struct {
		name  string
		child json.RawMessage
		want  []string
	}{
		{
			name: "matching owner",
			child: procJSON(t, "web-0", []v1alpha1.OwnerReference{
				{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindDaemon, Name: "web", UID: "u1"},
			}),
			want: []string{"Daemon/web"},
		},
		{
			name: "multiple matching owners",
			child: procJSON(t, "web-0", []v1alpha1.OwnerReference{
				{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindDaemon, Name: "web", UID: "u1"},
				{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindDaemon, Name: "api", UID: "u2"},
			}),
			want: []string{"Daemon/web", "Daemon/api"},
		},
		{
			name: "future version of the group still matches",
			child: procJSON(t, "web-0", []v1alpha1.OwnerReference{
				{APIVersion: "impd.sh/v2", Kind: v1alpha1.KindDaemon, Name: "web", UID: "u1"},
			}),
			want: []string{"Daemon/web"},
		},
		{
			name: "different kind",
			child: procJSON(t, "web-0", []v1alpha1.OwnerReference{
				{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindEvent, Name: "web", UID: "u1"},
			}),
			want: nil,
		},
		{
			name: "different API group",
			child: procJSON(t, "web-0", []v1alpha1.OwnerReference{
				{APIVersion: "apps/v1", Kind: v1alpha1.KindDaemon, Name: "web", UID: "u1"},
			}),
			want: nil,
		},
		{
			name: "group-less apiVersion",
			child: procJSON(t, "web-0", []v1alpha1.OwnerReference{
				{APIVersion: "v1", Kind: v1alpha1.KindDaemon, Name: "web", UID: "u1"},
			}),
			want: nil,
		},
		{
			name:  "no owner references",
			child: procJSON(t, "web-0", nil),
			want:  nil,
		},
		{
			name:  "malformed JSON",
			child: json.RawMessage(`{"metadata": nope`),
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMapStore()
			q := &recordingQueue{}
			handler := EnqueueOwner(store, v1alpha1.KindDaemon, q)

			key := v1alpha1.KindProc + "/web-0"
			store.set(key, tt.child)
			handler(key)

			if got := q.take(); !slices.Equal(got, tt.want) {
				t.Errorf("enqueued %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnqueueOwnerDeletion(t *testing.T) {
	store := newMapStore()
	q := &recordingQueue{}
	handler := EnqueueOwner(store, v1alpha1.KindDaemon, q)

	key := v1alpha1.KindProc + "/web-0"
	store.set(key, procJSON(t, "web-0", []v1alpha1.OwnerReference{
		{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindDaemon, Name: "web", UID: "u1"},
	}))

	// Live child: owner enqueued and the mapping remembered.
	handler(key)
	if got, want := q.take(), []string{"Daemon/web"}; !slices.Equal(got, want) {
		t.Fatalf("live child enqueued %v, want %v", got, want)
	}

	// Deletion: the store entry is gone before the handler fires; the
	// remembered owner must still be woken.
	store.delete(key)
	handler(key)
	if got, want := q.take(), []string{"Daemon/web"}; !slices.Equal(got, want) {
		t.Fatalf("deleted child enqueued %v, want %v (from remembered mapping)", got, want)
	}

	// The mapping was consumed: a further firing enqueues nothing.
	handler(key)
	if got := q.take(); len(got) != 0 {
		t.Fatalf("third firing enqueued %v, want nothing", got)
	}
}

func TestEnqueueOwnerDeletionOfUnknownChild(t *testing.T) {
	store := newMapStore()
	q := &recordingQueue{}
	handler := EnqueueOwner(store, v1alpha1.KindDaemon, q)

	// A deletion hint for a child never seen alive enqueues nothing.
	handler(v1alpha1.KindProc + "/ghost")
	if got := q.take(); len(got) != 0 {
		t.Fatalf("unknown child enqueued %v, want nothing", got)
	}
}
