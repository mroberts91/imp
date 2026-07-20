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
	var watch bool
	var interval int
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Show live CPU/memory usage of running Procs",
		Long: `Show live per-Proc resource usage read from impd's cgroup stats.

CPU% is computed from two samples. With --watch the table repaints every
--interval seconds (default 2) and CPU% covers that window; otherwise it is
a single reading one second apart. Under a fake cgroup root (rootless ad-hoc
mode) values read as zero — limits and accounting need a delegated cgroup
subtree; see docs/install.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("interval") && !watch {
				return fmt.Errorf("--interval applies only with --watch")
			}
			c := newClient()
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			gap := topSampleGap
			if watch {
				if interval < 1 {
					interval = 1
				}
				gap = time.Duration(interval) * time.Second
			}

			first, err := c.Stats(ctx)
			if err != nil {
				return err
			}
			for {
				start := time.Now()
				select {
				case <-ctx.Done():
					if watch {
						return nil // Ctrl-C is a clean exit while watching
					}
					return ctx.Err()
				case <-time.After(gap):
				}
				second, err := c.Stats(ctx)
				if err != nil {
					return err
				}
				elapsed := time.Since(start)

				if watch {
					fmt.Fprint(out, "\033[H\033[2J") // home + clear
				}
				printTopTable(out, first, second, elapsed, noHeaders)
				if !watch {
					return nil
				}
				first = second // previous sample is the next baseline
			}
		},
	}
	cmd.Flags().BoolVar(&noHeaders, "no-headers", false, "omit the header row")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "continuously repaint (Ctrl-C to exit)")
	cmd.Flags().IntVar(&interval, "interval", 2, "seconds between repaints in --watch mode (min 1)")
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
		fmt.Fprintln(tw, "NAME\tOWNER\tCPU%\tMEMORY\tPIDS\tTHROTTLED")
	}
	for _, s := range rows {
		cpu := "-"
		// Rate needs both samples; a Proc that appeared between them has
		// no baseline yet.
		if p, ok := prev[s.Proc]; ok && elapsed > 0 && s.CPUUsageUsec >= p.CPUUsageUsec {
			deltaUsec := float64(s.CPUUsageUsec - p.CPUUsageUsec)
			cpu = fmt.Sprintf("%.1f", deltaUsec/float64(elapsed.Microseconds())*100)
		}
		// THROTTLED is the kernel's cumulative CFS throttled-periods count
		// (M9-k); 0 without a cpu limit or under a fake cgroup root.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\n",
			s.Proc, ownerLabel(s.Owner), cpu,
			formatBytes(s.MemoryCurrentBytes), s.PidsCurrent, s.NrThrottled)
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
