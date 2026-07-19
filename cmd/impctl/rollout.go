// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newRolloutCmd(newClient func() *client.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollout",
		Short: "Inspect a daemon rollout",
	}
	cmd.AddCommand(newRolloutStatusCmd(newClient))
	return cmd
}

func newRolloutStatusCmd(newClient func() *client.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status DAEMON",
		Short: "Watch a daemon's rollout until it completes or exceeds its progress deadline",
		Long: `Watch DAEMON's rollout. Exits 0 when every replica is updated and
available; exits nonzero when Progressing reports ProgressDeadlineExceeded.

There is no 'rollout undo' or 'rollout history': the manifest directory
owns the spec, so the manifest file (and its version control) is the
revision history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c := newClient()
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			d, err := c.GetDaemon(ctx, name)
			if err != nil {
				if errors.Is(err, v1alpha1.ErrNotFound) {
					return fmt.Errorf("no daemon named %q", name)
				}
				return err
			}
			lastLine := reportRollout(out, d, "")
			if done, err := rolloutOutcome(d); done {
				if err == nil {
					fmt.Fprintf(out, "daemon %q successfully rolled out\n", name)
				}
				return err
			}

			// Same posture as get -w: list, watch from the list RV, and on
			// any stream end tell the caller to retry rather than looping.
			list, err := c.ListRaw(ctx, v1alpha1.KindDaemon)
			if err != nil {
				return err
			}
			events, cancel, err := c.Watch(ctx, v1alpha1.KindDaemon, list.ResourceVersion)
			if err != nil {
				return err
			}
			defer cancel()

			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ev, ok := <-events:
					if !ok {
						return fmt.Errorf("watch closed; re-run to keep watching")
					}
					if ev.Type == v1alpha1.WatchError {
						return fmt.Errorf("watch error (compacted or overflow); re-run to keep watching")
					}
					var got v1alpha1.Daemon
					if err := json.Unmarshal(ev.Object, &got); err != nil {
						return err
					}
					if got.Metadata.Name != name {
						continue
					}
					if ev.Type == v1alpha1.WatchDeleted {
						return fmt.Errorf("daemon %q was deleted", name)
					}
					lastLine = reportRollout(out, &got, lastLine)
					if done, err := rolloutOutcome(&got); done {
						if err == nil {
							fmt.Fprintf(out, "daemon %q successfully rolled out\n", name)
						}
						return err
					}
				}
			}
		},
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeNames(cmd, newClient, v1alpha1.KindDaemon), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

// reportRollout prints a progress line when it differs from the previous
// one, and returns the line printed (or carried).
func reportRollout(w io.Writer, d *v1alpha1.Daemon, prev string) string {
	replicas := int32(0)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	line := fmt.Sprintf("waiting for rollout: %d/%d updated, %d/%d available",
		d.Status.UpdatedReplicas, replicas, d.Status.AvailableReplicas, replicas)
	if line != prev {
		fmt.Fprintln(w, line)
	}
	return line
}

// rolloutOutcome reports whether the rollout reached a terminal state:
// complete (nil error) or deadline exceeded (the condition message as the
// error). Also prints nothing — the caller owns output.
func rolloutOutcome(d *v1alpha1.Daemon) (done bool, err error) {
	if d.Status.ObservedGeneration < d.Metadata.Generation {
		return false, nil // the controller has not seen this spec yet
	}
	prog := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
	if prog == nil {
		return false, nil
	}
	if prog.Status == v1alpha1.ConditionFalse && prog.Reason == v1alpha1.ReasonProgressDeadlineExceeded {
		return true, errors.New(prog.Message)
	}
	avail := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeAvailable)
	// "ProcsAvailable" is the DaemonController's terminal Progressing
	// reason (its vocabulary, pinned by its tests).
	if prog.Status == v1alpha1.ConditionTrue && prog.Reason == "ProcsAvailable" &&
		avail != nil && avail.Status == v1alpha1.ConditionTrue {
		return true, nil
	}
	return false, nil
}
