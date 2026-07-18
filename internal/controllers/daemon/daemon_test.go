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
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	cl := startServer(t)
	ctx := t.Context()

	dinf := cache.NewInformer(cl, v1alpha1.KindDaemon, func(string) {}, nil)
	pinf := cache.NewInformer(cl, v1alpha1.KindProc, func(string) {}, nil)
	go dinf.Run(ctx)
	go pinf.Run(ctx)
	if err := dinf.WaitForSync(ctx); err != nil {
		t.Fatalf("daemon informer WaitForSync: %v", err)
	}
	if err := pinf.WaitForSync(ctx); err != nil {
		t.Fatalf("proc informer WaitForSync: %v", err)
	}

	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	return &harness{
		cl:      cl,
		c:       New(cl, dinf.Store(), pinf.Store(), clk),
		clk:     clk,
		daemons: dinf.Store(),
		procs:   pinf.Store(),
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
	return snap
}

// cacheSnapshot is serverSnapshot's shape read from the informer stores.
func (h *harness) cacheSnapshot(t *testing.T) map[string]string {
	t.Helper()
	snap := map[string]string{}
	for _, store := range []*cache.Store{h.daemons, h.procs} {
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
// the server (no proc mutations, status write absorbed as a no-op).
func (h *harness) reconcileUntilSteady(t *testing.T, key string) {
	t.Helper()
	for range 10 {
		h.waitCaughtUp(t)
		before := h.serverSnapshot(t)
		err := h.c.Reconcile(t.Context(), key)
		var requeue controllers.RequeueAfter
		if errors.As(err, &requeue) {
			continue
		}
		if err != nil {
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
	if ev.Type != v1alpha1.EventTypeNormal || ev.ReportingComponent != componentName {
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

	avail := getCondition(d.Status, v1alpha1.ConditionTypeAvailable)
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
	prog := getCondition(d.Status, v1alpha1.ConditionTypeProgressing)
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
	// untouched transition time.
	h.clk.Step(90 * time.Second)
	h.waitCaughtUp(t)
	if err := h.c.Reconcile(ctx, "Daemon/web"); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}

	d2, err := h.cl.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon after second pass: %v", err)
	}
	if d2.Metadata.ResourceVersion != rv {
		t.Errorf("resourceVersion churned %s -> %s on a no-op pass", rv, d2.Metadata.ResourceVersion)
	}
	avail2 := getCondition(d2.Status, v1alpha1.ConditionTypeAvailable)
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
