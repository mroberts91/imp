// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Integration tests: the GC controller against a real apiserver + etcl
// store over a real Unix socket, with real informers feeding the cache
// stores. Reconcile is called directly (no Runner) so each scenario is
// deterministic.
package gc_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/controllers/gc"
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

// startInformer runs an informer for kind over c and waits for its initial
// sync; the returned informer's Store mirrors the server.
func startInformer(t *testing.T, c *client.Client, kind string) *cache.Informer {
	t.Helper()
	inf := cache.NewInformer(c, kind, func(string) {}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go inf.Run(ctx)
	if err := inf.WaitForSync(t.Context()); err != nil {
		t.Fatalf("WaitForSync(%s): %v", kind, err)
	}
	return inf
}

func applyDaemon(t *testing.T, c *client.Client, name string) *v1alpha1.Daemon {
	t.Helper()
	d, err := c.ApplyDaemon(t.Context(), &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.DaemonSpec{
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("ApplyDaemon(%s): %v", name, err)
	}
	return d
}

// applyOwnedProc creates a Proc the way the daemon controller would: system
// labels plus an ownerReference to the (already-created) Daemon.
func applyOwnedProc(t *testing.T, c *client.Client, owner *v1alpha1.Daemon, replica int) *v1alpha1.Proc {
	t.Helper()
	hash := v1alpha1.HashProcTemplate(&owner.Spec.Template)
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name: owner.Metadata.Name + "-" + strconv.Itoa(replica) + "-" + hash,
			Labels: map[string]string{
				v1alpha1.LabelDaemonName:   owner.Metadata.Name,
				v1alpha1.LabelTemplateHash: hash,
				v1alpha1.LabelReplicaIndex: strconv.Itoa(replica),
			},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindDaemon,
				Name:       owner.Metadata.Name,
				UID:        owner.Metadata.UID,
			}},
		},
		Spec: *owner.Spec.Template.Spec.DeepCopy(),
	}
	created, err := c.ApplyProc(t.Context(), p)
	if err != nil {
		t.Fatalf("ApplyProc(%s): %v", p.Metadata.Name, err)
	}
	return created
}

func procExists(t *testing.T, c *client.Client, name string) bool {
	t.Helper()
	_, err := c.GetProc(t.Context(), name)
	switch {
	case err == nil:
		return true
	case errors.Is(err, v1alpha1.ErrNotFound):
		return false
	default:
		t.Fatalf("GetProc(%s): %v", name, err)
		return false
	}
}

func TestReconcileDeletesOrphansAfterDaemonDelete(t *testing.T) {
	c := startServer(t)
	ctx := t.Context()

	d := applyDaemon(t, c, "web")
	p0 := applyOwnedProc(t, c, d, 0)
	p1 := applyOwnedProc(t, c, d, 1)

	dInf := startInformer(t, c, v1alpha1.KindDaemon)
	pInf := startInformer(t, c, v1alpha1.KindProc)
	ctrl := gc.New(c, dInf.Store(), startInformer(t, c, v1alpha1.KindTimer).Store(), startInformer(t, c, v1alpha1.KindNotifier).Store(), pInf.Store())

	if err := c.DeleteDaemon(ctx, "web"); err != nil {
		t.Fatalf("DeleteDaemon: %v", err)
	}
	waitFor(t, "daemon to leave the cache", func() bool {
		_, ok := dInf.Store().GetByKey("Daemon/web")
		return !ok
	})

	for _, p := range []*v1alpha1.Proc{p0, p1} {
		if err := ctrl.Reconcile(ctx, "Proc/"+p.Metadata.Name); err != nil {
			t.Fatalf("Reconcile(%s): %v", p.Metadata.Name, err)
		}
		if procExists(t, c, p.Metadata.Name) {
			t.Errorf("proc %s still exists after cascade reconcile", p.Metadata.Name)
		}
	}
}

func TestReconcileLeavesProcWithLiveOwner(t *testing.T) {
	c := startServer(t)

	d := applyDaemon(t, c, "web")
	p := applyOwnedProc(t, c, d, 0)

	dInf := startInformer(t, c, v1alpha1.KindDaemon)
	pInf := startInformer(t, c, v1alpha1.KindProc)
	ctrl := gc.New(c, dInf.Store(), startInformer(t, c, v1alpha1.KindTimer).Store(), startInformer(t, c, v1alpha1.KindNotifier).Store(), pInf.Store())

	if err := ctrl.Reconcile(t.Context(), "Proc/"+p.Metadata.Name); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !procExists(t, c, p.Metadata.Name) {
		t.Error("proc deleted despite a live owner")
	}
}

func TestReconcileIgnoresUnownedProc(t *testing.T) {
	c := startServer(t)

	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "loner"},
		Spec:     v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
	}
	if _, err := c.ApplyProc(t.Context(), p); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}

	dInf := startInformer(t, c, v1alpha1.KindDaemon)
	pInf := startInformer(t, c, v1alpha1.KindProc)
	ctrl := gc.New(c, dInf.Store(), startInformer(t, c, v1alpha1.KindTimer).Store(), startInformer(t, c, v1alpha1.KindNotifier).Store(), pInf.Store())

	if err := ctrl.Reconcile(t.Context(), "Proc/loner"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !procExists(t, c, "loner") {
		t.Error("unowned proc was deleted; GC must not touch it")
	}
}

func TestReconcileFreshnessCheckSavesProcFromStaleCache(t *testing.T) {
	c := startServer(t)

	d := applyDaemon(t, c, "web")
	p := applyOwnedProc(t, c, d, 0)

	// The daemon informer is constructed but never Run: its store is valid
	// and permanently empty, simulating a stale cache that has missed the
	// daemon. The live GetDaemon in the freshness check must save the proc.
	staleDaemons := cache.NewInformer(c, v1alpha1.KindDaemon, func(string) {}, nil)
	pInf := startInformer(t, c, v1alpha1.KindProc)
	ctrl := gc.New(c, staleDaemons.Store(), startInformer(t, c, v1alpha1.KindTimer).Store(), startInformer(t, c, v1alpha1.KindNotifier).Store(), pInf.Store())

	if err := ctrl.Reconcile(t.Context(), "Proc/"+p.Metadata.Name); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !procExists(t, c, p.Metadata.Name) {
		t.Error("proc deleted on stale cache alone; the live owner check must prevent this")
	}
}

func TestEnqueueOwnedProcs(t *testing.T) {
	c := startServer(t)

	web := applyDaemon(t, c, "web")
	other := applyDaemon(t, c, "other")
	p0 := applyOwnedProc(t, c, web, 0)
	p1 := applyOwnedProc(t, c, web, 1)
	foreign := applyOwnedProc(t, c, other, 0)

	pInf := startInformer(t, c, v1alpha1.KindProc)

	q := queue.NewRateLimiting(queue.DefaultRateLimiter())
	handler := gc.EnqueueOwnedProcs(pInf.Store(), q)
	handler("Daemon/web")
	q.ShutDown() // drain mode: Get returns the queued keys, then shutdown

	got := map[string]bool{}
	for {
		key, shutdown := q.Get()
		if shutdown {
			break
		}
		got[key] = true
		q.Done(key)
	}

	want := map[string]bool{
		"Proc/" + p0.Metadata.Name: true,
		"Proc/" + p1.Metadata.Name: true,
	}
	if len(got) != len(want) {
		t.Fatalf("enqueued keys = %v, want %v", got, want)
	}
	for key := range want {
		if !got[key] {
			t.Errorf("missing enqueued key %s (got %v)", key, got)
		}
	}
	if got["Proc/"+foreign.Metadata.Name] {
		t.Errorf("foreign proc %s was enqueued", foreign.Metadata.Name)
	}
}

func TestReconcileAbsentProcKeyIsNoop(t *testing.T) {
	c := startServer(t)

	dInf := startInformer(t, c, v1alpha1.KindDaemon)
	pInf := startInformer(t, c, v1alpha1.KindProc)
	ctrl := gc.New(c, dInf.Store(), startInformer(t, c, v1alpha1.KindTimer).Store(), startInformer(t, c, v1alpha1.KindNotifier).Store(), pInf.Store())

	if err := ctrl.Reconcile(t.Context(), "Proc/ghost"); err != nil {
		t.Fatalf("Reconcile of absent key = %v, want nil", err)
	}
}

// Timer owners cascade exactly like Daemon owners (M5 generalization).
func TestReconcileDeletesTimerOrphans(t *testing.T) {
	c := startServer(t)
	ctx := t.Context()

	tm, err := c.ApplyTimer(ctx, &v1alpha1.Timer{
		Metadata: v1alpha1.ObjectMeta{Name: "backup"},
		Spec: v1alpha1.TimerSpec{
			Schedule: "@every 1h",
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/true"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("ApplyTimer: %v", err)
	}
	run, err := c.ApplyProc(ctx, &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:   "backup-1234",
			Labels: map[string]string{v1alpha1.LabelTimerName: "backup"},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindTimer,
				Name:       "backup",
				UID:        tm.Metadata.UID,
			}},
		},
		Spec: *tm.Spec.Template.Spec.DeepCopy(),
	})
	if err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}

	dInf := startInformer(t, c, v1alpha1.KindDaemon)
	tInf := startInformer(t, c, v1alpha1.KindTimer)
	pInf := startInformer(t, c, v1alpha1.KindProc)
	ctrl := gc.New(c, dInf.Store(), tInf.Store(), startInformer(t, c, v1alpha1.KindNotifier).Store(), pInf.Store())

	// Owner alive: untouched.
	if err := ctrl.Reconcile(ctx, "Proc/"+run.Metadata.Name); err != nil {
		t.Fatalf("Reconcile with live timer: %v", err)
	}
	if !procExists(t, c, run.Metadata.Name) {
		t.Fatal("proc deleted while its Timer owner was alive")
	}

	if err := c.DeleteTimer(ctx, "backup"); err != nil {
		t.Fatalf("DeleteTimer: %v", err)
	}
	waitFor(t, "timer to leave the cache", func() bool {
		_, ok := tInf.Store().GetByKey(v1alpha1.KindTimer + "/backup")
		return !ok
	})
	if err := ctrl.Reconcile(ctx, "Proc/"+run.Metadata.Name); err != nil {
		t.Fatalf("Reconcile after timer delete: %v", err)
	}
	if procExists(t, c, run.Metadata.Name) {
		t.Fatal("orphaned timer run not deleted")
	}
}
