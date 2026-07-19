// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
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
		newDescribeCmd(newClient),
		newEventsCmd(newClient),
		newLogsCmd(newClient),
		newRestartCmd(newClient),
		newRunCmd(newClient),
		newRolloutCmd(newClient),
		newTopCmd(newClient),
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
	case "timer", "timers":
		return v1alpha1.KindTimer, nil
	default:
		return "", fmt.Errorf("unknown resource type %q (use daemon, proc, event, or timer)", arg)
	}
}

// completeKindThenName completes position 0 from kinds and position 1 with
// live object names of that kind. Fails soft (no completions) when impd is
// unreachable — completion must never error at the shell.
func completeKindThenName(newClient func() *client.Client, kinds ...string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return kinds, cobra.ShellCompDirectiveNoFileComp
		}
		if len(args) > 1 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		kind, err := resolveKindArg(args[0])
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeNames(cmd, newClient, kind), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeNames lists live object names of kind, soft-failing to nothing.
func completeNames(cmd *cobra.Command, newClient func() *client.Client, kind string) []string {
	list, err := newClient().ListRaw(cmd.Context(), kind)
	if err != nil {
		return nil
	}
	var names []string
	for _, raw := range list.Items {
		var envelope struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Metadata.Name != "" {
			names = append(names, envelope.Metadata.Name)
		}
	}
	return names
}
