// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package cgroups

import (
	"fmt"
	"os"
)

func signalKill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
