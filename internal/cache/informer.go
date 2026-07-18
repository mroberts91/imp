// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package cache keeps a local mirror of one kind's objects and turns store
// changes into reconcile hints. It is a reflector-lite: the ListAndWatch
// loop shape is a logical fork of client-go's tools/cache/reflector.go
// (Copyright The Kubernetes Authors, Apache-2.0) - list at a
// resourceVersion, watch from it, and on any stream end or compaction,
// relist.
//
// Handlers receive keys only ("kind/name"), never objects: events are
// hints to reconcile, and content must be read from the Store at reconcile
// time (level-triggered discipline). A periodic resync re-fires every key
// as the safety net.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
)

// ListWatcher is the slice of pkg/client the informer consumes.
// *client.Client satisfies it.
type ListWatcher interface {
	ListRaw(ctx context.Context, kind string) (*v1alpha1.ObjectList, error)
	Watch(ctx context.Context, kind, sinceRV string) (<-chan v1alpha1.WatchEvent, func(), error)
}

// Options tune an Informer; the zero value (nil pointer included) gives
// production defaults.
type Options struct {
	// ResyncEvery is how often every key is re-fired to the handler as a
	// safety net. Default 10m.
	ResyncEvery time.Duration
	// Clock injects a fake clock in tests. Default the real clock.
	Clock clock.Clock
}

const (
	defaultResyncEvery = 10 * time.Minute
	// relistBackoff spaces retries when the list itself fails (impd's
	// apiserver is in-process, so this is nearly theoretical).
	relistBackoff = time.Second
)

// Informer mirrors one kind into a Store and calls handler with the key of
// anything that may have changed. Run it in its own goroutine; it keeps
// the mirror fresh until the context ends.
type Informer struct {
	lw          ListWatcher
	kind        string
	handler     func(key string)
	resyncEvery time.Duration
	clock       clock.Clock
	log         *slog.Logger

	store  *Store
	synced chan struct{}
}

// NewInformer builds an Informer for kind. The handler is called
// sequentially from the informer's goroutine and must not block for long -
// typical use is queue.Add.
func NewInformer(lw ListWatcher, kind string, handler func(key string), opts *Options) *Informer {
	resyncEvery := defaultResyncEvery
	var c clock.Clock = clock.Real{}
	if opts != nil {
		if opts.ResyncEvery > 0 {
			resyncEvery = opts.ResyncEvery
		}
		if opts.Clock != nil {
			c = opts.Clock
		}
	}
	return &Informer{
		lw:          lw,
		kind:        kind,
		handler:     handler,
		resyncEvery: resyncEvery,
		clock:       c,
		log:         slog.With("component", "cache", "kind", kind),
		store:       newStore(),
		synced:      make(chan struct{}),
	}
}

// Store returns the informer's mirror. Valid immediately; empty until the
// first list succeeds.
func (i *Informer) Store() *Store { return i.store }

// HasSynced reports whether the first list has completed, i.e. the Store
// reflects at least one complete snapshot.
func (i *Informer) HasSynced() bool {
	select {
	case <-i.synced:
		return true
	default:
		return false
	}
}

// WaitForSync blocks until the first list completes or ctx ends.
func (i *Informer) WaitForSync(ctx context.Context) error {
	select {
	case <-i.synced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run drives the list-watch-relist loop until ctx ends.
func (i *Informer) Run(ctx context.Context) {
	resync := i.clock.NewTicker(i.resyncEvery)
	defer resync.Stop()

	for ctx.Err() == nil {
		list, err := i.lw.ListRaw(ctx, i.kind)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			i.log.Warn("list failed, backing off", "err", err)
			if !i.sleep(ctx, relistBackoff) {
				return
			}
			continue
		}

		items := make(map[string]json.RawMessage, len(list.Items))
		for _, raw := range list.Items {
			key, ok := i.keyOf(raw)
			if !ok {
				continue
			}
			items[key] = raw
		}
		removed := i.store.replace(items)

		if !i.HasSynced() {
			close(i.synced)
		}

		// Fire every present key plus everything the relist removed:
		// a relist is indistinguishable from "anything may have
		// changed".
		for key := range items {
			i.handler(key)
		}
		for _, key := range removed {
			i.handler(key)
		}

		events, cancel, err := i.lw.Watch(ctx, i.kind, list.ResourceVersion)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, v1alpha1.ErrCompacted) {
				// Normal operation: the list rv fell behind the
				// changelog floor. Relist immediately.
				continue
			}
			i.log.Warn("watch failed, backing off", "rv", list.ResourceVersion, "err", err)
			if !i.sleep(ctx, relistBackoff) {
				return
			}
			continue
		}

		i.consume(ctx, events, cancel, resync)
		// Stream ended (server shutdown, overflow eviction, mid-stream
		// error): relist.
	}
}

// consume applies watch events to the store until the stream ends.
func (i *Informer) consume(ctx context.Context, events <-chan v1alpha1.WatchEvent, cancel func(), resync clock.Ticker) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return

		case <-resync.C():
			for _, key := range i.store.Keys() {
				i.handler(key)
			}

		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Type {
			case v1alpha1.WatchAdded, v1alpha1.WatchModified:
				key, keyOK := i.keyOf(ev.Object)
				if !keyOK {
					continue
				}
				i.store.set(key, ev.Object)
				i.handler(key)
			case v1alpha1.WatchDeleted:
				key, keyOK := i.keyOf(ev.Object)
				if !keyOK {
					continue
				}
				i.store.delete(key)
				i.handler(key)
			case v1alpha1.WatchError:
				// Terminal by protocol; the channel closes next.
				return
			}
		}
	}
}

// keyOf extracts "kind/name" from a raw object. Failure is logged and the
// object skipped - the apiserver validated everything it stored, so this
// guards against wire corruption, not bad user input.
func (i *Informer) keyOf(raw json.RawMessage) (string, bool) {
	var envelope struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Metadata.Name == "" {
		i.log.Error("dropping object with unreadable metadata", "err", err)
		return "", false
	}
	return i.kind + "/" + envelope.Metadata.Name, true
}

// sleep waits d on the injected clock; false means ctx ended first.
func (i *Informer) sleep(ctx context.Context, d time.Duration) bool {
	t := i.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C():
		return true
	}
}
