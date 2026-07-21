// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// TestRlimitTableMatchesAPI pins the shim's RLIMIT_* mapping against the
// API's validation vocabulary: every name validation accepts must be
// settable, and the shim must not accept names validation rejects.
func TestRlimitTableMatchesAPI(t *testing.T) {
	for _, name := range v1alpha1.KnownRlimitNames() {
		if _, ok := rlimitResources[name]; !ok {
			t.Errorf("API accepts rlimit %q but the shim has no RLIMIT_* mapping", name)
		}
	}
	for name := range rlimitResources {
		if !v1alpha1.IsKnownRlimit(name) {
			t.Errorf("shim maps rlimit %q but API validation rejects it", name)
		}
	}
}

// reexec spawns this test binary as the shim with the given payload and
// returns combined output and exit code — exactly how the supervisor spawns
// Procs in production.
func reexec(t *testing.T, p *Payload) (out string, exitCode int) {
	t.Helper()
	entry, err := p.EnvEntry()
	if err != nil {
		t.Fatalf("EnvEntry: %v", err)
	}
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), entry)
	b, err := cmd.CombinedOutput()
	if err != nil {
		ee, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			t.Fatalf("running shim: %v (output %q)", err, b)
		}
		return string(b), ee.ExitCode()
	}
	return string(b), 0
}

func TestShimAppliesRlimitAndUmask(t *testing.T) {
	out, code := reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"sh", "-c", "ulimit -Sn; ulimit -Hn; umask"},
		Rlimits: []v1alpha1.Rlimit{
			{Resource: "nofile", Soft: new(int64(256)), Hard: new(int64(512))},
		},
		Umask: new("0077"),
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	lines := strings.Fields(out)
	if len(lines) != 3 {
		t.Fatalf("unexpected output %q", out)
	}
	if lines[0] != "256" || lines[1] != "512" {
		t.Errorf("nofile soft/hard = %s/%s, want 256/512", lines[0], lines[1])
	}
	if lines[2] != "0077" {
		t.Errorf("umask = %s, want 0077", lines[2])
	}
}

func TestShimAppliesNiceAndOOMScoreAdjust(t *testing.T) {
	out, code := reexec(t, &Payload{
		Exe:            "/bin/sh",
		Argv:           []string{"sh", "-c", "cat /proc/self/stat; echo; cat /proc/self/oom_score_adj"},
		Nice:           new(int32(5)),
		OOMScoreAdjust: new(int32(100)), // positive: legal without privilege
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("unexpected output %q", out)
	}
	// /proc/self/stat: fields after the parenthesized comm start at field 3;
	// nice is field 19 overall.
	_, rest, ok := strings.Cut(lines[0], ") ")
	if !ok {
		t.Fatalf("unparseable stat line %q", lines[0])
	}
	fields := strings.Fields(rest)
	if len(fields) < 17 {
		t.Fatalf("short stat line %q", lines[0])
	}
	if nice := fields[16]; nice != "5" {
		t.Errorf("nice = %s, want 5", nice)
	}
	if adj := strings.TrimSpace(lines[1]); adj != "100" {
		t.Errorf("oom_score_adj = %s, want 100", adj)
	}
}

func TestShimScrubsPayloadFromEnvironment(t *testing.T) {
	out, code := reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"sh", "-c", fmt.Sprintf("printenv %s || echo scrubbed", EnvName)},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if got := strings.TrimSpace(out); got != "scrubbed" {
		t.Errorf("payload env leaked to the target: %q", got)
	}
}

func TestShimPreservesArgvAndPlainSpawn(t *testing.T) {
	// A payload with no setup fields must behave exactly like a direct
	// exec — the always-shim regression guard. The rename target is this
	// test binary in TestMain's print-argv0 mode, argv[0]-agnostic on
	// every libc (see TestMain for why a shell can't play the part).
	t.Setenv("IMP_TEST_PRINT_ARGV0", "1")
	out, code := reexec(t, &Payload{
		Exe:  "/proc/self/exe",
		Argv: []string{"custom-argv0"},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if got := strings.TrimSpace(out); got != "custom-argv0" {
		t.Errorf("argv[0] = %q, want %q", got, "custom-argv0")
	}
}

func TestShimFailureExitsCode126(t *testing.T) {
	out, code := reexec(t, &Payload{
		Exe:     "/bin/sh",
		Argv:    []string{"sh", "-c", "true"},
		Rlimits: []v1alpha1.Rlimit{{Resource: "bogus", Soft: new(int64(1))}},
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
	if !strings.Contains(out, "imp child-setup:") {
		t.Errorf("stderr %q missing the child-setup marker", out)
	}
}

func TestShimExecFailureExitsCode126(t *testing.T) {
	out, code := reexec(t, &Payload{
		Exe:  "/no/such/binary",
		Argv: []string{"nope"},
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
	if !strings.Contains(out, "imp child-setup:") {
		t.Errorf("stderr %q missing the child-setup marker", out)
	}
}

// TestCapNumberTableMatchesUnix pins the hand-rolled capability numbers in
// plan.go against the x/sys/unix CAP_* constants.
func TestCapNumberTableMatchesUnix(t *testing.T) {
	unixCaps := map[string]int{
		"chown": unix.CAP_CHOWN, "dac_override": unix.CAP_DAC_OVERRIDE,
		"dac_read_search": unix.CAP_DAC_READ_SEARCH, "fowner": unix.CAP_FOWNER,
		"fsetid": unix.CAP_FSETID, "kill": unix.CAP_KILL,
		"setgid": unix.CAP_SETGID, "setuid": unix.CAP_SETUID,
		"setpcap": unix.CAP_SETPCAP, "linux_immutable": unix.CAP_LINUX_IMMUTABLE,
		"net_bind_service": unix.CAP_NET_BIND_SERVICE, "net_broadcast": unix.CAP_NET_BROADCAST,
		"net_admin": unix.CAP_NET_ADMIN, "net_raw": unix.CAP_NET_RAW,
		"ipc_lock": unix.CAP_IPC_LOCK, "ipc_owner": unix.CAP_IPC_OWNER,
		"sys_module": unix.CAP_SYS_MODULE, "sys_rawio": unix.CAP_SYS_RAWIO,
		"sys_chroot": unix.CAP_SYS_CHROOT, "sys_ptrace": unix.CAP_SYS_PTRACE,
		"sys_pacct": unix.CAP_SYS_PACCT, "sys_admin": unix.CAP_SYS_ADMIN,
		"sys_boot": unix.CAP_SYS_BOOT, "sys_nice": unix.CAP_SYS_NICE,
		"sys_resource": unix.CAP_SYS_RESOURCE, "sys_time": unix.CAP_SYS_TIME,
		"sys_tty_config": unix.CAP_SYS_TTY_CONFIG, "mknod": unix.CAP_MKNOD,
		"lease": unix.CAP_LEASE, "audit_write": unix.CAP_AUDIT_WRITE,
		"audit_control": unix.CAP_AUDIT_CONTROL, "setfcap": unix.CAP_SETFCAP,
		"mac_override": unix.CAP_MAC_OVERRIDE, "mac_admin": unix.CAP_MAC_ADMIN,
		"syslog": unix.CAP_SYSLOG, "wake_alarm": unix.CAP_WAKE_ALARM,
		"block_suspend": unix.CAP_BLOCK_SUSPEND, "audit_read": unix.CAP_AUDIT_READ,
		"perfmon": unix.CAP_PERFMON, "bpf": unix.CAP_BPF,
		"checkpoint_restore": unix.CAP_CHECKPOINT_RESTORE,
	}
	if len(unixCaps) != len(capNumbers) {
		t.Errorf("pin covers %d capabilities, table has %d", len(unixCaps), len(capNumbers))
	}
	for name, want := range unixCaps {
		if got, ok := capNumbers[name]; !ok || got != want {
			t.Errorf("capNumbers[%q] = %d (present %t), want %d", name, got, ok, want)
		}
	}
}

// TestShimAppliesNoNewPrivileges pins the rootless-legal M7 knob: the flag
// is observable in the target's /proc/self/status, and absent when not
// requested (the control).
func TestShimAppliesNoNewPrivileges(t *testing.T) {
	out, code := reexec(t, &Payload{
		Exe:             "/bin/sh",
		Argv:            []string{"sh", "-c", "grep NoNewPrivs /proc/self/status"},
		NoNewPrivileges: new(true),
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if !strings.Contains(out, "NoNewPrivs:\t1") {
		t.Errorf("NoNewPrivs not set: %q", out)
	}

	out, code = reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"sh", "-c", "grep NoNewPrivs /proc/self/status"},
	})
	if code != 0 {
		t.Fatalf("control shim exited %d: %s", code, out)
	}
	if !strings.Contains(out, "NoNewPrivs:\t0") {
		t.Errorf("control has NoNewPrivs set: %q", out)
	}
}

// TestShimCapabilitiesRequirePrivilege pins the rootless honesty path:
// a bounding drop without CAP_SETPCAP fails through exit 126 with the
// child-setup marker, never a silent no-op.
func TestShimCapabilitiesRequirePrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the unprivileged-failure path is not observable")
	}
	out, code := reexec(t, &Payload{
		Exe:          "/bin/sh",
		Argv:         []string{"sh", "-c", "true"},
		Capabilities: &v1alpha1.Capabilities{Bounding: []string{"net_bind_service"}},
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
	if !strings.Contains(out, "imp child-setup:") || !strings.Contains(out, "bounding") {
		t.Errorf("stderr %q missing the child-setup bounding-drop error", out)
	}
}

// TestShimAmbientRequiresPrivilege pins the rootless honesty path for the
// ambient raise: a normal user's permitted set cannot back the inheritable
// set, so capset fails through exit 126.
func TestShimAmbientRequiresPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the unprivileged-failure path is not observable")
	}
	out, code := reexec(t, &Payload{
		Exe:          "/bin/sh",
		Argv:         []string{"sh", "-c", "true"},
		Capabilities: &v1alpha1.Capabilities{Ambient: []string{"net_bind_service"}},
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
	if !strings.Contains(out, "imp child-setup:") {
		t.Errorf("stderr %q missing the child-setup marker", out)
	}
}

// rootSandboxGate skips unless the root-gated contributor tests are opted
// in: sudo IMP_ROOT_SANDBOX_TESTS=1 go test -run TestRootSandbox ./internal/execd/childsetup/
func rootSandboxGate(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("IMP_ROOT_SANDBOX_TESTS") != "1" {
		t.Skip("root-gated sandbox test; run as root with IMP_ROOT_SANDBOX_TESTS=1")
	}
}

// TestRootSandboxAmbient proves the full §4.3 transition: identity dropped
// to nobody, exactly net_bind_service's bit in CapAmb and CapEff.
func TestRootSandboxAmbient(t *testing.T) {
	rootSandboxGate(t)
	out, code := reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"sh", "-c", `grep -E "CapAmb|CapEff|Uid" /proc/self/status`},
		User: "nobody",
		Capabilities: &v1alpha1.Capabilities{
			Bounding: []string{"net_bind_service"},
			Ambient:  []string{"net_bind_service"},
		},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	// CAP_NET_BIND_SERVICE is bit 10 → mask 0x400.
	const netBindMask = "0000000000000400"
	if !strings.Contains(out, "CapAmb:\t"+netBindMask) {
		t.Errorf("CapAmb != net_bind_service only: %q", out)
	}
	if !strings.Contains(out, "CapEff:\t"+netBindMask) {
		t.Errorf("CapEff != net_bind_service only: %q", out)
	}
	if strings.Contains(out, "Uid:\t0\t") {
		t.Errorf("uid still root: %q", out)
	}
}

// TestRootSandboxBounding proves the bounding drop for a root target:
// CapBnd reduced to exactly chown's bit.
func TestRootSandboxBounding(t *testing.T) {
	rootSandboxGate(t)
	out, code := reexec(t, &Payload{
		Exe:          "/bin/sh",
		Argv:         []string{"sh", "-c", "grep CapBnd /proc/self/status"},
		Capabilities: &v1alpha1.Capabilities{Bounding: []string{"chown"}},
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	// CAP_CHOWN is bit 0 → mask 0x1.
	if !strings.Contains(out, "CapBnd:\t0000000000000001") {
		t.Errorf("CapBnd != chown only: %q", out)
	}
}

// TestShimPrivateTmpRequiresPrivilege pins the rootless honesty path:
// unshare(CLONE_NEWNS) without CAP_SYS_ADMIN fails through exit 126.
func TestShimPrivateTmpRequiresPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the unprivileged-failure path is not observable")
	}
	out, code := reexec(t, &Payload{
		Exe:        "/bin/sh",
		Argv:       []string{"sh", "-c", "true"},
		PrivateTmp: new(true),
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
	if !strings.Contains(out, "imp child-setup:") || !strings.Contains(out, "mount namespace") {
		t.Errorf("stderr %q missing the child-setup mount-namespace error", out)
	}
}

// TestRootSandboxPrivateTmp proves the private tmpfs: the target sees a
// tmpfs on /tmp, and a file it writes there does not exist on the host.
func TestRootSandboxPrivateTmp(t *testing.T) {
	rootSandboxGate(t)
	const canary = "/tmp/imp-root-sandbox-private-tmp-canary"
	out, code := reexec(t, &Payload{
		Exe:        "/bin/sh",
		Argv:       []string{"sh", "-c", "touch " + canary + " && grep -E '^tmpfs (/tmp|/var/tmp) ' /proc/self/mounts"},
		PrivateTmp: new(true),
	})
	if code != 0 {
		t.Fatalf("shim exited %d: %s", code, out)
	}
	if !strings.Contains(out, "tmpfs /tmp ") {
		t.Errorf("target does not see a tmpfs on /tmp: %q", out)
	}
	if _, err := os.Stat(canary); !errors.Is(err, fs.ErrNotExist) {
		os.Remove(canary)
		t.Errorf("canary leaked to the host /tmp (stat err=%v)", err)
	}
}

func TestShimIdentityDropRequiresPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the unprivileged-failure path is not observable")
	}
	// Rootless, an identity change involving setgroups must fail honestly
	// through the 126 path (the pre-M6 Credential path failed at fork with
	// the same EPERM).
	me, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	out, code := reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"sh", "-c", "true"},
		User: me.Username,
	})
	if code != ExitCode {
		t.Fatalf("exit code = %d, want %d (output %q)", code, ExitCode, out)
	}
}
