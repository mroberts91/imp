// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func fakeRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := SetupFakeRoot(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNewManagerRequiresCgroupV2(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewManager(dir); err == nil {
		t.Fatal("expected error for non-cgroup root")
	}
	if _, err := NewManager(""); err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestEnsureApplyLimitsStatsListRemove(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}

	path, err := m.Ensure("aabb-ccdd")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(m.Root(), "proc-aabb-ccdd")
	if path != wantPath {
		t.Fatalf("path = %q, want %q", path, wantPath)
	}

	limits := v1alpha1.ResourceLimits{
		Memory:    "256Mi",
		CPUWeight: new(int64(200)),
		Pids:      new(int64(64)),
	}
	if err := m.ApplyLimits(path, limits); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(path, fileMemoryMax), "268435456\n")
	assertFile(t, filepath.Join(path, fileCPUWeight), "200\n")
	assertFile(t, filepath.Join(path, filePidsMax), "64\n")

	if err := os.WriteFile(filepath.Join(path, fileMemoryCurrent), []byte("4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, fileCPUStat), []byte("usage_usec 12345\nuser_usec 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := m.Stats(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.MemoryCurrent != 4096 || st.CPUUsageUsec != 12345 {
		t.Fatalf("stats = %+v", st)
	}

	uids, err := m.ListProcDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 1 || uids[0] != "aabb-ccdd" {
		t.Fatalf("ListProcDirs = %v", uids)
	}

	if err := m.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(path); err != nil {
		t.Fatal(err) // idempotent
	}
	uids, err = m.ListProcDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 0 {
		t.Fatalf("ListProcDirs after remove = %v", uids)
	}
}

func TestAddAndPIDs(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure("u1")
	if err != nil {
		t.Fatal(err)
	}
	// Fake file accumulates only if we seed multi-line content; Add opens
	// O_WRONLY and writes one pid (kernel appends membership; fake overwrites
	// from offset 0). Seed two pids to exercise the reader.
	if err := os.WriteFile(filepath.Join(path, fileProcs), []byte("10\n20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pids, err := m.PIDs(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 2 || pids[0] != 10 || pids[1] != 20 {
		t.Fatalf("PIDs = %v", pids)
	}
	if err := m.Add(path, 99); err != nil {
		t.Fatal(err)
	}
}

func TestKillEmptyViaFreeze(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure("empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(path); err != nil {
		t.Fatal(err)
	}
}

func TestKillViaCgroupKillFile(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure("k")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, fileKill), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, fileKill))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("cgroup.kill = %q, want 1", data)
	}
}

func TestRunIn(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure("run")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := m.RunIn(ctx, path, []string{"true"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	code, err = m.RunIn(ctx, path, []string{"false"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("false exit = %d, want 1", code)
	}
}

func TestEnsureRejectsBadUID(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(""); err == nil {
		t.Fatal("expected error")
	}
	if _, err := m.Ensure("../etc"); err == nil {
		t.Fatal("expected error")
	}
}

func TestApplyLimitsEmptyNoOp(t *testing.T) {
	m, err := NewManager(fakeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure("x")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(path, fileMemoryMax))
	if err := m.ApplyLimits(path, v1alpha1.ResourceLimits{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(path, fileMemoryMax))
	if string(before) != string(after) {
		t.Fatalf("empty limits mutated memory.max: %q -> %q", before, after)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
