// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"fmt"
	"os"
	"strconv"
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
// or a negative nice all need it: the systemd ordering), then the identity
// drop, then umask, then execve. Returns only on error.
func apply(p *Payload) error {
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
	if p.User != "" || p.Group != "" {
		if err := dropIdentity(p.User, p.Group); err != nil {
			return err
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

// dropIdentity applies groups → gid → uid, in that order (groups need
// privilege, so they go first). The syscall package variants apply to all
// runtime threads (Go ≥1.16 AllThreadsSyscall support).
func dropIdentity(userName, groupName string) error {
	id, err := resolveIdentity(userName, groupName)
	if err != nil {
		return err
	}
	if err := syscall.Setgroups(id.groups); err != nil {
		return fmt.Errorf("setgroups %v: %w", id.groups, err)
	}
	if id.gid >= 0 {
		if err := syscall.Setgid(id.gid); err != nil {
			return fmt.Errorf("setgid %d: %w", id.gid, err)
		}
	}
	if id.uid >= 0 {
		if err := syscall.Setuid(id.uid); err != nil {
			return fmt.Errorf("setuid %d: %w", id.uid, err)
		}
	}
	return nil
}

func rlimValue(v int64) uint64 {
	if v == -1 {
		return unix.RLIM_INFINITY
	}
	return uint64(v)
}
