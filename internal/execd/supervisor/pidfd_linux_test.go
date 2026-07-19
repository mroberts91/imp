// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package supervisor

import (
	"log/slog"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mroberts91/imp/internal/clock"
)

func startSleeper(t *testing.T, seconds string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", seconds)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleeper: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck // best-effort cleanup
		cmd.Wait()         //nolint:errcheck
	})
	return cmd
}

func TestOpenVerifiedPidfd(t *testing.T) {
	cmd := startSleeper(t, "30")
	pid := cmd.Process.Pid
	ticks, err := readProcStartTicks(pid)
	if err != nil {
		t.Fatalf("readProcStartTicks: %v", err)
	}

	fd, ok := openVerifiedPidfd(pid, ticks)
	if !ok {
		t.Skip("pidfd_open unavailable on this kernel")
	}
	unix.Close(fd)

	// Wrong ticks (a recycled pid's signature) must be rejected.
	if _, ok := openVerifiedPidfd(pid, ticks+1); ok {
		t.Fatal("openVerifiedPidfd accepted mismatched start ticks")
	}
}

func newPidfdTestWorker(t *testing.T, pid int) *worker {
	t.Helper()
	ticks, err := readProcStartTicks(pid)
	if err != nil {
		t.Fatalf("readProcStartTicks: %v", err)
	}
	fd, ok := openVerifiedPidfd(pid, ticks)
	if !ok {
		t.Skip("pidfd_open unavailable on this kernel")
	}
	w := &worker{
		name:  "pidfd-test",
		clock: clock.Real{},
		log:   slog.Default(),
	}
	w.adoptPidfd = fd
	w.rt.PID = pid
	return w
}

func TestPidfdWatchDetectsExit(t *testing.T) {
	cmd := startSleeper(t, "0.2")
	w := newPidfdTestWorker(t, cmd.Process.Pid)
	w.startPidfdWatch()
	defer w.stopAdoptWatch()

	select {
	case res := <-w.exitCh:
		if res.err != errAdoptedExited {
			t.Fatalf("exit result = %v, want errAdoptedExited", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pidfd watch did not report the exit")
	}
	cmd.Wait() //nolint:errcheck
}

func TestPidfdWatchStopIsClean(t *testing.T) {
	cmd := startSleeper(t, "30")
	w := newPidfdTestWorker(t, cmd.Process.Pid)
	w.startPidfdWatch()

	done := make(chan struct{})
	go func() {
		w.stopAdoptWatch()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopAdoptWatch hung on the pidfd watch")
	}
	if w.adoptPidfd != 0 || w.adoptPollW != 0 {
		t.Fatalf("pidfd state not cleared: fd=%d pollW=%d", w.adoptPidfd, w.adoptPollW)
	}
	select {
	case res := <-w.exitCh:
		t.Fatalf("unexpected exit result after stop: %v", res.err)
	default:
	}
}

func TestSignalAdoptedViaPidfd(t *testing.T) {
	cmd := startSleeper(t, "30")
	w := newPidfdTestWorker(t, cmd.Process.Pid)
	defer w.stopAdoptWatch()

	if !w.signalAdopted(unix.SIGTERM) {
		t.Fatal("signalAdopted reported no pidfd despite one being held")
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
		// Signal-terminated processes report ExitCode -1.
		if cmd.ProcessState.ExitCode() != -1 {
			t.Fatalf("sleeper exited with %d, want signal termination", cmd.ProcessState.ExitCode())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM via pidfd never terminated the process")
	}
}
