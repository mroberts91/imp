// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Kill terminates every process in the cgroup. Prefers cgroup.kill=1 when the
// file exists; otherwise freezes the group, SIGKILLs each pid, and unfreezes.
func (m *Manager) Kill(path string) error {
	killPath := filepath.Join(path, fileKill)
	if _, err := os.Stat(killPath); err == nil {
		if err := writeFile(killPath, "1\n"); err != nil {
			return err
		}
		return waitEmpty(m, path, 2*time.Second)
	}

	freezePath := filepath.Join(path, fileFreeze)
	_ = writeFile(freezePath, "1\n")
	defer func() { _ = writeFile(freezePath, "0\n") }()

	pids, err := m.PIDs(path)
	if err != nil {
		return err
	}
	var first error
	for _, pid := range pids {
		if err := signalKill(pid); err != nil && first == nil {
			first = fmt.Errorf("kill pid %d: %w", pid, err)
		}
	}
	if first != nil {
		return first
	}
	return waitEmpty(m, path, 2*time.Second)
}

func waitEmpty(m *Manager, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pids, err := m.livePIDs(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup %s still has pids %v after kill", path, pids)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
