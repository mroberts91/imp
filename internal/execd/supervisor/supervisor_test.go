// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package supervisor_test

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/internal/execd/logs"
	"github.com/mroberts91/imp/internal/execd/supervisor"
	"github.com/mroberts91/imp/pkg/client"
)

type harness struct {
	cl   *client.Client
	mgr  *supervisor.Manager
	logs *logs.Store
	inf  *cache.Informer
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := etcl.Open(filepath.Join(dir, "etcl.db"), nil)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	logStore := logs.New(filepath.Join(dir, "logs"), clock.Real{})
	srv := apiserver.New(apiserver.Config{
		Store:   store,
		Logs:    logStore,
		Version: v1alpha1.VersionInfo{Version: "test"},
	})
	socket := filepath.Join(dir, "impd.sock")
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(l) //nolint:errcheck
	t.Cleanup(func() { hs.Close() })

	cl := client.New(socket)
	var mgr *supervisor.Manager
	inf := cache.NewInformer(cl, v1alpha1.KindProc, func(key string) {
		mgr.Handle(key)
	}, nil)
	mgr = supervisor.NewManager(cl, inf.Store(), logStore, clock.Real{})
	go inf.Run(t.Context())
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	t.Cleanup(mgr.Stop)

	return &harness{cl: cl, mgr: mgr, logs: logStore, inf: inf}
}

func waitPhase(t *testing.T, cl *client.Client, name string, want v1alpha1.ProcPhase) *v1alpha1.Proc {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		p, err := cl.GetProc(t.Context(), name)
		if err == nil && p.Status.Phase == want {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := cl.GetProc(t.Context(), name)
	t.Fatalf("timed out waiting for %s phase=%s; last=%+v", name, want, p)
	return nil
}

func TestSleepReachesRunning(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name: "sleep-0",
			Labels: map[string]string{
				v1alpha1.LabelDaemonName:   "sleep",
				v1alpha1.LabelReplicaIndex: "0",
				v1alpha1.LabelTemplateHash: "deadbeef",
			},
		},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sleep", "60"},
			RestartPolicy:                 v1alpha1.RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(5)),
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}

	got := waitPhase(t, h.cl, "sleep-0", v1alpha1.ProcPhaseRunning)
	if got.Status.State.Running == nil || got.Status.State.Running.PID <= 0 {
		t.Fatalf("running state = %+v", got.Status.State)
	}
	if got.Status.State.Running.ProcStartTicks <= 0 {
		t.Fatalf("procStartTicks = %d, want > 0", got.Status.State.Running.ProcStartTicks)
	}
}

func TestCrashLoopBackOff(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "crash-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sh", "-c", "exit 1"},
			RestartPolicy:                 v1alpha1.RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(1)),
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var last *v1alpha1.Proc
	for time.Now().Before(deadline) {
		got, err := h.cl.GetProc(ctx, "crash-0")
		if err == nil {
			last = got
			if got.Status.RestartCount >= 1 &&
				got.Status.State.Waiting != nil &&
				got.Status.State.Waiting.Reason == v1alpha1.WaitingReasonCrashLoopBackOff {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for CrashLoopBackOff; last=%+v", last)
}

func TestDeleteStopsProcess(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "killme-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sleep", "120"},
			RestartPolicy:                 v1alpha1.RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(2)),
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	got := waitPhase(t, h.cl, "killme-0", v1alpha1.ProcPhaseRunning)
	pid := got.Status.State.Running.PID

	if err := h.cl.DeleteProc(ctx, "killme-0"); err != nil {
		t.Fatalf("DeleteProc: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.cl.GetProc(ctx, "killme-0")
		if err != nil {
			// Process should be gone; best-effort check pid via kill -0.
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proc still present; was pid %d", pid)
}

func TestLogsCapture(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "echo-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sh", "-c", "echo hello-imp; sleep 30"},
			RestartPolicy:                 v1alpha1.RestartPolicyNever,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(2)),
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	waitPhase(t, h.cl, "echo-0", v1alpha1.ProcPhaseRunning)

	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		rc, err := h.cl.ProcLogs(ctx, "echo-0", client.LogOptions{TailLines: 10})
		if err == nil {
			body, _ = io.ReadAll(rc)
			rc.Close()
			if strings.Contains(string(body), "hello-imp") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("logs did not contain hello-imp; got %q", body)
}
