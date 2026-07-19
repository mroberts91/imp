// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
)

// SetupFakeRoot prepares dir as a NewManager-acceptable cgroup v2 root for
// unit tests and rootless acceptance harnesses. Limits/membership are not
// enforced by the kernel — only the filesystem contract is exercised.
func SetupFakeRoot(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, fileControllers), []byte("cpu memory pids\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", fileControllers, err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileSubtreeControl), []byte(""), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", fileSubtreeControl, err)
	}
	return nil
}
