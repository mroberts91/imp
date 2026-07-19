// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/pkg/client"
)

const backupTimerManifest = `
apiVersion: impd.sh/v1alpha1
kind: Timer
metadata:
  name: backup
spec:
  schedule: "0 5 * * *"
  template:
    spec:
      command: ["/bin/true"]
`

// seedProc creates a Proc labeled as owned by daemon via the client (no
// controllers run in this harness).
func seedProc(t *testing.T, socket, daemon, name string) {
	t.Helper()
	c := client.New(socket)
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1alpha1.LabelDaemonName: daemon},
		},
		Spec: v1alpha1.ProcSpec{Command: []string{"/bin/sleep", "60"}},
	}
	if _, err := c.ApplyProc(t.Context(), p); err != nil {
		t.Fatalf("seeding proc %s: %v", name, err)
	}
}

func TestRestartDeletesDaemonProcs(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, webManifest)
	impctl(t, socket, false, "apply", "-f", manifest)
	seedProc(t, socket, "web", "web-0-aaaa1111")
	seedProc(t, socket, "worker", "worker-0-bbbb2222")

	out := impctl(t, socket, false, "restart", "web")
	if !strings.Contains(out, `proc "web-0-aaaa1111" deleted`) {
		t.Errorf("restart output missing delete line: %q", out)
	}
	if !strings.Contains(out, `daemon "web" restarted`) {
		t.Errorf("restart output missing summary: %q", out)
	}

	c := client.New(socket)
	if _, err := c.GetProc(t.Context(), "web-0-aaaa1111"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("web proc still exists after restart (err=%v)", err)
	}
	// Another daemon's procs are untouched.
	if _, err := c.GetProc(t.Context(), "worker-0-bbbb2222"); err != nil {
		t.Errorf("restart touched another daemon's proc: %v", err)
	}
}

func TestRestartNoProcsAndUnknownDaemon(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, webManifest)
	impctl(t, socket, false, "apply", "-f", manifest)

	out := impctl(t, socket, false, "restart", "web")
	if !strings.Contains(out, "nothing to restart") {
		t.Errorf("restart with no procs: %q", out)
	}
	out = impctl(t, socket, true, "restart", "ghost")
	if !strings.Contains(out, `no daemon named "ghost"`) {
		t.Errorf("restart unknown daemon: %q", out)
	}
}

func TestRunCreatesManualTimerRun(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, backupTimerManifest)
	impctl(t, socket, false, "apply", "-f", manifest)

	out := impctl(t, socket, false, "run", "backup")
	if !strings.Contains(out, `proc "backup-manual-`) {
		t.Errorf("run output missing manual proc name: %q", out)
	}

	c := client.New(socket)
	procs, _, err := c.ListProcs(t.Context())
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	if len(procs) != 1 {
		t.Fatalf("got %d procs, want 1", len(procs))
	}
	p := procs[0]
	if p.Metadata.Annotations[v1alpha1.AnnotationManual] != "true" {
		t.Error("manual run missing impd.sh/manual annotation")
	}
	if p.Metadata.Labels[v1alpha1.LabelTimerName] != "backup" {
		t.Error("manual run missing timer-name label")
	}
	if len(p.Metadata.OwnerReferences) != 1 || p.Metadata.OwnerReferences[0].Kind != v1alpha1.KindTimer {
		t.Errorf("manual run ownerReferences = %+v, want the Timer", p.Metadata.OwnerReferences)
	}

	out = impctl(t, socket, true, "run", "ghost")
	if !strings.Contains(out, `no timer named "ghost"`) {
		t.Errorf("run unknown timer: %q", out)
	}
}

const rollingWebManifest = `
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  replicas: 2
  template:
    spec:
      command: ["/bin/sleep", "60"]
`

// seedReadyProc creates a Proc at an ordinal with a Ready=True status —
// what the controller and execd would have produced (neither runs in this
// harness). Returns the created proc's UID.
func seedReadyProc(t *testing.T, socket, daemon, name string, ordinal int) string {
	t.Helper()
	c := client.New(socket)
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1alpha1.LabelDaemonName:   daemon,
				v1alpha1.LabelReplicaIndex: strconv.Itoa(ordinal),
			},
		},
		Spec: v1alpha1.ProcSpec{Command: []string{"/bin/sleep", "60"}},
	}
	created, err := c.ApplyProc(t.Context(), p)
	if err != nil {
		t.Fatalf("seeding proc %s: %v", name, err)
	}
	created.Status.Phase = v1alpha1.ProcPhaseRunning
	v1alpha1.SetStatusCondition(&created.Status.Conditions, v1alpha1.Condition{
		Type:   v1alpha1.ConditionTypeReady,
		Status: v1alpha1.ConditionTrue,
		Reason: "Running",
	})
	if _, err := c.UpdateProcStatus(t.Context(), created); err != nil {
		t.Fatalf("stamping status on %s: %v", name, err)
	}
	return created.Metadata.UID
}

func TestRestartRollingReplacesHighToLow(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, rollingWebManifest)
	impctl(t, socket, false, "apply", "-f", manifest)
	seedReadyProc(t, socket, "web", "web-0-old", 0)
	seedReadyProc(t, socket, "web", "web-1-old", 1)

	// No controllers in this harness: a stand-in observes each incumbent's
	// deletion and seeds the Ready replacement the walk is polling for.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c := client.New(socket)
		seeded := map[int]bool{}
		for len(seeded) < 2 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			for ord, old := range map[int]string{0: "web-0-old", 1: "web-1-old"} {
				if seeded[ord] {
					continue
				}
				if _, err := c.GetProc(ctx, old); errors.Is(err, v1alpha1.ErrNotFound) {
					seedReadyProc(t, socket, "web", fmt.Sprintf("web-%d-new", ord), ord)
					seeded[ord] = true
				}
			}
		}
	}()

	out := impctl(t, socket, false, "restart", "--rolling", "web")
	cancel()
	<-done

	high := strings.Index(out, `proc "web-1-old" deleted (ordinal 1)`)
	low := strings.Index(out, `proc "web-0-old" deleted (ordinal 0)`)
	if high == -1 || low == -1 || high > low {
		t.Errorf("rolling restart did not walk high→low:\n%s", out)
	}
	if !strings.Contains(out, `daemon "web" rolling restart complete (2 procs replaced)`) {
		t.Errorf("rolling restart summary missing:\n%s", out)
	}
	c := client.New(socket)
	for _, name := range []string{"web-0-old", "web-1-old"} {
		if _, err := c.GetProc(t.Context(), name); !errors.Is(err, v1alpha1.ErrNotFound) {
			t.Errorf("incumbent %s still exists after rolling restart (err=%v)", name, err)
		}
	}
}

func TestRestartRollingBudgetExceeded(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, strings.NewReplacer(
		"replicas: 2", "replicas: 1\n  progressDeadlineSeconds: 1",
	).Replace(rollingWebManifest))
	impctl(t, socket, false, "apply", "-f", manifest)
	seedReadyProc(t, socket, "web", "web-0-only", 0)

	// Nothing seeds a replacement: the walk must give up after the daemon's
	// progressDeadlineSeconds and point at rollout status.
	out := impctl(t, socket, true, "restart", "--rolling", "web")
	if !strings.Contains(out, "ordinal 0 replacement not available after 1s") {
		t.Errorf("budget-exceeded error missing: %q", out)
	}
	if !strings.Contains(out, "rollout status") {
		t.Errorf("budget-exceeded error does not point at rollout status: %q", out)
	}
}

// setDaemonRolloutStatus stamps a terminal rollout status on the daemon (no
// controllers in this harness).
func setDaemonRolloutStatus(t *testing.T, socket string, exceeded bool) {
	t.Helper()
	c := client.New(socket)
	d, err := c.GetDaemon(t.Context(), "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	d.Status.ObservedGeneration = d.Metadata.Generation
	replicas := *d.Spec.Replicas
	d.Status.Replicas = replicas
	d.Status.UpdatedReplicas = replicas
	d.Status.ReadyReplicas = replicas
	d.Status.AvailableReplicas = replicas
	prog := v1alpha1.Condition{
		Type:   v1alpha1.ConditionTypeProgressing,
		Status: v1alpha1.ConditionTrue,
		Reason: "ProcsAvailable", Message: "all procs are updated and available",
	}
	if exceeded {
		prog.Status = v1alpha1.ConditionFalse
		prog.Reason = v1alpha1.ReasonProgressDeadlineExceeded
		prog.Message = "rollout has made no progress for 600s: 0/1 updated, 0 available"
	}
	v1alpha1.SetStatusCondition(&d.Status.Conditions, prog)
	v1alpha1.SetStatusCondition(&d.Status.Conditions, v1alpha1.Condition{
		Type:   v1alpha1.ConditionTypeAvailable,
		Status: v1alpha1.ConditionTrue,
		Reason: "MinimumReplicasAvailable", Message: "minimum number of replicas is available",
	})
	if _, err := c.UpdateDaemonStatus(t.Context(), d); err != nil {
		t.Fatalf("UpdateDaemonStatus: %v", err)
	}
}

func TestRolloutStatusTerminalStates(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, webManifest)
	impctl(t, socket, false, "apply", "-f", manifest)

	setDaemonRolloutStatus(t, socket, false)
	out := impctl(t, socket, false, "rollout", "status", "web")
	if !strings.Contains(out, `daemon "web" successfully rolled out`) {
		t.Errorf("rollout status success output: %q", out)
	}

	setDaemonRolloutStatus(t, socket, true)
	out = impctl(t, socket, true, "rollout", "status", "web")
	if !strings.Contains(out, "no progress") {
		t.Errorf("rollout status exceeded output: %q", out)
	}

	out = impctl(t, socket, true, "rollout", "status", "ghost")
	if !strings.Contains(out, `no daemon named "ghost"`) {
		t.Errorf("rollout status unknown daemon: %q", out)
	}
}
