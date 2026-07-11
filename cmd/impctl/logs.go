// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newLogsCmd(newClient func() *client.Client) *cobra.Command {
	var opts client.LogOptions
	cmd := &cobra.Command{
		Use:   "logs [-f] [--tail N] [--timestamps] (PROC|DAEMON)",
		Short: "Print the logs of a Proc (a Daemon name resolves to its Proc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			ctx := cmd.Context()

			procName, err := resolveLogTarget(ctx, c, args[0])
			if err != nil {
				return err
			}
			rc, err := c.ProcLogs(ctx, procName, opts)
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(cmd.OutOrStdout(), rc)
			// An interrupt while following is a clean exit, not an error.
			if ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&opts.Follow, "follow", "f", false, "stream new lines as they are written")
	cmd.Flags().IntVar(&opts.TailLines, "tail", 0, "show only the last N lines (0 shows everything)")
	cmd.Flags().BoolVar(&opts.Timestamps, "timestamps", false, "prefix each line with its capture time")
	return cmd
}

// resolveLogTarget accepts a Proc name directly, or a Daemon name when the
// Daemon has exactly one Proc.
func resolveLogTarget(ctx context.Context, c *client.Client, name string) (string, error) {
	if _, err := c.GetProc(ctx, name); err == nil {
		return name, nil
	} else if !errors.Is(err, v1alpha1.ErrNotFound) {
		return "", err
	}
	if _, err := c.GetDaemon(ctx, name); err != nil {
		return "", fmt.Errorf("no proc or daemon named %q: %w", name, v1alpha1.ErrNotFound)
	}
	procs, _, err := c.ListProcs(ctx)
	if err != nil {
		return "", err
	}
	var owned []string
	for i := range procs {
		if procs[i].Metadata.Labels[v1alpha1.LabelDaemonName] == name {
			owned = append(owned, procs[i].Metadata.Name)
		}
	}
	switch len(owned) {
	case 0:
		return "", fmt.Errorf("daemon %q has no procs yet", name)
	case 1:
		return owned[0], nil
	default:
		return "", fmt.Errorf("daemon %q has %d procs; pick one: %s",
			name, len(owned), strings.Join(owned, ", "))
	}
}
