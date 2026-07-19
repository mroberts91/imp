// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package eventttl prunes Events whose lastTimestamp is older than the
// configured retention. Not informer-driven: a ticker lists and deletes.
// Its existence as a named controller is what keeps Events a resource, not
// a component (doc 01). Default retention is 72h (D3).
package eventttl

import (
	"context"
	"log/slog"
	"time"

	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/pkg/client"
)

const (
	DefaultRetention = 72 * time.Hour
	defaultTick      = time.Minute
)

// Controller periodically deletes expired Events.
type Controller struct {
	client    *client.Client
	clock     clock.Clock
	retention time.Duration
	tick      time.Duration
	log       *slog.Logger
}

// New builds an EventTTL controller. A nil clk means the real clock.
// retention <= 0 selects DefaultRetention.
func New(cl *client.Client, retention time.Duration, clk clock.Clock) *Controller {
	if clk == nil {
		clk = clock.Real{}
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &Controller{
		client:    cl,
		clock:     clk,
		retention: retention,
		tick:      defaultTick,
		log:       slog.With("component", "eventttl"),
	}
}

// Run ticks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	ticker := c.clock.NewTicker(c.tick)
	defer ticker.Stop()
	c.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
			c.sweep(ctx)
		}
	}
}

func (c *Controller) sweep(ctx context.Context) {
	events, _, err := c.client.ListEvents(ctx)
	if err != nil {
		c.log.Warn("list events failed", "error", err)
		return
	}
	cutoff := c.clock.Now().Add(-c.retention)
	for i := range events {
		ev := &events[i]
		if ev.LastTimestamp.IsZero() || !ev.LastTimestamp.Before(cutoff) {
			continue
		}
		if err := c.client.DeleteEvent(ctx, ev.Metadata.Name); err != nil {
			c.log.Warn("delete expired event failed",
				"key", "Event/"+ev.Metadata.Name, "error", err)
			continue
		}
		c.log.Debug("deleted expired event",
			"key", "Event/"+ev.Metadata.Name,
			"lastTimestamp", ev.LastTimestamp)
	}
}
