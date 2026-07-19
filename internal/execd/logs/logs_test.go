// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/clock"
)

func TestFormatAndStrip(t *testing.T) {
	ts := time.Date(2026, 7, 18, 12, 0, 0, 123456789, time.UTC)
	line := FormatLine(ts, "stdout", false, "hello world")
	if !strings.Contains(line, "stdout F hello world") {
		t.Fatalf("FormatLine = %q", line)
	}
	got := stripCRIPrefix(line)
	if got != "hello world" {
		t.Fatalf("strip = %q, want hello world", got)
	}
}

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	s := New(dir, clk)

	out, errOut, err := s.Open("web-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		fmt.Fprintf(out, "line-%d\n", i) //nolint:errcheck
	}
	_ = errOut
	s.CloseCapture("web-0")

	rc, err := s.Tail("web-0", apiserver.LogOptions{TailLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 || lines[0] != "line-3" || lines[1] != "line-4" {
		t.Fatalf("Tail body = %q", body)
	}
}

func TestTailMissing(t *testing.T) {
	s := New(t.TempDir(), nil)
	_, err := s.Tail("missing", apiserver.LogOptions{})
	if !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLogRetentionRotation(t *testing.T) {
	s := New(t.TempDir(), nil)
	ret := &v1alpha1.LogRetention{MaxSizeMB: new(int32(1)), MaxBackups: new(int32(2))}
	out, _, err := s.Open("chatty-0", ret)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// ~1.25 MiB of scanner-friendly lines crosses the 1 MiB threshold and
	// forces a rotation.
	line := strings.Repeat("x", 8*1024)
	for range 160 {
		if _, err := out.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	s.CloseCapture("chatty-0")

	dir := filepath.Join(s.root, "chatty-0")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var backups int
	var haveCurrent bool
	for _, e := range entries {
		switch {
		case e.Name() == "current.log":
			haveCurrent = true
		case strings.HasPrefix(e.Name(), "current-") && strings.HasSuffix(e.Name(), ".log"):
			backups++
		}
	}
	if !haveCurrent || backups < 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("rotation did not happen: current=%v backups=%d files=%v", haveCurrent, backups, names)
	}
}

func TestResolveRetention(t *testing.T) {
	size, backups, age := resolveRetention(nil)
	if size != 10 || backups != 3 || age != 0 {
		t.Errorf("nil retention = %d/%d/%d, want 10/3/0", size, backups, age)
	}
	size, backups, age = resolveRetention(&v1alpha1.LogRetention{MaxSizeMB: new(int32(5))})
	if size != 5 || backups != 3 || age != 0 {
		t.Errorf("partial retention = %d/%d/%d, want 5/3/0 (nil fields default)", size, backups, age)
	}
}
