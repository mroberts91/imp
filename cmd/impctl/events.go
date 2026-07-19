// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newEventsCmd(newClient func() *client.Client) *cobra.Command {
	var forObj string
	cmd := &cobra.Command{
		Use:   "events",
		Short: "List events, optionally filtered to one object",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClient()
			ctx := cmd.Context()
			events, _, err := c.ListEvents(ctx)
			if err != nil {
				return err
			}
			if forObj != "" {
				kind, name, err := parseForArg(forObj)
				if err != nil {
					return err
				}
				var filtered []v1alpha1.Event
				for i := range events {
					ev := &events[i]
					if strings.EqualFold(ev.Regarding.Kind, kind) && ev.Regarding.Name == name {
						filtered = append(filtered, *ev)
					}
				}
				events = filtered
			}
			if len(events) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No resources found.")
				return nil
			}
			printEventTable(cmd.OutOrStdout(), events)
			return nil
		},
	}
	cmd.Flags().StringVar(&forObj, "for", "", "filter to regarding object as kind/name (e.g. daemon/web)")
	return cmd
}

func parseForArg(s string) (kind, name string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--for must be kind/name (e.g. daemon/web)")
	}
	kind, err = resolveKindArg(parts[0])
	if err != nil {
		return "", "", err
	}
	return kind, parts[1], nil
}
