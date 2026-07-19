// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// TestMain doubles this test binary as the shim (the same re-exec pattern
// production uses via impd's main): integration tests spawn os.Args[0] with
// the payload env set, and MaybeRun takes over in the child.
func TestMain(m *testing.M) {
	MaybeRun()
	os.Exit(m.Run())
}

func TestPayloadEnvEntryRoundTrip(t *testing.T) {
	p := &Payload{
		Exe:   "/bin/sh",
		Argv:  []string{"sh", "-c", "true"},
		User:  "www-data",
		Group: "adm",
		Rlimits: []v1alpha1.Rlimit{
			{Resource: "nofile", Soft: new(int64(256)), Hard: new(int64(1024))},
		},
		Nice:           new(int32(5)),
		OOMScoreAdjust: new(int32(-100)),
		Umask:          new("0077"),
	}
	entry, err := p.EnvEntry()
	if err != nil {
		t.Fatalf("EnvEntry: %v", err)
	}
	val, ok := strings.CutPrefix(entry, EnvName+"=")
	if !ok {
		t.Fatalf("entry %q does not start with %s=", entry, EnvName)
	}
	var got Payload
	if err := json.Unmarshal([]byte(val), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if diff := cmp.Diff(*p, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestCheckIdentity(t *testing.T) {
	if err := CheckIdentity("", ""); err != nil {
		t.Errorf("empty identity: %v", err)
	}
	if err := CheckIdentity("no-such-user-imp-test", ""); err == nil {
		t.Error("unknown user: want error")
	}
	if err := CheckIdentity("", "no-such-group-imp-test"); err == nil {
		t.Error("unknown group: want error")
	}
}
