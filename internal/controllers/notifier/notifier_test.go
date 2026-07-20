// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Integration tests: the NotifierController's Reconcile driven directly
// (not through a Runner) against the real client, apiserver, and etcl
// store over a real Unix socket, with real informers feeding the cache
// stores. The controller clock is fake and anchored near wall time (the
// server stamps real creation timestamps); cooldowns are crossed with
// Step.
package notifier

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
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
	cl        *client.Client
	c         *Controller
	clk       *clock.Fake
	notifiers *cache.Store
	daemons   *cache.Store
	timers    *cache.Store
	procs     *cache.Store
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
	ninf := cache.NewInformer(cl, v1alpha1.KindNotifier, func(string) {}, nil)
	dinf := cache.NewInformer(cl, v1alpha1.KindDaemon, func(string) {}, nil)
	tinf := cache.NewInformer(cl, v1alpha1.KindTimer, func(string) {}, nil)
	pinf := cache.NewInformer(cl, v1alpha1.KindProc, func(string) {}, nil)
	for _, inf := range []*cache.Informer{ninf, dinf, tinf, pinf} {
		go inf.Run(ctx)
		if err := inf.WaitForSync(ctx); err != nil {
			t.Fatalf("informer WaitForSync: %v", err)
		}
	}

	clk := clock.NewFake(time.Now())
	return &harness{
		cl:        cl,
		c:         New(cl, ninf.Store(), dinf.Store(), tinf.Store(), pinf.Store(), clk, nil),
		clk:       clk,
		notifiers: ninf.Store(),
		daemons:   dinf.Store(),
		timers:    tinf.Store(),
		procs:     pinf.Store(),
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// applyNotifier creates a Notifier and waits for the cache to see it.
func applyNotifier(t *testing.T, h *harness, name string, mutate func(*v1alpha1.Notifier)) *v1alpha1.Notifier {
	t.Helper()
	n := &v1alpha1.Notifier{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.NotifierSpec{
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{
					Command: []string{"/bin/true"},
					Env:     []v1alpha1.EnvVar{{Name: "CHANNEL", Value: "ops"}},
				},
			},
		},
	}
	if mutate != nil {
		mutate(n)
	}
	created, err := h.cl.ApplyNotifier(t.Context(), n)
	if err != nil {
		t.Fatalf("ApplyNotifier: %v", err)
	}
	waitFor(t, "notifier in cache", func() bool {
		_, ok := h.notifiers.GetByKey(v1alpha1.KindNotifier + "/" + name)
		return ok
	})
	return created
}

// applyDaemon creates a Daemon (for owner identity and labels) and waits
// for the cache.
func applyDaemon(t *testing.T, h *harness, name string, labels map[string]string) *v1alpha1.Daemon {
	t.Helper()
	d := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: name, Labels: labels},
		Spec: v1alpha1.DaemonSpec{
			Replicas: new(int32(1)),
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "1h"}},
			},
		},
	}
	created, err := h.cl.ApplyDaemon(t.Context(), d)
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	waitFor(t, "daemon in cache", func() bool {
		_, ok := h.daemons.GetByKey(v1alpha1.KindDaemon + "/" + name)
		return ok
	})
	return created
}

// applyProcWithStatus creates a Proc, then writes the given status through
// the status subresource, and waits for the cache to see the status.
func applyProcWithStatus(t *testing.T, h *harness, p *v1alpha1.Proc, status v1alpha1.ProcStatus) *v1alpha1.Proc {
	t.Helper()
	created, err := h.cl.ApplyProc(t.Context(), p)
	if err != nil {
		t.Fatalf("ApplyProc(%s): %v", p.Metadata.Name, err)
	}
	created.Status = status
	updated, err := h.cl.UpdateProcStatus(t.Context(), created)
	if err != nil {
		t.Fatalf("UpdateProcStatus(%s): %v", p.Metadata.Name, err)
	}
	waitFor(t, "proc status in cache", func() bool {
		raw, ok := h.procs.GetByKey(v1alpha1.KindProc + "/" + p.Metadata.Name)
		return ok && strings.Contains(string(raw), string(status.Phase)) &&
			(status.RestartCount == 0 || strings.Contains(string(raw), `"restartCount"`))
	})
	return updated
}

// crashLoopProc builds a Proc owned by daemon d, in CrashLoopBackOff with
// the given restart count and last exit code.
func crashLoopProc(name string, d *v1alpha1.Daemon, restarts int32, exitCode int) (*v1alpha1.Proc, v1alpha1.ProcStatus) {
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1alpha1.LabelDaemonName: d.Metadata.Name},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindDaemon,
				Name:       d.Metadata.Name,
				UID:        d.Metadata.UID,
			}},
		},
		Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/false"}},
	}
	status := v1alpha1.ProcStatus{
		Phase:        v1alpha1.ProcPhasePending,
		RestartCount: restarts,
		State: v1alpha1.ProcState{
			Waiting: &v1alpha1.ProcStateWaiting{
				Reason:  v1alpha1.WaitingReasonCrashLoopBackOff,
				Message: "backing off before restart",
			},
			LastTerminated: &v1alpha1.ProcStateTerminated{
				ExitCode: &exitCode,
				Message:  "exited with code 7",
			},
		},
	}
	return p, status
}

// notificationRuns lists the runs the notifier created, straight from the
// server (not the cache).
func notificationRuns(t *testing.T, h *harness, notifierName string) []v1alpha1.Proc {
	t.Helper()
	runs, _, err := h.cl.ListProcs(t.Context(),
		client.WithLabelSelector(v1alpha1.LabelNotifierName+"="+notifierName))
	if err != nil {
		t.Fatalf("ListProcs: %v", err)
	}
	return runs
}

func reconcile(t *testing.T, h *harness, name string) error {
	t.Helper()
	return h.c.Reconcile(t.Context(), v1alpha1.KindNotifier+"/"+name)
}

func envValue(env []v1alpha1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return "<absent>"
}

func TestCrashLoopNotifies(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", nil)
	d := applyDaemon(t, h, "web", map[string]string{"app": "web"})
	p, st := crashLoopProc("web-0-abc", d, 4, 7)
	applyProcWithStatus(t, h, p, st)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	runs := notificationRuns(t, h, "pager")
	if len(runs) != 1 {
		t.Fatalf("got %d notification runs, want 1", len(runs))
	}
	run := runs[0]
	if run.Spec.RestartPolicy != v1alpha1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", run.Spec.RestartPolicy)
	}
	ann := run.Metadata.Annotations
	if ann[v1alpha1.AnnotationNotifiedKind] != v1alpha1.KindDaemon ||
		ann[v1alpha1.AnnotationNotifiedName] != "web" ||
		ann[v1alpha1.AnnotationNotifiedUID] != d.Metadata.UID ||
		ann[v1alpha1.AnnotationNotifiedReason] != v1alpha1.NotifyReasonCrashLoop {
		t.Errorf("dedup annotations wrong: %v", ann)
	}
	// The IMP_NOTIFY_* contract (M10-a5): every variable present, exit code
	// from lastTerminated (M10-e), prepended before template env.
	env := run.Spec.Env
	for name, want := range map[string]string{
		"IMP_NOTIFY_KIND":      "Daemon",
		"IMP_NOTIFY_NAME":      "web",
		"IMP_NOTIFY_PROC":      "web-0-abc",
		"IMP_NOTIFY_REASON":    v1alpha1.NotifyReasonCrashLoop,
		"IMP_NOTIFY_EXIT_CODE": "7",
		"IMP_NOTIFY_RESTARTS":  "4",
		"IMP_NOTIFY_SIGNAL":    "",
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if envValue(env, "CHANNEL") != "ops" {
		t.Error("template env lost")
	}
	if env[0].Name != "IMP_NOTIFY_KIND" {
		t.Errorf("IMP_NOTIFY_* not prepended: first env is %s", env[0].Name)
	}

	// Status rollup: lastNotificationTime set, Active=True (run not
	// terminal yet).
	n, err := h.cl.GetNotifier(t.Context(), "pager")
	if err != nil {
		t.Fatalf("GetNotifier: %v", err)
	}
	if n.Status.LastNotificationTime.IsZero() {
		t.Error("LastNotificationTime not set")
	}
	cond := v1alpha1.FindStatusCondition(n.Status.Conditions, v1alpha1.ConditionTypeActive)
	if cond == nil || cond.Status != v1alpha1.ConditionTrue || cond.Reason != "Notifying" {
		t.Errorf("Active condition = %+v, want True/Notifying", cond)
	}
}

func TestMinRestartsGates(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", nil) // default minRestarts 3
	d := applyDaemon(t, h, "web", nil)
	p, st := crashLoopProc("web-0-abc", d, 2, 1)
	applyProcWithStatus(t, h, p, st)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runs := notificationRuns(t, h, "pager"); len(runs) != 0 {
		t.Fatalf("got %d runs below minRestarts, want 0", len(runs))
	}
}

func TestCooldownHoldsThenReleases(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", func(n *v1alpha1.Notifier) {
		n.Spec.CooldownSeconds = new(int32(60))
	})
	d := applyDaemon(t, h, "web", nil)
	p, st := crashLoopProc("web-0-abc", d, 5, 7)
	applyProcWithStatus(t, h, p, st)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	waitFor(t, "run in cache", func() bool { return len(h.c.ownRuns("pager")) == 1 })

	// Second pass inside the cooldown: no new run, and a wake scheduled for
	// the cooldown's expiry.
	err := reconcile(t, h, "pager")
	requeue, ok := errors.AsType[controllers.RequeueAfter](err)
	if !ok {
		t.Fatalf("second Reconcile = %v, want RequeueAfter", err)
	}
	if requeue.After <= 0 || requeue.After > 60*time.Second {
		t.Errorf("RequeueAfter = %s, want within (0, 60s]", requeue.After)
	}
	if runs := notificationRuns(t, h, "pager"); len(runs) != 1 {
		t.Fatalf("cooldown violated: %d runs", len(runs))
	}

	// Cross the cooldown: the level still holds, so it re-fires.
	h.clk.Step(61 * time.Second)
	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if runs := notificationRuns(t, h, "pager"); len(runs) != 2 {
		t.Fatalf("level did not re-fire after cooldown: %d runs", len(runs))
	}
}

func TestRunFailedFromTimer(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", nil)
	// A failed timer run: owner identity from the ownerReference alone —
	// the Timer object itself need not be in the cache for the signal.
	exit := 3
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:   "cli-fetch-1752888300",
			Labels: map[string]string{v1alpha1.LabelTimerName: "cli-fetch"},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindTimer,
				Name:       "cli-fetch",
				UID:        "timer-uid-1",
			}},
		},
		Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/usr/local/bin/ft", "cli-fetch"}, RestartPolicy: v1alpha1.RestartPolicyNever},
	}
	status := v1alpha1.ProcStatus{
		Phase: v1alpha1.ProcPhaseFailed,
		State: v1alpha1.ProcState{Terminated: &v1alpha1.ProcStateTerminated{
			ExitCode: &exit,
			Message:  "exited with code 3",
		}},
	}
	applyProcWithStatus(t, h, p, status)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	runs := notificationRuns(t, h, "pager")
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	env := runs[0].Spec.Env
	if envValue(env, "IMP_NOTIFY_KIND") != "Timer" ||
		envValue(env, "IMP_NOTIFY_NAME") != "cli-fetch" ||
		envValue(env, "IMP_NOTIFY_REASON") != v1alpha1.NotifyReasonRunFailed ||
		envValue(env, "IMP_NOTIFY_EXIT_CODE") != "3" {
		t.Errorf("env contract wrong: %v", env)
	}
}

func TestRolloutStuckNotifies(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", nil)
	d := applyDaemon(t, h, "web", nil)
	d.Status = v1alpha1.DaemonStatus{
		Conditions: []v1alpha1.Condition{{
			Type:               v1alpha1.ConditionTypeProgressing,
			Status:             v1alpha1.ConditionFalse,
			Reason:             v1alpha1.ReasonProgressDeadlineExceeded,
			Message:            "rollout has made no progress for 600s",
			LastTransitionTime: v1alpha1.NewTime(time.Now()),
		}},
	}
	if _, err := h.cl.UpdateDaemonStatus(t.Context(), d); err != nil {
		t.Fatalf("UpdateDaemonStatus: %v", err)
	}
	waitFor(t, "daemon condition in cache", func() bool {
		raw, ok := h.daemons.GetByKey(v1alpha1.KindDaemon + "/web")
		return ok && strings.Contains(string(raw), v1alpha1.ReasonProgressDeadlineExceeded)
	})

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	runs := notificationRuns(t, h, "pager")
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	env := runs[0].Spec.Env
	if envValue(env, "IMP_NOTIFY_REASON") != v1alpha1.NotifyReasonRolloutStuck ||
		envValue(env, "IMP_NOTIFY_KIND") != "Daemon" {
		t.Errorf("env contract wrong: %v", env)
	}
}

func TestNoMetaAlerting(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", nil)
	// A FAILED notification run — labeled with a notifier name (even
	// another notifier's) — must never signal.
	exit := 1
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{
			Name:   "other-abc-1752888300",
			Labels: map[string]string{v1alpha1.LabelNotifierName: "other"},
		},
		Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/false"}, RestartPolicy: v1alpha1.RestartPolicyNever},
	}
	applyProcWithStatus(t, h, p, v1alpha1.ProcStatus{
		Phase: v1alpha1.ProcPhaseFailed,
		State: v1alpha1.ProcState{Terminated: &v1alpha1.ProcStateTerminated{ExitCode: &exit}},
	})

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runs := notificationRuns(t, h, "pager"); len(runs) != 0 {
		t.Fatalf("meta-alerting: %d runs for a failed notification run", len(runs))
	}
}

func TestSelectorScopesTargets(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", func(n *v1alpha1.Notifier) {
		n.Spec.Selector = "app=web"
	})
	dOther := applyDaemon(t, h, "db", map[string]string{"app": "db"})
	pOther, stOther := crashLoopProc("db-0-abc", dOther, 5, 1)
	applyProcWithStatus(t, h, pOther, stOther)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if runs := notificationRuns(t, h, "pager"); len(runs) != 0 {
		t.Fatalf("selector ignored: %d runs for non-matching target", len(runs))
	}

	dWeb := applyDaemon(t, h, "web", map[string]string{"app": "web"})
	pWeb, stWeb := crashLoopProc("web-0-abc", dWeb, 5, 1)
	applyProcWithStatus(t, h, pWeb, stWeb)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	runs := notificationRuns(t, h, "pager")
	if len(runs) != 1 || runs[0].Metadata.Annotations[v1alpha1.AnnotationNotifiedName] != "web" {
		t.Fatalf("selector scoping wrong: %v", runs)
	}
}

func TestReplicasCollapseToOneNotification(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", nil)
	d := applyDaemon(t, h, "web", nil)
	p0, st0 := crashLoopProc("web-0-abc", d, 4, 7)
	applyProcWithStatus(t, h, p0, st0)
	p1, st1 := crashLoopProc("web-1-abc", d, 6, 7)
	applyProcWithStatus(t, h, p1, st1)

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	runs := notificationRuns(t, h, "pager")
	if len(runs) != 1 {
		t.Fatalf("got %d runs for one daemon, want 1 (dedup key is target+reason)", len(runs))
	}
	// The worst replica wins the message.
	if got := envValue(runs[0].Spec.Env, "IMP_NOTIFY_RESTARTS"); got != "6" {
		t.Errorf("IMP_NOTIFY_RESTARTS = %s, want 6 (worst replica)", got)
	}
}

func TestHistoryPruning(t *testing.T) {
	h := startHarness(t)
	applyNotifier(t, h, "pager", func(n *v1alpha1.Notifier) {
		n.Spec.HistoryLimit = new(int32(1))
	})
	// Three finished runs, distinct targets so dedup does not interfere.
	for i, name := range []string{"pager-r1", "pager-r2", "pager-r3"} {
		exit := 0
		p := &v1alpha1.Proc{
			Metadata: v1alpha1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{v1alpha1.LabelNotifierName: "pager"},
				Annotations: map[string]string{
					v1alpha1.AnnotationNotifiedKind:   v1alpha1.KindProc,
					v1alpha1.AnnotationNotifiedName:   name,
					v1alpha1.AnnotationNotifiedUID:    name + "-uid",
					v1alpha1.AnnotationNotifiedReason: v1alpha1.NotifyReasonRunFailed,
				},
			},
			Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/true"}, RestartPolicy: v1alpha1.RestartPolicyNever},
		}
		applyProcWithStatus(t, h, p, v1alpha1.ProcStatus{
			Phase: v1alpha1.ProcPhaseSucceeded,
			State: v1alpha1.ProcState{Terminated: &v1alpha1.ProcStateTerminated{ExitCode: &exit}},
		})
		_ = i
	}

	if err := reconcile(t, h, "pager"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitFor(t, "history pruned", func() bool {
		return len(notificationRuns(t, h, "pager")) == 1
	})
}
