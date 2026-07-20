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
	"bufio"
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
	scannerMaxToken = 16 * 1024
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
	mu     sync.Mutex
	wg     sync.WaitGroup
}

// pump copies one stream (stdout/stderr) into the rotated log as CRI lines.
//
// KNOWN BUG (see docs/.local/05-implementation-progress.md §6): the
// bufio.Scanner (default ScanLines) emits a line lacking a trailing '\n' only
// at EOF. While the child keeps this stream open, buffered-but-unterminated
// bytes block inside sc.Scan() and never reach lumberjack — which creates
// current.log lazily on first write — so a long-lived process whose output
// does not end in a newline shows NOTHING in `impctl logs` (and, if that is
// its only output, the file never exists and the log route 404s). Fix
// direction: read bytes in chunks and emit CRI 'F' on newline / 'P' on an
// idle-timer or buffer-threshold flush (kubelet kuberuntime/logs shape), so no
// output is ever withheld pending a newline.
func (pw *procWriter) pump(r *io.PipeReader, stream string) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, scannerMaxToken), scannerMaxToken)
	for sc.Scan() {
		line := sc.Text()
		ts := pw.clock.Now().UTC().Format(time.RFC3339Nano)
		pw.mu.Lock()
		_, _ = fmt.Fprintf(pw.lj, "%s %s F %s\n", ts, stream, line)
		pw.mu.Unlock()
	}
}
