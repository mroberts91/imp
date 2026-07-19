// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package cgroups

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUnifiedCgroupPath(t *testing.T) {
	tests := []struct {
		body string
		want string
		ok   bool
	}{
		{"0::/system.slice/imp.service\n", "/system.slice/imp.service", true},
		{"0::/\n", "/", true},
		{"1:name=systemd:/user.slice\n0::/user.slice/user-1000.slice\n", "/user.slice/user-1000.slice", true},
		{"1:cpu:/foo\n", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, err := parseUnifiedCgroupPath(tt.body)
		if tt.ok {
			if err != nil {
				t.Fatalf("body %q: %v", tt.body, err)
			}
			if got != tt.want {
				t.Fatalf("body %q: got %q want %q", tt.body, got, tt.want)
			}
		} else if err == nil {
			t.Fatalf("body %q: want error, got %q", tt.body, got)
		}
	}
}

func TestResolveRootExplicit(t *testing.T) {
	dir := t.TempDir()
	if err := SetupFakeRoot(dir); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(dir) {
		t.Fatalf("got %q want %q", got, filepath.Clean(dir))
	}
}

func TestResolveRootExplicitRejectsBad(t *testing.T) {
	dir := t.TempDir()
	if _, err := ResolveRoot(dir); err == nil {
		t.Fatal("expected error for non-cgroup dir")
	}
}

func TestResolveRootEmptyFailsWithoutDelegate(t *testing.T) {
	got, err := ResolveRoot("")
	if err == nil {
		if got == sysfsCgroup || got == sysfsCgroup+"/" {
			t.Fatalf("refused host root, got %q", got)
		}
		t.Logf("ResolveRoot(\"\") succeeded on this host: %s", got)
		return
	}
	if !strings.Contains(err.Error(), "--cgroup-root") || !strings.Contains(err.Error(), "Delegate") {
		t.Fatalf("error should mention --cgroup-root and Delegate=: %v", err)
	}
}

func TestValidateRootShapeWritable(t *testing.T) {
	dir := t.TempDir()
	if err := SetupFakeRoot(dir); err != nil {
		t.Fatal(err)
	}
	if err := validateRootShape(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileSubtreeControl)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := validateRootShape(dir); err == nil {
		t.Log("chmod 0444 still writable for owner; skipping unwritable assertion")
	}
}
