// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// The mount-level sandbox (privateTmp, M7; filesystem, M10-b). Everything
// here needs CAP_SYS_ADMIN — rootless the first unshare fails EPERM into
// the exit-126 path. Already-open fds (log writers, sockets) are unaffected
// by the new namespace. Note the working directory was chdir'ed before the
// shim ran and stays pinned: a workingDir under a masked path keeps
// referencing the host directory it resolved to at spawn.

// setupMounts gives this (locked) thread a private mount namespace and
// applies the mount sandbox in M10-b2 order: privateTmp tmpfs, protectHome
// tmpfs, read-write carve-out binds, then the read-only sweep — the sweep
// last, so every earlier mount is visible to it as an exception. execve
// carries the namespace into the target.
func setupMounts(privateTmp bool, policy *v1alpha1.FilesystemPolicy) error {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unsharing mount namespace: %w", err)
	}
	// The recursive slave remount is load-bearing: systemd hosts mount /
	// shared by default, and without it every mount below would propagate
	// back to the host.
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("making mount propagation slave: %w", err)
	}
	// keep collects mount points the read-only sweep must leave writable.
	var keep []string
	if privateTmp {
		targets, err := mountPrivateTmp()
		if err != nil {
			return err
		}
		keep = append(keep, targets...)
	}
	if policy == nil {
		return nil
	}
	if policy.ProtectHome != nil && *policy.ProtectHome {
		targets, err := protectHomes()
		if err != nil {
			return err
		}
		keep = append(keep, targets...)
	}
	if policy.ReadOnlyRoot != nil && *policy.ReadOnlyRoot {
		for _, path := range policy.ReadWritePaths {
			// Resolve symlinks so the carve-out and the sweep's mountinfo
			// view (which shows resolved paths) agree on the exception.
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolving readWritePath %s: %w", path, err)
			}
			// A self-bind is a distinct mount: it stays read-write when the
			// mount under it goes read-only.
			if err := unix.Mount(target, target, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("bind-mounting readWritePath %s: %w", target, err)
			}
			keep = append(keep, target)
		}
		if err := remountAllReadOnly(keep); err != nil {
			return err
		}
	}
	return nil
}

// mountPrivateTmp mounts a fresh tmpfs over /tmp and /var/tmp, returning
// the resolved targets. /var/tmp may be a symlink (sometimes to /tmp
// itself): mount the resolved target, tolerating only its absence.
func mountPrivateTmp() ([]string, error) {
	var targets []string
	for _, dir := range []string{"/tmp", "/var/tmp"} {
		target, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolving %s: %w", dir, err)
		}
		if slices.Contains(targets, target) {
			continue
		}
		if err := unix.Mount("tmpfs", target, "tmpfs", 0, "mode=1777"); err != nil {
			return nil, fmt.Errorf("mounting tmpfs on %s: %w", target, err)
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// protectHomes hides /home, /root, and /run/user behind empty mode-000
// read-only tmpfs mounts (the systemd ProtectHome=true "inaccessible"
// flavor: a non-root process cannot even list them; nobody can write).
// Absent directories are tolerated.
func protectHomes() ([]string, error) {
	var targets []string
	for _, dir := range []string{"/home", "/root", "/run/user"} {
		target, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolving %s: %w", dir, err)
		}
		if slices.Contains(targets, target) {
			continue
		}
		if err := unix.Mount("tmpfs", target, "tmpfs", 0, "mode=000"); err != nil {
			return nil, fmt.Errorf("mounting tmpfs on %s: %w", target, err)
		}
		if err := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
			return nil, fmt.Errorf("remounting %s read-only: %w", target, err)
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// readOnlySkipPrefixes are subtrees the sweep never touches: the API
// filesystems, and /run — sockets and pid files are runtime chatter a
// sandboxed service legitimately writes (systemd ProtectSystem=strict
// leaves /dev, /proc, /sys alone and pairs with the /run-preserving
// RuntimeDirectory= convention).
var readOnlySkipPrefixes = []string{"/dev", "/proc", "/sys", "/run"}

// remountAllReadOnly walks /proc/self/mountinfo and remounts every mount
// read-only except the skip prefixes and the keep list — the whole tree,
// not just the root mount, so a separate-filesystem /var goes read-only
// too (M10-b2: a bare / remount would silently leave it writable, a
// half-measure in a security knob).
func remountAllReadOnly(keep []string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("reading mountinfo: %w", err)
	}
	defer f.Close()
	entries, err := parseMountInfo(f)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipReadOnly(e.point, keep) {
			continue
		}
		// MS_REMOUNT|MS_BIND changes only this mount's per-mount flags —
		// and clears any flag not re-specified, so e.flags carries the
		// existing nosuid/nodev/noexec/atime flags through (mount(2)).
		if err := unix.Mount("", e.point, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|e.flags, ""); err != nil {
			// A mount unmounted concurrently (slave propagation from the
			// host) is gone: nothing left to protect there.
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("remounting %s read-only: %w", e.point, err)
		}
	}
	return nil
}

// skipReadOnly reports whether the sweep leaves point untouched: it is at
// or below a skip prefix or a keep entry.
func skipReadOnly(point string, keep []string) bool {
	for _, base := range readOnlySkipPrefixes {
		if underPath(point, base) {
			return true
		}
	}
	for _, base := range keep {
		if underPath(point, base) {
			return true
		}
	}
	return false
}

// underPath reports whether point is base itself or below it ("/runx" is
// not under "/run").
func underPath(point, base string) bool {
	return point == base || strings.HasPrefix(point, base+"/")
}

// mountEntry is one /proc/self/mountinfo row's relevant columns.
type mountEntry struct {
	point string  // field 5: mount point, octal escapes decoded
	flags uintptr // per-mount MS_* flags parsed from field 6
}

// mountFlagNames maps mountinfo per-mount option words to the MS_* flags a
// read-only bind remount must repeat. Unknown words (rw, superblock noise)
// map to 0 and are ignored.
var mountFlagNames = map[string]uintptr{
	"ro":          unix.MS_RDONLY,
	"nosuid":      unix.MS_NOSUID,
	"nodev":       unix.MS_NODEV,
	"noexec":      unix.MS_NOEXEC,
	"noatime":     unix.MS_NOATIME,
	"nodiratime":  unix.MS_NODIRATIME,
	"relatime":    unix.MS_RELATIME,
	"strictatime": unix.MS_STRICTATIME,
	"nosymfollow": unix.MS_NOSYMFOLLOW,
}

// parseMountInfo reads proc_pid_mountinfo(5) rows: space-separated fields,
// octal-escaped paths, optional fields between field 7 and the " - "
// separator (which this parser never needs to reach — mount point and
// per-mount options sit at fixed indices 4 and 5).
func parseMountInfo(r io.Reader) ([]mountEntry, error) {
	var entries []mountEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 6 {
			return nil, fmt.Errorf("mountinfo line %q: too few fields", line)
		}
		point, err := unescapeMountPath(fields[4])
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %q: %w", line, err)
		}
		var flags uintptr
		for opt := range strings.SplitSeq(fields[5], ",") {
			flags |= mountFlagNames[opt]
		}
		entries = append(entries, mountEntry{point: point, flags: flags})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading mountinfo: %w", err)
	}
	return entries, nil
}

// unescapeMountPath decodes proc(5)'s octal escapes (\040 space, \011 tab,
// \012 newline, \134 backslash).
func unescapeMountPath(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+3 >= len(s) {
			return "", fmt.Errorf("truncated octal escape in %q", s)
		}
		v := 0
		for j := 1; j <= 3; j++ {
			d := s[i+j]
			if d < '0' || d > '7' {
				return "", fmt.Errorf("bad octal escape in %q", s)
			}
			v = v*8 + int(d-'0')
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), nil
}
