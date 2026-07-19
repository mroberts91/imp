// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package supervisor

import (
	"golang.org/x/sys/unix"
)

// pidfd support for adopted Procs (D1 polish). Adopted processes are not
// impd's children: nothing pins their pid, so the legacy 200ms /proc poll
// can mistake a recycled pid for the adopted process, and a graceful
// kill(-pid) could signal an innocent process group. A pidfd references
// the process, not the number: exit notification is a poll on the fd and
// signals cannot mis-target. Teardown was already reuse-safe (cgroup.kill
// is cgroup-scoped) — this hardens the watch and the graceful signal.
// Kernels without pidfd_open (< 5.3) fall back to the legacy poll watch.

// openVerifiedPidfd opens a pidfd for pid and re-verifies the process
// start ticks afterwards: if they still match, the fd is pinned to the
// ticks-matched process — a pid recycled between the caller's identity
// check and the open would show different ticks here and be rejected.
func openVerifiedPidfd(pid int, wantTicks int64) (int, bool) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		// ENOSYS (old kernel) or the process is already gone.
		return 0, false
	}
	t, err := readProcStartTicks(pid)
	if err != nil || t != wantTicks {
		unix.Close(fd)
		return 0, false
	}
	return fd, true
}

// startPidfdWatch watches w.adoptPidfd for process exit (POLLIN on a pidfd
// fires when the process becomes a zombie) and reports it on w.exitCh.
// A pipe interrupts the poll for teardown. Falls back to the legacy poll
// watch if the pipe cannot be created.
func (w *worker) startPidfdWatch() {
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		unix.Close(w.adoptPidfd)
		w.adoptPidfd = 0
		w.startAdoptWatch(w.rt.PID)
		return
	}
	w.exitCh = make(chan exitResult, 1)
	stopR := p[0]
	w.adoptPollW = p[1]
	w.adoptPidfdDone = make(chan struct{})

	pidfd, exitCh, done := w.adoptPidfd, w.exitCh, w.adoptPidfdDone
	go func() {
		defer close(done)
		defer unix.Close(stopR)
		for {
			fds := []unix.PollFd{
				{Fd: int32(pidfd), Events: unix.POLLIN},
				{Fd: int32(stopR), Events: unix.POLLIN},
			}
			if _, err := unix.Poll(fds, -1); err != nil {
				if err == unix.EINTR {
					continue
				}
				return
			}
			if fds[1].Revents != 0 {
				// Teardown: the write end was closed.
				return
			}
			if fds[0].Revents != 0 {
				select {
				case exitCh <- exitResult{err: errAdoptedExited}:
				default:
				}
				return
			}
		}
	}()
}

// signalAdopted delivers sig through the pidfd, guaranteeing it reaches
// the adopted process and never a recycled pid. Reports false when no
// pidfd is held (caller falls back to the group kill). Note the pidfd
// signals the process, not its group — descendants are reaped by the
// cgroup-scoped kill that follows in doStop.
func (w *worker) signalAdopted(sig unix.Signal) bool {
	if w.adoptPidfd <= 0 {
		return false
	}
	if err := unix.PidfdSendSignal(w.adoptPidfd, sig, nil, 0); err != nil && err != unix.ESRCH {
		w.log.Warn("pidfd signal failed", "error", err)
	}
	return true
}

// closePidfdWatch interrupts the poll goroutine, waits for it to exit,
// and only then closes the pidfd — closing a polled fd out from under the
// goroutine would race fd-number reuse.
func (w *worker) closePidfdWatch() {
	if w.adoptPollW > 0 {
		unix.Close(w.adoptPollW)
		w.adoptPollW = 0
		<-w.adoptPidfdDone
		w.adoptPidfdDone = nil
	}
	if w.adoptPidfd > 0 {
		unix.Close(w.adoptPidfd)
		w.adoptPidfd = 0
	}
}
