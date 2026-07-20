// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Integration tests: the DaemonController's Reconcile driven directly (not
// through a Runner) against the real client, apiserver, and etcl store
// over a real Unix socket, with real informers feeding the cache stores.
package daemon

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/controllers"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/internal/queue"
	"github.com/mroberts91/imp/pkg/client"
)

func startServer(t *testing.T) *client.Client {
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

	return client.New(socket)
}

// harness is one running stack: server, client, both informers, and the
// controller under test on a fake clock.
type harness struct {
	cl      *client.Client
	c       *Controller
	clk     *clock.Fake
	daemons *cache.Store
	procs   *cache.Store
	configs *cache.Store
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	cl := startServer(t)
	ctx := t.Context()

	dinf := cache.NewInformer(cl, v1alpha1.KindDaemon, func(string) {}, nil)
	pinf := cache.NewInformer(cl, v1alpha1.KindProc, func(string) {}, nil)
	cinf := cache.NewInformer(cl, v1alpha1.KindConfig, func(string) {}, nil)
	go dinf.Run(ctx)
	go pinf.Run(ctx)
	go cinf.Run(ctx)
	for name, inf := range map[string]*cache.Informer{"daemon": dinf, "proc": pinf, "config": cinf} {
		if err := inf.WaitForSync(ctx); err != nil {
			t.Fatalf("%s informer WaitForSync: %v", name, err)
		}
	}

	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	return &harness{
		cl:      cl,
		c:       New(cl, dinf.Store(), pinf.Store(), cinf.Store(), clk, nil),
		clk:     clk,
		daemons: dinf.Store(),
		procs:   pinf.Store(),
		configs: cinf.Store(),
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

// serverSnapshot maps "Kind/name" -> resourceVersion for every Daemon and
// Proc on the server. Events are deliberately excluded.
func (h *harness) serverSnapshot(t *testing.T) map[string]string {
	t.Helper()
	ctx := t.Context()
	snap := map[string]string{}
	ds, _, err := h.cl.ListDaemons(ctx)
	if err != nil {
		t.Fatalf("ListDaemons: %v", err)
	}
	for _, d := range ds {
		snap[v1alpha1.KindDaemon+"/"+d.Metadata.Name] = d.Metadata.ResourceVersion
	}
	ps, _, err := h.cl.ListProcs(ctx)
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	for _, p := range ps {
		snap[v1alpha1.KindProc+"/"+p.Metadata.Name] = p.Metadata.ResourceVersion
	}
	cs, _, err := h.cl.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	for _, cfg := range cs {
		snap[v1alpha1.KindConfig+"/"+cfg.Metadata.Name] = cfg.Metadata.ResourceVersion
	}
	return snap
}

// cacheSnapshot is serverSnapshot's shape read from the informer stores.
func (h *harness) cacheSnapshot(t *testing.T) map[string]string {
	t.Helper()
	snap := map[string]string{}
	for _, store := range []*cache.Store{h.daemons, h.procs, h.configs} {
		for _, raw := range store.List() {
			var envelope struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name            string `json:"name"`
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatalf("decoding cached object: %v", err)
			}
			snap[envelope.Kind+"/"+envelope.Metadata.Name] = envelope.Metadata.ResourceVersion
		}
	}
	return snap
}

// waitCaughtUp blocks until both informer mirrors match the server. Only
// the test mutates the server, so the comparison cannot race.
func (h *harness) waitCaughtUp(t *testing.T) {
	t.Helper()
	waitFor(t, "informer caches to catch up to the server", func() bool {
		return maps.Equal(h.cacheSnapshot(t), h.serverSnapshot(t))
	})
}

// reconcileUntilSteady drives Reconcile until a pass changes nothing on
// the server (no proc mutations, status write absorbed as a no-op). A
// RequeueAfter with an unchanged server also counts as steady: since M6 an
// incomplete rollout at rest always schedules a progress-deadline (or
// availability) wake-up, which is a timer, not pending work.
func (h *harness) reconcileUntilSteady(t *testing.T, key string) {
	t.Helper()
	for range 10 {
		h.waitCaughtUp(t)
		before := h.serverSnapshot(t)
		err := h.c.Reconcile(t.Context(), key)
		var requeue controllers.RequeueAfter
		requeued := errors.As(err, &requeue)
		if err != nil && !requeued {
			t.Fatalf("Reconcile(%s): %v", key, err)
		}
		h.waitCaughtUp(t)
		if maps.Equal(before, h.serverSnapshot(t)) {
			return
		}
	}
	t.Fatalf("no steady state after 10 passes for %s", key)
}

func applyDaemon(t *testing.T, h *harness, replicas int32) *v1alpha1.Daemon {
	t.Helper()
	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas: new(replicas),
			Template: v1alpha1.ProcTemplate{
				Metadata: v1alpha1.TemplateMeta{
					Labels:      map[string]string{"app": "web"},
					Annotations: map[string]string{"team": "core"},
				},
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	applied, err := h.cl.ApplyDaemon(t.Context(), d)
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	return applied
}

func listProcsSorted(t *testing.T, h *harness) []v1alpha1.Proc {
	t.Helper()
	ps, _, err := h.cl.ListProcs(t.Context())
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	slices.SortFunc(ps, func(a, b v1alpha1.Proc) int {
		return strings.Compare(a.Metadata.Name, b.Metadata.Name)
	})
	return ps
}

func findEvent(t *testing.T, h *harness, reason string) *v1alpha1.Event {
	t.Helper()
	evs, _, err := h.cl.ListEvents(t.Context())
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for i := range evs {
		if evs[i].Reason == reason {
			return &evs[i]
		}
	}
	return nil
}

func TestReconcileExpandsDaemonToProcs(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemon(t, h, 2)

	h.reconcileUntilSteady(t, "Daemon/web")

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	hash := v1alpha1.HashProcTemplate(&d.Spec.Template)

	procs := listProcsSorted(t, h)
	if len(procs) != 2 {
		t.Fatalf("got %d procs, want 2", len(procs))
	}
	for i, p := range procs {
		wantName := "web-" + strconv.Itoa(i) + "-" + hash
		if p.Metadata.Name != wantName {
			t.Errorf("proc %d name = %q, want %q", i, p.Metadata.Name, wantName)
		}
		wantLabels := map[string]string{
			"app":                      "web",
			v1alpha1.LabelDaemonName:   "web",
			v1alpha1.LabelTemplateHash: hash,
			v1alpha1.LabelReplicaIndex: strconv.Itoa(i),
		}
		if diff := cmp.Diff(wantLabels, p.Metadata.Labels); diff != "" {
			t.Errorf("proc %d labels mismatch (-want +got):\n%s", i, diff)
		}
		if got := p.Metadata.Annotations["team"]; got != "core" {
			t.Errorf("proc %d annotation team = %q, want %q", i, got, "core")
		}
		wantOwner := []v1alpha1.OwnerReference{{
			APIVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindDaemon,
			Name:       "web",
			UID:        d.Metadata.UID,
		}}
		if diff := cmp.Diff(wantOwner, p.Metadata.OwnerReferences); diff != "" {
			t.Errorf("proc %d ownerReferences mismatch (-want +got):\n%s", i, diff)
		}
		if diff := cmp.Diff(d.Spec.Template.Spec, p.Spec); diff != "" {
			t.Errorf("proc %d spec mismatch with template (-want +got):\n%s", i, diff)
		}
	}

	ev := findEvent(t, h, v1alpha1.ReasonCreated)
	if ev == nil {
		t.Fatal("no Created event recorded")
	}
	if ev.Regarding.Kind != v1alpha1.KindDaemon || ev.Regarding.Name != "web" || ev.Regarding.UID != d.Metadata.UID {
		t.Errorf("Created event regarding = %+v, want Daemon/web with UID %s", ev.Regarding, d.Metadata.UID)
	}
	if ev.Type != v1alpha1.EventTypeNormal || ev.ReportingComponent != ReportingComponent {
		t.Errorf("Created event type/component = %s/%s", ev.Type, ev.ReportingComponent)
	}
}

func TestReconcileScaleDownDeletesHighestOrdinals(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemon(t, h, 2)
	h.reconcileUntilSteady(t, "Daemon/web")

	procs := listProcsSorted(t, h)
	if len(procs) != 2 {
		t.Fatalf("got %d procs before scale-down, want 2", len(procs))
	}
	web0 := procs[0]

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	d.Spec.Replicas = new(int32(1))
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon(scale down): %v", err)
	}

	h.reconcileUntilSteady(t, "Daemon/web")

	procs = listProcsSorted(t, h)
	if len(procs) != 1 {
		t.Fatalf("got %d procs after scale-down, want 1", len(procs))
	}
	if procs[0].Metadata.Name != web0.Metadata.Name {
		t.Errorf("survivor = %q, want lowest ordinal %q", procs[0].Metadata.Name, web0.Metadata.Name)
	}
	if procs[0].Metadata.ResourceVersion != web0.Metadata.ResourceVersion {
		t.Errorf("web-0 resourceVersion changed %s -> %s; scale-down must not touch survivors",
			web0.Metadata.ResourceVersion, procs[0].Metadata.ResourceVersion)
	}
	if findEvent(t, h, v1alpha1.ReasonScalingReplicas) == nil {
		t.Error("no ScalingReplicas event recorded")
	}
}

func TestReconcileTemplateChangeRecreates(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemon(t, h, 2)
	h.reconcileUntilSteady(t, "Daemon/web")

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)

	d.Spec.Template.Spec.Command = []string{"/bin/sleep", "120"}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon(template change): %v", err)
	}
	h.waitCaughtUp(t)

	// Pass one: all stale procs deleted, nothing created yet, requeue.
	err = h.c.Reconcile(ctx, "Daemon/web")
	var requeue controllers.RequeueAfter
	if !errors.As(err, &requeue) {
		t.Fatalf("Reconcile after template change returned %v, want RequeueAfter", err)
	}
	if requeue.After <= 0 {
		t.Errorf("RequeueAfter.After = %v, want > 0", requeue.After)
	}
	if procs := listProcsSorted(t, h); len(procs) != 0 {
		t.Fatalf("got %d procs after recreate pass one, want 0", len(procs))
	}
	if findEvent(t, h, v1alpha1.ReasonTemplateChanged) == nil {
		t.Error("no TemplateChanged event recorded")
	}

	// Later passes: replacements with the new hash.
	h.reconcileUntilSteady(t, "Daemon/web")

	d, err = h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	newHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	if newHash == oldHash {
		t.Fatalf("template hash did not change (%s)", newHash)
	}
	procs := listProcsSorted(t, h)
	if len(procs) != 2 {
		t.Fatalf("got %d procs after recreate, want 2", len(procs))
	}
	for i, p := range procs {
		wantName := "web-" + strconv.Itoa(i) + "-" + newHash
		if p.Metadata.Name != wantName {
			t.Errorf("proc %d name = %q, want %q", i, p.Metadata.Name, wantName)
		}
		if got := p.Metadata.Labels[v1alpha1.LabelTemplateHash]; got != newHash {
			t.Errorf("proc %d template-hash label = %q, want %q", i, got, newHash)
		}
	}
}

func TestReconcileStatusRollupConvergesWithoutChurn(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemon(t, h, 2)
	h.reconcileUntilSteady(t, "Daemon/web")

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if d.Status.ObservedGeneration != d.Metadata.Generation {
		t.Errorf("observedGeneration = %d, want generation %d", d.Status.ObservedGeneration, d.Metadata.Generation)
	}
	if d.Status.Replicas != 2 || d.Status.UpdatedReplicas != 2 {
		t.Errorf("replicas/updatedReplicas = %d/%d, want 2/2", d.Status.Replicas, d.Status.UpdatedReplicas)
	}
	// Honest M1: nothing writes Proc status yet, so nothing is Ready.
	if d.Status.ReadyReplicas != 0 {
		t.Errorf("readyReplicas = %d, want 0", d.Status.ReadyReplicas)
	}

	avail := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeAvailable)
	if avail == nil {
		t.Fatal("no Available condition")
	}
	if avail.Status != v1alpha1.ConditionFalse || avail.Reason != reasonMinReplicasUnavailable {
		t.Errorf("Available = %s/%s, want False/%s", avail.Status, avail.Reason, reasonMinReplicasUnavailable)
	}
	if avail.LastTransitionTime.IsZero() {
		t.Error("Available.lastTransitionTime is zero")
	}
	if avail.ObservedGeneration != d.Metadata.Generation {
		t.Errorf("Available.observedGeneration = %d, want %d", avail.ObservedGeneration, d.Metadata.Generation)
	}
	prog := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
	if prog == nil {
		t.Fatal("no Progressing condition")
	}
	if prog.Status != v1alpha1.ConditionTrue || prog.Reason != reasonProcsUpdated {
		t.Errorf("Progressing = %s/%s, want True/%s", prog.Status, prog.Reason, reasonProcsUpdated)
	}

	rv := d.Metadata.ResourceVersion
	availTransition := avail.LastTransitionTime

	// A later pass with the clock advanced must be a no-op: same
	// resourceVersion (etcl absorbed the identical body) and an
	// untouched transition time. (The pass returns a deadline requeue —
	// the rollout is incomplete — which is a timer, not a write.)
	h.clk.Step(90 * time.Second)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	d2, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon after second pass: %v", err)
	}
	if d2.Metadata.ResourceVersion != rv {
		t.Errorf("resourceVersion churned %s -> %s on a no-op pass", rv, d2.Metadata.ResourceVersion)
	}
	avail2 := v1alpha1.FindStatusCondition(d2.Status.Conditions, v1alpha1.ConditionTypeAvailable)
	if avail2 == nil {
		t.Fatal("Available condition vanished")
	}
	if !avail2.LastTransitionTime.Equal(availTransition) {
		t.Errorf("Available.lastTransitionTime moved %v -> %v on a no-op pass",
			availTransition, avail2.LastTransitionTime)
	}
}

func TestReconcileAbsentDaemonIsNil(t *testing.T) {
	h := startHarness(t)
	if err := h.c.Reconcile(t.Context(), "Daemon/ghost"); err != nil {
		t.Fatalf("Reconcile of absent daemon = %v, want nil", err)
	}
}

func applyDaemonOpts(t *testing.T, h *harness, replicas int32, strategy v1alpha1.UpdateStrategy) *v1alpha1.Daemon {
	t.Helper()
	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas:       new(replicas),
			UpdateStrategy: strategy,
			Template: v1alpha1.ProcTemplate{
				Metadata: v1alpha1.TemplateMeta{
					Labels: map[string]string{"app": "web"},
				},
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	applied, err := h.cl.ApplyDaemon(t.Context(), d)
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	return applied
}

func markProcsReady(t *testing.T, h *harness, names ...string) {
	t.Helper()
	ctx := t.Context()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	procs, _, err := h.cl.ListProcs(ctx)
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	for i := range procs {
		p := &procs[i]
		if len(want) > 0 && !want[p.Metadata.Name] {
			continue
		}
		p.Status.Phase = v1alpha1.ProcPhaseRunning
		v1alpha1.SetStatusCondition(&p.Status.Conditions, v1alpha1.Condition{
			Type:   v1alpha1.ConditionTypeReady,
			Status: v1alpha1.ConditionTrue,
			Reason: "TestReady",
		})
		if _, err := h.cl.UpdateProcStatus(ctx, p); err != nil {
			t.Fatalf("UpdateProcStatus(%s): %v", p.Metadata.Name, err)
		}
	}
	h.waitCaughtUp(t)
}

func TestReconcileRollingUpdateSequential(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemonOpts(t, h, 3, v1alpha1.UpdateStrategy{Type: v1alpha1.UpdateStrategyRollingUpdate})
	h.reconcileUntilSteady(t, "Daemon/web")

	procs := listProcsSorted(t, h)
	if len(procs) != 3 {
		t.Fatalf("got %d procs, want 3", len(procs))
	}
	markProcsReady(t, h) // all

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)

	d.Spec.Template.Spec.Command = []string{"/bin/sleep", "120"}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon(template change): %v", err)
	}
	h.waitCaughtUp(t)

	// Pass 1: delete highest ordinal only; lower stale procs remain.
	err = h.c.Reconcile(ctx, "Daemon/web")
	var requeue controllers.RequeueAfter
	if !errors.As(err, &requeue) {
		t.Fatalf("Reconcile after template change returned %v, want RequeueAfter", err)
	}
	h.waitCaughtUp(t)
	procs = listProcsSorted(t, h)
	if len(procs) != 2 {
		t.Fatalf("after first rolling delete got %d procs, want 2", len(procs))
	}
	for _, p := range procs {
		idx := p.Metadata.Labels[v1alpha1.LabelReplicaIndex]
		if idx == "2" {
			t.Fatalf("ordinal 2 still present after first rolling delete: %s", p.Metadata.Name)
		}
		if p.Metadata.Labels[v1alpha1.LabelTemplateHash] != oldHash {
			t.Errorf("unexpected hash on remaining proc %s", p.Metadata.Name)
		}
	}
	if findEvent(t, h, v1alpha1.ReasonTemplateChanged) == nil {
		t.Error("no TemplateChanged event recorded")
	}

	// Pass 2: create replacement at ordinal 2; do not delete ordinal 1 yet
	// because the new ordinal-2 Proc is not Ready.
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile create replacement: %v", err)
	}
	h.waitCaughtUp(t)
	d, err = h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	newHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	procs = listProcsSorted(t, h)
	var new2, stale1 bool
	for _, p := range procs {
		idx := p.Metadata.Labels[v1alpha1.LabelReplicaIndex]
		hash := p.Metadata.Labels[v1alpha1.LabelTemplateHash]
		switch {
		case idx == "2" && hash == newHash:
			new2 = true
		case idx == "1" && hash == oldHash:
			stale1 = true
		case idx == "1" && hash == newHash:
			t.Fatalf("ordinal 1 already rolled while ordinal 2 not Ready: %s", p.Metadata.Name)
		}
	}
	if !new2 || !stale1 {
		t.Fatalf("after create pass: new2=%v stale1=%v procs=%v", new2, stale1, procNames(procs))
	}

	// Mark only the new ordinal-2 Ready; next pass may delete ordinal 1.
	markProcsReady(t, h, "web-2-"+newHash)
	err = h.c.Reconcile(ctx, "Daemon/web")
	if !errors.As(err, &requeue) {
		t.Fatalf("Reconcile after Ready returned %v, want RequeueAfter (delete ordinal 1)", err)
	}
	h.waitCaughtUp(t)
	for _, p := range listProcsSorted(t, h) {
		if p.Metadata.Labels[v1alpha1.LabelReplicaIndex] == "1" &&
			p.Metadata.Labels[v1alpha1.LabelTemplateHash] == oldHash {
			t.Fatalf("stale ordinal 1 still present after its rolling delete")
		}
	}

	// Drive to completion: create, mark Ready, roll next ordinal. Requeues
	// are expected (delete passes and the deadline timer) and never skip
	// the mark-Ready step.
	for range 15 {
		h.waitCaughtUp(t)
		if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		h.waitCaughtUp(t)
		markProcsReady(t, h)
		d, err := h.cl.GetDaemon(ctx, "web")
		if err != nil {
			t.Fatalf("GetDaemon: %v", err)
		}
		procs = listProcsSorted(t, h)
		if len(procs) != 3 || d.Status.UpdatedReplicas != 3 || d.Status.ReadyReplicas != 3 {
			continue
		}
		allNew := true
		for _, p := range procs {
			if p.Metadata.Labels[v1alpha1.LabelTemplateHash] != newHash || !procReady(&p) {
				allNew = false
				break
			}
		}
		if !allNew {
			continue
		}
		// One more pass so rollup sees Ready.
		if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
			t.Fatalf("final Reconcile: %v", err)
		}
		h.waitCaughtUp(t)
		d, err = h.cl.GetDaemon(ctx, "web")
		if err != nil {
			t.Fatalf("GetDaemon: %v", err)
		}
		prog := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
		if prog == nil || prog.Reason != reasonProcsAvailable {
			t.Fatalf("Progressing = %+v, want Reason=%s", prog, reasonProcsAvailable)
		}
		return
	}
	t.Fatalf("rolling update did not complete; procs=%v", procNames(listProcsSorted(t, h)))
}

// rollingStrategy builds a RollingUpdate strategy with an explicit
// maxUnavailable and partition (M9-e).
func rollingStrategy(maxUnavailable, partition int32) v1alpha1.UpdateStrategy {
	return v1alpha1.UpdateStrategy{
		Type: v1alpha1.UpdateStrategyRollingUpdate,
		RollingUpdate: &v1alpha1.RollingUpdateDaemonStrategy{
			MaxUnavailable: new(maxUnavailable),
			Partition:      new(partition),
		},
	}
}

// staleCount counts procs still labeled with oldHash.
func staleCount(procs []v1alpha1.Proc, oldHash string) int {
	n := 0
	for i := range procs {
		if procs[i].Metadata.Labels[v1alpha1.LabelTemplateHash] == oldHash {
			n++
		}
	}
	return n
}

// rollTemplate mutates the Daemon's command and applies it, returning the new
// template hash.
func rollTemplate(t *testing.T, h *harness) string {
	t.Helper()
	ctx := t.Context()
	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	d.Spec.Template.Spec.Command = []string{"/bin/sleep", "120"}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon(template change): %v", err)
	}
	h.waitCaughtUp(t)
	d, err = h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	return v1alpha1.HashProcTemplate(&d.Spec.Template)
}

// TestReconcileRollingUpdateMaxUnavailableOnePin pins the M9-e identity: an
// explicit maxUnavailable:1 rolls byte-for-byte like the nil default — exactly
// one stale (the highest ordinal) deleted per pass.
func TestReconcileRollingUpdateMaxUnavailableOnePin(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemonOpts(t, h, 3, rollingStrategy(1, 0))
	h.reconcileUntilSteady(t, "Daemon/web")
	markProcsReady(t, h)

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	_ = rollTemplate(t, h)

	var requeue controllers.RequeueAfter
	if err := h.c.Reconcile(ctx, "Daemon/web"); !errors.As(err, &requeue) {
		t.Fatalf("pass 1 returned %v, want RequeueAfter", err)
	}
	h.waitCaughtUp(t)
	procs := listProcsSorted(t, h)
	if len(procs) != 2 || staleCount(procs, oldHash) != 2 {
		t.Fatalf("explicit maxUnavailable:1 deleted != 1 proc: procs=%v", procNames(procs))
	}
	for _, p := range procs {
		if p.Metadata.Labels[v1alpha1.LabelReplicaIndex] == "2" {
			t.Fatalf("ordinal 2 (highest) should have been the single deletion")
		}
	}
}

// TestReconcileRollingUpdateMaxUnavailableBatched pins M9-e: replicas=4 with
// maxUnavailable:2 deletes two stale at once, then holds while the two
// replacements are unavailable (the budget-exhausted "freeze"), then rolls the
// next pair — never more than two ordinals down at once.
func TestReconcileRollingUpdateMaxUnavailableBatched(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemonOpts(t, h, 4, rollingStrategy(2, 0))
	h.reconcileUntilSteady(t, "Daemon/web")
	markProcsReady(t, h)

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	newHash := rollTemplate(t, h)

	// Pass 1: exactly two stale deleted (the two highest ordinals).
	var requeue controllers.RequeueAfter
	if err := h.c.Reconcile(ctx, "Daemon/web"); !errors.As(err, &requeue) {
		t.Fatalf("pass 1 returned %v, want RequeueAfter", err)
	}
	h.waitCaughtUp(t)
	procs := listProcsSorted(t, h)
	if len(procs) != 2 || staleCount(procs, oldHash) != 2 {
		t.Fatalf("after pass 1: procs=%v, want 2 stale (ordinals 0,1)", procNames(procs))
	}
	for _, p := range procs {
		if idx := p.Metadata.Labels[v1alpha1.LabelReplicaIndex]; idx == "2" || idx == "3" {
			t.Fatalf("ordinal %s not deleted in pass 1", idx)
		}
	}

	// Pass 2: creation pass fills ordinals 2,3 with new (not-ready) Procs.
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	h.waitCaughtUp(t)
	if procs = listProcsSorted(t, h); len(procs) != 4 || staleCount(procs, oldHash) != 2 {
		t.Fatalf("after pass 2: procs=%v, want 2 stale + 2 new", procNames(procs))
	}

	// Pass 3: the two replacements are unavailable, so the budget is exhausted
	// by unavailability — no further stale deleted (the freeze).
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	h.waitCaughtUp(t)
	if staleCount(listProcsSorted(t, h), oldHash) != 2 {
		t.Fatalf("pass 3 deleted stale while replacements were unavailable: %v", procNames(listProcsSorted(t, h)))
	}

	// Replacements ready → the next pass deletes the last two stale.
	markProcsReady(t, h, "web-2-"+newHash, "web-3-"+newHash)
	if err := h.c.Reconcile(ctx, "Daemon/web"); !errors.As(err, &requeue) {
		t.Fatalf("pass 4 returned %v, want RequeueAfter", err)
	}
	h.waitCaughtUp(t)
	if staleCount(listProcsSorted(t, h), oldHash) != 0 {
		t.Fatalf("pass 4 did not delete both remaining stale: %v", procNames(listProcsSorted(t, h)))
	}

	// Drive to completion.
	for range 15 {
		h.waitCaughtUp(t)
		if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		h.waitCaughtUp(t)
		markProcsReady(t, h)
		d, err := h.cl.GetDaemon(ctx, "web")
		if err != nil {
			t.Fatalf("GetDaemon: %v", err)
		}
		prog := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
		if d.Status.UpdatedReplicas == 4 && d.Status.ReadyReplicas == 4 && prog != nil && prog.Reason == reasonProcsAvailable {
			return
		}
	}
	t.Fatalf("batched rolling update did not complete; procs=%v", procNames(listProcsSorted(t, h)))
}

// TestReconcileRollingUpdateMaxUnavailableAll pins M9-e: maxUnavailable ≥
// replicas rolls every ordinal at once, but never touches ordinals below the
// partition.
func TestReconcileRollingUpdateMaxUnavailableAll(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemonOpts(t, h, 4, rollingStrategy(4, 1)) // mu == replicas, partition 1
	h.reconcileUntilSteady(t, "Daemon/web")
	markProcsReady(t, h)

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	_ = rollTemplate(t, h)

	// One pass rolls ordinals 1..3 (three deletions), leaving ordinal 0 (below
	// the partition) untouched on the old hash.
	var requeue controllers.RequeueAfter
	if err := h.c.Reconcile(ctx, "Daemon/web"); !errors.As(err, &requeue) {
		t.Fatalf("pass 1 returned %v, want RequeueAfter", err)
	}
	h.waitCaughtUp(t)
	procs := listProcsSorted(t, h)
	if len(procs) != 1 {
		t.Fatalf("mu>=replicas pass left %d procs, want 1 (ordinal 0 only): %v", len(procs), procNames(procs))
	}
	if procs[0].Metadata.Labels[v1alpha1.LabelReplicaIndex] != "0" ||
		procs[0].Metadata.Labels[v1alpha1.LabelTemplateHash] != oldHash {
		t.Fatalf("remaining proc = %s, want ordinal 0 on the old hash (below the partition)", procs[0].Metadata.Name)
	}
}

func TestReconcileRollingUpdatePartition(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemonOpts(t, h, 3, v1alpha1.UpdateStrategy{
		Type:          v1alpha1.UpdateStrategyRollingUpdate,
		RollingUpdate: &v1alpha1.RollingUpdateDaemonStrategy{Partition: new(int32(1))},
	})
	h.reconcileUntilSteady(t, "Daemon/web")
	markProcsReady(t, h)

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)

	d.Spec.Template.Spec.Command = []string{"/bin/sleep", "90"}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}

	// Drive roll for ordinals >= 1 only. Requeues never skip the
	// mark-Ready step (creation passes requeue for the deadline too).
	for range 20 {
		h.waitCaughtUp(t)
		if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		h.waitCaughtUp(t)
		markProcsReady(t, h)
	}
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	h.waitCaughtUp(t)

	d, err = h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	newHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	procs := listProcsSorted(t, h)
	if len(procs) != 3 {
		t.Fatalf("got %d procs, want 3", len(procs))
	}
	for _, p := range procs {
		idx := p.Metadata.Labels[v1alpha1.LabelReplicaIndex]
		hash := p.Metadata.Labels[v1alpha1.LabelTemplateHash]
		switch idx {
		case "0":
			if hash != oldHash {
				t.Errorf("ordinal 0 hash = %s, want old %s (partition)", hash, oldHash)
			}
		case "1", "2":
			if hash != newHash {
				t.Errorf("ordinal %s hash = %s, want new %s", idx, hash, newHash)
			}
		}
	}
	prog := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
	if prog == nil || prog.Reason != reasonProcsAvailable {
		t.Fatalf("Progressing = %+v, want complete with partition holding ordinal 0", prog)
	}
}

func applyConfig(t *testing.T, h *harness, name string, data map[string]string) *v1alpha1.Config {
	t.Helper()
	cfg := &v1alpha1.Config{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ConfigSpec{Data: data},
	}
	applied, err := h.cl.ApplyConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("ApplyConfig(%s): %v", name, err)
	}
	return applied
}

func applyNamedDaemonWithConfigs(t *testing.T, h *harness, name string, replicas int32, configs ...string) *v1alpha1.Daemon {
	t.Helper()
	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.DaemonSpec{
			Replicas: new(replicas),
			Template: v1alpha1.ProcTemplate{
				Metadata: v1alpha1.TemplateMeta{Labels: map[string]string{"app": name}},
				Spec:     v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}, Configs: configRefsOf(configs...)},
			},
		},
	}
	applied, err := h.cl.ApplyDaemon(t.Context(), d)
	if err != nil {
		t.Fatalf("ApplyDaemon(%s): %v", name, err)
	}
	return applied
}

// wantRevision computes the desired revision the controller would use for the
// named Daemon and its referenced Configs (same functions the controller runs).
func wantRevision(t *testing.T, h *harness, daemon string, configs ...string) (templateHash, revision string) {
	t.Helper()
	ctx := t.Context()
	d, err := h.cl.GetDaemon(ctx, daemon)
	if err != nil {
		t.Fatalf("GetDaemon(%s): %v", daemon, err)
	}
	templateHash = v1alpha1.HashProcTemplate(&d.Spec.Template)
	refs := make([]v1alpha1.RevisionRef, 0, len(configs))
	for _, name := range configs {
		cfg, err := h.cl.GetConfig(ctx, name)
		if err != nil {
			t.Fatalf("GetConfig(%s): %v", name, err)
		}
		refs = append(refs, v1alpha1.RevisionRef{Name: name, Hash: v1alpha1.HashConfigSpec(&cfg.Spec)})
	}
	return templateHash, v1alpha1.HashConfigRevision(templateHash, refs)
}

// configRefsOf builds bare-name ConfigRefs (no path) from config names, so the
// test helpers keep their string-variadic ergonomics after the union type.
func configRefsOf(names ...string) []v1alpha1.ConfigRef {
	if len(names) == 0 {
		return nil
	}
	refs := make([]v1alpha1.ConfigRef, len(names))
	for i, n := range names {
		refs[i] = v1alpha1.ConfigRef{Name: n}
	}
	return refs
}

// TestReconcileConfigRevisionIdentity pins M8-g: a Daemon referencing a Config
// gets Procs whose name suffix and config-hash label are the combined
// revision (≠ the pure template hash), while template-hash keeps its pure
// meaning.
func TestReconcileConfigRevisionIdentity(t *testing.T) {
	h := startHarness(t)
	applyConfig(t, h, "app", map[string]string{"app.conf": "listen 8080\n"})
	applyNamedDaemonWithConfigs(t, h, "web", 1, "app")
	h.reconcileUntilSteady(t, "Daemon/web")

	templateHash, revision := wantRevision(t, h, "web", "app")
	if revision == templateHash {
		t.Fatal("combined revision equals template hash; config content did not participate")
	}

	procs := listProcsSorted(t, h)
	if len(procs) != 1 {
		t.Fatalf("got %d procs, want 1", len(procs))
	}
	p := procs[0]
	if want := "web-0-" + revision; p.Metadata.Name != want {
		t.Errorf("proc name = %q, want %q", p.Metadata.Name, want)
	}
	if got := p.Metadata.Labels[v1alpha1.LabelConfigHash]; got != revision {
		t.Errorf("config-hash label = %q, want revision %q", got, revision)
	}
	if got := p.Metadata.Labels[v1alpha1.LabelTemplateHash]; got != templateHash {
		t.Errorf("template-hash label = %q, want pure template hash %q", got, templateHash)
	}
}

// TestReconcileNoConfigsHasNoConfigLabel pins the M7 upgrade guarantee: a
// Daemon without config refs produces Procs with no config-hash label and a
// name suffix equal to the pure template hash — byte-identical to pre-M8.
func TestReconcileNoConfigsHasNoConfigLabel(t *testing.T) {
	h := startHarness(t)
	applyDaemon(t, h, 1)
	h.reconcileUntilSteady(t, "Daemon/web")

	templateHash, revision := wantRevision(t, h, "web")
	if revision != templateHash {
		t.Fatalf("no-config revision %q != template hash %q", revision, templateHash)
	}
	procs := listProcsSorted(t, h)
	if len(procs) != 1 {
		t.Fatalf("got %d procs, want 1", len(procs))
	}
	if _, ok := procs[0].Metadata.Labels[v1alpha1.LabelConfigHash]; ok {
		t.Errorf("no-config daemon's proc carries a config-hash label: %v", procs[0].Metadata.Labels)
	}
	if want := "web-0-" + templateHash; procs[0].Metadata.Name != want {
		t.Errorf("proc name = %q, want %q", procs[0].Metadata.Name, want)
	}
}

// TestReconcileConfigChangeRolls pins M8-b: editing a referenced Config's
// content rolls the Daemon (new Proc identity) and emits ConfigChanged — never
// TemplateChanged — since the template itself is unchanged.
func TestReconcileConfigChangeRolls(t *testing.T) {
	h := startHarness(t)
	applyConfig(t, h, "app", map[string]string{"app.conf": "listen 8080\n"})
	applyNamedDaemonWithConfigs(t, h, "web", 1, "app")
	h.reconcileUntilSteady(t, "Daemon/web")
	before := listProcsSorted(t, h)
	if len(before) != 1 {
		t.Fatalf("want 1 proc, got %d", len(before))
	}
	oldName := before[0].Metadata.Name

	applyConfig(t, h, "app", map[string]string{"app.conf": "listen 9090\n"})
	h.reconcileUntilSteady(t, "Daemon/web")

	after := listProcsSorted(t, h)
	if len(after) != 1 {
		t.Fatalf("want 1 proc after roll, got %d: %v", len(after), procNames(after))
	}
	if after[0].Metadata.Name == oldName {
		t.Fatalf("proc did not roll on config change: still %s", oldName)
	}
	_, revision := wantRevision(t, h, "web", "app")
	if want := "web-0-" + revision; after[0].Metadata.Name != want {
		t.Errorf("rolled proc name = %q, want %q", after[0].Metadata.Name, want)
	}
	waitFor(t, "ConfigChanged event", func() bool { return findEvent(t, h, v1alpha1.ReasonConfigChanged) != nil })
	if findEvent(t, h, v1alpha1.ReasonTemplateChanged) != nil {
		t.Error("config-only change emitted a TemplateChanged event")
	}
}

// TestReconcileMissingConfigHolds pins M8-h: a Daemon referencing an absent
// Config creates no Procs, emits a ConfigMissing Warning, and reports
// Progressing/ConfigMissing — then self-heals when the Config appears.
func TestReconcileMissingConfigHolds(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyNamedDaemonWithConfigs(t, h, "web", 2, "app") // "app" does not exist
	h.waitCaughtUp(t)

	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.waitCaughtUp(t)
	if procs := listProcsSorted(t, h); len(procs) != 0 {
		t.Fatalf("missing config did not hold: %d procs created (%v)", len(procs), procNames(procs))
	}
	waitFor(t, "ConfigMissing event", func() bool { return findEvent(t, h, v1alpha1.ReasonConfigMissing) != nil })
	if ev := findEvent(t, h, v1alpha1.ReasonConfigMissing); ev.Type != v1alpha1.EventTypeWarning {
		t.Errorf("ConfigMissing event type = %s, want Warning", ev.Type)
	}
	_, prog := getDaemonProgressing(t, h)
	if prog.Reason != reasonConfigMissing {
		t.Errorf("Progressing reason = %q, want %q", prog.Reason, reasonConfigMissing)
	}

	// Self-heal: the Config appears → the hold releases and Procs are created.
	applyConfig(t, h, "app", map[string]string{"app.conf": "x\n"})
	h.reconcileUntilSteady(t, "Daemon/web")
	if procs := listProcsSorted(t, h); len(procs) != 2 {
		t.Fatalf("after config appeared got %d procs, want 2", len(procs))
	}
}

// TestEnqueueReferencingDaemons pins §4.3: a Config event enqueues exactly the
// Daemons whose template references that Config.
func TestEnqueueReferencingDaemons(t *testing.T) {
	h := startHarness(t)
	applyNamedDaemonWithConfigs(t, h, "web", 1, "app")
	applyNamedDaemonWithConfigs(t, h, "api", 1, "other")
	h.waitCaughtUp(t)

	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	defer q.ShutDown()
	enqueue := EnqueueReferencingDaemons(h.daemons, q)

	enqueue("Config/app")
	if q.Len() != 1 {
		t.Fatalf("after Config/app, q.Len() = %d, want 1", q.Len())
	}
	if key, _ := q.Get(); key != "Daemon/web" {
		t.Errorf("enqueued %q, want Daemon/web", key)
	}

	// A Config nobody references enqueues nothing.
	enqueue("Config/unreferenced")
	if q.Len() != 0 {
		t.Errorf("unreferenced Config enqueued %d daemons, want 0", q.Len())
	}
}

// TestEnqueueReferencingDaemonsObjectForm is the M9-o regression: a daemon
// whose config ref uses the object form ({name, path}) must still enqueue on a
// Config event. A naive []string decode would fail to unmarshal the object and
// silently skip the daemon, breaking roll-on-change for path: refs.
func TestEnqueueReferencingDaemonsObjectForm(t *testing.T) {
	h := startHarness(t)
	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas: new(int32(1)),
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{
					Command: []string{"/bin/sleep", "60"},
					Configs: []v1alpha1.ConfigRef{{Name: "app", Path: new("/etc/app")}},
				},
			},
		},
	}
	if _, err := h.cl.ApplyDaemon(t.Context(), d); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	h.waitCaughtUp(t)

	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	defer q.ShutDown()
	enqueue := EnqueueReferencingDaemons(h.daemons, q)

	enqueue("Config/app")
	if q.Len() != 1 {
		t.Fatalf("object-form config ref not enqueued (q.Len()=%d) — union decode regressed", q.Len())
	}
	if key, _ := q.Get(); key != "Daemon/web" {
		t.Errorf("enqueued %q, want Daemon/web", key)
	}
}

func procNames(procs []v1alpha1.Proc) []string {
	out := make([]string, len(procs))
	for i := range procs {
		out[i] = procs[i].Metadata.Name
	}
	return out
}

// ignoreRequeue strips the RequeueAfter sentinel: since M6 any incomplete
// rollout schedules a progress-deadline (or availability) wake-up, so
// passes that used to return nil may return a timer instead. Real errors
// pass through.
func ignoreRequeue(err error) error {
	if _, ok := errors.AsType[controllers.RequeueAfter](err); ok {
		return nil
	}
	return err
}

// A rolling pass that creates replacement Procs must stop there: the
// informer has not observed the creations yet, so continuing into the
// rolling walk would see their ordinals as empty slots and delete a second
// stale ordinal — two replicas down at once. Pins the one-action-per-pass
// gate in the RollingUpdate branch.
func TestReconcileRollingUpdateCreationPassDeletesNothing(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemonOpts(t, h, 3, v1alpha1.UpdateStrategy{Type: v1alpha1.UpdateStrategyRollingUpdate})
	h.reconcileUntilSteady(t, "Daemon/web")
	markProcsReady(t, h)

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	oldHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	d.Spec.Template.Spec.Command = []string{"/bin/sleep", "120"}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon(template change): %v", err)
	}
	h.waitCaughtUp(t)

	// Pass 1 deletes ordinal 2 (highest stale).
	err = h.c.Reconcile(ctx, "Daemon/web")
	var requeue controllers.RequeueAfter
	if !errors.As(err, &requeue) {
		t.Fatalf("first rolling pass returned %v, want RequeueAfter", err)
	}
	h.waitCaughtUp(t)

	// Pass 2 creates the ordinal-2 replacement. It must stop there — no
	// requeue-driven walk into a second delete; the only acceptable
	// non-nil return is the progress-deadline timer. It must not have
	// deleted anything: both stale lower ordinals are still on the
	// server, regardless of whether the informer observed the creation.
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("creation pass returned %v, want nil or a deadline requeue", err)
	}
	procs := listProcsSorted(t, h)
	if len(procs) != 3 {
		t.Fatalf("after creation pass got %d procs, want 3 (no same-pass delete): %v",
			len(procs), procNames(procs))
	}
	staleLeft := 0
	for _, p := range procs {
		if p.Metadata.Labels[v1alpha1.LabelTemplateHash] == oldHash {
			staleLeft++
		}
	}
	if staleLeft != 2 {
		t.Fatalf("creation pass deleted a stale ordinal: %d stale procs left, want 2 (%v)",
			staleLeft, procNames(procs))
	}
}

// Scale-up must fill the lowest free replica index, not append past the
// highest one (StatefulSet monotonic identity). Deleting the middle
// ordinal and reconciling must recreate ordinal 1, never mint ordinal 3.
func TestReconcileScaleUpFillsLowestFreeIndex(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemon(t, h, 3)
	h.reconcileUntilSteady(t, "Daemon/web")

	d, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	hash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	if err := h.cl.DeleteProc(ctx, "web-1-"+hash); err != nil {
		t.Fatalf("DeleteProc(web-1): %v", err)
	}
	h.waitCaughtUp(t)

	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	indices := map[string]bool{}
	for _, p := range listProcsSorted(t, h) {
		indices[p.Metadata.Labels[v1alpha1.LabelReplicaIndex]] = true
	}
	if !indices["1"] {
		t.Errorf("gap at ordinal 1 was not refilled; indices = %v", indices)
	}
	if indices["3"] {
		t.Errorf("scale-up appended ordinal 3 instead of filling the gap; indices = %v", indices)
	}
}

// rollupStatus must never stamp a recreated Daemon (same name, new UID)
// with the dead incarnation's observation: without the UID guard the write
// would carry another object's counts and an observedGeneration the new
// object's generation may never have reached.
func TestRollupStatusSkipsRecreatedDaemon(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()
	applyDaemon(t, h, 2)
	h.reconcileUntilSteady(t, "Daemon/web")

	old, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if err := h.cl.DeleteDaemon(ctx, "web"); err != nil {
		t.Fatalf("DeleteDaemon: %v", err)
	}
	fresh := applyDaemon(t, h, 2)
	if fresh.Metadata.UID == old.Metadata.UID {
		t.Fatalf("recreated daemon kept UID %s; cannot exercise the guard", old.Metadata.UID)
	}

	// Roll up with the dead incarnation's copy and a non-empty observation:
	// must be a silent no-op against the new object.
	if err := h.c.rollupStatus(ctx, old, []v1alpha1.Proc{{}, {}}, nil, 0); err != nil {
		t.Fatalf("rollupStatus with stale incarnation: %v", err)
	}
	got, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon after rollup: %v", err)
	}
	if got.Status.ObservedGeneration != 0 || got.Status.Replicas != 0 || len(got.Status.Conditions) != 0 {
		t.Fatalf("stale incarnation's status was written onto the new object: %+v", got.Status)
	}
}

// markProcsReadyAt is markProcsReady with an explicit Ready transition time,
// for minReadySeconds tests that measure availability from it.
func markProcsReadyAt(t *testing.T, h *harness, at time.Time) {
	t.Helper()
	ctx := t.Context()
	procs, _, err := h.cl.ListProcs(ctx)
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	for i := range procs {
		p := &procs[i]
		p.Status.Phase = v1alpha1.ProcPhaseRunning
		v1alpha1.SetStatusCondition(&p.Status.Conditions, v1alpha1.Condition{
			Type:               v1alpha1.ConditionTypeReady,
			Status:             v1alpha1.ConditionTrue,
			Reason:             "TestReady",
			LastTransitionTime: v1alpha1.NewTime(at),
		})
		if _, err := h.cl.UpdateProcStatus(ctx, p); err != nil {
			t.Fatalf("UpdateProcStatus(%s): %v", p.Metadata.Name, err)
		}
	}
	h.waitCaughtUp(t)
}

func getDaemonProgressing(t *testing.T, h *harness) (*v1alpha1.Daemon, *v1alpha1.Condition) {
	t.Helper()
	d, err := h.cl.GetDaemon(t.Context(), "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	prog := v1alpha1.FindStatusCondition(d.Status.Conditions, v1alpha1.ConditionTypeProgressing)
	if prog == nil {
		t.Fatal("no Progressing condition")
	}
	return d, prog
}

// TestProgressDeadlineExceededFlipsOnceAndRecovers pins the M6 deadline:
// a stalled rollout flips Progressing to False/ProgressDeadlineExceeded
// exactly once (one Warning event), later passes keep the exceeded state,
// and convergence recovers it to True/ProcsAvailable.
func TestProgressDeadlineExceededFlipsOnceAndRecovers(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas:                new(int32(1)),
			ProgressDeadlineSeconds: new(int32(30)),
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	h.reconcileUntilSteady(t, "Daemon/web")

	_, prog := getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionTrue || prog.Reason != reasonProcsUpdated {
		t.Fatalf("Progressing = %s/%s, want True/%s", prog.Status, prog.Reason, reasonProcsUpdated)
	}
	if prog.LastUpdateTime.IsZero() {
		t.Fatal("Progressing.lastUpdateTime not stamped")
	}

	// The pass at rest must schedule the deadline wake-up.
	h.waitCaughtUp(t)
	rq, ok := errors.AsType[controllers.RequeueAfter](h.c.Reconcile(ctx, "Daemon/web"))
	if !ok {
		t.Fatal("incomplete rollout did not schedule a deadline requeue")
	}
	if rq.After <= 0 || rq.After > 30*time.Second {
		t.Fatalf("deadline requeue = %v, want (0, 30s]", rq.After)
	}

	// No progress for 31s: the deadline fires.
	h.clk.Step(31 * time.Second)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile past deadline: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionFalse || prog.Reason != reasonProgressDeadlineExceeded {
		t.Fatalf("Progressing = %s/%s, want False/%s", prog.Status, prog.Reason, reasonProgressDeadlineExceeded)
	}
	ev := findEvent(t, h, v1alpha1.ReasonProgressDeadlineExceeded)
	if ev == nil {
		t.Fatal("no ProgressDeadlineExceeded event")
	}
	if ev.Type != v1alpha1.EventTypeWarning {
		t.Errorf("event type = %s, want Warning", ev.Type)
	}

	// Further passes keep the exceeded state and do not re-emit.
	h.clk.Step(time.Minute)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile while exceeded: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionFalse || prog.Reason != reasonProgressDeadlineExceeded {
		t.Fatalf("exceeded state not sticky: %s/%s", prog.Status, prog.Reason)
	}
	if ev := findEvent(t, h, v1alpha1.ReasonProgressDeadlineExceeded); ev == nil || ev.Count > 1 {
		t.Fatalf("event re-emitted while already exceeded: %+v", ev)
	}

	// The world converges: the condition recovers.
	markProcsReady(t, h)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile after convergence: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionTrue || prog.Reason != reasonProcsAvailable {
		t.Fatalf("Progressing = %s/%s, want recovered True/%s", prog.Status, prog.Reason, reasonProcsAvailable)
	}
}

// TestProgressDeadlineReanchorsOnProgress pins the lastUpdateTime anchor:
// real progress (count changes in the message) bumps it, restarting the
// deadline window; a stall freezes it.
func TestProgressDeadlineReanchorsOnProgress(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas:                new(int32(2)),
			ProgressDeadlineSeconds: new(int32(30)),
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	h.reconcileUntilSteady(t, "Daemon/web")
	_, prog := getDaemonProgressing(t, h)
	anchor := prog.LastUpdateTime

	// 20s in, one proc becomes ready: progress bumps the anchor.
	h.clk.Step(20 * time.Second)
	applied, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	hash := v1alpha1.HashProcTemplate(&applied.Spec.Template)
	markProcsReady(t, h, "web-0-"+hash)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if !prog.LastUpdateTime.After(anchor.Time) {
		t.Fatalf("lastUpdateTime did not advance on progress: %v -> %v", anchor, prog.LastUpdateTime)
	}
	if prog.Status != v1alpha1.ConditionTrue {
		t.Fatalf("Progressing flipped early: %s/%s", prog.Status, prog.Reason)
	}

	// 20 more seconds (40 total, but only 20 since the re-anchor): still True.
	h.clk.Step(20 * time.Second)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionTrue {
		t.Fatalf("deadline measured from the wrong anchor: %s/%s", prog.Status, prog.Reason)
	}

	// 11 more with no progress (31 since re-anchor): flips.
	h.clk.Step(11 * time.Second)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionFalse || prog.Reason != reasonProgressDeadlineExceeded {
		t.Fatalf("Progressing = %s/%s, want False/%s", prog.Status, prog.Reason, reasonProgressDeadlineExceeded)
	}
}

// TestMinReadySecondsGatesAvailability pins M6 availability: a Ready proc
// counts as available only after minReadySeconds, the controller schedules
// the maturation wake-up, and Available flips without any further object
// change.
func TestMinReadySecondsGatesAvailability(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas:        new(int32(1)),
			MinReadySeconds: 5,
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	h.reconcileUntilSteady(t, "Daemon/web")

	markProcsReadyAt(t, h, h.clk.Now())
	rq, ok := errors.AsType[controllers.RequeueAfter](h.c.Reconcile(ctx, "Daemon/web"))
	if !ok {
		t.Fatal("ready-but-not-available proc did not schedule a maturation requeue")
	}
	if rq.After <= 0 || rq.After > 5*time.Second {
		t.Fatalf("maturation requeue = %v, want (0, 5s]", rq.After)
	}
	h.waitCaughtUp(t)
	got, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if got.Status.ReadyReplicas != 1 || got.Status.AvailableReplicas != 0 {
		t.Fatalf("ready/available = %d/%d, want 1/0 before minReadySeconds",
			got.Status.ReadyReplicas, got.Status.AvailableReplicas)
	}
	avail := v1alpha1.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionTypeAvailable)
	if avail == nil || avail.Status != v1alpha1.ConditionFalse {
		t.Fatalf("Available = %+v, want False before minReadySeconds", avail)
	}

	// Nothing changes but time: the proc matures.
	h.clk.Step(6 * time.Second)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile after maturation: %v", err)
	}
	h.waitCaughtUp(t)
	got, err = h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if got.Status.AvailableReplicas != 1 {
		t.Fatalf("availableReplicas = %d, want 1 after minReadySeconds", got.Status.AvailableReplicas)
	}
	avail = v1alpha1.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionTypeAvailable)
	if avail == nil || avail.Status != v1alpha1.ConditionTrue {
		t.Fatalf("Available = %+v, want True after minReadySeconds", avail)
	}
}

// TestProgressDeadlineResetsOnNewGeneration pins that an exceeded
// Progressing condition is sticky only within its generation: a spec change
// is a new rollout and gets a fresh deadline (Deployment posture).
func TestProgressDeadlineResetsOnNewGeneration(t *testing.T) {
	h := startHarness(t)
	ctx := t.Context()

	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Replicas:                new(int32(1)),
			ProgressDeadlineSeconds: new(int32(30)),
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	if _, err := h.cl.ApplyDaemon(ctx, d); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	h.reconcileUntilSteady(t, "Daemon/web")
	h.clk.Step(31 * time.Second)
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile past deadline: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog := getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionFalse || prog.Reason != reasonProgressDeadlineExceeded {
		t.Fatalf("setup: Progressing = %s/%s, want exceeded", prog.Status, prog.Reason)
	}

	// A spec change (generation bump) resets the rollout and its deadline.
	applied, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	applied.Spec.Template.Spec.Command = []string{"/bin/sleep", "90"}
	if _, err := h.cl.ApplyDaemon(ctx, applied); err != nil {
		t.Fatalf("ApplyDaemon(new template): %v", err)
	}
	h.waitCaughtUp(t)
	if err := ignoreRequeue(h.c.Reconcile(ctx, "Daemon/web")); err != nil {
		t.Fatalf("Reconcile of new generation: %v", err)
	}
	h.waitCaughtUp(t)
	_, prog = getDaemonProgressing(t, h)
	if prog.Status != v1alpha1.ConditionTrue {
		t.Fatalf("new generation did not reset the exceeded state: %s/%s", prog.Status, prog.Reason)
	}
}
