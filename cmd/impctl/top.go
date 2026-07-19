// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

// topSampleGap separates the two /stats samples CPU% is computed from.
const topSampleGap = 1 * time.Second

func newTopCmd(newClient func() *client.Client) *cobra.Command {
	var noHeaders bool
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Show live CPU/memory usage of running Procs",
		Long: `Show live per-Proc resource usage read from impd's cgroup stats.

CPU% is computed from two samples one second apart. Under a fake cgroup
root (rootless ad-hoc mode) values read as zero — limits and accounting
need a delegated cgroup subtree; see docs/install.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClient()
			ctx := cmd.Context()

			first, err := c.Stats(ctx)
			if err != nil {
				return err
			}
			start := time.Now()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(topSampleGap):
			}
			second, err := c.Stats(ctx)
			if err != nil {
				return err
			}
			elapsed := time.Since(start)

			printTopTable(cmd.OutOrStdout(), first, second, elapsed, noHeaders)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header row")
	return cmd
}

func printTopTable(w io.Writer, first, second []v1alpha1.ProcStat, elapsed time.Duration, noHeaders bool) {
	prev := make(map[string]v1alpha1.ProcStat, len(first))
	for _, s := range first {
		prev[s.Proc] = s
	}

	rows := make([]v1alpha1.ProcStat, len(second))
	copy(rows, second)
	sort.Slice(rows, func(i, j int) bool {
		oi, oj := ownerLabel(rows[i].Owner), ownerLabel(rows[j].Owner)
		if oi != oj {
			return oi < oj
		}
		return rows[i].Proc < rows[j].Proc
	})

	tw := newTabWriter(w)
	if !noHeaders {
		fmt.Fprintln(tw, "NAME\tOWNER\tCPU%\tMEMORY\tPIDS")
	}
	for _, s := range rows {
		cpu := "-"
		// Rate needs both samples; a Proc that appeared between them has
		// no baseline yet.
		if p, ok := prev[s.Proc]; ok && elapsed > 0 && s.CPUUsageUsec >= p.CPUUsageUsec {
			deltaUsec := float64(s.CPUUsageUsec - p.CPUUsageUsec)
			cpu = fmt.Sprintf("%.1f", deltaUsec/float64(elapsed.Microseconds())*100)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			s.Proc, ownerLabel(s.Owner), cpu,
			formatBytes(s.MemoryCurrentBytes), s.PidsCurrent)
	}
	if len(rows) == 0 {
		fmt.Fprintln(tw, "No running procs.")
	}
	tw.Flush()
}

func ownerLabel(o v1alpha1.ObjectRef) string {
	if o.Kind == "" {
		return "-"
	}
	return strings.ToLower(o.Kind) + "/" + o.Name
}
