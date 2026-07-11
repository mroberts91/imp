// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newGetCmd(newClient func() *client.Client) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "get (daemons|procs|events) [NAME]",
		Short: "Display one or many objects",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := resolveKindArg(args[0])
			if err != nil {
				return err
			}
			c := newClient()
			ctx := cmd.Context()

			var items []json.RawMessage
			if len(args) == 2 {
				raw, err := c.GetRaw(ctx, kind, args[1])
				if err != nil {
					return err
				}
				items = []json.RawMessage{raw}
			} else {
				list, err := c.ListRaw(ctx, kind)
				if err != nil {
					return err
				}
				items = list.Items
			}

			switch output {
			case "":
				return printTable(cmd.OutOrStdout(), kind, items)
			case "json", "yaml":
				return printSerialized(cmd.OutOrStdout(), output, items)
			default:
				return fmt.Errorf("unknown output format %q (use json or yaml)", output)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml (default is a table)")
	return cmd
}

func printSerialized(w io.Writer, format string, items []json.RawMessage) error {
	for i, raw := range items {
		switch format {
		case "json":
			var obj any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return err
			}
			buf, err := json.MarshalIndent(obj, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(w, string(buf))
		case "yaml":
			if i > 0 {
				fmt.Fprintln(w, "---")
			}
			buf, err := yaml.JSONToYAML(raw)
			if err != nil {
				return err
			}
			fmt.Fprint(w, string(buf))
		}
	}
	return nil
}

func printTable(w io.Writer, kind string, items []json.RawMessage) error {
	if len(items) == 0 {
		fmt.Fprintln(w, "No resources found.")
		return nil
	}
	switch kind {
	case v1alpha1.KindDaemon:
		daemons, err := decodeItems[v1alpha1.Daemon](items)
		if err != nil {
			return err
		}
		printDaemonTable(w, daemons)
	case v1alpha1.KindProc:
		procs, err := decodeItems[v1alpha1.Proc](items)
		if err != nil {
			return err
		}
		printProcTable(w, procs)
	case v1alpha1.KindEvent:
		events, err := decodeItems[v1alpha1.Event](items)
		if err != nil {
			return err
		}
		printEventTable(w, events)
	}
	return nil
}

func decodeItems[T any](items []json.RawMessage) ([]T, error) {
	out := make([]T, 0, len(items))
	for _, raw := range items {
		var obj T
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("decoding object: %w", err)
		}
		out = append(out, obj)
	}
	return out, nil
}
