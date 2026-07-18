// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// parseSignal maps a stopSignal name (with or without SIG prefix) to a
// unix signal. Unknown names return an error; the apiserver already
// validates known names on write, so this is a defensive check.
func parseSignal(name string) (unix.Signal, error) {
	key := strings.TrimPrefix(strings.ToUpper(name), "SIG")
	sig, ok := signalByName[key]
	if !ok {
		return 0, fmt.Errorf("unknown signal %q", name)
	}
	return sig, nil
}

var signalByName = map[string]unix.Signal{
	"ABRT": unix.SIGABRT, "ALRM": unix.SIGALRM, "BUS": unix.SIGBUS,
	"CHLD": unix.SIGCHLD, "CONT": unix.SIGCONT, "FPE": unix.SIGFPE,
	"HUP": unix.SIGHUP, "ILL": unix.SIGILL, "INT": unix.SIGINT,
	"IO": unix.SIGIO, "KILL": unix.SIGKILL, "PIPE": unix.SIGPIPE,
	"PROF": unix.SIGPROF, "QUIT": unix.SIGQUIT, "SEGV": unix.SIGSEGV,
	"STOP": unix.SIGSTOP, "SYS": unix.SIGSYS, "TERM": unix.SIGTERM,
	"TRAP": unix.SIGTRAP, "TSTP": unix.SIGTSTP, "TTIN": unix.SIGTTIN,
	"TTOU": unix.SIGTTOU, "URG": unix.SIGURG, "USR1": unix.SIGUSR1,
	"USR2": unix.SIGUSR2, "VTALRM": unix.SIGVTALRM, "WINCH": unix.SIGWINCH,
	"XCPU": unix.SIGXCPU, "XFSZ": unix.SIGXFSZ,
}

// signalName returns a short name for a WaitStatus terminating signal.
func signalName(sig unix.Signal) string {
	for name, s := range signalByName {
		if s == sig {
			return name
		}
	}
	return fmt.Sprintf("%d", sig)
}

// killGroup sends sig to the process group of pid (negative kill).
func killGroup(pid int, sig unix.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := unix.Kill(-pid, sig)
	if err == unix.ESRCH {
		return nil
	}
	return err
}
