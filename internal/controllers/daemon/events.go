// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// emit records a Normal event about d via the recorder. Best-effort.
func (c *Controller) emit(ctx context.Context, d *v1alpha1.Daemon, reason, message string) {
	c.recorder.Eventf(ctx, v1alpha1.ObjectRef{
		Kind: v1alpha1.KindDaemon,
		Name: d.Metadata.Name,
		UID:  d.Metadata.UID,
	}, v1alpha1.EventTypeNormal, reason, "%s", message)
}

// emitWarning records a Warning event about d via the recorder. Best-effort.
func (c *Controller) emitWarning(ctx context.Context, d *v1alpha1.Daemon, reason, message string) {
	c.recorder.Eventf(ctx, v1alpha1.ObjectRef{
		Kind: v1alpha1.KindDaemon,
		Name: d.Metadata.Name,
		UID:  d.Metadata.UID,
	}, v1alpha1.EventTypeWarning, reason, "%s", message)
}
