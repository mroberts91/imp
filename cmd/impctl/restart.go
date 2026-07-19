// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newRestartCmd(newClient func() *client.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart DAEMON",
		Short: "Restart a daemon by deleting its procs (the controller recreates them)",
		Long: `Restart a daemon: delete all of its Procs and let the DaemonController
recreate them from the current template.

This is systemctl-restart semantics — every replica stops at once and comes
back per the daemon's update strategy, so expect a brief downtime window. A
strategy-respecting rolling restart may layer on later; the verb's contract
(procs are replaced, spec untouched) will not change.

The daemon's spec is not modified, so nothing fights the manifest
directory's ownership of it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c := newClient()
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if _, err := c.GetDaemon(ctx, name); err != nil {
				if errors.Is(err, v1alpha1.ErrNotFound) {
					return fmt.Errorf("no daemon named %q", name)
				}
				return err
			}
			procs, _, err := c.ListProcs(ctx)
			if err != nil {
				return err
			}
			deleted := 0
			for i := range procs {
				p := &procs[i]
				if p.Metadata.Labels[v1alpha1.LabelDaemonName] != name {
					continue
				}
				switch err := c.DeleteProc(ctx, p.Metadata.Name); {
				case err == nil:
					fmt.Fprintf(out, "proc %q deleted\n", p.Metadata.Name)
					deleted++
				case errors.Is(err, v1alpha1.ErrNotFound):
					// Already gone (racing the controller); fine.
				default:
					return fmt.Errorf("deleting proc %s: %w", p.Metadata.Name, err)
				}
			}
			if deleted == 0 {
				fmt.Fprintf(out, "daemon %q has no procs; nothing to restart\n", name)
				return nil
			}
			fmt.Fprintf(out, "daemon %q restarted; run 'impctl rollout status %s' to watch\n", name, name)
			return nil
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
