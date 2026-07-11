// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newVersionCmd(newClient func() *client.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the client and server versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "Client: %s\n", formatVersion(&v1alpha1.VersionInfo{
				Version: version, Commit: commit, Branch: branch,
				BuildTime: buildTime, GoVersion: runtime.Version(),
			}))
			server, err := newClient().ServerVersion(cmd.Context())
			if err != nil {
				return fmt.Errorf("server unreachable: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Server: %s\n", formatVersion(server))
			return nil
		},
	}
}

func formatVersion(v *v1alpha1.VersionInfo) string {
	out := v.Version
	if v.Commit != "" && v.Commit != "unknown" {
		out += " (" + v.Commit + ")"
	}
	if v.GoVersion != "" {
		out += " " + v.GoVersion
	}
	return out
}
