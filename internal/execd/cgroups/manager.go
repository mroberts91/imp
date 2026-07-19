// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package cgroups manages per-Proc cgroup v2 groups under a delegated root.
// Hierarchy idea from pkg/kubelet/cm/; mechanics are direct filesystem ops
// against the unified hierarchy (see docs/.local/06-m3-implementation-blueprint.md §3).
package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	dirPrefix = "proc-"

	fileControllers    = "cgroup.controllers"
	fileSubtreeControl = "cgroup.subtree_control"
	fileProcs          = "cgroup.procs"
	fileKill           = "cgroup.kill"
	fileFreeze         = "cgroup.freeze"
	fileMemoryMax      = "memory.max"
	fileMemoryCurrent  = "memory.current"
	fileCPUWeight      = "cpu.weight"
	filePidsMax        = "pids.max"
	fileCPUStat        = "cpu.stat"
)

// Stats is instantaneous usage from a Proc cgroup.
type Stats struct {
	MemoryCurrent uint64
	CPUUsageUsec  uint64
}

// Manager owns a writable cgroup v2 subtree and creates per-Proc children.
type Manager struct {
	root string
}

// NewManager validates root as a writable cgroup v2 directory and best-effort
// enables memory/cpu/pids in cgroup.subtree_control.
func NewManager(root string) (*Manager, error) {
	if root == "" {
		return nil, fmt.Errorf("cgroup root is empty")
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("cgroup root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("cgroup root %q is not a directory", root)
	}
	controllersPath := filepath.Join(root, fileControllers)
	if _, err := os.Stat(controllersPath); err != nil {
		return nil, fmt.Errorf("cgroup root %q is not cgroup v2 (missing %s): %w", root, fileControllers, err)
	}
	// Writability probe: open subtree_control for write (may be empty).
	f, err := os.OpenFile(filepath.Join(root, fileSubtreeControl), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cgroup root %q is not writable: %w", root, err)
	}
	_ = f.Close()

	m := &Manager{root: root}
	if err := m.enableControllers(); err != nil {
		return nil, err
	}
	return m, nil
}

// Root returns the configured cgroup root path.
func (m *Manager) Root() string { return m.root }

// Path returns the absolute path for a Proc UID's cgroup directory.
func (m *Manager) Path(procUID string) string {
	return filepath.Join(m.root, dirPrefix+procUID)
}

// Ensure creates (if needed) the cgroup directory for procUID and returns its path.
func (m *Manager) Ensure(procUID string) (string, error) {
	if procUID == "" {
		return "", fmt.Errorf("proc UID is empty")
	}
	if strings.Contains(procUID, "/") || strings.Contains(procUID, "..") {
		return "", fmt.Errorf("invalid proc UID %q", procUID)
	}
	path := m.Path(procUID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", path, err)
	}
	if err := synthesizeLeaf(path); err != nil {
		return "", err
	}
	return path, nil
}

// Add moves pid into the cgroup by writing to cgroup.procs.
func (m *Manager) Add(path string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return writeProcs(filepath.Join(path, fileProcs), pid)
}

// writeProcs writes a single pid to cgroup.procs without truncating the file
// (kernel accumulates membership; WriteFile truncate is rejected / wrong).
func writeProcs(path string, pid int) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%d\n", pid); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// PIDs lists process IDs currently in the cgroup.
func (m *Manager) PIDs(path string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(path, fileProcs))
	if err != nil {
		return nil, err
	}
	var pids []int
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("parse cgroup.procs %q: %w", line, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// livePIDs returns PIDs that still exist in the OS. Used by Kill's wait loop so
// fake cgroup trees (which do not auto-prune cgroup.procs) still converge.
func (m *Manager) livePIDs(path string) ([]int, error) {
	pids, err := m.PIDs(path)
	if err != nil {
		return nil, err
	}
	var live []int
	for _, pid := range pids {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err == nil {
			live = append(live, pid)
		}
	}
	return live, nil
}

// Stats reads memory.current and cpu.stat usage_usec.
func (m *Manager) Stats(path string) (Stats, error) {
	var st Stats
	mem, err := os.ReadFile(filepath.Join(path, fileMemoryCurrent))
	if err != nil {
		return st, err
	}
	st.MemoryCurrent, err = strconv.ParseUint(strings.TrimSpace(string(mem)), 10, 64)
	if err != nil {
		return st, fmt.Errorf("parse memory.current: %w", err)
	}
	cpuStat, err := os.ReadFile(filepath.Join(path, fileCPUStat))
	if err != nil {
		return st, err
	}
	for line := range strings.SplitSeq(string(cpuStat), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			st.CPUUsageUsec, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return st, fmt.Errorf("parse cpu.stat usage_usec: %w", err)
			}
			return st, nil
		}
	}
	return st, fmt.Errorf("cpu.stat: usage_usec not found")
}

// Remove deletes the cgroup directory. Missing path is a no-op.
// Real cgroup dirs are removed with rmdir; synthesized fake trees fall back
// to RemoveAll when regular leaf files remain.
func (m *Manager) Remove(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if err2 := os.RemoveAll(path); err2 == nil {
		return nil
	}
	pids, _ := m.PIDs(path)
	if len(pids) > 0 {
		return fmt.Errorf("remove %s: still has pids %v: %w", path, pids, err)
	}
	return fmt.Errorf("remove %s: %w", path, err)
}

// ListProcDirs returns Proc UIDs for every proc-<uid> directory under the root.
func (m *Manager) ListProcDirs() ([]string, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var uids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, dirPrefix) {
			continue
		}
		uid := strings.TrimPrefix(name, dirPrefix)
		if uid == "" {
			continue
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

func (m *Manager) enableControllers() error {
	data, err := os.ReadFile(filepath.Join(m.root, fileControllers))
	if err != nil {
		return fmt.Errorf("read %s: %w", fileControllers, err)
	}
	available := strings.Fields(string(data))
	have := map[string]bool{}
	for _, c := range available {
		have[c] = true
	}
	want := []string{"cpu", "memory", "pids"}
	var missing []string
	var enable []string
	for _, c := range want {
		if !have[c] {
			missing = append(missing, c)
			continue
		}
		enable = append(enable, "+"+c)
	}
	if len(missing) == len(want) {
		return fmt.Errorf("cgroup root lacks cpu/memory/pids (have %q)", strings.TrimSpace(string(data)))
	}
	if len(enable) == 0 {
		return nil
	}

	subtreePath := filepath.Join(m.root, fileSubtreeControl)
	cur, _ := os.ReadFile(subtreePath)
	curFields := strings.Fields(string(cur))
	enabled := map[string]bool{}
	for _, c := range curFields {
		enabled[strings.TrimPrefix(c, "+")] = true
	}
	var still []string
	for _, c := range want {
		if have[c] && !enabled[c] {
			still = append(still, "+"+c)
		}
	}
	if len(still) == 0 {
		return nil
	}
	if err := writeFile(subtreePath, strings.Join(still, " ")+"\n"); err != nil {
		// Re-check: some kernels return an error when controllers are already
		// partially enabled; accept if all wanted-available are now enabled.
		cur2, _ := os.ReadFile(subtreePath)
		enabled2 := map[string]bool{}
		for c := range strings.FieldsSeq(string(cur2)) {
			enabled2[strings.TrimPrefix(c, "+")] = true
		}
		for _, c := range want {
			if have[c] && !enabled2[c] {
				return fmt.Errorf("enable %v in %s: %w", still, fileSubtreeControl, err)
			}
		}
	}
	return nil
}

// synthesizeLeaf creates stub controller files when absent so unit tests can
// operate against a tempdir fake. On a real cgroup mkdir already populates them.
func synthesizeLeaf(path string) error {
	defaults := map[string]string{
		fileProcs:         "",
		fileMemoryMax:     "max",
		fileMemoryCurrent: "0",
		fileCPUWeight:     "100",
		filePidsMax:       "max",
		fileCPUStat:       "usage_usec 0\nuser_usec 0\nsystem_usec 0\n",
		fileFreeze:        "0",
	}
	for name, content := range defaults {
		p := filepath.Join(path, name)
		if _, err := os.Stat(p); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return fmt.Errorf("synthesize %s: %w", p, err)
		}
	}
	return nil
}

func writeFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
