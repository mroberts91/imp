// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"

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
	// exec — the always-shim regression guard.
	out, code := reexec(t, &Payload{
		Exe:  "/bin/sh",
		Argv: []string{"custom-argv0", "-c", `echo "$0"`},
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
