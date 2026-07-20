// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"bytes"
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
	got, partial, ok := stripCRIPrefix(line)
	if got != "hello world" || partial || !ok {
		t.Fatalf("strip = (%q, partial=%v, ok=%v), want (hello world, false, true)", got, partial, ok)
	}
	p := FormatLine(ts, "stderr", true, "no-newline")
	got, partial, ok = stripCRIPrefix(p)
	if got != "no-newline" || !partial || !ok {
		t.Fatalf("strip P = (%q, partial=%v, ok=%v), want (no-newline, true, true)", got, partial, ok)
	}
}

// pumpWaitFor polls cond until it holds or the deadline passes. The pump
// processes chunks asynchronously, so tests can't assert synchronously.
func pumpWaitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// rawRecords reads current.log and returns its records verbatim (with the CRI
// prefix), so a test can assert the F/P tag.
func rawRecords(t *testing.T, s *Store, proc string) []string {
	t.Helper()
	b, err := os.ReadFile(s.currentPath(proc))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestPumpPartialThenFull is the log-pump bug's regression (docs/.local/05 §6):
// unterminated output must appear as a P record after the idle timer, and a
// later completion emits an F with only the new segment; the read path
// concatenates the two into one logical line.
func TestPumpPartialThenFull(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	s := New(dir, clk)
	out, _, err := s.Open("web-0", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.WriteString(out, "hello"); err != nil { // no trailing newline
		t.Fatal(err)
	}
	// Nothing emitted before the idle timer fires; the pump has armed it.
	pumpWaitFor(t, func() bool { return clk.Waiters() >= 1 })
	if recs := rawRecords(t, s, "web-0"); len(recs) != 0 {
		t.Fatalf("emitted before idle flush: %v", recs)
	}
	clk.Step(idleFlushDelay)
	pumpWaitFor(t, func() bool { return len(rawRecords(t, s, "web-0")) == 1 })
	if recs := rawRecords(t, s, "web-0"); !strings.Contains(recs[0], "stdout P hello") {
		t.Fatalf("record[0] = %q, want a P hello record", recs[0])
	}

	if _, err := io.WriteString(out, "world\n"); err != nil { // completes the line
		t.Fatal(err)
	}
	pumpWaitFor(t, func() bool { return len(rawRecords(t, s, "web-0")) == 2 })
	if recs := rawRecords(t, s, "web-0"); !strings.Contains(recs[1], "stdout F world") {
		t.Fatalf("record[1] = %q, want an F world record (post-P segment only)", recs[1])
	}
	s.CloseCapture("web-0")

	rc, err := s.Tail("web-0", apiserver.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "helloworld\n" {
		t.Fatalf("rendered = %q, want %q (P+F concatenation)", body, "helloworld\n")
	}
}

// TestPumpThresholdFlush pins the memory bound: a long run without a newline
// is flushed in partialThreshold-sized P records rather than held.
func TestPumpThresholdFlush(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	s := New(dir, clk)
	out, _, err := s.Open("web-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(bytes.Repeat([]byte("x"), partialThreshold+100)); err != nil {
		t.Fatal(err)
	}
	pumpWaitFor(t, func() bool { return len(rawRecords(t, s, "web-0")) >= 1 })
	recs := rawRecords(t, s, "web-0")
	content, partial, ok := stripCRIPrefix(recs[0])
	if !ok || !partial || len(content) != partialThreshold {
		t.Fatalf("record[0] = %q (partial=%v ok=%v len=%d), want a P of %d bytes",
			recs[0], partial, ok, len(content), partialThreshold)
	}
}

// TestPumpEOFFlushesFull pins that a process which exits mid-line (no trailing
// newline) still has its output captured — flushed as F at EOF.
func TestPumpEOFFlushesFull(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	s := New(dir, clk)
	out, _, err := s.Open("web-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(out, "banner-no-newline"); err != nil {
		t.Fatal(err)
	}
	s.CloseCapture("web-0") // closes pipes, waits for pumps to drain

	recs := rawRecords(t, s, "web-0")
	if len(recs) != 1 || !strings.Contains(recs[0], "stdout F banner-no-newline") {
		t.Fatalf("records = %v, want one F banner-no-newline record", recs)
	}
}

// TestPumpEOFTerminatesDanglingPartial pins that a line left dangling as a P
// record by an idle flush is completed with a terminating F at EOF, so the
// rendered output ends in a newline (matching the pre-M9 Scanner).
func TestPumpEOFTerminatesDanglingPartial(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))
	s := New(dir, clk)
	out, _, err := s.Open("web-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(out, "hello"); err != nil { // no newline
		t.Fatal(err)
	}
	pumpWaitFor(t, func() bool { return clk.Waiters() >= 1 })
	clk.Step(idleFlushDelay) // idle-flush "hello" as a P record
	pumpWaitFor(t, func() bool { return len(rawRecords(t, s, "web-0")) == 1 })
	s.CloseCapture("web-0") // EOF after the dangling P

	recs := rawRecords(t, s, "web-0")
	if len(recs) != 2 || !strings.Contains(recs[0], "stdout P hello") || !strings.Contains(recs[1], "stdout F ") {
		t.Fatalf("records = %v, want P hello + a terminating F", recs)
	}
	rc, err := s.Tail("web-0", apiserver.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello\n" {
		t.Fatalf("rendered = %q, want %q (dangling P terminated by F)", body, "hello\n")
	}
}

// TestTailEmptyForLiveProc pins the honesty fix: an existing capture dir with
// no output yet is an empty stream, not ErrNotFound.
func TestTailEmptyForLiveProc(t *testing.T) {
	s := New(t.TempDir(), nil)
	out, _, err := s.Open("web-0", nil) // creates the dir, no output written
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	rc, err := s.Tail("web-0", apiserver.LogOptions{})
	if err != nil {
		t.Fatalf("Tail of a live, output-less proc: %v, want empty stream", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("body = %q, want empty", body)
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
