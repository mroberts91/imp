// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package logs captures Proc stdout/stderr into rotated files and serves
// tail/follow for the api-server's /log route. Line format and the follow
// loop are a logical fork of pkg/kubelet/kuberuntime/logs/logs.go
// (Copyright The Kubernetes Authors, Apache-2.0; see LICENSES/kubernetes/):
// CRI-simplified "<RFC3339Nano> <stdout|stderr> <F|P> <raw>", follow =
// read-to-EOF then wait for growth, rotation-aware reopen.
package logs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/clock"
)

const (
	// readChunk is the per-Read buffer size for copying a stream.
	readChunk = 4 * 1024
	// partialThreshold forces a P (partial) flush once this many bytes have
	// accumulated without a newline. Bounds pump memory and keeps parity with
	// the old bufio.Scanner token cap.
	partialThreshold = 16 * 1024
	// idleFlushDelay is how long unterminated output may sit before it is
	// flushed as a P record, so `impctl logs` never withholds a line pending a
	// trailing newline (docs/.local/05 §6).
	idleFlushDelay = 150 * time.Millisecond
)

var _ apiserver.LogStreamer = (*Store)(nil)

// Store owns per-Proc log directories under root.
type Store struct {
	root  string
	clock clock.Clock

	mu      sync.Mutex
	writers map[string]*procWriter
}

// New returns a Store rooted at root (e.g. {dataDir}/logs).
func New(root string, clk clock.Clock) *Store {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Store{
		root:    root,
		clock:   clk,
		writers: map[string]*procWriter{},
	}
}

func (s *Store) procDir(name string) string {
	return filepath.Join(s.root, name)
}

func (s *Store) currentPath(name string) string {
	return filepath.Join(s.procDir(name), "current.log")
}

// Open prepares rotated capture for procName and returns stdout/stderr
// writers that prefix CRI lines into the shared file. Call CloseCapture
// when the process is reaped. A nil retention (or nil fields) means the
// built-in defaults — resolution happens here, never in the API (doc 08
// M5-d keeps pre-M5 template hashes stable).
func (s *Store) Open(procName string, retention *v1alpha1.LogRetention) (stdout, stderr io.WriteCloser, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.writers[procName]; ok {
		return nil, nil, fmt.Errorf("logs already open for %s", procName)
	}
	if err := os.MkdirAll(s.procDir(procName), 0o750); err != nil {
		return nil, nil, err
	}
	size, backups, age := resolveRetention(retention)
	lj := &lumberjack.Logger{
		Filename:   s.currentPath(procName),
		MaxSize:    size,
		MaxBackups: backups,
		MaxAge:     age,
		LocalTime:  true,
	}
	pw := &procWriter{lj: lj, clock: s.clock}
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	pw.stdout = stdoutW
	pw.stderr = stderrW
	pw.wg.Go(func() { pw.pump(stdoutR, "stdout") })
	pw.wg.Go(func() { pw.pump(stderrR, "stderr") })
	s.writers[procName] = pw
	return stdoutW, stderrW, nil
}

// resolveRetention maps a Proc's logRetention (possibly nil, possibly with
// nil fields) onto concrete lumberjack values.
func resolveRetention(r *v1alpha1.LogRetention) (sizeMB, backups, ageDays int) {
	sizeMB = int(v1alpha1.DefaultLogMaxSizeMB)
	backups = int(v1alpha1.DefaultLogMaxBackups)
	ageDays = int(v1alpha1.DefaultLogMaxAgeDays)
	if r == nil {
		return sizeMB, backups, ageDays
	}
	if r.MaxSizeMB != nil {
		sizeMB = int(*r.MaxSizeMB)
	}
	if r.MaxBackups != nil {
		backups = int(*r.MaxBackups)
	}
	if r.MaxAgeDays != nil {
		ageDays = int(*r.MaxAgeDays)
	}
	return sizeMB, backups, ageDays
}

// CloseCapture finishes capture for procName.
func (s *Store) CloseCapture(procName string) {
	s.mu.Lock()
	pw, ok := s.writers[procName]
	delete(s.writers, procName)
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = pw.stdout.Close()
	_ = pw.stderr.Close()
	pw.wg.Wait()
	_ = pw.lj.Close()
}

// Remove deletes the Proc's log directory after teardown.
func (s *Store) Remove(procName string) error {
	s.CloseCapture(procName)
	err := os.RemoveAll(s.procDir(procName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

type procWriter struct {
	lj     *lumberjack.Logger
	clock  clock.Clock
	stdout *io.PipeWriter
	stderr *io.PipeWriter
	mu     sync.Mutex // guards lj: the two streams share one rotated file
	wg     sync.WaitGroup
}

// writeRecord appends one CRI line: "<ts> <stream> <F|P> <content>\n". content
// never contains a newline (F splits on it, P is only ever a run without one),
// so the trailing '\n' is an unambiguous record delimiter. Holds mu because
// stdout and stderr write to the same lumberjack file.
func (pw *procWriter) writeRecord(stream, tag string, content []byte) {
	ts := pw.clock.Now().UTC().Format(time.RFC3339Nano)
	pw.mu.Lock()
	_, _ = fmt.Fprintf(pw.lj, "%s %s %s %s\n", ts, stream, tag, content)
	pw.mu.Unlock()
}

// pump copies one stream (stdout/stderr) into the rotated log as CRI lines,
// never withholding output pending a trailing newline. Shape is a logical
// fork of pkg/kubelet's log writer: emit an F (full) record per complete line;
// a run without a newline is flushed as a P (partial) record on a 150 ms idle
// timer or once partialThreshold bytes accumulate; EOF flushes the remainder
// as F. The read path (tail.go) concatenates by re-adding a newline for F only.
//
// A blocking Read cannot be select'd against the idle timer, so an inner
// goroutine feeds chunks over a channel and this goroutine owns pending +
// timer in one select loop. It returns only after the reader closes the
// channel (EOF), so CloseCapture's wg.Wait() sees no writes after it returns.
func (pw *procWriter) pump(r *io.PipeReader, stream string) {
	defer r.Close()

	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		buf := make([]byte, readChunk)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	var pending []byte
	// unterminated is true when the last record emitted for this stream was a P
	// (an incomplete line left dangling by an idle/threshold flush). At EOF such
	// a line is completed with a terminating F so the rendered output ends in a
	// newline, matching the pre-M9 Scanner.
	unterminated := false
	var idle clock.Timer
	var idleC <-chan time.Time
	arm := func() { // (re)start the idle countdown for the current pending run
		if idle != nil {
			idle.Stop()
		}
		idle = pw.clock.NewTimer(idleFlushDelay)
		idleC = idle.C()
	}
	disarm := func() {
		if idle != nil {
			idle.Stop()
			idle, idleC = nil, nil
		}
	}

	for {
		select {
		case b, ok := <-chunks:
			if !ok { // EOF
				switch {
				case len(pending) > 0:
					pw.writeRecord(stream, "F", pending) // completes the line
				case unterminated:
					pw.writeRecord(stream, "F", nil) // terminate a dangling P line
				}
				disarm()
				return
			}
			pending = append(pending, b...)
			for { // every complete line becomes an F record
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				pw.writeRecord(stream, "F", pending[:i])
				pending = pending[i+1:]
				unterminated = false
			}
			for len(pending) >= partialThreshold { // bound memory: force P flushes
				pw.writeRecord(stream, "P", pending[:partialThreshold])
				pending = pending[partialThreshold:]
				unterminated = true
			}
			if len(pending) > 0 {
				arm()
			} else {
				disarm()
			}
		case <-idleC:
			if len(pending) > 0 {
				pw.writeRecord(stream, "P", pending)
				pending = nil
				unterminated = true
			}
			disarm()
		}
	}
}
