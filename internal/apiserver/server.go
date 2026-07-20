// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package apiserver is the state authority in front of etcl: the single
// path through which every read and write of every object flows. It owns
// decoding, defaulting, validation, the simplified apply semantics,
// optimistic concurrency, the status sub-resource that makes the
// spec/status writer split mechanical, and the ndjson watch stream.
package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/etcl"
)

var kindByPlural = map[string]string{
	"daemons":   v1alpha1.KindDaemon,
	"procs":     v1alpha1.KindProc,
	"events":    v1alpha1.KindEvent,
	"timers":    v1alpha1.KindTimer,
	"configs":   v1alpha1.KindConfig,
	"notifiers": v1alpha1.KindNotifier,
}

type Config struct {
	Store   *etcl.Store
	Logs    LogStreamer
	Stats   StatsProvider
	Version v1alpha1.VersionInfo
}

type Server struct {
	store   *etcl.Store
	logs    LogStreamer
	stats   StatsProvider
	version v1alpha1.VersionInfo
	mux     *http.ServeMux
}

func New(cfg Config) *Server {
	s := &Server{store: cfg.Store, logs: cfg.Logs, stats: cfg.Stats, version: cfg.Version}
	if s.version.GoVersion == "" {
		s.version.GoVersion = runtime.Version()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /apis/impd.sh/v1alpha1/{kinds}", s.handleList)
	mux.HandleFunc("GET /apis/impd.sh/v1alpha1/{kinds}/{name}", s.handleGet)
	mux.HandleFunc("PUT /apis/impd.sh/v1alpha1/{kinds}/{name}", s.handleApply)
	mux.HandleFunc("DELETE /apis/impd.sh/v1alpha1/{kinds}/{name}", s.handleDelete)
	mux.HandleFunc("PUT /apis/impd.sh/v1alpha1/{kinds}/{name}/status", s.handleStatus)
	mux.HandleFunc("GET /apis/impd.sh/v1alpha1/procs/{name}/log", s.handleLogs)
	// "stats" is a literal segment, so it wins over the {kinds} pattern.
	mux.HandleFunc("GET /apis/impd.sh/v1alpha1/stats", s.handleStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.version)
	})
	s.mux = mux
	return s
}

// Handler returns the http.Handler serving the API.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Listen opens the Unix domain socket, replacing a stale socket left by an
// unclean shutdown and setting mode 0660.
func Listen(socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, fmt.Errorf("apiserver: creating socket directory: %w", err)
	}
	if fi, err := os.Stat(socketPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("apiserver: %s exists and is not a socket; refusing to remove it", socketPath)
		}
		conn, err := net.DialTimeout("unix", socketPath, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("apiserver: %s is in use — another impd is running; refusing to start", socketPath)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("apiserver: probing existing socket %s: %w", socketPath, err)
		}
		// Connection refused: nobody is accepting, the file is a leftover
		// from an unclean shutdown.
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("apiserver: removing stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("apiserver: listening on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		l.Close()
		return nil, fmt.Errorf("apiserver: setting socket permissions: %w", err)
	}
	return l, nil
}

// SetSocketGroup chgrps the listening socket to the named group (M10-c2):
// under a root impd the socket would otherwise be root:root, locking
// impctl to sudo. Listen's 0660 already grants the group; this points the
// group bit at the operators. Called only when --socket-group is set.
func SetSocketGroup(socketPath, group string) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("apiserver: looking up socket group %q: %w", group, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("apiserver: parsing gid for group %q: %w", group, err)
	}
	if err := os.Chown(socketPath, -1, gid); err != nil {
		return fmt.Errorf("apiserver: chgrp %s to %s: %w", socketPath, group, err)
	}
	return nil
}

// resolveKind maps the {kinds} path segment. Unknown resources are 404s.
func resolveKind(r *http.Request) (string, error) {
	plural := r.PathValue("kinds")
	kind, ok := kindByPlural[plural]
	if !ok {
		return "", fmt.Errorf("apiserver: no resource type %q: %w", plural, v1alpha1.ErrNotFound)
	}
	return kind, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("writing response", "component", "apiserver", "error", err)
	}
}

// writeRaw sends an already-serialized object without re-decoding it.
func writeRaw(w http.ResponseWriter, status int, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(body, '\n')) //nolint:errcheck // client gone; nothing to do
}

func writeError(w http.ResponseWriter, err error) {
	apiErr, status := v1alpha1.APIErrorFrom(err)
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", "component", "apiserver", "error", err)
	}
	writeJSON(w, status, apiErr)
}
