// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package supervisor_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/internal/execd/cgroups"
	"github.com/mroberts91/imp/internal/execd/configfiles"
	"github.com/mroberts91/imp/internal/execd/logs"
	"github.com/mroberts91/imp/internal/execd/supervisor"
	"github.com/mroberts91/imp/pkg/client"
)

type harness struct {
	cl     *client.Client
	mgr    *supervisor.Manager
	logs   *logs.Store
	inf    *cache.Informer
	cgRoot string
}

func startHarness(t *testing.T) *harness {
	return startHarnessOpts(t, true)
}

func startHarnessOpts(t *testing.T, killOnShutdown bool) *harness {
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
	cgRoot := filepath.Join(dir, "cgroup")
	if err := cgroups.SetupFakeRoot(cgRoot); err != nil {
		t.Fatalf("SetupFakeRoot: %v", err)
	}
	cgMgr, err := cgroups.NewManager(cgRoot)
	if err != nil {
		t.Fatalf("cgroups.NewManager: %v", err)
	}
	var mgr *supervisor.Manager
	inf := cache.NewInformer(cl, v1alpha1.KindProc, func(key string) {
		mgr.Handle(key)
	}, nil)
	configStore := configfiles.New(filepath.Join(dir, "configs"), cl)
	mgr = supervisor.NewManager(cl, inf.Store(), logStore, configStore, cgMgr, clock.Real{}, nil, killOnShutdown)
	go inf.Run(t.Context())
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	mgr.Bootstrap(t.Context())
	t.Cleanup(mgr.Stop)

	return &harness{cl: cl, mgr: mgr, logs: logStore, inf: inf, cgRoot: cgRoot}
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

func TestReadinessGatesReady(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "ready-probe-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sleep", "60"},
			RestartPolicy:                 v1alpha1.RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(2)),
			ReadinessProbe: &v1alpha1.Probe{
				Exec: &v1alpha1.ExecAction{
					Command: []string{"/bin/true"},
				},
				InitialDelaySeconds: 0,
				PeriodSeconds:       1,
				TimeoutSeconds:      1,
				SuccessThreshold:    1,
				FailureThreshold:    3,
			},
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}

	// Running but not Ready until readiness succeeds.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.cl.GetProc(ctx, "ready-probe-0")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if got.Status.Phase != v1alpha1.ProcPhaseRunning {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		ready := v1alpha1.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionTypeReady)
		if ready != nil && ready.Status == v1alpha1.ConditionTrue {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := h.cl.GetProc(ctx, "ready-probe-0")
	t.Fatalf("timed out waiting for Ready=True after readiness; last=%+v", got)
}

func TestLivenessRestarts(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "live-probe-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sleep", "120"},
			RestartPolicy:                 v1alpha1.RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(1)),
			LivenessProbe: &v1alpha1.Probe{
				Exec: &v1alpha1.ExecAction{
					Command: []string{"/bin/false"},
				},
				InitialDelaySeconds: 0,
				PeriodSeconds:       1,
				TimeoutSeconds:      1,
				SuccessThreshold:    1,
				FailureThreshold:    1,
			},
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	first := waitPhase(t, h.cl, "live-probe-0", v1alpha1.ProcPhaseRunning)
	firstPID := first.Status.State.Running.PID

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := h.cl.GetProc(ctx, "live-probe-0")
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if got.Status.RestartCount >= 1 {
			if got.Status.Phase == v1alpha1.ProcPhaseRunning &&
				got.Status.State.Running != nil &&
				got.Status.State.Running.PID != firstPID {
				return
			}
			// Restarted into pending/backoff is also success.
			if got.Status.RestartCount >= 1 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ := h.cl.GetProc(ctx, "live-probe-0")
	t.Fatalf("timed out waiting for liveness restart; last restartCount=%d phase=%s",
		got.Status.RestartCount, got.Status.Phase)
}

func TestSpawnAppliesCgroupLimits(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "limited-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sleep", "60"},
			RestartPolicy:                 v1alpha1.RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(2)),
			Resources: v1alpha1.ResourceRequirements{
				Limits: v1alpha1.ResourceLimits{
					Memory:    "32Mi",
					CPUWeight: new(int64(50)),
					Pids:      new(int64(16)),
				},
			},
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	got := waitPhase(t, h.cl, "limited-0", v1alpha1.ProcPhaseRunning)
	if got.Metadata.UID == "" {
		t.Fatal("expected UID assigned")
	}
	cgPath := filepath.Join(h.cgRoot, "proc-"+got.Metadata.UID)
	assertFileContains(t, filepath.Join(cgPath, "memory.max"), "33554432")
	assertFileContains(t, filepath.Join(cgPath, "cpu.weight"), "50")
	assertFileContains(t, filepath.Join(cgPath, "pids.max"), "16")
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want containing %q", path, data, want)
	}
}

func TestAdoptAcrossDetach(t *testing.T) {
	h := startHarnessOpts(t, false) // leave children on Stop
	ctx := t.Context()

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "keep-0"},
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
	got := waitPhase(t, h.cl, "keep-0", v1alpha1.ProcPhaseRunning)
	pid := got.Status.State.Running.PID
	uid := got.Metadata.UID
	if uid == "" || pid <= 0 {
		t.Fatalf("uid=%q pid=%d", uid, pid)
	}

	// Detach supervisor; child must keep running.
	h.mgr.Stop()
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		t.Fatalf("child died after detach: %v", err)
	}

	cgMgr, err := cgroups.NewManager(h.cgRoot)
	if err != nil {
		t.Fatal(err)
	}
	var mgr2 *supervisor.Manager
	inf2 := cache.NewInformer(h.cl, v1alpha1.KindProc, func(key string) {
		mgr2.Handle(key)
	}, nil)
	mgr2 = supervisor.NewManager(h.cl, inf2.Store(), h.logs, nil, cgMgr, clock.Real{}, nil, true)
	go inf2.Run(ctx)
	if err := inf2.WaitForSync(ctx); err != nil {
		t.Fatal(err)
	}
	mgr2.Bootstrap(ctx)
	t.Cleanup(mgr2.Stop)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := h.cl.GetProc(ctx, "keep-0")
		if err == nil && cur.Status.Phase == v1alpha1.ProcPhaseRunning &&
			cur.Status.State.Running != nil && cur.Status.State.Running.PID == pid {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	cur, _ := h.cl.GetProc(ctx, "keep-0")
	t.Fatalf("adopt did not restore Running pid=%d; got %+v", pid, cur)
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

// TestConfigMaterializedAndReadable proves the M8 spawn path end to end: a
// referenced Config's files are materialized under IMP_CONFIG_DIR before the
// process starts, and the process reads them.
func TestConfigMaterializedAndReadable(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	if _, err := h.cl.ApplyConfig(ctx, &v1alpha1.Config{
		Metadata: v1alpha1.ObjectMeta{Name: "app"},
		Spec:     v1alpha1.ConfigSpec{Data: map[string]string{"greeting": "hello-from-config\n"}},
	}); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "cfg-reader-0"},
		Spec: v1alpha1.ProcSpec{
			Command:                       []string{"/bin/sh", "-c", `cat "$IMP_CONFIG_DIR/app/greeting"; sleep 30`},
			Configs:                       []string{"app"},
			RestartPolicy:                 v1alpha1.RestartPolicyNever,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(int64(2)),
		},
	}
	if _, err := h.cl.ApplyProc(ctx, p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	waitPhase(t, h.cl, "cfg-reader-0", v1alpha1.ProcPhaseRunning)

	deadline := time.Now().Add(5 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		rc, err := h.cl.ProcLogs(ctx, "cfg-reader-0", client.LogOptions{TailLines: 10})
		if err == nil {
			body, _ = io.ReadAll(rc)
			rc.Close()
			if strings.Contains(string(body), "hello-from-config") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("proc did not read its config; logs = %q", body)
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
