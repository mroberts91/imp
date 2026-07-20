// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

// Termination ordering (stopSignal → grace → SIGKILL to the process group)
// is a logical fork of killContainer in
// pkg/kubelet/kuberuntime/kuberuntime_container.go (Copyright The Kubernetes
// Authors, Apache-2.0; see LICENSES/kubernetes/).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
)

// stopProcess delivers stopSignal to the child's process group, waits up to
// grace for exitCh, then SIGKILLs the group. It returns the wait result
// (caller must apply it via onExit) or a zero result if ctx canceled first.
func stopProcess(ctx context.Context, clk clock.Clock, cmd *exec.Cmd, stopSignal string, graceSeconds int64, exitCh <-chan exitResult, log *slog.Logger) exitResult {
	if cmd == nil || cmd.Process == nil {
		return exitResult{}
	}
	pid := cmd.Process.Pid
	sig, err := parseSignal(stopSignal)
	if err != nil {
		log.Warn("invalid stopSignal, falling back to TERM", "error", err)
		sig = unix.SIGTERM
	}

	log.Info("sending stop signal", "pid", pid, "signal", stopSignal)
	if err := killGroup(pid, sig); err != nil {
		log.Warn("stop signal failed", "pid", pid, "error", err)
	}

	grace := time.Duration(max(graceSeconds, 0)) * time.Second
	timer := clk.NewTimer(grace)
	defer timer.Stop()

	select {
	case res := <-exitCh:
		return res
	case <-timer.C():
		log.Info("grace elapsed; sending SIGKILL", "pid", pid)
		_ = killGroup(pid, unix.SIGKILL)
	case <-ctx.Done():
		_ = killGroup(pid, unix.SIGKILL)
	}

	select {
	case res := <-exitCh:
		return res
	case <-ctx.Done():
		return exitResult{}
	}
}

// dissectExit turns a Wait error / ProcessState into ExitInfo.
func dissectExit(err error, state *os.ProcessState, finishedAt time.Time) ExitInfo {
	info := ExitInfo{FinishedAt: finishedAt}
	if state == nil && err != nil {
		info.Nonzero = true
		info.Message = err.Error()
		return info
	}
	if state == nil {
		info.Nonzero = true
		return info
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		code := state.ExitCode()
		info.ExitCode = &code
		info.Nonzero = code != 0
		return info
	}
	if ws.Signaled() {
		info.Signal = signalName(ws.Signal())
		info.Nonzero = true
		return info
	}
	code := ws.ExitStatus()
	info.ExitCode = &code
	info.Nonzero = code != 0
	return info
}

func gracePeriod(p *v1alpha1.Proc) int64 {
	if p.Spec.TerminationGracePeriodSeconds != nil {
		return *p.Spec.TerminationGracePeriodSeconds
	}
	return 30
}

// livenessProbeGrace / startupProbeGrace return the probe's own
// terminationGracePeriodSeconds (M9-l), nil when the probe is absent or sets
// none — in which case the Proc's grace applies to a probe-triggered kill.
func livenessProbeGrace(p *v1alpha1.Proc) *int64 {
	if p.Spec.LivenessProbe != nil {
		return p.Spec.LivenessProbe.TerminationGracePeriodSeconds
	}
	return nil
}

func startupProbeGrace(p *v1alpha1.Proc) *int64 {
	if p.Spec.StartupProbe != nil {
		return p.Spec.StartupProbe.TerminationGracePeriodSeconds
	}
	return nil
}

func stopSignalOf(p *v1alpha1.Proc) string {
	if p.Spec.StopSignal != "" {
		return p.Spec.StopSignal
	}
	return v1alpha1.DefaultStopSignal
}

// exitResult is delivered by the Wait side-goroutine.
type exitResult struct {
	state *os.ProcessState
	err   error
}

func fmtExitMessage(info ExitInfo) string {
	switch {
	case info.Signal != "":
		return fmt.Sprintf("terminated by signal %s", info.Signal)
	case info.ExitCode != nil:
		return fmt.Sprintf("exited with code %d", *info.ExitCode)
	case info.Message != "":
		return info.Message
	default:
		return "exited"
	}
}
