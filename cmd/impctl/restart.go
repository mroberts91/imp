// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

// rollingPollInterval paces the client-side wait for a replacement proc
// (M7-i: the per-ordinal wait budget is the daemon's own
// progressDeadlineSeconds).
const rollingPollInterval = 500 * time.Millisecond

func newRestartCmd(newClient func() *client.Client) *cobra.Command {
	var rolling bool
	cmd := &cobra.Command{
		Use:   "restart DAEMON",
		Short: "Restart a daemon by deleting its procs (the controller recreates them)",
		Long: `Restart a daemon: delete all of its Procs and let the DaemonController
recreate them from the current template.

This is systemctl-restart semantics — every replica stops at once and comes
back per the daemon's update strategy, so expect a brief downtime window.

With --rolling, procs are replaced one ordinal at a time (highest first,
the RollingUpdate direction), waiting for each replacement to become
available — Ready, and Ready for minReadySeconds when set — before moving
on. This is one-at-a-time regardless of the daemon's maxUnavailable —
an operator restart is conservative by design; the daemon's own rollouts
honor maxUnavailable. The per-ordinal wait budget is the daemon's
progressDeadlineSeconds. With replicas: 1 a rolling restart is still a
full-stop restart with a wait, since there is no second replica to hold
availability.

Either way the daemon's spec is not modified, so nothing fights the
manifest directory's ownership of it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			c := newClient()
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			daemon, err := c.GetDaemon(ctx, name)
			if err != nil {
				if errors.Is(err, v1alpha1.ErrNotFound) {
					return fmt.Errorf("no daemon named %q", name)
				}
				return err
			}
			if rolling {
				return rollingRestart(ctx, c, daemon, out)
			}
			procs, err := daemonProcs(ctx, c, name)
			if err != nil {
				return err
			}
			deleted := 0
			for i := range procs {
				p := &procs[i]
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
	cmd.Flags().BoolVar(&rolling, "rolling", false,
		"replace procs one ordinal at a time (highest first), waiting for each replacement to become available")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeNames(cmd, newClient, v1alpha1.KindDaemon), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

// daemonProcs lists the procs owned by daemon name (by the daemon-name
// label; timer-owned procs never carry it for a daemon).
func daemonProcs(ctx context.Context, c *client.Client, name string) ([]v1alpha1.Proc, error) {
	procs, _, err := c.ListProcs(ctx)
	if err != nil {
		return nil, err
	}
	var owned []v1alpha1.Proc
	for i := range procs {
		if procs[i].Metadata.Labels[v1alpha1.LabelDaemonName] == name {
			owned = append(owned, procs[i])
		}
	}
	return owned, nil
}

// rollingRestart walks the daemon's ordinals high→low (the RollingUpdate
// direction), deleting each incumbent and polling until the controller's
// replacement — a proc at the same ordinal with a different UID — is
// available. Procs with a malformed replica-index label are restarted
// last, together, full-stop (the anomalous tail). Works identically for
// Recreate-strategy daemons: a single-ordinal delete is just a hole the
// controller refills.
func rollingRestart(ctx context.Context, c *client.Client, d *v1alpha1.Daemon, out io.Writer) error {
	name := d.Metadata.Name
	minReady := d.Spec.MinReadySeconds
	budget := v1alpha1.DefaultProgressDeadlineSeconds
	if d.Spec.ProgressDeadlineSeconds != nil {
		budget = *d.Spec.ProgressDeadlineSeconds
	}

	procs, err := daemonProcs(ctx, c, name)
	if err != nil {
		return err
	}
	if len(procs) == 0 {
		fmt.Fprintf(out, "daemon %q has no procs; nothing to restart\n", name)
		return nil
	}
	byOrdinal := map[int]*v1alpha1.Proc{}
	var ordinals []int
	var anomalous []*v1alpha1.Proc
	for i := range procs {
		p := &procs[i]
		ord, err := strconv.Atoi(p.Metadata.Labels[v1alpha1.LabelReplicaIndex])
		if err != nil || ord < 0 || byOrdinal[ord] != nil {
			anomalous = append(anomalous, p)
			continue
		}
		byOrdinal[ord] = p
		ordinals = append(ordinals, ord)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ordinals)))

	replaced := 0
	for _, ord := range ordinals {
		incumbent := byOrdinal[ord]
		if err := c.DeleteProc(ctx, incumbent.Metadata.Name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
			return fmt.Errorf("deleting proc %s: %w", incumbent.Metadata.Name, err)
		}
		fmt.Fprintf(out, "proc %q deleted (ordinal %d)\n", incumbent.Metadata.Name, ord)
		if err := waitForReplacement(ctx, c, name, ord, incumbent.Metadata.UID, minReady, budget); err != nil {
			return err
		}
		replaced++
	}
	for _, p := range anomalous {
		if err := c.DeleteProc(ctx, p.Metadata.Name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
			return fmt.Errorf("deleting proc %s: %w", p.Metadata.Name, err)
		}
		fmt.Fprintf(out, "proc %q deleted (no valid ordinal)\n", p.Metadata.Name)
		replaced++
	}
	fmt.Fprintf(out, "daemon %q rolling restart complete (%d procs replaced)\n", name, replaced)
	return nil
}

// waitForReplacement polls (client GETs — a CLI walk, not a watch loop)
// until a proc exists at the ordinal with a different UID and is available
// per the controller's procAvailable rule, or the budget runs out.
func waitForReplacement(ctx context.Context, c *client.Client, daemon string, ordinal int, oldUID string, minReady, budgetSeconds int32) error {
	deadline := time.Now().Add(time.Duration(budgetSeconds) * time.Second)
	for {
		procs, err := daemonProcs(ctx, c, daemon)
		if err != nil {
			return err
		}
		for i := range procs {
			p := &procs[i]
			if p.Metadata.Labels[v1alpha1.LabelReplicaIndex] != strconv.Itoa(ordinal) || p.Metadata.UID == oldUID {
				continue
			}
			if procAvailable(p, minReady, time.Now()) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ordinal %d replacement not available after %ds; daemon may be mid-rollout — check 'impctl rollout status %s'",
				ordinal, budgetSeconds, daemon)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rollingPollInterval):
		}
	}
}

// procAvailable mirrors the DaemonController's availability rule
// (internal/controllers/daemon procAvailable) client-side: Ready condition
// True for at least minReadySeconds measured against its
// lastTransitionTime; a proc without a Ready condition falls back to
// Phase==Running, and a Ready condition with a zero transition time counts
// immediately.
func procAvailable(p *v1alpha1.Proc, minReady int32, now time.Time) bool {
	c := v1alpha1.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionTypeReady)
	if c == nil {
		return p.Status.Phase == v1alpha1.ProcPhaseRunning
	}
	if c.Status != v1alpha1.ConditionTrue {
		return false
	}
	if minReady <= 0 || c.LastTransitionTime.IsZero() {
		return true
	}
	return !now.Before(c.LastTransitionTime.Add(time.Duration(minReady) * time.Second))
}
