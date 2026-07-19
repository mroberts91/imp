// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package etcl

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireLock takes an exclusive, non-blocking flock on path, creating it
// if absent. The lock is advisory and lives as long as the returned file
// stays open (the kernel releases it on close or process exit, so a
// crashed impd never wedges the data dir). A held lock means another
// process — a second impd pointed at the same --data-dir, whatever its
// socket — already owns this store: ErrLocked.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("etcl: opening lock file %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock file %s)", ErrLocked, path)
		}
		return nil, fmt.Errorf("etcl: locking %s: %w", path, err)
	}
	return f, nil
}
