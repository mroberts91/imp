// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func TestParseMountInfo(t *testing.T) {
	// Real proc_pid_mountinfo(5) shapes: optional fields (shared:1,
	// master:2) of varying count before the " - " separator, per-mount
	// flags in field 6, an octal-escaped mount point.
	const sample = `36 25 0:31 / / rw,relatime shared:1 - ext4 /dev/root rw,errors=continue
37 36 0:32 / /proc rw,nosuid,nodev,noexec,relatime shared:2 - proc proc rw
38 36 8:1 / /var ro,nosuid,noatime shared:3 master:2 - ext4 /dev/sda1 rw
39 36 0:33 / /mnt/with\040space rw,relatime - tmpfs tmpfs rw
`
	entries, err := parseMountInfo(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseMountInfo: %v", err)
	}
	want := []mountEntry{
		{point: "/", flags: unix.MS_RELATIME},
		{point: "/proc", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_RELATIME},
		{point: "/var", flags: unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NOATIME},
		{point: "/mnt/with space", flags: unix.MS_RELATIME},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries %v, want %d", len(entries), entries, len(want))
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestParseMountInfoRejectsShortLine(t *testing.T) {
	if _, err := parseMountInfo(strings.NewReader("36 25 0:31 / /\n")); err == nil {
		t.Error("short line accepted")
	}
}

func TestParseMountInfoSelf(t *testing.T) {
	// The live file must parse, and the root mount must be in it.
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	entries, err := parseMountInfo(f)
	if err != nil {
		t.Fatalf("parseMountInfo(/proc/self/mountinfo): %v", err)
	}
	for _, e := range entries {
		if e.point == "/" {
			return
		}
	}
	t.Errorf("root mount not found in %d entries", len(entries))
}

func TestUnescapeMountPath(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "/plain", want: "/plain"},
		{in: `/with\040space`, want: "/with space"},
		{in: `/tab\011and\012newline`, want: "/tab\tand\nnewline"},
		{in: `/back\134slash`, want: `/back\slash`},
		{in: `/truncated\04`, wantErr: true},
		{in: `/bad\049`, wantErr: true},
	}
	for _, tc := range cases {
		got, err := unescapeMountPath(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("unescapeMountPath(%q) accepted, want error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("unescapeMountPath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestSkipReadOnly(t *testing.T) {
	keep := []string{"/var/lib/app", "/tmp"}
	cases := []struct {
		point string
		want  bool
	}{
		{"/", false},
		{"/usr", false},
		{"/var", false},
		{"/dev", true},
		{"/dev/pts", true},
		{"/proc", true},
		{"/sys/fs/cgroup", true},
		{"/run", true},
		{"/run/user/1000", true},
		{"/runx", false}, // prefix boundary: /runx is not under /run
		{"/var/lib/app", true},
		{"/var/lib/app/sub", true},
		{"/var/lib/apple", false}, // prefix boundary on keep entries too
		{"/tmp", true},
	}
	for _, tc := range cases {
		if got := skipReadOnly(tc.point, keep); got != tc.want {
			t.Errorf("skipReadOnly(%q) = %v, want %v", tc.point, got, tc.want)
		}
	}
}

// TestShimFilesystemRequiresPrivilege pins the rootless honesty path
// (M10-b): unshare(CLONE_NEWNS) without CAP_SYS_ADMIN fails through exit
// 126 — which the NotifierController then pages about, rather than the
// process silently running unconfined.
func TestShimFilesystemRequiresPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the unprivileged-failure path is not observable")
	}
	out, code := reexec(t, &Payload{
		Exe:        "/bin/sh",
		Argv:       []string{"sh", "-c", "true"},
		Filesystem: &v1alpha1.FilesystemPolicy{ReadOnlyRoot: new(true)},
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
	if !strings.Contains(out, "imp child-setup:") || !strings.Contains(out, "mount namespace") {
		t.Errorf("stderr %q missing the child-setup mount-namespace error", out)
	}
}

// TestRootSandboxFilesystem proves the M10-b strict sweep end to end: the
// read-write carve-out accepts writes (and they land on the host — the bind
// targets the real directory), the root tree refuses them, /var/tmp outside
// the carve-out refuses them (the mountinfo walk, not a bare / remount),
// and the root mount reports ro in the target's own view.
func TestRootSandboxFilesystem(t *testing.T) {
	rootSandboxGate(t)
	carve, err := os.MkdirTemp("/var/tmp", "imp-root-fs-carve-")
	if err != nil {
		t.Fatalf("carve dir: %v", err)
	}
	defer os.RemoveAll(carve)

	script := strings.Join([]string{
		"touch " + carve + "/ok",
		"! touch /usr/imp-fs-canary 2>/dev/null",
		"! touch /var/tmp/imp-fs-canary 2>/dev/null",
		`awk '$2=="/"{print "rootflags="$4}' /proc/self/mounts`,
	}, " && ")
	out, code := reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"sh", "-c", script},
		Filesystem: &v1alpha1.FilesystemPolicy{
			ReadOnlyRoot:   new(true),
			ReadWritePaths: []string{carve},
		},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if !strings.Contains(out, "rootflags=ro") {
		t.Errorf("root mount not read-only in target view: %q", out)
	}
	if _, err := os.Stat(filepath.Join(carve, "ok")); err != nil {
		t.Errorf("carve-out write did not land on the host: %v", err)
	}
}

// TestRootSandboxProtectHome proves the ProtectHome flavor standalone (no
// readOnlyRoot): an unprivileged target cannot even list /root, and writes
// are refused by the read-only tmpfs.
func TestRootSandboxProtectHome(t *testing.T) {
	rootSandboxGate(t)
	script := "! ls /root 2>/dev/null && ! touch /home/imp-canary 2>/dev/null && echo HIDDEN"
	out, code := reexec(t, &Payload{
		Exe:        "/bin/sh",
		Argv:       []string{"sh", "-c", script},
		User:       "nobody",
		Filesystem: &v1alpha1.FilesystemPolicy{ProtectHome: new(true)},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if !strings.Contains(out, "HIDDEN") {
		t.Errorf("protectHome assertions failed: %q", out)
	}
}

// TestRootSandboxFilesystemWithPrivateTmp proves the two mount features
// share one namespace: private /tmp stays writable through the read-only
// sweep (it is in the keep list), the rest of the tree does not.
func TestRootSandboxFilesystemWithPrivateTmp(t *testing.T) {
	rootSandboxGate(t)
	script := "touch /tmp/ok && ! touch /usr/imp-fs-canary 2>/dev/null && echo COMBINED"
	out, code := reexec(t, &Payload{
		Exe:        "/bin/sh",
		Argv:       []string{"sh", "-c", script},
		PrivateTmp: new(true),
		Filesystem: &v1alpha1.FilesystemPolicy{ReadOnlyRoot: new(true)},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if !strings.Contains(out, "COMBINED") {
		t.Errorf("combined privateTmp+filesystem assertions failed: %q", out)
	}
	if _, err := os.Stat("/tmp/ok"); err == nil {
		t.Error("/tmp/ok visible on the host — private tmpfs leaked")
	}
}
