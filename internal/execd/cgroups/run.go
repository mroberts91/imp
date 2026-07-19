// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// RunOptions optional settings for RunIn.
type RunOptions struct {
	Dir string
	Env []string // if nil, command inherits; if non-nil (including empty), replaces
}

// RunIn starts command, moves it into the cgroup (Add after Start — brief
// race window before join is accepted for M3), and waits for exit. Exit code
// is returned even on non-zero exit; ctx cancellation kills the process.
func (m *Manager) RunIn(ctx context.Context, path string, command []string, opts RunOptions) (exitCode int, err error) {
	if len(command) == 0 {
		return -1, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}
	if err := m.Add(path, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return -1, fmt.Errorf("add to cgroup: %w", err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode(), nil
	}
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return -1, waitErr
}
