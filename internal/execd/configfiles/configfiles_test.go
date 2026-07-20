// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package configfiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
)

type fakeGetter struct{ configs map[string]*v1alpha1.Config }

func (f fakeGetter) GetConfig(_ context.Context, name string) (*v1alpha1.Config, error) {
	cfg, ok := f.configs[name]
	if !ok {
		return nil, fmt.Errorf("config %q: %w", name, v1alpha1.ErrNotFound)
	}
	return cfg, nil
}

func config(name string, mode *string, data map[string]string) *v1alpha1.Config {
	return &v1alpha1.Config{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ConfigSpec{Data: data, Mode: mode},
	}
}

func proc(name string, configs ...string) *v1alpha1.Proc {
	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ProcSpec{Command: []string{"/bin/true"}, Configs: configs},
	}
}

func newStore(t *testing.T, cfgs ...*v1alpha1.Config) *Store {
	t.Helper()
	m := map[string]*v1alpha1.Config{}
	for _, c := range cfgs {
		m[c.Metadata.Name] = c
	}
	return New(filepath.Join(t.TempDir(), "configs"), fakeGetter{configs: m})
}

func TestMaterializeWritesFiles(t *testing.T) {
	s := newStore(t, config("app", nil, map[string]string{
		"app.conf":  "listen 8080\n",
		"logrotate": "daily\n",
	}))
	p := proc("web-0", "app")
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	base := s.Dir("web-0")
	got, err := os.ReadFile(filepath.Join(base, "app", "app.conf"))
	if err != nil {
		t.Fatalf("reading materialized file: %v", err)
	}
	if string(got) != "listen 8080\n" {
		t.Errorf("content = %q, want %q", got, "listen 8080\n")
	}
	// Default mode is 0644.
	info, err := os.Stat(filepath.Join(base, "app", "app.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %o, want 0644 (default)", info.Mode().Perm())
	}
	// Per-proc dir is private (0700).
	dirInfo, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("per-proc dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestMaterializeMode(t *testing.T) {
	s := newStore(t, config("secret", new("0600"), map[string]string{"token": "s3cr3t\n"}))
	if err := s.Materialize(t.Context(), proc("web-0", "secret")); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.Dir("web-0"), "secret", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestMaterializeNoConfigsIsNoop(t *testing.T) {
	s := newStore(t)
	if err := s.Materialize(t.Context(), proc("web-0")); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := os.Stat(s.Dir("web-0")); !os.IsNotExist(err) {
		t.Errorf("config-less proc created a dir: %v", err)
	}
}

func TestMaterializeMissingConfigIsNotFound(t *testing.T) {
	s := newStore(t) // no configs registered
	err := s.Materialize(t.Context(), proc("web-0", "absent"))
	if !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("Materialize error = %v, want wrapping ErrNotFound (so execd emits ConfigMissing)", err)
	}
}

func TestMaterializeDropsStaleFiles(t *testing.T) {
	s := newStore(t, config("app", nil, map[string]string{"old.conf": "x\n", "keep.conf": "y\n"}))
	p := proc("web-0", "app")
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// The Config drops old.conf; a re-spawn must not leave it behind.
	s.getter = fakeGetter{configs: map[string]*v1alpha1.Config{
		"app": config("app", nil, map[string]string{"keep.conf": "y\n"}),
	}}
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("re-Materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("web-0"), "app", "old.conf")); !os.IsNotExist(err) {
		t.Errorf("stale file survived re-materialization: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("web-0"), "app", "keep.conf")); err != nil {
		t.Errorf("kept file missing after re-materialization: %v", err)
	}
}

func TestRemove(t *testing.T) {
	s := newStore(t, config("app", nil, map[string]string{"app.conf": "x\n"}))
	p := proc("web-0", "app")
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if err := s.Remove("web-0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(s.Dir("web-0")); !os.IsNotExist(err) {
		t.Errorf("per-proc dir survived Remove: %v", err)
	}
	// Remove is idempotent (safe for config-less procs).
	if err := s.Remove("never-materialized"); err != nil {
		t.Errorf("Remove of absent dir = %v, want nil", err)
	}
}

func TestResolveMode(t *testing.T) {
	m, err := resolveMode(nil)
	if err != nil || m != 0o644 {
		t.Errorf("resolveMode(nil) = %o, %v; want 0644, nil", m, err)
	}
	m, err = resolveMode(new("0600"))
	if err != nil || m != 0o600 {
		t.Errorf("resolveMode(0600) = %o, %v; want 0600, nil", m, err)
	}
}
