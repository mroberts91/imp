// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

// info is the kubectl cluster-info analog: the running impd's effective
// configuration over the API. impd is flags-only, so this is the one
// answer to "what is this daemon actually using" that does not require
// finding the unit file.
func newInfoCmd(newClient func() *client.Client) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show the running impd's effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, err := newClient().ServerInfo(cmd.Context())
			if errors.Is(err, v1alpha1.ErrNotFound) {
				return fmt.Errorf("the running impd does not serve /info (older version); upgrade impd")
			}
			if err != nil {
				return err
			}
			switch output {
			case "":
				printInfo(cmd.OutOrStdout(), info)
				return nil
			case "json", "yaml":
				raw, err := json.Marshal(info)
				if err != nil {
					return err
				}
				return printSerialized(cmd.OutOrStdout(), output, []json.RawMessage{raw})
			default:
				return fmt.Errorf("unknown output format %q (use json or yaml)", output)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml (default is text)")
	return cmd
}

func printInfo(w io.Writer, info *v1alpha1.ServerInfo) {
	tw := newTabWriter(w)
	defer tw.Flush()

	fmt.Fprintf(tw, "Server:\t%s\n", formatVersion(&info.Version))
	if !info.StartedAt.IsZero() {
		fmt.Fprintf(tw, "Started:\t%s (%s ago)\n", info.StartedAt.UTC().Format(time.RFC3339), age(info.StartedAt))
	}
	fmt.Fprintf(tw, "PID:\t%d\n", info.PID)
	fmt.Fprintf(tw, "Privileged:\t%s\n", yesNo(info.Privileged))

	socket := info.Socket
	if info.SocketGroup != "" {
		socket += fmt.Sprintf(" (group: %s)", info.SocketGroup)
	}
	fmt.Fprintf(tw, "Socket:\t%s\n", socket)
	fmt.Fprintf(tw, "Manifest directory:\t%s\n", info.ManifestDir)
	fmt.Fprintf(tw, "Data directory:\t%s\n", info.DataDir)
	fmt.Fprintf(tw, "Process logs:\t%s\n", info.LogDir)
	fmt.Fprintf(tw, "Config files:\t%s\n", info.ConfigDir)

	cgroup := info.CgroupRoot
	if info.CgroupKernelEnforced {
		cgroup += " (kernel-enforced)"
	} else {
		cgroup += " (FAKE — limits not kernel-enforced)"
	}
	fmt.Fprintf(tw, "Cgroup root:\t%s\n", cgroup)

	metrics := info.MetricsAddr
	if metrics == "" {
		metrics = "disabled"
	}
	fmt.Fprintf(tw, "Metrics:\t%s\n", metrics)
	fmt.Fprintf(tw, "Event TTL:\t%s\n", (time.Duration(info.EventTTLSeconds) * time.Second).String())
	fmt.Fprintf(tw, "Log level:\t%s\n", info.LogLevel)
	fmt.Fprintf(tw, "Kill Procs on shutdown:\t%s\n", yesNo(info.KillProcsOnShutdown))
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
