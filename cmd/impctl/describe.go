// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

// describe layout is forked from kubectl's section order in
// staging/src/k8s.io/kubectl/pkg/describe/describe.go (Copyright The
// Kubernetes Authors, Apache-2.0): metadata → spec summary →
// status/conditions → events tail.

func newDescribeCmd(newClient func() *client.Client) *cobra.Command {
	return &cobra.Command{
		Use:               "describe (daemon|proc|timer) NAME",
		Short:             "Show details of a specific resource, including events",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeKindThenName(newClient, "daemon", "proc", "timer"),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := resolveKindArg(args[0])
			if err != nil {
				return err
			}
			if kind == v1alpha1.KindEvent {
				return fmt.Errorf("describe does not support events; use get events or events --for")
			}
			c := newClient()
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			switch kind {
			case v1alpha1.KindDaemon:
				d, err := c.GetDaemon(ctx, args[1])
				if err != nil {
					return err
				}
				events, err := eventsForDaemon(ctx, c, d)
				if err != nil {
					return err
				}
				describeDaemon(out, d, events)
			case v1alpha1.KindProc:
				p, err := c.GetProc(ctx, args[1])
				if err != nil {
					return err
				}
				events, err := eventsRegarding(ctx, c, kind, p.Metadata.Name)
				if err != nil {
					return err
				}
				describeProc(out, p, events)
			case v1alpha1.KindTimer:
				tm, err := c.GetTimer(ctx, args[1])
				if err != nil {
					return err
				}
				events, err := eventsForTimer(ctx, c, tm)
				if err != nil {
					return err
				}
				describeTimer(out, tm, events)
			}
			return nil
		},
	}
}

func eventsRegarding(ctx context.Context, c *client.Client, kind, name string) ([]v1alpha1.Event, error) {
	all, _, err := c.ListEvents(ctx)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.Event
	for i := range all {
		ev := &all[i]
		if strings.EqualFold(ev.Regarding.Kind, kind) && ev.Regarding.Name == name {
			out = append(out, *ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastTimestamp.Before(out[j].LastTimestamp.Time)
	})
	return out, nil
}

// eventsForDaemon returns Events regarding the Daemon plus any regarding
// its owned Procs, so describe tells the full crash-loop story.
func eventsForDaemon(ctx context.Context, c *client.Client, d *v1alpha1.Daemon) ([]v1alpha1.Event, error) {
	procs, _, err := c.ListProcs(ctx)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{d.Metadata.Name: true}
	for i := range procs {
		p := &procs[i]
		if p.Metadata.Labels[v1alpha1.LabelDaemonName] == d.Metadata.Name {
			owned[p.Metadata.Name] = true
		}
	}
	all, _, err := c.ListEvents(ctx)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.Event
	for i := range all {
		ev := &all[i]
		if owned[ev.Regarding.Name] {
			out = append(out, *ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastTimestamp.Before(out[j].LastTimestamp.Time)
	})
	return out, nil
}

func describeDaemon(w io.Writer, d *v1alpha1.Daemon, events []v1alpha1.Event) {
	fmt.Fprintf(w, "Name:\t%s\n", d.Metadata.Name)
	fmt.Fprintf(w, "UID:\t%s\n", d.Metadata.UID)
	fmt.Fprintf(w, "CreationTimestamp:\t%s\n", formatTime(d.Metadata.CreationTimestamp))
	fmt.Fprintf(w, "Generation:\t%d\n", d.Metadata.Generation)
	printLabels(w, d.Metadata.Labels)
	printAnnotations(w, d.Metadata.Annotations)

	desired := int32(0)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	fmt.Fprintf(w, "\nReplicas:\t%d desired | %d updated | %d ready | %d total\n",
		desired, d.Status.UpdatedReplicas, d.Status.ReadyReplicas, d.Status.Replicas)
	fmt.Fprintf(w, "UpdateStrategy:\t%s\n", d.Spec.UpdateStrategy.Type)
	if d.Spec.UpdateStrategy.Type == v1alpha1.UpdateStrategyRollingUpdate {
		partition := int32(0)
		if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
			partition = *ru.Partition
		}
		fmt.Fprintf(w, "  Partition:\t%d\n", partition)
	}
	fmt.Fprintf(w, "Command:\t%s\n", strings.Join(d.Spec.Template.Spec.Command, " "))
	if d.Spec.Template.Spec.RestartPolicy != "" {
		fmt.Fprintf(w, "RestartPolicy:\t%s\n", d.Spec.Template.Spec.RestartPolicy)
	}

	fmt.Fprintf(w, "\nStatus:\n")
	fmt.Fprintf(w, "  ObservedGeneration:\t%d\n", d.Status.ObservedGeneration)
	printConditions(w, d.Status.Conditions)
	printEventsTail(w, events)
}

func describeProc(w io.Writer, p *v1alpha1.Proc, events []v1alpha1.Event) {
	fmt.Fprintf(w, "Name:\t%s\n", p.Metadata.Name)
	fmt.Fprintf(w, "UID:\t%s\n", p.Metadata.UID)
	fmt.Fprintf(w, "CreationTimestamp:\t%s\n", formatTime(p.Metadata.CreationTimestamp))
	fmt.Fprintf(w, "Generation:\t%d\n", p.Metadata.Generation)
	printLabels(w, p.Metadata.Labels)
	printAnnotations(w, p.Metadata.Annotations)

	fmt.Fprintf(w, "\nCommand:\t%s\n", strings.Join(p.Spec.Command, " "))
	fmt.Fprintf(w, "RestartPolicy:\t%s\n", p.Spec.RestartPolicy)
	if p.Spec.WorkingDir != "" {
		fmt.Fprintf(w, "WorkingDir:\t%s\n", p.Spec.WorkingDir)
	}

	fmt.Fprintf(w, "\nStatus:\n")
	fmt.Fprintf(w, "  Phase:\t%s\n", p.Status.Phase)
	fmt.Fprintf(w, "  RestartCount:\t%d\n", p.Status.RestartCount)
	if p.Status.State.Running != nil {
		fmt.Fprintf(w, "  Running:\tPID=%d started=%s\n",
			p.Status.State.Running.PID, formatTime(p.Status.State.Running.StartedAt))
	}
	if p.Status.State.Waiting != nil {
		fmt.Fprintf(w, "  Waiting:\t%s: %s\n",
			p.Status.State.Waiting.Reason, p.Status.State.Waiting.Message)
	}
	if p.Status.State.Terminated != nil {
		fmt.Fprintf(w, "  Terminated:\texit=%d signal=%s: %s\n",
			p.Status.State.Terminated.ExitCode,
			p.Status.State.Terminated.Signal,
			p.Status.State.Terminated.Message)
	}
	printConditions(w, p.Status.Conditions)
	printEventsTail(w, events)
}

func printLabels(w io.Writer, labels map[string]string) {
	if len(labels) == 0 {
		fmt.Fprintln(w, "Labels:\t<none>")
		return
	}
	keys := sortedKeys(labels)
	fmt.Fprintf(w, "Labels:\t%s=%s\n", keys[0], labels[keys[0]])
	for _, k := range keys[1:] {
		fmt.Fprintf(w, "       \t%s=%s\n", k, labels[k])
	}
}

func printAnnotations(w io.Writer, ann map[string]string) {
	if len(ann) == 0 {
		fmt.Fprintln(w, "Annotations:\t<none>")
		return
	}
	keys := sortedKeys(ann)
	fmt.Fprintf(w, "Annotations:\t%s=%s\n", keys[0], ann[keys[0]])
	for _, k := range keys[1:] {
		fmt.Fprintf(w, "            \t%s=%s\n", k, ann[k])
	}
}

func printConditions(w io.Writer, conditions []v1alpha1.Condition) {
	fmt.Fprintln(w, "Conditions:")
	if len(conditions) == 0 {
		fmt.Fprintln(w, "  <none>")
		return
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  Type\tStatus\tReason\tMessage")
	for i := range conditions {
		c := &conditions[i]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", c.Type, c.Status, c.Reason, c.Message)
	}
	tw.Flush()
}

func printEventsTail(w io.Writer, events []v1alpha1.Event) {
	fmt.Fprintln(w, "\nEvents:")
	if len(events) == 0 {
		fmt.Fprintln(w, "  <none>")
		return
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  Type\tReason\tAge\tFrom\tMessage")
	for i := range events {
		e := &events[i]
		msg := e.Message
		if e.Count > 1 {
			msg = fmt.Sprintf("%s (x%d)", msg, e.Count)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			e.Type, e.Reason, age(e.LastTimestamp), e.ReportingComponent, msg)
	}
	tw.Flush()
}

func formatTime(t v1alpha1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return t.Time.UTC().Format("2006-01-02 15:04:05") + " +0000 UTC"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// eventsForTimer returns Events regarding the Timer plus any regarding its
// runs, so describe tells the full scheduling story.
func eventsForTimer(ctx context.Context, c *client.Client, tm *v1alpha1.Timer) ([]v1alpha1.Event, error) {
	procs, _, err := c.ListProcs(ctx)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{tm.Metadata.Name: true}
	for i := range procs {
		if procs[i].Metadata.Labels[v1alpha1.LabelTimerName] == tm.Metadata.Name {
			owned[procs[i].Metadata.Name] = true
		}
	}
	all, _, err := c.ListEvents(ctx)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.Event
	for i := range all {
		if owned[all[i].Regarding.Name] {
			out = append(out, all[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastTimestamp.Before(out[j].LastTimestamp.Time)
	})
	return out, nil
}

func describeTimer(w io.Writer, tm *v1alpha1.Timer, events []v1alpha1.Event) {
	fmt.Fprintf(w, "Name:\t%s\n", tm.Metadata.Name)
	printLabels(w, tm.Metadata.Labels)
	printAnnotations(w, tm.Metadata.Annotations)
	fmt.Fprintf(w, "Created:\t%s\n", formatTime(tm.Metadata.CreationTimestamp))

	fmt.Fprintf(w, "\nSchedule:\t%s\n", tm.Spec.Schedule)
	suspend := "false"
	if tm.Spec.Suspend != nil && *tm.Spec.Suspend {
		suspend = "true"
	}
	fmt.Fprintf(w, "Suspend:\t%s\n", suspend)
	fmt.Fprintf(w, "ConcurrencyPolicy:\t%s\n", tm.Spec.ConcurrencyPolicy)
	if v := tm.Spec.StartingDeadlineSeconds; v != nil {
		fmt.Fprintf(w, "StartingDeadlineSeconds:\t%d\n", *v)
	}
	fmt.Fprintf(w, "Command:\t%s\n", strings.Join(tm.Spec.Template.Spec.Command, " "))

	fmt.Fprintf(w, "\nLast Schedule:\t%s\n", formatTime(tm.Status.LastScheduleTime))
	fmt.Fprintf(w, "Last Successful:\t%s\n", formatTime(tm.Status.LastSuccessfulTime))
	fmt.Fprintf(w, "Active Proc:\t%s\n", timerActive(tm))

	// Next fire times, computed client-side; unparseable schedules are
	// rejected at apply, so failure here means skew worth surfacing as-is.
	if sched, err := cron.ParseStandard(tm.Spec.Schedule); err == nil {
		next := time.Now()
		fmt.Fprintf(w, "Next Runs:")
		for range 3 {
			next = sched.Next(next)
			fmt.Fprintf(w, "\t%s", next.Format(time.RFC3339))
		}
		fmt.Fprintln(w)
	}

	printConditions(w, tm.Status.Conditions)
	printEventsTail(w, events)
}
