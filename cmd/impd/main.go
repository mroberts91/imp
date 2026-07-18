// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// impd is the imp daemon: one process hosting the api-server, the object
// store, the controllers, and execd. This package is wiring only
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/controllers"
	"github.com/mroberts91/imp/internal/controllers/daemon"
	"github.com/mroberts91/imp/internal/controllers/gc"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/internal/execd/logs"
	"github.com/mroberts91/imp/internal/execd/supervisor"
	"github.com/mroberts91/imp/internal/manifest"
	"github.com/mroberts91/imp/internal/queue"
	"github.com/mroberts91/imp/pkg/client"
)

var (
	version   = "dev"
	commit    = "unknown"
	branch    = "unknown"
	buildTime = "unknown"
)

func main() {
	var (
		socketPath  = flag.String("socket", "/run/imp/impd.sock", "path of the API's unix domain socket")
		dataDir     = flag.String("data-dir", "/var/lib/imp", "state directory (object store, process logs)")
		manifestDir = flag.String("manifest-dir", "/etc/imp/manifests", "directory of declarative YAML manifests")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, or error")
	)
	flag.Parse()

	if err := run(*socketPath, *dataDir, *manifestDir, *logLevel); err != nil {
		slog.Error("impd exiting", "error", err)
		os.Exit(1)
	}
}

func run(socketPath, dataDir, manifestDir, logLevel string) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	store, err := etcl.Open(filepath.Join(dataDir, "etcl.db"), nil)
	if err != nil {
		return err
	}

	logStore := logs.New(filepath.Join(dataDir, "logs"), clock.Real{})
	server := apiserver.New(apiserver.Config{
		Store: store,
		Logs:  logStore,
		Version: v1alpha1.VersionInfo{
			Version: version, Commit: commit, Branch: branch, BuildTime: buildTime,
		},
	})
	listener, err := apiserver.Listen(socketPath)
	if err != nil {
		store.Close()
		return err
	}
	httpServer := &http.Server{Handler: server.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		watcher := manifest.NewWatcher(client.New(socketPath), manifestDir, nil)
		if err := watcher.Run(ctx); err != nil {
			slog.Error("manifest watcher failed", "component", "manifest", "error", err)
		}
	}()

	ctlClient := client.New(socketPath)

	dcQueue := queue.NewRateLimiting(queue.DefaultRateLimiter())
	dcDaemonInf := cache.NewInformer(ctlClient, v1alpha1.KindDaemon, func(key string) { dcQueue.Add(key) }, nil)
	var dcEnqueueOwner func(key string)
	dcProcInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) { dcEnqueueOwner(key) }, nil)
	dcEnqueueOwner = controllers.EnqueueOwner(dcProcInf.Store(), v1alpha1.KindDaemon, dcQueue)
	dcRunner := controllers.NewRunner("daemon-controller", dcQueue,
		daemon.New(ctlClient, dcDaemonInf.Store(), dcProcInf.Store(), nil), 1, dcDaemonInf, dcProcInf)

	gcQueue := queue.NewRateLimiting(queue.DefaultRateLimiter())
	gcProcInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) { gcQueue.Add(key) }, nil)
	gcDaemonInf := cache.NewInformer(ctlClient, v1alpha1.KindDaemon, gc.EnqueueOwnedProcs(gcProcInf.Store(), gcQueue), nil)
	gcRunner := controllers.NewRunner("gc", gcQueue,
		gc.New(ctlClient, gcDaemonInf.Store(), gcProcInf.Store()), 1, gcDaemonInf, gcProcInf)

	// execd supervisor: handlers never fire before Run, so assigning the
	// manager after NewInformer (same pattern as dcEnqueueOwner) is safe.
	var execMgr *supervisor.Manager
	execInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) {
		execMgr.Handle(key)
	}, nil)
	execMgr = supervisor.NewManager(ctlClient, execInf.Store(), logStore, clock.Real{})

	go dcDaemonInf.Run(ctx)
	go dcProcInf.Run(ctx)
	go gcDaemonInf.Run(ctx)
	go gcProcInf.Run(ctx)
	go execInf.Run(ctx)

	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		if err := dcRunner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("daemon controller failed", "component", "daemon-controller", "error", err)
		}
	}()
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		if err := gcRunner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("gc controller failed", "component", "gc", "error", err)
		}
	}()

	slog.Info("impd started",
		"version", version, "commit", commit,
		"socket", socketPath, "dataDir", dataDir, "manifestDir", manifestDir)

	select {
	case <-ctx.Done():
		slog.Info("shutting down", "reason", "signal")
	case err := <-serveErr:
		store.Close()
		return fmt.Errorf("api-server: %w", err)
	}

	// Ordered shutdown: manifest → controllers → execd → store → HTTP.
	select {
	case <-watcherDone:
	case <-time.After(5 * time.Second):
		slog.Warn("manifest watcher did not stop in time")
	}
	select {
	case <-daemonDone:
	case <-time.After(5 * time.Second):
		slog.Warn("daemon controller did not stop in time")
	}
	select {
	case <-gcDone:
	case <-time.After(5 * time.Second):
		slog.Warn("gc controller did not stop in time")
	}
	slog.Info("controllers stopped")

	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		execMgr.Stop()
	}()
	select {
	case <-execDone:
	case <-time.After(30 * time.Second):
		slog.Warn("execd did not stop in time")
	}
	slog.Info("execd stopped")

	storeErr := store.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("api-server shutdown incomplete; forcing close", "error", err)
		httpServer.Close()
	}
	slog.Info("impd stopped")
	return storeErr
}
