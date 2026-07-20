// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// End-to-end tests
package apiserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/pkg/client"
)

type fixture struct {
	client *client.Client
	store  *etcl.Store
	socket string
}

func start(t *testing.T, storeOpts *etcl.Options, logs apiserver.LogStreamer) *fixture {
	t.Helper()
	return startWith(t, storeOpts, func(cfg *apiserver.Config) { cfg.Logs = logs })
}

func startWith(t *testing.T, storeOpts *etcl.Options, mutate func(*apiserver.Config)) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "imp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	store, err := etcl.Open(filepath.Join(dir, "etcl.db"), storeOpts)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := apiserver.Config{
		Store:   store,
		Version: v1alpha1.VersionInfo{Version: "test", Commit: "abc123"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := apiserver.New(cfg)
	socket := filepath.Join(dir, "impd.sock")
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(l) //nolint:errcheck // ends with Close
	t.Cleanup(func() { hs.Close() })

	return &fixture{client: client.New(socket), store: store, socket: socket}
}

func testDaemon(name string) *v1alpha1.Daemon {
	return &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.DaemonSpec{
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
}

func TestApplyCreatesWithDefaults(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	created, err := f.client.ApplyDaemon(ctx, testDaemon("web"))
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	if created.Metadata.UID == "" || created.Metadata.ResourceVersion != "1" || created.Metadata.Generation != 1 {
		t.Errorf("identity not assigned: %+v", created.Metadata)
	}
	if created.APIVersion != v1alpha1.APIVersion || created.Kind != v1alpha1.KindDaemon {
		t.Errorf("TypeMeta not stamped: %+v", created.TypeMeta)
	}
	// Server-side defaulting happened before the write.
	if created.Spec.Replicas == nil || *created.Spec.Replicas != 1 {
		t.Errorf("Replicas = %v, want 1", created.Spec.Replicas)
	}
	if created.Spec.UpdateStrategy.Type != v1alpha1.UpdateStrategyRecreate {
		t.Errorf("UpdateStrategy = %q", created.Spec.UpdateStrategy.Type)
	}
	if created.Spec.Template.Spec.StopSignal != "TERM" {
		t.Errorf("StopSignal = %q, want TERM", created.Spec.Template.Spec.StopSignal)
	}

	got, err := f.client.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if got.Metadata.UID != created.Metadata.UID {
		t.Errorf("Get returned a different object: %+v", got.Metadata)
	}
}

func TestApplyReplaceSemantics(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	created, err := f.client.ApplyDaemon(ctx, testDaemon("web"))
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}

	// Seed status through the status path, as the controller would.
	created.Status = v1alpha1.DaemonStatus{Replicas: 1, ObservedGeneration: 1}
	withStatus, err := f.client.UpdateDaemonStatus(ctx, created)
	if err != nil {
		t.Fatalf("UpdateDaemonStatus: %v", err)
	}

	changed := testDaemon("web")
	changed.Spec.Template.Spec.Command = []string{"/bin/sleep", "120"}
	changed.Status = v1alpha1.DaemonStatus{Replicas: 99} // must be ignored
	replaced, err := f.client.ApplyDaemon(ctx, changed)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if replaced.Metadata.Generation != 2 {
		t.Errorf("generation = %d, want 2", replaced.Metadata.Generation)
	}
	if replaced.Metadata.UID != withStatus.Metadata.UID {
		t.Error("replace changed identity")
	}
	if replaced.Status.Replicas != 1 || replaced.Status.ObservedGeneration != 1 {
		t.Errorf("status not preserved across apply: %+v", replaced.Status)
	}

	// Idempotent re-apply, no rv churn.
	again, err := f.client.ApplyDaemon(ctx, changed)
	if err != nil {
		t.Fatalf("idempotent re-apply: %v", err)
	}
	if again.Metadata.ResourceVersion != replaced.Metadata.ResourceVersion {
		t.Errorf("no-op apply churned rv: %s -> %s",
			replaced.Metadata.ResourceVersion, again.Metadata.ResourceVersion)
	}
}

func TestApplyValidationErrors(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	bad := testDaemon("web")
	bad.Spec.Template.Spec.Command = nil
	bad.Spec.Replicas = new(int32(-1))
	_, err := f.client.ApplyDaemon(ctx, bad)
	if !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	var inv *v1alpha1.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("err %T does not unwrap to *InvalidError", err)
	}
	fields := make(map[string]bool)
	for _, fe := range inv.Errs {
		fields[fe.Field] = true
	}
	if !fields["spec.replicas"] || !fields["spec.template.spec.command"] {
		t.Errorf("field errors lost in transit: %v", inv.Errs)
	}

	_, err = f.client.Apply(ctx, v1alpha1.KindDaemon, "web",
		[]byte(`{"metadata":{"name":"web"},"spec":{"replicaz":3,"template":{"spec":{"command":["/bin/true"]}}}}`))
	if !errors.Is(err, v1alpha1.ErrInvalid) || !strings.Contains(err.Error(), "replicaz") {
		t.Errorf("unknown field: err = %v, want ErrInvalid naming replicaz", err)
	}

	_, err = f.client.Apply(ctx, v1alpha1.KindDaemon, "web", []byte(`{"kind":"Proc","metadata":{"name":"web"}}`))
	if !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("kind mismatch: err = %v, want ErrInvalid", err)
	}

	_, err = f.client.Apply(ctx, v1alpha1.KindDaemon, "web", []byte(`{"metadata":{"name":"other"},"spec":{"template":{"spec":{"command":["/bin/true"]}}}}`))
	if !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("name mismatch: err = %v, want ErrInvalid", err)
	}
}

func TestApplyCAS(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	created, err := f.client.ApplyDaemon(ctx, testDaemon("web"))
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}

	// An apply carrying a stale rv is a conflict, not a silent overwrite.
	stale := testDaemon("web")
	stale.Metadata.ResourceVersion = "999"
	if _, err := f.client.ApplyDaemon(ctx, stale); !errors.Is(err, v1alpha1.ErrConflict) {
		t.Errorf("stale rv apply: err = %v, want ErrConflict", err)
	}

	// Carrying the current rv works.
	fresh := testDaemon("web")
	fresh.Metadata.ResourceVersion = created.Metadata.ResourceVersion
	fresh.Spec.Template.Spec.Command = []string{"/bin/sleep", "90"}
	if _, err := f.client.ApplyDaemon(ctx, fresh); err != nil {
		t.Errorf("current rv apply: %v", err)
	}
}

func TestListAndDelete(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	for _, name := range []string{"beta", "alpha"} {
		if _, err := f.client.ApplyDaemon(ctx, testDaemon(name)); err != nil {
			t.Fatalf("ApplyDaemon(%s): %v", name, err)
		}
	}
	daemons, listRV, err := f.client.ListDaemons(ctx)
	if err != nil {
		t.Fatalf("ListDaemons: %v", err)
	}
	if len(daemons) != 2 || daemons[0].Metadata.Name != "alpha" || daemons[1].Metadata.Name != "beta" {
		t.Errorf("ListDaemons = %+v", daemons)
	}
	if listRV == "" || listRV == "0" {
		t.Errorf("listRV = %q, want a real snapshot rv", listRV)
	}

	// Empty kinds list as empty, not error.
	procs, _, err := f.client.ListProcs(ctx)
	if err != nil || len(procs) != 0 {
		t.Errorf("ListProcs = %v, %v; want empty, nil", procs, err)
	}

	if err := f.client.DeleteDaemon(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteDaemon: %v", err)
	}
	if _, err := f.client.GetDaemon(ctx, "alpha"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("get after delete: err = %v, want ErrNotFound", err)
	}
	if err := f.client.DeleteDaemon(ctx, "alpha"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("double delete: err = %v, want ErrNotFound", err)
	}
}

func TestStatusSubResource(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	created, err := f.client.ApplyDaemon(ctx, testDaemon("web"))
	if err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}

	// No rv -> required-field error: status writes are always CAS.
	noRV := *created
	noRV.Metadata.ResourceVersion = ""
	if _, err := f.client.UpdateDaemonStatus(ctx, &noRV); !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("status without rv: err = %v, want ErrInvalid", err)
	}

	// Stale rv -> conflict.
	staleRV := *created
	staleRV.Metadata.ResourceVersion = "999"
	if _, err := f.client.UpdateDaemonStatus(ctx, &staleRV); !errors.Is(err, v1alpha1.ErrConflict) {
		t.Errorf("status with stale rv: err = %v, want ErrConflict", err)
	}

	// A status write cannot contain a spec change.
	sneaky := *created
	sneaky.Spec.Template.Spec.Command = []string{"/bin/evil"}
	sneaky.Status = v1alpha1.DaemonStatus{Replicas: 1}
	updated, err := f.client.UpdateDaemonStatus(ctx, &sneaky)
	if err != nil {
		t.Fatalf("UpdateDaemonStatus: %v", err)
	}
	if updated.Spec.Template.Spec.Command[0] != "/bin/sleep" {
		t.Errorf("status write changed spec: %v", updated.Spec.Template.Spec.Command)
	}
	if updated.Status.Replicas != 1 {
		t.Errorf("status not written: %+v", updated.Status)
	}
	if updated.Metadata.Generation != 1 {
		t.Errorf("status write bumped generation to %d", updated.Metadata.Generation)
	}
}

func TestRetryOnConflict(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()
	if _, err := f.client.ApplyDaemon(ctx, testDaemon("web")); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}

	// Two writers race the same status update; RetryOnConflict absorbs the
	// loser's conflict.
	race := func(replicas int32) error {
		return client.RetryOnConflict(func() error {
			d, err := f.client.GetDaemon(ctx, "web")
			if err != nil {
				return err
			}
			d.Status.Replicas = replicas
			_, err = f.client.UpdateDaemonStatus(ctx, d)
			return err
		})
	}
	errc := make(chan error, 2)
	go func() { errc <- race(1) }()
	go func() { errc <- race(2) }()
	for range 2 {
		if err := <-errc; err != nil {
			t.Errorf("racing status update failed despite retry: %v", err)
		}
	}
}

func TestProcSpecImmutable(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	proc := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "web-0-abc123"},
		Spec:     v1alpha1.ProcSpec{Command: []string{"/bin/sleep", "60"}},
	}
	if _, err := f.client.ApplyProc(ctx, proc); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	// Identical re-apply is fine (idempotent).
	if _, err := f.client.ApplyProc(ctx, proc); err != nil {
		t.Errorf("idempotent proc re-apply: %v", err)
	}
	// A changed spec is rejected, even if the rv is current. Procs are immutable.
	mutated := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "web-0-abc123"},
		Spec:     v1alpha1.ProcSpec{Command: []string{"/bin/sleep", "999"}},
	}
	if _, err := f.client.ApplyProc(ctx, mutated); !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("mutating proc spec: err = %v, want ErrInvalid", err)
	}
}

func TestEventCountIncrement(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	ev := &v1alpha1.Event{
		Metadata:  v1alpha1.ObjectMeta{Name: "web-0.1a2b3c"},
		Regarding: v1alpha1.ObjectRef{Kind: v1alpha1.KindProc, Name: "web-0"},
		Type:      v1alpha1.EventTypeWarning,
		Reason:    v1alpha1.ReasonBackOff,
		Message:   "back-off restarting failed process",
		Count:     1,
	}
	created, err := f.client.ApplyEvent(ctx, ev)
	if err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}

	created.Count++
	updated, err := f.client.ApplyEvent(ctx, created)
	if err != nil {
		t.Fatalf("count++ apply: %v", err)
	}
	if updated.Count != 2 {
		t.Errorf("count = %d, want 2", updated.Count)
	}
}

func TestWatchOverHTTP(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	if _, err := f.client.ApplyDaemon(ctx, testDaemon("pre")); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	_, listRV, err := f.client.ListDaemons(ctx)
	if err != nil {
		t.Fatalf("ListDaemons: %v", err)
	}

	events, cancel, err := f.client.Watch(ctx, v1alpha1.KindDaemon, listRV)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer cancel()

	if _, err := f.client.ApplyDaemon(ctx, testDaemon("post")); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	select {
	case ev := <-events:
		if ev.Type != v1alpha1.WatchAdded {
			t.Errorf("event type = %s, want ADDED", ev.Type)
		}
		var d v1alpha1.Daemon
		if err := json.Unmarshal(ev.Object, &d); err != nil || d.Metadata.Name != "post" {
			t.Errorf("event object = %s (err %v), want daemon post", ev.Object, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no watch event arrived over HTTP")
	}

	cancel()
	select {
	case _, ok := <-events:
		if ok {
			// One buffered event may race the cancel; the close must follow.
			if _, ok := <-events; ok {
				t.Error("watch channel still open after cancel")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch channel did not close after cancel")
	}
}

func TestWatchCompactedOverHTTP(t *testing.T) {
	var offset atomic.Int64
	f := start(t, &etcl.Options{
		RetainRows:   5,
		RetainWindow: time.Minute,
		CompactEvery: 10_000,
		Now:          func() time.Time { return time.Now().Add(time.Duration(offset.Load()) * time.Second) },
	}, nil)
	ctx := t.Context()

	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if _, err := f.client.ApplyDaemon(ctx, testDaemon(name)); err != nil {
			t.Fatalf("ApplyDaemon: %v", err)
		}
	}
	offset.Store(3600) // age every changelog row past the retention window
	if err := f.store.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	_, _, err := f.client.Watch(ctx, v1alpha1.KindDaemon, "1")
	if !errors.Is(err, v1alpha1.ErrCompacted) {
		t.Errorf("watch from compacted rv: err = %v, want ErrCompacted", err)
	}
}

func TestUnknownResourceAndVersion(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	// Unknown kind paths 404 through the wire error translation.
	hc := &http.Client{Transport: unixTransport(f.socket)}
	resp, err := hc.Get("http://impd/apis/impd.sh/v1alpha1/gizmos")
	if err != nil {
		t.Fatalf("GET gizmos: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown kind status = %d, want 404", resp.StatusCode)
	}

	v, err := f.client.ServerVersion(ctx)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if v.Version != "test" || v.Commit != "abc123" || v.GoVersion == "" {
		t.Errorf("version = %+v", v)
	}

	resp2, err := hc.Get("http://impd/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp2.StatusCode)
	}
}

// fakeStreamer serves log content and records the request.
type fakeStreamer struct {
	gotName string
	gotOpts apiserver.LogOptions
	content string
}

func (fs *fakeStreamer) Tail(name string, opts apiserver.LogOptions) (io.ReadCloser, error) {
	fs.gotName, fs.gotOpts = name, opts
	if name == "missing" {
		return nil, v1alpha1.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(fs.content)), nil
}

func TestLogRoute(t *testing.T) {
	ctx := t.Context()

	f := start(t, nil, nil)
	if _, err := f.client.ProcLogs(ctx, "web-0", client.LogOptions{}); err == nil {
		t.Error("ProcLogs with no streamer succeeded")
	}

	fs := &fakeStreamer{content: "line one\nline two\n"}
	f = start(t, nil, fs)
	// The handler now confirms the Proc exists before streaming, so a 404 is
	// an honest "no such Proc" independent of the streamer.
	if _, err := f.client.ApplyProc(ctx, &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "web-0"},
		Spec:     v1alpha1.ProcSpec{Command: []string{"/bin/sleep", "60"}},
	}); err != nil {
		t.Fatalf("ApplyProc: %v", err)
	}
	rc, err := f.client.ProcLogs(ctx, "web-0", client.LogOptions{Follow: true, TailLines: 10, Timestamps: true})
	if err != nil {
		t.Fatalf("ProcLogs: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading log stream: %v", err)
	}
	if string(data) != fs.content {
		t.Errorf("log content = %q, want %q", data, fs.content)
	}
	if fs.gotName != "web-0" || !fs.gotOpts.Follow || fs.gotOpts.TailLines != 10 || !fs.gotOpts.Timestamps {
		t.Errorf("streamer got name=%q opts=%+v", fs.gotName, fs.gotOpts)
	}
	if _, err := f.client.ProcLogs(ctx, "missing", client.LogOptions{}); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("missing proc logs: err = %v, want ErrNotFound", err)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	f := start(t, nil, nil)

	// A second Listen on a socket someone is serving must fail — this is
	// the single-instance guard.
	if _, err := apiserver.Listen(f.socket); err == nil {
		t.Fatal("Listen on a live socket succeeded; the second instance should be refused")
	}

	if _, err := f.client.ServerVersion(t.Context()); err != nil {
		t.Fatalf("first server broken after refused second Listen: %v", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "impd.sock")

	// Manufacture an unclean shutdown: a socket file whose owner is gone.
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("stale socket file missing after close: %v", err)
	}

	l2, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	l2.Close()
}

func TestListenRefusesNonSocketFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "impd.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := apiserver.Listen(path); err == nil {
		t.Fatal("Listen over a regular file succeeded; it should refuse")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "not a socket" {
		t.Errorf("regular file was disturbed: %q, %v", data, err)
	}
}

func unixTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
}

func testTimer(name string) *v1alpha1.Timer {
	return &v1alpha1.Timer{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.TimerSpec{
			Schedule: "@every 1m",
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/true"}},
			},
		},
	}
}

// TestListLabelSelector covers the list-only server-side label selector (M9-i):
// equality, inequality, AND, missing-key semantics, malformed 400, and that
// watch ignores the selector entirely.
func TestListLabelSelector(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	apply := func(name string, labels map[string]string) {
		d := testDaemon(name)
		d.Metadata.Labels = labels
		if _, err := f.client.ApplyDaemon(ctx, d); err != nil {
			t.Fatalf("ApplyDaemon(%s): %v", name, err)
		}
	}
	apply("web-a", map[string]string{"app": "web", "tier": "frontend"})
	apply("web-b", map[string]string{"app": "web", "tier": "backend"})
	apply("db", map[string]string{"app": "db"})
	apply("bare", nil)

	names := func(sel string) []string {
		ds, _, err := f.client.ListDaemons(ctx, client.WithLabelSelector(sel))
		if err != nil {
			t.Fatalf("ListDaemons(%q): %v", sel, err)
		}
		out := make([]string, 0, len(ds))
		for i := range ds {
			out = append(out, ds[i].Metadata.Name)
		}
		slices.Sort(out)
		return out
	}

	if got := names("app=web"); !slices.Equal(got, []string{"web-a", "web-b"}) {
		t.Errorf("app=web → %v", got)
	}
	if got := names("app==web"); !slices.Equal(got, []string{"web-a", "web-b"}) {
		t.Errorf("app==web → %v", got)
	}
	if got := names("app!=web"); !slices.Equal(got, []string{"bare", "db"}) {
		t.Errorf("app!=web → %v (a missing key satisfies !=)", got)
	}
	if got := names("app=web,tier=frontend"); !slices.Equal(got, []string{"web-a"}) {
		t.Errorf("AND app=web,tier=frontend → %v", got)
	}
	if got := names("app=missing"); len(got) != 0 {
		t.Errorf("app=missing → %v, want none", got)
	}
	if got := names(""); !slices.Equal(got, []string{"bare", "db", "web-a", "web-b"}) {
		t.Errorf("empty selector → %v, want all", got)
	}

	for _, bad := range []string{"app", "=web", "a=b,"} {
		if _, _, err := f.client.ListDaemons(ctx, client.WithLabelSelector(bad)); !errors.Is(err, v1alpha1.ErrInvalid) {
			t.Errorf("malformed selector %q: err = %v, want ErrInvalid", bad, err)
		}
	}

	// Watch ignores labelSelector: a malformed one must not 400 a watch.
	hc := &http.Client{Transport: unixTransport(f.socket)}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(wctx, http.MethodGet,
		"http://impd/apis/impd.sh/v1alpha1/daemons?watch=true&labelSelector=bad", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("watch GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("watch with malformed selector = %d, want 200 (watch ignores selector)", resp.StatusCode)
	}
}

func testConfig(name string) *v1alpha1.Config {
	return &v1alpha1.Config{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ConfigSpec{Data: map[string]string{"app.conf": "listen 8080\n"}},
	}
}

func TestConfigLifecycle(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	applied, err := f.client.ApplyConfig(ctx, testConfig("app"))
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if applied.APIVersion != v1alpha1.APIVersion || applied.Kind != v1alpha1.KindConfig {
		t.Errorf("TypeMeta = %+v", applied.TypeMeta)
	}
	if applied.Spec.Data["app.conf"] != "listen 8080\n" {
		t.Errorf("round-trip lost content: %v", applied.Spec.Data)
	}
	// No defaulting: mode stays nil so the content hash is upgrade-stable.
	if applied.Spec.Mode != nil {
		t.Errorf("apply materialized mode = %q, want nil", *applied.Spec.Mode)
	}

	// A bad filename is a 422 naming the offending key.
	bad := testConfig("bad")
	bad.Spec.Data = map[string]string{"../evil": "x"}
	if _, err := f.client.ApplyConfig(ctx, bad); !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("bad filename apply error = %v, want ErrInvalid", err)
	}

	// M8-i: a Config has no status subresource — PUT .../status is a 404.
	body := []byte(`{"apiVersion":"impd.sh/v1alpha1","kind":"Config","metadata":{"name":"app","resourceVersion":"` + applied.Metadata.ResourceVersion + `"}}`)
	if _, err := f.client.UpdateStatusRaw(ctx, v1alpha1.KindConfig, "app", body); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("status write error = %v, want ErrNotFound (no status subresource)", err)
	}

	// Only "app" persisted (the invalid apply must not have).
	configs, _, err := f.client.ListConfigs(ctx)
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs = %d configs, err %v; want 1", len(configs), err)
	}
	if err := f.client.DeleteConfig(ctx, "app"); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
}

// TestConfigAtContentCapApplies pins that the request-body cap accommodates a
// Config at the 1 MiB content limit validation allows. The body (content +
// envelope) exceeds a 1 MiB cap, so a too-tight maxBodyBytes would reject a
// valid, documented-size Config with an opaque "request body too large".
func TestConfigAtContentCapApplies(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	big := testConfig("big")
	big.Spec.Data = map[string]string{"big.conf": strings.Repeat("a", 1<<20)} // exactly the cap
	applied, err := f.client.ApplyConfig(ctx, big)
	if err != nil {
		t.Fatalf("ApplyConfig at content cap: %v", err)
	}
	if len(applied.Spec.Data["big.conf"]) != 1<<20 {
		t.Errorf("round-trip truncated content: got %d bytes", len(applied.Spec.Data["big.conf"]))
	}
}

func TestTimerLifecycle(t *testing.T) {
	f := start(t, nil, nil)
	ctx := t.Context()

	applied, err := f.client.ApplyTimer(ctx, testTimer("backup"))
	if err != nil {
		t.Fatalf("ApplyTimer: %v", err)
	}
	// Server-side defaulting ran.
	if applied.Spec.ConcurrencyPolicy != v1alpha1.ConcurrencyForbid {
		t.Errorf("ConcurrencyPolicy = %q, want Forbid", applied.Spec.ConcurrencyPolicy)
	}
	if applied.Spec.Template.Spec.RestartPolicy != v1alpha1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", applied.Spec.Template.Spec.RestartPolicy)
	}

	// Invalid schedule is a 422 naming the field.
	bad := testTimer("bad")
	bad.Spec.Schedule = "whenever"
	if _, err := f.client.ApplyTimer(ctx, bad); !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("bad schedule apply error = %v, want ErrInvalid", err)
	}

	// Status subresource is CAS and does not bump generation.
	applied.Status.LastScheduleTime = v1alpha1.NewTime(time.Now())
	updated, err := f.client.UpdateTimerStatus(ctx, applied)
	if err != nil {
		t.Fatalf("UpdateTimerStatus: %v", err)
	}
	if updated.Metadata.Generation != applied.Metadata.Generation {
		t.Errorf("status write bumped generation %d -> %d", applied.Metadata.Generation, updated.Metadata.Generation)
	}
	if updated.Status.LastScheduleTime.IsZero() {
		t.Error("status write dropped lastScheduleTime")
	}

	// Only "backup" persisted: the invalid apply above must not have.
	timers, _, err := f.client.ListTimers(ctx)
	if err != nil || len(timers) != 1 {
		t.Fatalf("ListTimers = %d timers, err %v; want 1", len(timers), err)
	}
	if err := f.client.DeleteTimer(ctx, "backup"); err != nil {
		t.Fatalf("DeleteTimer: %v", err)
	}
}

type fakeStats struct{ items []v1alpha1.ProcStat }

func (f fakeStats) ProcStats() []v1alpha1.ProcStat { return f.items }

func TestStatsRoute(t *testing.T) {
	ctx := t.Context()

	// No provider wired: an error, not an empty success.
	f := start(t, nil, nil)
	if _, err := f.client.Stats(ctx); err == nil {
		t.Error("Stats with no provider returned nil error, want 501-shaped failure")
	}

	want := []v1alpha1.ProcStat{{
		Proc:               "web-0-abc",
		Owner:              v1alpha1.ObjectRef{Kind: v1alpha1.KindDaemon, Name: "web"},
		CPUUsageUsec:       123456,
		MemoryCurrentBytes: 7 * 1024 * 1024,
		PidsCurrent:        3,
		SampledAt:          v1alpha1.NewTime(time.Now()),
	}}
	f = startWith(t, nil, func(cfg *apiserver.Config) { cfg.Stats = fakeStats{items: want} })
	got, err := f.client.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("stats round-trip mismatch (-want +got):\n%s", diff)
	}

	// Empty provider result serves an empty list, not null.
	f = startWith(t, nil, func(cfg *apiserver.Config) { cfg.Stats = fakeStats{} })
	got, err = f.client.Stats(ctx)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty stats = %v (err %v), want empty non-nil slice", got, err)
	}
}
