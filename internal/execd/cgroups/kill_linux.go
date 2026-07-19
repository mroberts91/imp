// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cgroups

import (
	"golang.org/x/sys/unix"
)

func signalKill(pid int) error {
	err := unix.Kill(pid, unix.SIGKILL)
	if err == unix.ESRCH {
		return nil
	}
	return err
}
