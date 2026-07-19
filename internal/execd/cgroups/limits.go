// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// ApplyLimits writes configured ResourceLimits into the cgroup. Unset fields
// are left untouched (kernel / parent defaults).
func (m *Manager) ApplyLimits(path string, limits v1alpha1.ResourceLimits) error {
	if limits.Empty() {
		return nil
	}
	if limits.Memory != "" {
		bytes, err := v1alpha1.ParseMemoryBytes(limits.Memory)
		if err != nil {
			return fmt.Errorf("memory: %w", err)
		}
		if err := writeFile(filepath.Join(path, fileMemoryMax), strconv.FormatUint(bytes, 10)+"\n"); err != nil {
			return err
		}
	}
	if limits.CPUWeight != nil {
		w := *limits.CPUWeight
		if w < 1 || w > 10000 {
			return fmt.Errorf("cpuWeight %d out of range [1,10000]", w)
		}
		if err := writeFile(filepath.Join(path, fileCPUWeight), strconv.FormatInt(w, 10)+"\n"); err != nil {
			return err
		}
	}
	if limits.Pids != nil {
		p := *limits.Pids
		if p < 1 {
			return fmt.Errorf("pids %d must be >= 1", p)
		}
		if err := writeFile(filepath.Join(path, filePidsMax), strconv.FormatInt(p, 10)+"\n"); err != nil {
			return err
		}
	}
	return nil
}
