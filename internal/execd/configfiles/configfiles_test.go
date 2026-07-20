// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package configfiles

import (
	"bytes"
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
	refs := make([]v1alpha1.ConfigRef, len(configs))
	for i, c := range configs {
		refs[i] = v1alpha1.ConfigRef{Name: c}
	}
	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ProcSpec{Command: []string{"/bin/true"}, Configs: refs},
	}
}

// configFull builds a Config with binary content and per-file modes (M9-c/d).
func configFull(name string, data map[string]string, binary map[string][]byte, mode *string, modes map[string]string) *v1alpha1.Config {
	return &v1alpha1.Config{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ConfigSpec{Data: data, BinaryData: binary, Mode: mode, Modes: modes},
	}
}

// procPath builds a Proc with a single path: config ref (M9-b).
func procPath(name, config, path string) *v1alpha1.Proc {
	return &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.ProcSpec{
			Command: []string{"/bin/true"},
			Configs: []v1alpha1.ConfigRef{{Name: config, Path: &path}},
		},
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

// TestMaterializePathRef pins M9-b path: refs — files land in the named
// absolute directory with per-file modes and binary round-trip, and a path
// record is written.
func TestMaterializePathRef(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "etc", "myapp")
	s := newStore(t, configFull("web",
		map[string]string{"nginx.conf": "server {}\n"},
		map[string][]byte{"cert.der": {0x00, 0x01, 0xff, 0x7f}},
		new("0644"),
		map[string]string{"nginx.conf": "0640"},
	))
	if err := s.Materialize(t.Context(), procPath("web-0", "web", dest)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "nginx.conf"))
	if err != nil || string(got) != "server {}\n" {
		t.Fatalf("nginx.conf = %q, %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(dest, "nginx.conf")); info.Mode().Perm() != 0o640 {
		t.Errorf("nginx.conf mode = %o, want 0640 (per-file)", info.Mode().Perm())
	}
	bin, err := os.ReadFile(filepath.Join(dest, "cert.der"))
	if err != nil || !bytes.Equal(bin, []byte{0x00, 0x01, 0xff, 0x7f}) {
		t.Fatalf("cert.der = %v, %v (binary round-trip)", bin, err)
	}
	if info, _ := os.Stat(filepath.Join(dest, "cert.der")); info.Mode().Perm() != 0o644 {
		t.Errorf("cert.der mode = %o, want 0644 (config Mode)", info.Mode().Perm())
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o755 {
		t.Errorf("shared dir mode = %o, want 0755", info.Mode().Perm())
	}
	if _, err := os.Stat(s.pathRecordFile("web-0")); err != nil {
		t.Errorf("path record missing: %v", err)
	}
}

// TestMaterializePathConflict pins the safety rail: a file imp did not write is
// never overwritten — materialization fails with ErrConfigPathConflict and the
// hand-made file is untouched.
func TestMaterializePathConflict(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "etc")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "app.conf"), []byte("hand-made\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, config("web", nil, map[string]string{"app.conf": "imp\n"}))
	err := s.Materialize(t.Context(), procPath("web-0", "web", dest))
	if !errors.Is(err, v1alpha1.ErrConfigPathConflict) {
		t.Fatalf("Materialize = %v, want ErrConfigPathConflict", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "app.conf")); string(got) != "hand-made\n" {
		t.Errorf("hand-made file overwritten: %q", got)
	}
}

// TestMaterializePathReMaterialize pins that a Proc freely overwrites its own
// path: files on restart (no self-conflict).
func TestMaterializePathReMaterialize(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "etc")
	s := newStore(t, config("web", nil, map[string]string{"app.conf": "v1\n"}))
	p := procPath("web-0", "web", dest)
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("Materialize v1: %v", err)
	}
	s.getter = fakeGetter{configs: map[string]*v1alpha1.Config{
		"web": config("web", nil, map[string]string{"app.conf": "v2\n"}),
	}}
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("re-Materialize its own file = %v, want nil", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "app.conf")); string(got) != "v2\n" {
		t.Errorf("app.conf = %q, want v2", got)
	}
}

// TestRemoveCleansPathFiles pins teardown: recorded path: files are deleted but
// their shared directory is left (imp owns only files it wrote).
func TestRemoveCleansPathFiles(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "etc")
	s := newStore(t, config("web", nil, map[string]string{"app.conf": "x\n"}))
	if err := s.Materialize(t.Context(), procPath("web-0", "web", dest)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if err := s.Remove("web-0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "app.conf")); !os.IsNotExist(err) {
		t.Errorf("path file survived Remove: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("shared dir removed on teardown (should stay): %v", err)
	}
	if _, err := os.Stat(s.pathRecordFile("web-0")); !os.IsNotExist(err) {
		t.Errorf("path record survived Remove: %v", err)
	}
}

// TestMaterializePartialFailureDoesNotPoisonRetry pins the fix for the wedge:
// a path: file written before a later conflict must be recorded as ours, so a
// retry (after the conflict is resolved) does not reject imp's own file.
func TestMaterializePartialFailureDoesNotPoisonRetry(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	// A foreign file makes ref-B conflict on the first pass.
	if err := os.WriteFile(filepath.Join(dirB, "b.conf"), []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStore(t,
		config("cfga", nil, map[string]string{"a.conf": "A\n"}),
		config("cfgb", nil, map[string]string{"b.conf": "B\n"}),
	)
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "web-0"},
		Spec: v1alpha1.ProcSpec{
			Command: []string{"/bin/true"},
			Configs: []v1alpha1.ConfigRef{{Name: "cfga", Path: &dirA}, {Name: "cfgb", Path: &dirB}},
		},
	}
	// First pass: ref-A writes a.conf, ref-B conflicts.
	if err := s.Materialize(t.Context(), p); !errors.Is(err, v1alpha1.ErrConfigPathConflict) {
		t.Fatalf("first Materialize = %v, want ErrConfigPathConflict", err)
	}
	if _, err := os.Stat(filepath.Join(dirA, "a.conf")); err != nil {
		t.Fatalf("a.conf not written on the failing pass: %v", err)
	}
	// Operator resolves the conflict; the retry must NOT reject our own a.conf.
	if err := os.Remove(filepath.Join(dirB, "b.conf")); err != nil {
		t.Fatal(err)
	}
	if err := s.Materialize(t.Context(), p); err != nil {
		t.Fatalf("retry after resolving the conflict = %v, want nil (a.conf must be recognized as ours)", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dirB, "b.conf")); string(got) != "B\n" {
		t.Errorf("b.conf = %q, want B", got)
	}
}

// TestMaterializeNestedPathDirTraversable pins that every directory component
// imp creates for a multi-level path: ref is world-traversable (0755), so a
// dropped-privilege proc can reach its files regardless of impd's umask.
func TestMaterializeNestedPathDirTraversable(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "opt", "app", "etc", "conf.d") // several new levels
	s := newStore(t, config("web", nil, map[string]string{"app.conf": "x\n"}))
	if err := s.Materialize(t.Context(), procPath("web-0", "web", dest)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, d := range []string{
		filepath.Join(base, "opt"),
		filepath.Join(base, "opt", "app"),
		filepath.Join(base, "opt", "app", "etc"),
		dest,
	} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o001 == 0 {
			t.Errorf("%s mode = %o, want world-traversable (execute bit set)", d, info.Mode().Perm())
		}
	}
}

// TestMaterializeBinaryAndModes pins binaryData + per-file Modes in the
// per-proc tree.
func TestMaterializeBinaryAndModes(t *testing.T) {
	s := newStore(t, configFull("app",
		map[string]string{"text": "hello\n"},
		map[string][]byte{"blob": {0xde, 0xad, 0xbe, 0xef}},
		nil,
		map[string]string{"blob": "0600"},
	))
	if err := s.Materialize(t.Context(), proc("web-0", "app")); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	base := filepath.Join(s.Dir("web-0"), "app")
	if blob, err := os.ReadFile(filepath.Join(base, "blob")); err != nil || !bytes.Equal(blob, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("blob = %v, %v (binary round-trip)", blob, err)
	}
	if info, _ := os.Stat(filepath.Join(base, "blob")); info.Mode().Perm() != 0o600 {
		t.Errorf("blob mode = %o, want 0600 (per-file)", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(base, "text")); info.Mode().Perm() != 0o644 {
		t.Errorf("text mode = %o, want 0644 (default)", info.Mode().Perm())
	}
}
