// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const sysfsCgroup = "/sys/fs/cgroup"

// ResolveRoot picks the cgroup v2 subtree impd should manage.
//
// Order (doc 06 §7.1):
//  1. explicit non-empty path (caller flag) — must look like cgroup2 + writable
//  2. this process's cgroup from /proc/self/cgroup (systemd Delegate= path)
//  3. well-known /sys/fs/cgroup/system.slice/imp.service
//
// Returns a cleaned absolute path suitable for NewManager, or an error that
// tells the operator to set --cgroup-root / Delegate=.
func ResolveRoot(explicit string) (string, error) {
	if explicit != "" {
		root := filepath.Clean(explicit)
		if err := validateRootShape(root); err != nil {
			return "", err
		}
		return root, nil
	}

	if self, err := selfCgroupPath(); err == nil {
		if err := validateRootShape(self); err == nil {
			return self, nil
		}
	}

	fallback := filepath.Join(sysfsCgroup, "system.slice", "imp.service")
	if err := validateRootShape(fallback); err == nil {
		return fallback, nil
	}

	return "", fmt.Errorf("no usable cgroup root: set --cgroup-root to a writable cgroup v2 directory, or run under systemd with Delegate=yes on the imp unit (ad-hoc/rootless: pass a SetupFakeRoot path from bootstrap)")
}

// validateRootShape checks cgroup.controllers exists and subtree_control is writable.
// Does not enable controllers (NewManager does that).
func validateRootShape(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("cgroup root %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cgroup root %q is not a directory", root)
	}
	if _, err := os.Stat(filepath.Join(root, fileControllers)); err != nil {
		return fmt.Errorf("cgroup root %q is not cgroup v2 (missing %s): %w", root, fileControllers, err)
	}
	f, err := os.OpenFile(filepath.Join(root, fileSubtreeControl), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("cgroup root %q is not writable: %w", root, err)
	}
	_ = f.Close()
	return nil
}

// selfCgroupPath maps /proc/self/cgroup (unified hierarchy) to a sysfs path.
func selfCgroupPath() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	rel, err := parseUnifiedCgroupPath(string(data))
	if err != nil {
		return "", err
	}
	if rel == "/" || rel == "" {
		return "", fmt.Errorf("process is in the cgroup root; refuse to use host root as --cgroup-root")
	}
	return filepath.Join(sysfsCgroup, strings.TrimPrefix(rel, "/")), nil
}

// parseUnifiedCgroupPath extracts the path from a cgroup v2 /proc/*/cgroup body.
// Expected line: "0::/system.slice/imp.service"
func parseUnifiedCgroupPath(body string) (string, error) {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// hierarchy-ID:controller-list:cgroup-path
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		// Unified hierarchy: empty controller list.
		if parts[1] != "" {
			continue
		}
		path := parts[2]
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		return path, nil
	}
	return "", fmt.Errorf("no unified cgroup line in /proc/self/cgroup")
}
