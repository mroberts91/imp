// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"io"
	"net/http"
	"strconv"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// LogStreamer is what the api-server needs from the log subsystem. It is
// implemented by internal/execd/logs. The api-server's /log route is a thin
// HTTP adapter over it.
type LogStreamer interface {
	// Tail returns the Proc's log stream per opts. It reports
	// v1alpha1.ErrNotFound for a Proc with no logs.
	Tail(procName string, opts LogOptions) (io.ReadCloser, error)
}

type LogOptions struct {
	Follow     bool
	TailLines  int
	Timestamps bool
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeJSON(w, http.StatusNotImplemented, &v1alpha1.APIError{
			Reason:  v1alpha1.ErrorReasonInternal,
			Message: "log streaming is not available in this build",
		})
		return
	}
	q := r.URL.Query()
	opts := LogOptions{
		Follow:     q.Get("follow") == "true",
		Timestamps: q.Get("timestamps") == "true",
	}
	if tl := q.Get("tailLines"); tl != "" {
		n, err := strconv.Atoi(tl)
		if err != nil || n < 0 {
			writeError(w, &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
				Type: v1alpha1.ErrorTypeInvalid, Field: "tailLines", BadValue: tl,
				Detail: "must be a non-negative integer",
			}}})
			return
		}
		opts.TailLines = n
	}

	rc, err := s.logs.Tail(r.PathValue("name"), opts)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rc.Close()
	stop := closeOnDisconnect(r, rc)
	defer stop()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flushingCopy(w, rc)
}

func closeOnDisconnect(r *http.Request, rc io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			rc.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// flushingCopy copies rc to w, flushing after every read so follow-mode
// lines arrive as they happen rather than when a buffer fills.
func flushingCopy(w http.ResponseWriter, rc io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
