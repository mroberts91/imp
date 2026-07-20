// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/pkg/client"
)

func newDeleteCmd(newClient func() *client.Client) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "delete (KIND NAME | -f FILE)",
		Short: "Delete objects by kind and name, or from manifests",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			ctx := cmd.Context()

			type target struct{ kind, name string }
			var targets []target
			switch {
			case len(files) > 0 && len(args) > 0:
				return errors.New("use either KIND NAME or -f, not both")
			case len(files) > 0:
				objects, err := loadManifests(files)
				if err != nil {
					return err
				}
				for _, obj := range objects {
					targets = append(targets, target{obj.Kind, obj.Name})
				}
			case len(args) == 2:
				kind, err := resolveKindArg(args[0])
				if err != nil {
					return err
				}
				targets = append(targets, target{kind, args[1]})
			default:
				return errors.New("specify KIND NAME or -f FILE")
			}

			failed := 0
			for _, tgt := range targets {
				if err := c.Delete(ctx, tgt.kind, tgt.name); err != nil {
					failed++
					fmt.Fprintf(cmd.ErrOrStderr(), "%s/%s: %v\n", strings.ToLower(tgt.kind), tgt.name, err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s deleted\n", strings.ToLower(tgt.kind), tgt.name)
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d objects failed to delete", failed, len(targets))
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVarP(&files, "filename", "f", nil, "delete the objects named in this manifest file or directory (repeatable)")
	cmd.ValidArgsFunction = completeKindThenName(newClient, "daemon", "proc", "event", "timer", "config")
	return cmd
}
