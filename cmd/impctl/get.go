// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

func newGetCmd(newClient func() *client.Client) *cobra.Command {
	var output string
	var watch bool
	var selector string
	cmd := &cobra.Command{
		Use:   "get (daemons|procs|events|timers|configs|notifiers) [NAME]",
		Short: "Display one or many objects",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := resolveKindArg(args[0])
			if err != nil {
				return err
			}
			c := newClient()
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if watch {
				if output != "" {
					return fmt.Errorf("cannot use --watch with -o")
				}
				if selector != "" {
					// Watch is unfiltered (M9-i): the changelog stream stays
					// selector-free. Filtering a running watch would be
					// client-side only and is out of scope for M9.
					return fmt.Errorf("the -l/--selector flag applies to list, not --watch")
				}
				if len(args) == 2 {
					return fmt.Errorf("get -w watches a kind, not a single named object")
				}
				return watchKind(ctx, c, out, kind)
			}
			if selector != "" && len(args) == 2 {
				return fmt.Errorf("the -l/--selector flag applies to a list, not a single named object")
			}

			var items []json.RawMessage
			if len(args) == 2 {
				raw, err := c.GetRaw(ctx, kind, args[1])
				if err != nil {
					return err
				}
				items = []json.RawMessage{raw}
			} else {
				list, err := c.ListRaw(ctx, kind, client.WithLabelSelector(selector))
				if err != nil {
					return err
				}
				items = list.Items
			}

			switch output {
			case "":
				return printTable(out, kind, items)
			case "json", "yaml":
				return printSerialized(out, output, items)
			default:
				return fmt.Errorf("unknown output format %q (use json or yaml)", output)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml (default is a table)")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "watch for changes after listing")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "filter by label (equality terms: k=v, k==v, k!=v, comma-joined); list only")
	cmd.ValidArgsFunction = completeKindThenName(newClient, "daemons", "procs", "events", "timers", "configs", "notifiers")
	return cmd
}

func watchKind(ctx context.Context, c *client.Client, w io.Writer, kind string) error {
	list, err := c.ListRaw(ctx, kind)
	if err != nil {
		return err
	}
	if err := printTable(w, kind, list.Items); err != nil {
		return err
	}

	events, cancel, err := c.Watch(ctx, kind, list.ResourceVersion)
	if err != nil {
		return err
	}
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("watch closed; relist and retry")
			}
			if ev.Type == v1alpha1.WatchError {
				return fmt.Errorf("watch error (compacted or overflow); relist and retry")
			}
			if err := printWatchRow(w, kind, string(ev.Type), ev.Object); err != nil {
				return err
			}
		}
	}
}

func printWatchRow(w io.Writer, kind, eventType string, raw json.RawMessage) error {
	switch kind {
	case v1alpha1.KindDaemon:
		var d v1alpha1.Daemon
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%d\t%s\n",
			eventType, d.Metadata.Name,
			d.Status.ReadyReplicas, desired,
			d.Status.UpdatedReplicas,
			age(d.Metadata.CreationTimestamp))
	case v1alpha1.KindProc:
		var p v1alpha1.Proc
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		pid := "-"
		if p.Status.State.Running != nil {
			pid = fmt.Sprint(p.Status.State.Running.PID)
		}
		phase := string(p.Status.Phase)
		if phase == "" {
			phase = string(v1alpha1.ProcPhasePending)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			eventType, p.Metadata.Name,
			p.Metadata.Labels[v1alpha1.LabelDaemonName],
			phase, p.Status.RestartCount, pid,
			age(p.Metadata.CreationTimestamp))
	case v1alpha1.KindEvent:
		var e v1alpha1.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s/%s\t%s\n",
			eventType, age(e.LastTimestamp), e.Type, e.Reason,
			strings.ToLower(e.Regarding.Kind), e.Regarding.Name, e.Message)
	case v1alpha1.KindTimer:
		var tm v1alpha1.Timer
		if err := json.Unmarshal(raw, &tm); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			eventType, tm.Metadata.Name, tm.Spec.Schedule,
			timerActive(&tm), lastRun(&tm),
			age(tm.Metadata.CreationTimestamp))
	case v1alpha1.KindConfig:
		var cfg v1alpha1.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			eventType, cfg.Metadata.Name, configFileCount(&cfg),
			configTotalSize(&cfg), age(cfg.Metadata.CreationTimestamp))
	case v1alpha1.KindNotifier:
		var n v1alpha1.Notifier
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			eventType, n.Metadata.Name, notifierCooldown(&n),
			lastNotified(&n), age(n.Metadata.CreationTimestamp))
	}
	return nil
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
	case v1alpha1.KindTimer:
		timers, err := decodeItems[v1alpha1.Timer](items)
		if err != nil {
			return err
		}
		printTimerTable(w, timers)
	case v1alpha1.KindConfig:
		configs, err := decodeItems[v1alpha1.Config](items)
		if err != nil {
			return err
		}
		printConfigTable(w, configs)
	case v1alpha1.KindNotifier:
		notifiers, err := decodeItems[v1alpha1.Notifier](items)
		if err != nil {
			return err
		}
		printNotifierTable(w, notifiers)
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
