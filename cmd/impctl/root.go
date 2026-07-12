// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newRootCmd() *cobra.Command {
	var socketPath string

	root := &cobra.Command{
		Use:   "impctl",
		Short: "impctl controls imp, the declarative process orchestrator",
		Long: `impctl controls imp, the declarative single-host process orchestrator.

It talks to impd over a local Unix domain socket; socket permissions are
the access model.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	defaultSocket := client.DefaultSocketPath
	if env := os.Getenv("IMP_SOCKET"); env != "" {
		defaultSocket = env
	}
	root.PersistentFlags().StringVar(&socketPath, "socket", defaultSocket,
		"path of impd's unix domain socket (env IMP_SOCKET)")

	newClient := func() *client.Client { return client.New(socketPath) }

	root.AddCommand(
		newGetCmd(newClient),
		newApplyCmd(newClient),
		newDeleteCmd(newClient),
		newLogsCmd(newClient),
		newVersionCmd(newClient),
	)
	return root
}

func resolveKindArg(arg string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "daemon", "daemons":
		return v1alpha1.KindDaemon, nil
	case "proc", "procs":
		return v1alpha1.KindProc, nil
	case "event", "events":
		return v1alpha1.KindEvent, nil
	default:
		return "", fmt.Errorf("unknown resource type %q (use daemon, proc, or event)", arg)
	}
}
