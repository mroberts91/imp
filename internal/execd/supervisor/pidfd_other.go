// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package supervisor

import "golang.org/x/sys/unix"

// pidfd is a Linux facility; elsewhere adoption falls back to the legacy
// /proc poll watch (adoption itself is Linux-only in practice — it needs
// cgroups — so these stubs exist to keep the package compiling).

func openVerifiedPidfd(int, int64) (int, bool) { return 0, false }

func (w *worker) startPidfdWatch() { w.startAdoptWatch(w.rt.PID) }

func (w *worker) signalAdopted(unix.Signal) bool { return false }

func (w *worker) closePidfdWatch() {}
