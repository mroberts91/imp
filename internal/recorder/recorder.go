// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package recorder emits Events with aggregation and spam filtering.
//
// Logical forks (Copyright The Kubernetes Authors, Apache-2.0; see
// LICENSES/kubernetes/): the Eventf interface shape from
// staging/src/k8s.io/client-go/tools/record/event.go, and the correlator/
// spam-filter idea from events_cache.go — simplified for single-host scale:
// dedup key = (regarding.uid, type, reason, message); LRU (~4096, TTL ~10m)
// maps key → Event name; hit ⇒ CAS count++ + lastTimestamp; miss ⇒ Create.
// Per-component token bucket (~1/sec sustained, burst 25). Failures are
// slog'd and never returned — event emission must never fail a reconcile.
package recorder

import (
	"container/list"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/pkg/client"
)

const (
	defaultLRUSize   = 4096
	defaultKeyTTL    = 10 * time.Minute
	defaultSpamQPS   = 1.0
	defaultSpamBurst = 25
)

// EventClient is the Event write path the recorder needs. *client.Client
// satisfies it.
type EventClient interface {
	GetEvent(ctx context.Context, name string) (*v1alpha1.Event, error)
	ApplyEvent(ctx context.Context, e *v1alpha1.Event) (*v1alpha1.Event, error)
}

// Recorder emits aggregated Events for one reportingComponent.
type Recorder struct {
	client    EventClient
	component string
	clock     clock.Clock
	log       *slog.Logger

	mu         sync.Mutex
	ll         *list.List // front = most recently used
	cache      map[string]*list.Element
	maxEntries int
	keyTTL     time.Duration

	tokens     float64
	lastRefill time.Time
	spamQPS    float64
	spamBurst  float64
}

type cacheEntry struct {
	key       string
	eventName string
	expires   time.Time
}

// New builds a Recorder that stamps reportingComponent on every Event.
// A nil clk means the real clock.
func New(cl EventClient, reportingComponent string, clk clock.Clock) *Recorder {
	if clk == nil {
		clk = clock.Real{}
	}
	now := clk.Now()
	return &Recorder{
		client:     cl,
		component:  reportingComponent,
		clock:      clk,
		log:        slog.With("component", "recorder", "reportingComponent", reportingComponent),
		ll:         list.New(),
		cache:      make(map[string]*list.Element),
		maxEntries: defaultLRUSize,
		keyTTL:     defaultKeyTTL,
		tokens:     defaultSpamBurst,
		lastRefill: now,
		spamQPS:    defaultSpamQPS,
		spamBurst:  defaultSpamBurst,
	}
}

// Eventf records an Event about regarding. Best-effort: errors and spam
// drops are logged and swallowed.
func (r *Recorder) Eventf(ctx context.Context, regarding v1alpha1.ObjectRef, typ v1alpha1.EventType, reason, messageFmt string, args ...any) {
	message := fmt.Sprintf(messageFmt, args...)
	if !r.allow() {
		r.log.Debug("dropping event (spam filter)",
			"kind", regarding.Kind, "name", regarding.Name, "reason", reason)
		return
	}

	key := dedupKey(regarding, typ, reason, message)
	now := r.clock.Now()

	if name, ok := r.lookup(key, now); ok {
		r.bump(ctx, name, now)
		return
	}

	name := eventName(regarding.Name, key)
	ev := &v1alpha1.Event{
		Metadata: v1alpha1.ObjectMeta{
			Name: name,
		},
		Regarding:          regarding,
		Type:               typ,
		Reason:             reason,
		Message:            message,
		Count:              1,
		FirstTimestamp:     v1alpha1.NewTime(now),
		LastTimestamp:      v1alpha1.NewTime(now),
		ReportingComponent: r.component,
	}
	if _, err := r.client.ApplyEvent(ctx, ev); err != nil {
		r.log.Warn("dropping event",
			"kind", regarding.Kind, "name", regarding.Name,
			"reason", reason, "error", err)
		return
	}
	r.remember(key, name, now)
}

func dedupKey(regarding v1alpha1.ObjectRef, typ v1alpha1.EventType, reason, message string) string {
	return regarding.UID + "|" + string(typ) + "|" + reason + "|" + message
}

func eventName(regardingName, key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("%s.%08x", regardingName, h.Sum32())
}

func (r *Recorder) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now()
	elapsed := now.Sub(r.lastRefill).Seconds()
	if elapsed > 0 {
		r.tokens += elapsed * r.spamQPS
		if r.tokens > r.spamBurst {
			r.tokens = r.spamBurst
		}
		r.lastRefill = now
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}

func (r *Recorder) lookup(key string, now time.Time) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.cache[key]
	if !ok {
		return "", false
	}
	entry := el.Value.(*cacheEntry)
	if now.After(entry.expires) {
		r.ll.Remove(el)
		delete(r.cache, key)
		return "", false
	}
	r.ll.MoveToFront(el)
	entry.expires = now.Add(r.keyTTL)
	return entry.eventName, true
}

func (r *Recorder) remember(key, name string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.cache[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.eventName = name
		entry.expires = now.Add(r.keyTTL)
		r.ll.MoveToFront(el)
		return
	}
	for r.ll.Len() >= r.maxEntries {
		oldest := r.ll.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		r.ll.Remove(oldest)
		delete(r.cache, entry.key)
	}
	el := r.ll.PushFront(&cacheEntry{
		key:       key,
		eventName: name,
		expires:   now.Add(r.keyTTL),
	})
	r.cache[key] = el
}

func (r *Recorder) bump(ctx context.Context, name string, now time.Time) {
	err := client.RetryOnConflict(func() error {
		fresh, err := r.client.GetEvent(ctx, name)
		if err != nil {
			return err
		}
		fresh.Count++
		fresh.LastTimestamp = v1alpha1.NewTime(now)
		_, err = r.client.ApplyEvent(ctx, fresh)
		return err
	})
	if err != nil {
		r.log.Warn("dropping event bump", "event", name, "error", err)
	}
}
