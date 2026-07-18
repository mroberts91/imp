// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"errors"
	"fmt"
	"io"
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

	out, errOut, err := s.Open("web-0")
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
