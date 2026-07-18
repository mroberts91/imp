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
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
)

// Tail implements apiserver.LogStreamer.
func (s *Store) Tail(procName string, opts apiserver.LogOptions) (io.ReadCloser, error) {
	path := s.currentPath(procName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, v1alpha1.ErrNotFound
		}
		return nil, err
	}

	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		err := s.stream(ctx, path, opts, pw)
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

func (s *Store) stream(ctx context.Context, path string, opts apiserver.LogOptions, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return v1alpha1.ErrNotFound
		}
		return err
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

func copyRemaining(f *os.File, w io.Writer, timestamps bool) error {
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if err := writeLogLine(w, sc.Text(), timestamps); err != nil {
			return err
		}
	}
	return sc.Err()
}

func writeLogLine(w io.Writer, line string, timestamps bool) error {
	out := line
	if !timestamps {
		out = stripCRIPrefix(line)
	}
	_, err := fmt.Fprintln(w, out)
	return err
}

// stripCRIPrefix removes "<ts> <stream> <F|P> " from a CRI line.
func stripCRIPrefix(line string) string {
	parts := strings.SplitN(line, " ", 4)
	if len(parts) < 4 {
		return line
	}
	if parts[2] != "F" && parts[2] != "P" {
		return line
	}
	if parts[1] != "stdout" && parts[1] != "stderr" {
		return line
	}
	return parts[3]
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
