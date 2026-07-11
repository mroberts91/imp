// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// AGE/LAST-SEEN humanization is a
// fork of HumanDuration from k8s apimachinery's util/duration (Copyright
// The Kubernetes Authors, Apache-2.0) - matching kubectl's exact style is
// free familiarity.

func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
}

func printDaemonTable(w io.Writer, daemons []v1alpha1.Daemon) {
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "NAME\tREADY\tUP-TO-DATE\tAGE")
	for i := range daemons {
		d := &daemons[i]
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%s\n",
			d.Metadata.Name,
			d.Status.ReadyReplicas, desired,
			d.Status.UpdatedReplicas,
			age(d.Metadata.CreationTimestamp))
	}
	tw.Flush()
}

func printProcTable(w io.Writer, procs []v1alpha1.Proc) {
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "NAME\tDAEMON\tPHASE\tRESTARTS\tPID\tAGE")
	for i := range procs {
		p := &procs[i]
		pid := "-"
		if p.Status.State.Running != nil {
			pid = fmt.Sprint(p.Status.State.Running.PID)
		}
		phase := string(p.Status.Phase)
		if phase == "" {
			phase = string(v1alpha1.ProcPhasePending)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			p.Metadata.Name,
			p.Metadata.Labels[v1alpha1.LabelDaemonName],
			phase,
			p.Status.RestartCount,
			pid,
			age(p.Metadata.CreationTimestamp))
	}
	tw.Flush()
}

func printEventTable(w io.Writer, events []v1alpha1.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].LastTimestamp.Before(events[j].LastTimestamp.Time)
	})
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "LAST SEEN\tTYPE\tREASON\tOBJECT\tMESSAGE")
	for i := range events {
		e := &events[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%s\n",
			age(e.LastTimestamp),
			e.Type,
			e.Reason,
			strings.ToLower(e.Regarding.Kind), e.Regarding.Name,
			e.Message)
	}
	tw.Flush()
}

func age(t v1alpha1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return humanDuration(time.Since(t.Time))
}

// humanDuration renders a duration at kubectl precision: ~2-3 significant
// figures, coarser as it grows.
func humanDuration(d time.Duration) string {
	switch seconds := int(d.Seconds()); {
	case seconds < -1:
		return "<invalid>"
	case seconds < 0:
		return "0s"
	case seconds < 60*2:
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := int(d / time.Minute)
	if minutes < 10 {
		if s := int(d/time.Second) % 60; s != 0 {
			return fmt.Sprintf("%dm%ds", minutes, s)
		}
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes < 60*3 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := int(d / time.Hour)
	switch {
	case hours < 8:
		if m := minutes % 60; m != 0 {
			return fmt.Sprintf("%dh%dm", hours, m)
		}
		return fmt.Sprintf("%dh", hours)
	case hours < 48:
		return fmt.Sprintf("%dh", hours)
	case hours < 24*8:
		if h := hours % 24; h != 0 {
			return fmt.Sprintf("%dd%dh", hours/24, h)
		}
		return fmt.Sprintf("%dd", hours/24)
	case hours < 24*365*2:
		return fmt.Sprintf("%dd", hours/24)
	case hours < 24*365*8:
		if dy := (hours / 24) % 365; dy != 0 {
			return fmt.Sprintf("%dy%dd", hours/24/365, dy)
		}
		return fmt.Sprintf("%dy", hours/24/365)
	default:
		return fmt.Sprintf("%dy", hours/24/365)
	}
}
