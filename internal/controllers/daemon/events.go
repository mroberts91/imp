// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package daemon

// Event emission, applied directly through the client - the recorder
// abstraction is M2. Events are best-effort: a failure to record one is
// logged and swallowed, never failing the reconcile that emitted it.

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// emit records a Normal event about d. Best-effort by contract.
func (c *Controller) emit(ctx context.Context, d *v1alpha1.Daemon, reason, message string) {
	now := v1alpha1.NewTime(c.clock.Now())
	ev := &v1alpha1.Event{
		Metadata: v1alpha1.ObjectMeta{
			Name: eventName(d.Metadata.Name, reason, message),
		},
		Regarding: v1alpha1.ObjectRef{
			Kind: v1alpha1.KindDaemon,
			Name: d.Metadata.Name,
			UID:  d.Metadata.UID,
		},
		Type:               v1alpha1.EventTypeNormal,
		Reason:             reason,
		Message:            message,
		Count:              1,
		FirstTimestamp:     now,
		LastTimestamp:      now,
		ReportingComponent: componentName,
	}
	if _, err := c.client.ApplyEvent(ctx, ev); err != nil {
		c.log.Warn("dropping event",
			"kind", v1alpha1.KindDaemon,
			"key", v1alpha1.KindDaemon+"/"+d.Metadata.Name,
			"reason", reason,
			"error", err)
	}
}

// eventName builds a stable event name: the daemon's name plus an
// FNV-1a-32 of reason|message, so identical happenings collapse onto one
// object instead of piling up.
func eventName(daemonName, reason, message string) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%s", reason, message)
	return fmt.Sprintf("%s.%08x", daemonName, h.Sum32())
}
