// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package supervisor

func readProcStartTicks(pid int) (int64, error) {
	return 0, nil
}
