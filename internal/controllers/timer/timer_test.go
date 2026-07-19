// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Integration tests: the TimerController's Reconcile driven directly (not
// through a Runner) against the real client, apiserver, and etcl store
// over a real Unix socket, with real informers feeding the cache stores.
// The controller clock is fake and anchored near wall time (the server
// stamps real creation timestamps); ticks are crossed with Step.
package timer

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/controllers"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/pkg/client"
)

type harness struct {
	cl     *client.Client
	c      *Controller
	clk    *clock.Fake
	timers *cache.Store
	procs  *cache.Store
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	store, err := etcl.Open(filepath.Join(dir, "etcl.db"), nil)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := apiserver.New(apiserver.Config{
		Store:   store,
		Version: v1alpha1.VersionInfo{Version: "test"},
	})
	socket := filepath.Join(dir, "impd.sock")
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(l) //nolint:errcheck // ends with Close
	t.Cleanup(func() { hs.Close() })

	cl := client.New(socket)
	ctx := t.Context()
	tinf := cache.NewInformer(cl, v1alpha1.KindTimer, func(string) {}, nil)
	pinf := cache.NewInformer(cl, v1alpha1.KindProc, func(string) {}, nil)
	go tinf.Run(ctx)
	go pinf.Run(ctx)
	if err := tinf.WaitForSync(ctx); err != nil {
		t.Fatalf("timer informer WaitForSync: %v", err)
	}
	if err := pinf.WaitForSync(ctx); err != nil {
		t.Fatalf("proc informer WaitForSync: %v", err)
	}

	clk := clock.NewFake(time.Now())
	return &harness{
		cl:     cl,
		c:      New(cl, tinf.Store(), pinf.Store(), clk, nil),
		clk:    clk,
		timers: tinf.Store(),
		procs:  pinf.Store(),
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitCaughtUp blocks until both informer mirrors match the server.
func (h *harness) waitCaughtUp(t *testing.T) {
	t.Helper()
	waitFor(t, "informer caches to catch up", func() bool {
		want := map[string]string{}
		ts, _, err := h.cl.ListTimers(t.Context())
		if err != nil {
			return false
		}
		for _, x := range ts {
			want[v1alpha1.KindTimer+"/"+x.Metadata.Name] = x.Metadata.ResourceVersion
		}
		ps, _, err := h.cl.ListProcs(t.Context())
		if err != nil {
			return false
		}
		for _, p := range ps {
			want[v1alpha1.KindProc+"/"+p.Metadata.Name] = p.Metadata.ResourceVersion
		}
		got := map[string]string{}
		for kind, s := range map[string]*cache.Store{v1alpha1.KindTimer: h.timers, v1alpha1.KindProc: h.procs} {
			for _, key := range s.Keys() {
				raw, ok := s.GetByKey(key)
				if !ok {
					continue
				}
				var envelope struct {
					Metadata struct {
						Name            string `json:"name"`
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil {
					return false
				}
				got[kind+"/"+envelope.Metadata.Name] = envelope.Metadata.ResourceVersion
			}
		}
		return maps.Equal(want, got)
	})
}

func applyTimer(t *testing.T, h *harness, mutate func(*v1alpha1.Timer)) *v1alpha1.Timer {
	t.Helper()
	tm := &v1alpha1.Timer{
		Metadata: v1alpha1.ObjectMeta{Name: "backup"},
		Spec: v1alpha1.TimerSpec{
			Schedule: "@every 1h",
			Template: v1alpha1.ProcTemplate{
				Metadata: v1alpha1.TemplateMeta{Labels: map[string]string{"app": "backup"}},
				Spec:     v1alpha1.ProcTemplateSpec{Command: []string{"/bin/true"}},
			},
		},
	}
	if mutate != nil {
		mutate(tm)
	}
	applied, err := h.cl.ApplyTimer(t.Context(), tm)
	if err != nil {
		t.Fatalf("ApplyTimer: %v", err)
	}
	h.waitCaughtUp(t)
	return applied
}

func listRuns(t *testing.T, h *harness) []v1alpha1.Proc {
	t.Helper()
	ps, _, err := h.cl.ListProcs(t.Context())
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	return ps
}

func reconcile(t *testing.T, h *harness) error {
	t.Helper()
	err := h.c.Reconcile(t.Context(), "Timer/backup")
	h.waitCaughtUp(t)
	return err
}

func hasEvent(t *testing.T, h *harness, reason string) bool {
	t.Helper()
	evs, _, err := h.cl.ListEvents(t.Context())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for i := range evs {
		if evs[i].Reason == reason {
			return true
		}
	}
	return false
}

func TestTimerFiresOnDueTick(t *testing.T) {
	h := startHarness(t)
	applied := applyTimer(t, h, nil)

	// Nothing due yet: requeue until the first tick.
	err := reconcile(t, h)
	var requeue controllers.RequeueAfter
	if !errors.As(err, &requeue) {
		t.Fatalf("pre-tick reconcile = %v, want RequeueAfter", err)
	}
	if len(listRuns(t, h)) != 0 {
		t.Fatal("run created before the tick")
	}

	// Cross the tick (well within the 10s built-in deadline).
	h.clk.Step(time.Hour + 2*time.Second)
	if err := reconcile(t, h); !errors.As(err, &requeue) {
		t.Fatalf("due-tick reconcile = %v, want RequeueAfter", err)
	}

	runs := listRuns(t, h)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	p := runs[0]
	if p.Metadata.Labels[v1alpha1.LabelTimerName] != "backup" || p.Metadata.Labels["app"] != "backup" {
		t.Errorf("run labels = %v", p.Metadata.Labels)
	}
	if p.Metadata.Annotations[v1alpha1.AnnotationScheduledAt] == "" {
		t.Error("run missing scheduled-at annotation")
	}
	if len(p.Metadata.OwnerReferences) != 1 ||
		p.Metadata.OwnerReferences[0].Kind != v1alpha1.KindTimer ||
		p.Metadata.OwnerReferences[0].UID != applied.Metadata.UID {
		t.Errorf("run ownerReferences = %+v", p.Metadata.OwnerReferences)
	}
	if p.Spec.RestartPolicy != v1alpha1.RestartPolicyNever {
		t.Errorf("run restartPolicy = %q, want Never", p.Spec.RestartPolicy)
	}

	// Status is computed from the pass-start observation; the pass after
	// the fire sees the new run in the cache and reports it active.
	if err := reconcile(t, h); !errors.As(err, &requeue) {
		t.Fatalf("follow-up reconcile = %v, want RequeueAfter", err)
	}
	got, err := h.cl.GetTimer(t.Context(), "backup")
	if err != nil {
		t.Fatalf("GetTimer: %v", err)
	}
	if got.Status.LastScheduleTime.IsZero() || got.Status.ActiveProc != p.Metadata.Name {
		t.Errorf("status = %+v, want lastScheduleTime set and activeProc %s", got.Status, p.Metadata.Name)
	}
	if c := v1alpha1.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionTypeActive); c == nil || c.Status != v1alpha1.ConditionTrue {
		t.Errorf("Active condition = %+v, want True", c)
	}
	waitFor(t, "ScheduledRun event", func() bool { return hasEvent(t, h, v1alpha1.ReasonScheduledRun) })
}

func TestTimerForbidSkipsWhileActive(t *testing.T) {
	h := startHarness(t)
	applyTimer(t, h, nil)

	h.clk.Step(time.Hour + 2*time.Second)
	if err := reconcile(t, h); err != nil {
		var rq controllers.RequeueAfter
		if !errors.As(err, &rq) {
			t.Fatalf("first fire: %v", err)
		}
	}
	if len(listRuns(t, h)) != 1 {
		t.Fatal("first run not created")
	}

	// Next tick arrives while the run is still active (no execd here, so it
	// stays Pending forever): Forbid skips, advances the bookkeeping.
	h.clk.Step(time.Hour)
	var rq controllers.RequeueAfter
	if err := reconcile(t, h); !errors.As(err, &rq) {
		t.Fatalf("skip pass = %v, want RequeueAfter", err)
	}
	if n := len(listRuns(t, h)); n != 1 {
		t.Fatalf("Forbid created a second run (%d total)", n)
	}
	waitFor(t, "SkippedRun event", func() bool { return hasEvent(t, h, v1alpha1.ReasonSkippedRun) })
}

func TestTimerReplaceDeletesActive(t *testing.T) {
	h := startHarness(t)
	applyTimer(t, h, func(tm *v1alpha1.Timer) {
		tm.Spec.ConcurrencyPolicy = v1alpha1.ConcurrencyReplace
	})

	h.clk.Step(time.Hour + 2*time.Second)
	var rq controllers.RequeueAfter
	if err := reconcile(t, h); !errors.As(err, &rq) {
		t.Fatalf("first fire: %v", err)
	}
	first := listRuns(t, h)
	if len(first) != 1 {
		t.Fatal("first run not created")
	}

	// Next tick: Replace deletes the active run, then creates the new one
	// on the following pass.
	h.clk.Step(time.Hour)
	if err := reconcile(t, h); !errors.As(err, &rq) {
		t.Fatalf("replace pass = %v, want RequeueAfter", err)
	}
	if n := len(listRuns(t, h)); n != 0 {
		t.Fatalf("active run not replaced (still %d)", n)
	}
	if err := reconcile(t, h); !errors.As(err, &rq) {
		t.Fatalf("create-after-replace pass = %v, want RequeueAfter", err)
	}
	second := listRuns(t, h)
	if len(second) != 1 || second[0].Metadata.Name == first[0].Metadata.Name {
		t.Fatalf("replacement run wrong: %+v", second)
	}
}

func TestTimerMissedTickSkipped(t *testing.T) {
	h := startHarness(t)
	applyTimer(t, h, nil)

	// The tick came due 30s ago — past the 10s built-in deadline (impd was
	// "down"): skip with a MissedRun event, never fire late.
	h.clk.Step(time.Hour + 30*time.Second)
	var rq controllers.RequeueAfter
	if err := reconcile(t, h); !errors.As(err, &rq) {
		t.Fatalf("missed pass = %v, want RequeueAfter", err)
	}
	if len(listRuns(t, h)) != 0 {
		t.Fatal("missed tick fired anyway")
	}
	got, err := h.cl.GetTimer(t.Context(), "backup")
	if err != nil {
		t.Fatalf("GetTimer: %v", err)
	}
	if got.Status.LastScheduleTime.IsZero() {
		t.Error("missed tick did not advance lastScheduleTime")
	}
	waitFor(t, "MissedRun event", func() bool { return hasEvent(t, h, v1alpha1.ReasonMissedRun) })
}

func TestTimerSuspend(t *testing.T) {
	h := startHarness(t)
	applyTimer(t, h, func(tm *v1alpha1.Timer) {
		tm.Spec.Suspend = new(true)
	})
	h.clk.Step(2 * time.Hour)
	if err := reconcile(t, h); err != nil {
		t.Fatalf("suspended reconcile = %v, want nil", err)
	}
	if len(listRuns(t, h)) != 0 {
		t.Fatal("suspended timer fired")
	}
}

func TestTimerHistoryPruning(t *testing.T) {
	h := startHarness(t)
	applied := applyTimer(t, h, func(tm *v1alpha1.Timer) {
		tm.Spec.SuccessfulHistoryLimit = new(int32(1))
	})
	ctx := t.Context()

	// Seed three finished runs by hand, oldest first.
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	var names []string
	for i := range 3 {
		at := base.Add(time.Duration(i) * time.Hour)
		p := newRunProc(applied, at)
		created, err := h.cl.ApplyProc(ctx, p)
		if err != nil {
			t.Fatalf("seeding run: %v", err)
		}
		created.Status.Phase = v1alpha1.ProcPhaseSucceeded
		if _, err := h.cl.UpdateProcStatus(ctx, created); err != nil {
			t.Fatalf("marking run Succeeded: %v", err)
		}
		names = append(names, created.Metadata.Name)
	}
	h.waitCaughtUp(t)

	var rq controllers.RequeueAfter
	if err := reconcile(t, h); err != nil && !errors.As(err, &rq) {
		t.Fatalf("prune pass: %v", err)
	}
	runs := listRuns(t, h)
	if len(runs) != 1 || runs[0].Metadata.Name != names[2] {
		got := make([]string, 0, len(runs))
		for _, r := range runs {
			got = append(got, r.Metadata.Name)
		}
		t.Fatalf("after pruning got %v, want only newest %s", got, names[2])
	}
}
