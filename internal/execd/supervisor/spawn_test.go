// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/execd/childsetup"
)

func payloadFromCmd(t *testing.T, env []string) childsetup.Payload {
	t.Helper()
	for _, e := range env {
		if val, ok := strings.CutPrefix(e, childsetup.EnvName+"="); ok {
			var p childsetup.Payload
			if err := json.Unmarshal([]byte(val), &p); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			return p
		}
	}
	t.Fatalf("no %s entry in env %v", childsetup.EnvName, env)
	return childsetup.Payload{}
}

// TestBuildCmdShimPayload pins that every spawn goes through the shim and
// that the spec's hardening fields land in the payload.
func TestBuildCmdShimPayload(t *testing.T) {
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "web-0"},
		Spec: v1alpha1.ProcSpec{
			Command: []string{"/bin/sleep", "300"},
			Rlimits: []v1alpha1.Rlimit{{Resource: "nofile", Soft: new(int64(256))}},
			Nice:    new(int32(5)),
			Umask:   new("0077"),
		},
	}
	cmd, err := buildCmd(p, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	if cmd.Path != shimExe {
		t.Errorf("cmd.Path = %q, want %q (always-shim)", cmd.Path, shimExe)
	}
	pl := payloadFromCmd(t, cmd.Env)
	if pl.Exe != "/bin/sleep" {
		t.Errorf("payload.Exe = %q, want /bin/sleep", pl.Exe)
	}
	if len(pl.Argv) != 2 || pl.Argv[0] != "/bin/sleep" || pl.Argv[1] != "300" {
		t.Errorf("payload.Argv = %v, want [/bin/sleep 300]", pl.Argv)
	}
	if len(pl.Rlimits) != 1 || pl.Rlimits[0].Resource != "nofile" {
		t.Errorf("payload.Rlimits = %v, want the spec's nofile entry", pl.Rlimits)
	}
	if pl.Nice == nil || *pl.Nice != 5 {
		t.Errorf("payload.Nice = %v, want 5", pl.Nice)
	}
	if pl.Umask == nil || *pl.Umask != "0077" {
		t.Errorf("payload.Umask = %v, want 0077", pl.Umask)
	}
}

// TestBuildCmdPathResolution pins the pre-shim exec.Command resolution
// semantics: bare names resolve through PATH parent-side; names with a
// separator are passed through untouched for the kernel to resolve after
// the workingDir chdir.
func TestBuildCmdPathResolution(t *testing.T) {
	bare := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "a"},
		Spec:     v1alpha1.ProcSpec{Command: []string{"sh", "-c", "true"}},
	}
	cmd, err := buildCmd(bare, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	if pl := payloadFromCmd(t, cmd.Env); !strings.HasPrefix(pl.Exe, "/") {
		t.Errorf("bare name not PATH-resolved: %q", pl.Exe)
	}

	rel := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "b"},
		Spec:     v1alpha1.ProcSpec{Command: []string{"./run.sh"}, WorkingDir: "/srv"},
	}
	cmd, err = buildCmd(rel, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("buildCmd: %v", err)
	}
	if pl := payloadFromCmd(t, cmd.Env); pl.Exe != "./run.sh" {
		t.Errorf("relative path rewritten to %q, want ./run.sh (kernel resolves after chdir)", pl.Exe)
	}

	missing := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "c"},
		Spec:     v1alpha1.ProcSpec{Command: []string{"no-such-cmd-imp-test"}},
	}
	if _, err := buildCmd(missing, io.Discard, io.Discard); err == nil {
		t.Error("unresolvable bare name: want error")
	}
}

// TestBuildCmdRejectsUnknownIdentity pins the parent-side identity
// pre-check: a typo'd user fails at start, not as an exit-126 crash loop.
func TestBuildCmdRejectsUnknownIdentity(t *testing.T) {
	p := &v1alpha1.Proc{
		Metadata: v1alpha1.ObjectMeta{Name: "web-0"},
		Spec: v1alpha1.ProcSpec{
			Command: []string{"/bin/true"},
			User:    "no-such-user-imp-test",
		},
	}
	if _, err := buildCmd(p, io.Discard, io.Discard); err == nil {
		t.Error("unknown user: want error from buildCmd")
	}
}
