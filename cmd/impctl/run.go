// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/controllers/timer"
	"github.com/mroberts91/imp/pkg/client"
)

func newRunCmd(newClient func() *client.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run TIMER",
		Short: "Trigger a timer's run now, outside its schedule",
		Long: `Create a run of TIMER immediately, from its current template — the
kubectl create job --from=cronjob analog.

A manual run is a real run: with concurrencyPolicy Forbid, scheduled ticks
are skipped while it is active. It does not consult the policy itself —
operator intent runs even alongside an active scheduled run — and it never
advances the timer's lastScheduleTime.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c := newClient()
			ctx := cmd.Context()

			t, err := c.GetTimer(ctx, name)
			if err != nil {
				if errors.Is(err, v1alpha1.ErrNotFound) {
					return fmt.Errorf("no timer named %q", name)
				}
				return err
			}
			// The controller's exported builder keeps run identity
			// single-sourced (M6-d).
			p := timer.BuildRun(t, time.Now(), true)
			created, err := c.ApplyProc(ctx, p)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "proc %q created\n", created.Metadata.Name)
			return nil
		},
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeNames(cmd, newClient, v1alpha1.KindTimer), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}
