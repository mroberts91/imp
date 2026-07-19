// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/pkg/client"
)

type fixture struct {
	client *client.Client
	dir    string
	w      *Watcher
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base, err := os.MkdirTemp("", "imp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	store, err := etcl.Open(filepath.Join(base, "etcl.db"), nil)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := apiserver.New(apiserver.Config{Store: store})
	socket := filepath.Join(base, "impd.sock")
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(l) //nolint:errcheck // ends with Close
	t.Cleanup(func() { hs.Close() })

	c := client.New(socket)
	dir := filepath.Join(base, "manifests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return &fixture{client: c, dir: dir, w: NewWatcher(c, dir, nil, nil)}
}

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func (f *fixture) scan(t *testing.T) {
	t.Helper()
	if err := f.w.scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

func daemonYAML(name, arg string) string {
	return fmt.Sprintf(`apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: %s
spec:
  template:
    spec:
      command: ["/bin/sleep", %q]
`, name, arg)
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestScanAppliesAndAnnotates(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.write(t, "web.yaml", daemonYAML("web", "60"))
	f.write(t, "worker.yaml", daemonYAML("worker", "60"))
	f.scan(t)

	web, err := f.client.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if web.Metadata.Annotations[v1alpha1.AnnotationManagedBy] != v1alpha1.ManagedByManifest {
		t.Errorf("managed-by annotation missing: %v", web.Metadata.Annotations)
	}
	if web.Metadata.Annotations[v1alpha1.AnnotationSourcePath] != "web.yaml" {
		t.Errorf("source-path = %q, want web.yaml", web.Metadata.Annotations[v1alpha1.AnnotationSourcePath])
	}
	if _, err := f.client.GetDaemon(ctx, "worker"); err != nil {
		t.Fatalf("GetDaemon(worker): %v", err)
	}

	// Idempotent. a second scan writes nothing.
	rvBefore := web.Metadata.ResourceVersion
	f.scan(t)
	webAfter, err := f.client.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if webAfter.Metadata.ResourceVersion != rvBefore {
		t.Errorf("idempotent re-scan churned rv: %s -> %s", rvBefore, webAfter.Metadata.ResourceVersion)
	}
}

func TestScanFirstFileWins(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// Sorted path order decides a.yaml beats z.yaml.
	f.write(t, "z.yaml", daemonYAML("web", "999"))
	f.write(t, "a.yaml", daemonYAML("web", "111"))
	f.scan(t)

	web, err := f.client.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if got := web.Spec.Template.Spec.Command[1]; got != "111" {
		t.Errorf("winning spec came from the wrong file: arg = %q", got)
	}
	if web.Metadata.Annotations[v1alpha1.AnnotationSourcePath] != "a.yaml" {
		t.Errorf("source-path = %q, want a.yaml", web.Metadata.Annotations[v1alpha1.AnnotationSourcePath])
	}

	// The loser is reported as a Warning event; rescans aggregate onto the
	// same object rather than creating another.
	f.scan(t)
	events, _, err := f.client.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Count < 2 {
		t.Errorf("count = %d, want >= 2 after rescan aggregation", ev.Count)
	}
	if ev.Type != v1alpha1.EventTypeWarning || ev.Reason != v1alpha1.ReasonFailedValidation {
		t.Errorf("event = %s/%s", ev.Type, ev.Reason)
	}
	if !strings.Contains(ev.Message, "a.yaml") || !strings.Contains(ev.Message, "z.yaml") {
		t.Errorf("event message does not name both files: %s", ev.Message)
	}
	if ev.Regarding.Name != "web" || ev.ReportingComponent != "manifest" {
		t.Errorf("event envelope: %+v", ev)
	}
}

func TestScanDeletesRemovedManifests(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.write(t, "web.yaml", daemonYAML("web", "60"))
	f.write(t, "worker.yaml", daemonYAML("worker", "60"))
	f.scan(t)

	if err := os.Remove(filepath.Join(f.dir, "web.yaml")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	f.scan(t)

	if _, err := f.client.GetDaemon(ctx, "web"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("web after manifest removal: err = %v, want ErrNotFound", err)
	}
	if _, err := f.client.GetDaemon(ctx, "worker"); err != nil {
		t.Errorf("worker should have survived: %v", err)
	}
}

func TestScanCoexistenceRule(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// An impctl-style apply: no manifest annotations.
	manual := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "manual"},
		Spec: v1alpha1.DaemonSpec{
			Template: v1alpha1.ProcTemplate{Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}}},
		},
	}
	if _, err := f.client.ApplyDaemon(ctx, manual); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}

	// Scans (with and without manifests present) never touch it.
	f.scan(t)
	f.write(t, "web.yaml", daemonYAML("web", "60"))
	f.scan(t)
	if err := os.Remove(filepath.Join(f.dir, "web.yaml")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	f.scan(t)

	if _, err := f.client.GetDaemon(ctx, "manual"); err != nil {
		t.Errorf("sweep deleted an object it does not manage: %v", err)
	}
	if _, err := f.client.GetDaemon(ctx, "web"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("manifest-managed web should be gone: err = %v", err)
	}
}

func TestScanParseErrorProtectsObjects(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.write(t, "web.yaml", daemonYAML("web", "60"))
	f.scan(t)

	// The file goes bad (half-written save, stray edit): its object must
	// survive.
	f.write(t, "web.yaml", "kind: [broken\n")
	f.scan(t)
	if _, err := f.client.GetDaemon(ctx, "web"); err != nil {
		t.Fatalf("daemon deleted while its source was unparseable: %v", err)
	}

	// The file heals with a new spec: the object converges.
	f.write(t, "web.yaml", daemonYAML("web", "120"))
	f.scan(t)
	web, err := f.client.GetDaemon(ctx, "web")
	if err != nil {
		t.Fatalf("GetDaemon: %v", err)
	}
	if web.Metadata.Generation != 2 || web.Spec.Template.Spec.Command[1] != "120" {
		t.Errorf("healed file not applied: generation=%d command=%v",
			web.Metadata.Generation, web.Spec.Template.Spec.Command)
	}
}

func TestScanIgnoresEditorNoise(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.write(t, ".hidden.yaml", daemonYAML("hidden", "60"))
	f.write(t, "backup.yaml~", daemonYAML("backup", "60"))
	f.write(t, ".web.yaml.swp", "vim swap garbage \x00\x01")
	f.write(t, "empty.yaml", "")
	if err := os.MkdirAll(filepath.Join(f.dir, "subdir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f.write(t, "subdir/nested.yaml", daemonYAML("nested", "60"))

	f.scan(t)
	daemons, _, err := f.client.ListDaemons(ctx)
	if err != nil {
		t.Fatalf("ListDaemons: %v", err)
	}
	if len(daemons) != 0 {
		t.Errorf("noise produced objects: %+v", daemons)
	}
}

func TestWatcherFsnotifyPath(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// A one-hour re-scan proves reactions come from fsnotify, not the ticker.
	w := NewWatcher(f.client, f.dir, &Options{RescanInterval: time.Hour}, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	f.write(t, "web.yaml", daemonYAML("web", "60"))
	waitFor(t, "daemon created via fsnotify", func() bool {
		_, err := f.client.GetDaemon(ctx, "web")
		return err == nil
	})

	f.write(t, "web.yaml", daemonYAML("web", "120"))
	waitFor(t, "daemon updated via fsnotify", func() bool {
		d, err := f.client.GetDaemon(ctx, "web")
		return err == nil && d.Metadata.Generation == 2
	})

	if err := os.Remove(filepath.Join(f.dir, "web.yaml")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitFor(t, "daemon deleted via fsnotify", func() bool {
		_, err := f.client.GetDaemon(ctx, "web")
		return errors.Is(err, v1alpha1.ErrNotFound)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop on context cancel")
	}
}

// TestWatcherRescanHealsDeadFsnotify replaces the whole watched directory.
// which kills the inotify watch.
func TestWatcherRescanHealsDeadFsnotify(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := NewWatcher(f.client, f.dir, &Options{RescanInterval: 100 * time.Millisecond}, nil)
	go w.Run(ctx) //nolint:errcheck // exercised via effects

	f.write(t, "web.yaml", daemonYAML("web", "60"))
	waitFor(t, "initial create", func() bool {
		_, err := f.client.GetDaemon(ctx, "web")
		return err == nil
	})

	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f.write(t, "other.yaml", daemonYAML("other", "60"))

	waitFor(t, "re-scan catching up after directory replacement", func() bool {
		_, otherErr := f.client.GetDaemon(ctx, "other")
		_, webErr := f.client.GetDaemon(ctx, "web")
		return otherErr == nil && errors.Is(webErr, v1alpha1.ErrNotFound)
	})
}
