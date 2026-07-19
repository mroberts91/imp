// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// rlimitResources maps the API's lowercase resource names (validated against
// v1alpha1.IsKnownRlimit) to RLIMIT_* constants. The two tables are pinned
// against each other in apply_linux_test.go.
var rlimitResources = map[string]int{
	"as":         unix.RLIMIT_AS,
	"core":       unix.RLIMIT_CORE,
	"cpu":        unix.RLIMIT_CPU,
	"data":       unix.RLIMIT_DATA,
	"fsize":      unix.RLIMIT_FSIZE,
	"locks":      unix.RLIMIT_LOCKS,
	"memlock":    unix.RLIMIT_MEMLOCK,
	"msgqueue":   unix.RLIMIT_MSGQUEUE,
	"nice":       unix.RLIMIT_NICE,
	"nofile":     unix.RLIMIT_NOFILE,
	"nproc":      unix.RLIMIT_NPROC,
	"rss":        unix.RLIMIT_RSS,
	"rtprio":     unix.RLIMIT_RTPRIO,
	"sigpending": unix.RLIMIT_SIGPENDING,
	"stack":      unix.RLIMIT_STACK,
}

// apply performs the setup in strict order — rlimits, oom_score_adj, and
// nice while still privileged (raising a hard limit, a negative adjustment,
// or a negative nice all need it: the systemd ordering), then privateTmp
// and the bounding drops (both need capabilities the identity drop clears),
// then the capability-aware identity drop, then noNewPrivileges (late, so
// it cannot interfere with the privileged steps), then umask, then execve.
// Returns only on error.
func apply(p *Payload) error {
	// Capability sets, prctl state, and namespaces are per-THREAD; only the
	// syscall-package identity calls apply to all threads. Pin this
	// goroutine so every step below and the final execve happen on the same
	// OS thread — execve carries that thread's credentials and namespaces
	// into the target.
	runtime.LockOSThread()
	for _, rl := range p.Rlimits {
		res, ok := rlimitResources[rl.Resource]
		if !ok {
			return fmt.Errorf("unknown rlimit resource %q", rl.Resource)
		}
		soft, hard := rl.Values()
		lim := unix.Rlimit{Cur: rlimValue(soft), Max: rlimValue(hard)}
		if err := unix.Setrlimit(res, &lim); err != nil {
			return fmt.Errorf("setrlimit %s (soft %d, hard %d): %w", rl.Resource, soft, hard, err)
		}
	}
	if p.OOMScoreAdjust != nil {
		v := strconv.Itoa(int(*p.OOMScoreAdjust))
		if err := os.WriteFile("/proc/self/oom_score_adj", []byte(v), 0); err != nil {
			return fmt.Errorf("writing oom_score_adj %s: %w", v, err)
		}
	}
	if p.Nice != nil {
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, int(*p.Nice)); err != nil {
			return fmt.Errorf("setpriority %d: %w", *p.Nice, err)
		}
	}
	if p.PrivateTmp != nil && *p.PrivateTmp {
		if err := setupPrivateTmp(); err != nil {
			return err
		}
	}
	var id *identity
	if p.User != "" || p.Group != "" {
		resolved, err := resolveIdentity(p.User, p.Group)
		if err != nil {
			return err
		}
		id = resolved
	}
	if err := executeSandbox(sandboxPlan(id, p.Capabilities)); err != nil {
		return err
	}
	if p.NoNewPrivileges != nil && *p.NoNewPrivileges {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("setting no_new_privs: %w", err)
		}
	}
	if p.Umask != nil {
		mask, err := strconv.ParseUint(*p.Umask, 8, 32)
		if err != nil || mask > 0o777 {
			return fmt.Errorf("invalid umask %q", *p.Umask)
		}
		unix.Umask(int(mask))
	}
	return unix.Exec(p.Exe, p.Argv, os.Environ())
}

// executeSandbox performs the plan's steps in order. The identity syscalls
// use the syscall package (all runtime threads, Go ≥1.16 AllThreadsSyscall
// support); the capability and prctl steps act on the locked thread only —
// the one that will execve.
func executeSandbox(steps []step) error {
	lastCap := kernelLastCap()
	for _, s := range steps {
		switch s.op {
		case opCapBsetDrop:
			name := s.arg.(string)
			if capNumbers[name] > lastCap {
				// The running kernel predates this capability: it cannot be
				// in any bounding set, so there is nothing to drop (and the
				// prctl would fail EINVAL).
				continue
			}
			if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capNumbers[name]), 0, 0, 0); err != nil {
				return fmt.Errorf("dropping capability %s from the bounding set: %w", name, err)
			}
		case opKeepcapsOn:
			if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
				return fmt.Errorf("enabling keep-caps: %w", err)
			}
		case opSetgroups:
			groups := s.arg.([]int)
			if err := syscall.Setgroups(groups); err != nil {
				return fmt.Errorf("setgroups %v: %w", groups, err)
			}
		case opSetgid:
			gid := s.arg.(int)
			if err := syscall.Setgid(gid); err != nil {
				return fmt.Errorf("setgid %d: %w", gid, err)
			}
		case opSetuid:
			uid := s.arg.(int)
			if err := syscall.Setuid(uid); err != nil {
				return fmt.Errorf("setuid %d: %w", uid, err)
			}
		case opCapsetInheritable:
			if err := capsetInheritable(s.arg.([]string)); err != nil {
				return err
			}
		case opAmbientRaise:
			name := s.arg.(string)
			if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, uintptr(capNumbers[name]), 0, 0); err != nil {
				return fmt.Errorf("raising ambient capability %s: %w", name, err)
			}
		case opKeepcapsOff:
			if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 0, 0, 0, 0); err != nil {
				return fmt.Errorf("disabling keep-caps: %w", err)
			}
		default:
			return fmt.Errorf("unknown sandbox step %q", s.op)
		}
	}
	return nil
}

// capsetInheritable sets the calling thread's inheritable capability set to
// exactly the named capabilities, passing permitted and effective through
// unchanged. Uses the V3 (64-bit) header: capget/capset read and write two
// CapUserData words through the pointer to the array's first element.
func capsetInheritable(names []string) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capget: %w", err)
	}
	data[0].Inheritable, data[1].Inheritable = 0, 0
	for _, name := range names {
		if n := capNumbers[name]; n < 32 {
			data[0].Inheritable |= 1 << n
		} else {
			data[1].Inheritable |= 1 << (n - 32)
		}
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset inheritable %v: %w", names, err)
	}
	return nil
}

// setupPrivateTmp gives this (locked) thread a private mount namespace with
// a fresh tmpfs over /tmp and /var/tmp; execve carries the namespace into
// the target. Needs CAP_SYS_ADMIN — rootless this fails EPERM into the
// exit-126 path. Already-open fds (log writers, sockets) are unaffected by
// the new namespace. Note the working directory was chdir'ed before the
// shim ran and stays pinned: a workingDir under /tmp keeps referencing the
// host directory it resolved to at spawn, not the private tmpfs.
func setupPrivateTmp() error {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unsharing mount namespace: %w", err)
	}
	// The recursive slave remount is load-bearing: systemd hosts mount /
	// shared by default, and without it the tmpfs mounts below would
	// propagate back to the host.
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("making mount propagation slave: %w", err)
	}
	mounted := map[string]bool{}
	for _, dir := range []string{"/tmp", "/var/tmp"} {
		// /var/tmp may be a symlink (sometimes to /tmp itself): mount the
		// resolved target, tolerating only its absence.
		target, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("resolving %s: %w", dir, err)
		}
		if mounted[target] {
			continue
		}
		if err := unix.Mount("tmpfs", target, "tmpfs", 0, "mode=1777"); err != nil {
			return fmt.Errorf("mounting tmpfs on %s: %w", target, err)
		}
		mounted[target] = true
	}
	return nil
}

// kernelLastCap reads the highest capability number the running kernel
// supports, falling back to the full table when unreadable.
func kernelLastCap() int {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return len(capNumbers) - 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return len(capNumbers) - 1
	}
	return n
}

func rlimValue(v int64) uint64 {
	if v == -1 {
		return unix.RLIM_INFINITY
	}
	return uint64(v)
}
