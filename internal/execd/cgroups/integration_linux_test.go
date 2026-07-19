// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cgroups

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// TestIntegrationRealCgroup exercises a real delegated subtree when
// IMP_CGROUP_ROOT points at a writable cgroup v2 directory. Skipped otherwise.
func TestIntegrationRealCgroup(t *testing.T) {
	root := os.Getenv("IMP_CGROUP_ROOT")
	if root == "" {
		t.Skip("set IMP_CGROUP_ROOT to a writable cgroup v2 subtree to run")
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager(%q): %v", root, err)
	}
	uid := "imp-cgroups-test"
	path, err := m.Ensure(uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.Kill(path)
		_ = m.Remove(path)
	})

	if err := m.ApplyLimits(path, v1alpha1.ResourceLimits{
		Memory:    "64Mi",
		CPUWeight: new(int64(50)),
		Pids:      new(int64(32)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stats(path); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	if err := m.Add(path, self); err != nil {
		t.Logf("Add(self) skipped (often restricted): %v", err)
		return
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(m.Root(), fileProcs), []byte(strconv.Itoa(self)+"\n"), 0o644)
	})
	pids, err := m.PIDs(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, self) {
		t.Fatalf("self pid %d not in cgroup: %v", self, pids)
	}
}
