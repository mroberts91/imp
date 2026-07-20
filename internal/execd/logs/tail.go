// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
)

// Tail implements apiserver.LogStreamer.
//
// A missing proc *directory* means capture was never set up for this name
// (the dir is created at Open, before spawn) → ErrNotFound. A present dir with
// no current.log yet means the proc simply hasn't produced output: non-follow
// returns an empty, immediately-EOF stream; follow waits for the file to
// appear. "no logs yet" is a stream, not an error (docs/.local/05 §6).
func (s *Store) Tail(procName string, opts apiserver.LogOptions) (io.ReadCloser, error) {
	if _, err := os.Stat(s.procDir(procName)); err != nil {
		if os.IsNotExist(err) {
			return nil, v1alpha1.ErrNotFound
		}
		return nil, err
	}

	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		err := s.stream(ctx, procName, opts, pw)
		_ = pw.CloseWithError(err)
	}()
	return &tailCloser{ReadCloser: pr, cancel: cancel}, nil
}

type tailCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (t *tailCloser) Close() error {
	t.cancel()
	return t.ReadCloser.Close()
}

func (s *Store) stream(ctx context.Context, procName string, opts apiserver.LogOptions, w io.Writer) error {
	path := s.currentPath(procName)
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// No capture file yet. Non-follow: empty stream. Follow: wait for it.
		if !opts.Follow {
			return nil
		}
		f, err = s.waitForCreate(ctx, path)
		if err != nil {
			return err
		}
		if f == nil { // ctx cancelled before the file appeared
			return nil
		}
	}
	defer f.Close()

	if opts.TailLines > 0 {
		lines, err := readLastLines(f, opts.TailLines)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if err := writeLogLine(w, line, opts.Timestamps); err != nil {
				return err
			}
		}
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	}

	if !opts.Follow {
		if opts.TailLines > 0 {
			return nil
		}
		return copyRemaining(f, w, opts.Timestamps)
	}

	if opts.TailLines == 0 {
		if err := copyRemaining(f, w, opts.Timestamps); err != nil {
			return err
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	_ = watcher.Add(path)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	buf := bufio.NewReader(f)

	for {
		for {
			line, err := buf.ReadString('\n')
			if len(line) > 0 {
				if err := writeLogLine(w, strings.TrimSuffix(line, "\n"), opts.Timestamps); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				_ = f.Close()
				time.Sleep(20 * time.Millisecond)
				f, err = os.Open(path)
				if err != nil {
					if os.IsNotExist(err) {
						return nil
					}
					return err
				}
				buf = bufio.NewReader(f)
				_ = watcher.Add(path)
			}
		case <-watcher.Errors:
		case <-ticker.C:
		}
	}
}

// waitForCreate blocks until path can be opened, ctx is done, or a real error
// occurs. The 500 ms ticker is the guaranteed fallback; the proc-dir fsnotify
// watch is a best-effort latency win. Returns (nil, nil) on ctx cancellation.
func (s *Store) waitForCreate(ctx context.Context, path string) (*os.File, error) {
	var events chan fsnotify.Event
	if watcher, err := fsnotify.NewWatcher(); err == nil {
		defer watcher.Close()
		if watcher.Add(filepath.Dir(path)) == nil {
			events = watcher.Events
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-ticker.C:
		case <-events: // nil when no watcher: this case never fires, ticker covers it
		}
	}
}

func copyRemaining(f *os.File, w io.Writer, timestamps bool) error {
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if err := writeLogLine(w, sc.Text(), timestamps); err != nil {
			return err
		}
	}
	return sc.Err()
}

// writeLogLine renders one stored CRI record. In timestamps mode the raw
// record is printed verbatim (debug view, one record per line). Otherwise the
// prefix is stripped and the record's tag decides the line ending: an F (full)
// record completes a line (trailing newline re-added), a P (partial) record is
// printed without one so a following P/F record concatenates seamlessly —
// kubelet's P/F rendering (verified against cri-client logs.go).
func writeLogLine(w io.Writer, line string, timestamps bool) error {
	if timestamps {
		_, err := fmt.Fprintln(w, line)
		return err
	}
	content, partial, ok := stripCRIPrefix(line)
	if ok && partial {
		_, err := io.WriteString(w, content)
		return err
	}
	_, err := fmt.Fprintln(w, content)
	return err
}

// stripCRIPrefix removes "<ts> <stream> <F|P> " from a CRI line, reporting
// whether it was a P (partial) record. ok is false for an unrecognizable line,
// which is then printed as-is (with a newline).
func stripCRIPrefix(line string) (content string, partial, ok bool) {
	parts := strings.SplitN(line, " ", 4)
	if len(parts) < 4 {
		return line, false, false
	}
	if parts[1] != "stdout" && parts[1] != "stderr" {
		return line, false, false
	}
	switch parts[2] {
	case "F":
		return parts[3], false, true
	case "P":
		return parts[3], true, true
	default:
		return line, false, false
	}
}

func readLastLines(f *os.File, n int) ([]string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// FormatLine builds one CRI line (test helper).
func FormatLine(ts time.Time, stream string, partial bool, content string) string {
	tag := "F"
	if partial {
		tag = "P"
	}
	return fmt.Sprintf("%s %s %s %s", ts.UTC().Format(time.RFC3339Nano), stream, tag, content)
}
