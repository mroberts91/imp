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
	"github.com/mroberts91/imp/internal/controllers/eventttl"
	"github.com/mroberts91/imp/internal/controllers/gc"
	"github.com/mroberts91/imp/internal/controllers/notifier"
	"github.com/mroberts91/imp/internal/controllers/timer"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/internal/execd/cgroups"
	"github.com/mroberts91/imp/internal/execd/childsetup"
	"github.com/mroberts91/imp/internal/execd/configfiles"
	"github.com/mroberts91/imp/internal/execd/logs"
	"github.com/mroberts91/imp/internal/execd/supervisor"
	"github.com/mroberts91/imp/internal/manifest"
	"github.com/mroberts91/imp/internal/metrics"
	"github.com/mroberts91/imp/internal/queue"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

var (
	version   = "dev"
	commit    = "unknown"
	branch    = "unknown"
	buildTime = "unknown"
)

func main() {
	// When this process is a freshly spawned Proc child (M6-a always-shim),
	// take over before flags, logging, or anything else — MaybeRun never
	// returns in that case.
	childsetup.MaybeRun()

	var (
		socketPath          = flag.String("socket", "/run/imp/impd.sock", "path of the API's unix domain socket")
		dataDir             = flag.String("data-dir", "/var/lib/imp", "state directory (object store, process logs)")
		manifestDir         = flag.String("manifest-dir", "/etc/imp/manifests", "directory of declarative YAML manifests")
		logLevel            = flag.String("log-level", "info", "log level: debug, info, warn, or error")
		eventTTL            = flag.Duration("event-ttl", eventttl.DefaultRetention, "how long to retain Events before pruning (D3)")
		cgroupRoot          = flag.String("cgroup-root", "", "writable cgroup v2 subtree (empty: auto-detect systemd Delegate= / self cgroup)")
		killProcsOnShutdown = flag.Bool("kill-procs-on-shutdown", false, "terminate Procs and remove cgroups on impd stop (default: leave them for D1 re-attach)")
		metricsAddr         = flag.String("metrics-addr", "127.0.0.1:9090", "Prometheus metrics listen address (empty disables)")
		socketGroup         = flag.String("socket-group", "", "chgrp the API socket to this group (privileged installs: keeps impctl sudo-less for group members; empty: no change)")
	)
	flag.Parse()

	if err := run(*socketPath, *dataDir, *manifestDir, *logLevel, *eventTTL, *cgroupRoot, *killProcsOnShutdown, *metricsAddr, *socketGroup); err != nil {
		slog.Error("impd exiting", "error", err)
		os.Exit(1)
	}
}

func run(socketPath, dataDir, manifestDir, logLevel string, eventTTL time.Duration, cgroupRootFlag string, killProcsOnShutdown bool, metricsAddr, socketGroup string) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cgroupRoot, err := cgroups.ResolveRoot(cgroupRootFlag)
	if err != nil {
		return err
	}
	cgMgr, err := cgroups.NewManager(cgroupRoot)
	if err != nil {
		return fmt.Errorf("cgroup root %q: %w", cgroupRoot, err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	store, err := etcl.Open(filepath.Join(dataDir, "etcl.db"), nil)
	if err != nil {
		return err
	}

	logStore := logs.New(filepath.Join(dataDir, "logs"), clock.Real{})

	// Everything below is inert construction — nothing dials or runs until
	// Serve / Run. The supervisor is built before the api-server so it can
	// be wired in as the StatsProvider behind /stats (impctl top).
	ctlClient := client.New(socketPath)
	configStore := configfiles.New(filepath.Join(dataDir, "configs"), ctlClient)
	clk := clock.Real{}
	met := metrics.New()
	manifestRec := recorder.New(ctlClient, "manifest", clk)
	daemonRec := recorder.New(ctlClient, daemon.ReportingComponent, clk)
	timerRec := recorder.New(ctlClient, timer.ReportingComponent, clk)
	notifierRec := recorder.New(ctlClient, notifier.ReportingComponent, clk)
	execRec := recorder.New(ctlClient, "execd", clk)

	// execd supervisor: handlers never fire before Run, so assigning the
	// manager after NewInformer (same pattern as dcEnqueueOwner) is safe.
	var execMgr *supervisor.Manager
	execInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) {
		execMgr.Handle(key)
	}, nil)
	execMgr = supervisor.NewManager(ctlClient, execInf.Store(), logStore, configStore, cgMgr, clk, execRec, killProcsOnShutdown)
	execMgr.SetMetrics(met)

	server := apiserver.New(apiserver.Config{
		Store: store,
		Logs:  logStore,
		Stats: execMgr,
		Version: v1alpha1.VersionInfo{
			Version: version, Commit: commit, Branch: branch, BuildTime: buildTime,
		},
	})
	listener, err := apiserver.Listen(socketPath)
	if err != nil {
		store.Close()
		return err
	}
	if socketGroup != "" {
		if err := apiserver.SetSocketGroup(socketPath, socketGroup); err != nil {
			listener.Close()
			store.Close()
			return err
		}
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
		watcher := manifest.NewWatcher(client.New(socketPath), manifestDir, nil, manifestRec)
		if err := watcher.Run(ctx); err != nil {
			slog.Error("manifest watcher failed", "component", "manifest", "error", err)
		}
	}()

	dcQueue := queue.NewRateLimiting(queue.DefaultRateLimiter())
	dcDaemonInf := cache.NewInformer(ctlClient, v1alpha1.KindDaemon, func(key string) { dcQueue.Add(key) }, nil)
	var dcEnqueueOwner func(key string)
	dcProcInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) { dcEnqueueOwner(key) }, nil)
	dcEnqueueOwner = controllers.EnqueueOwner(dcProcInf.Store(), v1alpha1.KindDaemon, dcQueue)
	// A Config change enqueues every Daemon that references it (M8): an edit
	// rolls them, an appearance heals a ConfigMissing hold.
	dcConfigInf := cache.NewInformer(ctlClient, v1alpha1.KindConfig,
		daemon.EnqueueReferencingDaemons(dcDaemonInf.Store(), dcQueue), nil)
	dcRunner := controllers.NewRunner("daemon-controller", dcQueue,
		daemon.New(ctlClient, dcDaemonInf.Store(), dcProcInf.Store(), dcConfigInf.Store(), clk, daemonRec),
		1, dcDaemonInf, dcProcInf, dcConfigInf)
	dcRunner.SetMetrics(met, metrics.ControllerDaemon)

	// Timer controller: a Timer change enqueues its own key; a Proc change
	// enqueues the owning Timer.
	tcQueue := queue.NewRateLimiting(queue.DefaultRateLimiter())
	tcTimerInf := cache.NewInformer(ctlClient, v1alpha1.KindTimer, func(key string) { tcQueue.Add(key) }, nil)
	var tcEnqueueOwner func(key string)
	tcProcInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) { tcEnqueueOwner(key) }, nil)
	tcEnqueueOwner = controllers.EnqueueOwner(tcProcInf.Store(), v1alpha1.KindTimer, tcQueue)
	tcRunner := controllers.NewRunner("timer-controller", tcQueue,
		timer.New(ctlClient, tcTimerInf.Store(), tcProcInf.Store(), clk, timerRec), 1, tcTimerInf, tcProcInf)
	tcRunner.SetMetrics(met, metrics.ControllerTimer)

	// Notifier controller (M10-a): a Notifier change enqueues its own key;
	// any Daemon, Timer, or Proc change enqueues every Notifier — failure
	// signals are levels on other kinds. Single-host object counts make the
	// fan-out cheap; resync is the safety net.
	ncQueue := queue.NewRateLimiting(queue.DefaultRateLimiter())
	ncNotifierInf := cache.NewInformer(ctlClient, v1alpha1.KindNotifier, func(key string) { ncQueue.Add(key) }, nil)
	ncFanOut := notifier.EnqueueAllNotifiers(ncNotifierInf.Store(), ncQueue)
	ncDaemonInf := cache.NewInformer(ctlClient, v1alpha1.KindDaemon, ncFanOut, nil)
	ncTimerInf := cache.NewInformer(ctlClient, v1alpha1.KindTimer, ncFanOut, nil)
	ncProcInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, ncFanOut, nil)
	ncRunner := controllers.NewRunner("notifier-controller", ncQueue,
		notifier.New(ctlClient, ncNotifierInf.Store(), ncDaemonInf.Store(), ncTimerInf.Store(), ncProcInf.Store(), clk, notifierRec),
		1, ncNotifierInf, ncDaemonInf, ncTimerInf, ncProcInf)
	ncRunner.SetMetrics(met, metrics.ControllerNotifier)

	gcQueue := queue.NewRateLimiting(queue.DefaultRateLimiter())
	gcProcInf := cache.NewInformer(ctlClient, v1alpha1.KindProc, func(key string) { gcQueue.Add(key) }, nil)
	gcDaemonInf := cache.NewInformer(ctlClient, v1alpha1.KindDaemon, gc.EnqueueOwnedProcs(gcProcInf.Store(), gcQueue), nil)
	gcTimerInf := cache.NewInformer(ctlClient, v1alpha1.KindTimer, gc.EnqueueOwnedProcs(gcProcInf.Store(), gcQueue), nil)
	gcNotifierInf := cache.NewInformer(ctlClient, v1alpha1.KindNotifier, gc.EnqueueOwnedProcs(gcProcInf.Store(), gcQueue), nil)
	gcRunner := controllers.NewRunner("gc", gcQueue,
		gc.New(ctlClient, gcDaemonInf.Store(), gcTimerInf.Store(), gcNotifierInf.Store(), gcProcInf.Store()),
		1, gcDaemonInf, gcTimerInf, gcNotifierInf, gcProcInf)
	gcRunner.SetMetrics(met, metrics.ControllerGC)

	ttlCtl := eventttl.New(ctlClient, eventTTL, clk)

	metricsBound, metricsShutdown, err := metrics.ListenAndServe(metricsAddr, met)
	if err != nil {
		store.Close()
		return err
	}

	scraperDone := make(chan struct{})
	go func() {
		defer close(scraperDone)
		metrics.NewScraper(met, cgMgr, execMgr, clk, 0).Run(ctx)
	}()

	go dcDaemonInf.Run(ctx)
	go dcProcInf.Run(ctx)
	go dcConfigInf.Run(ctx)
	go tcTimerInf.Run(ctx)
	go tcProcInf.Run(ctx)
	go ncNotifierInf.Run(ctx)
	go ncDaemonInf.Run(ctx)
	go ncTimerInf.Run(ctx)
	go ncProcInf.Run(ctx)
	go gcDaemonInf.Run(ctx)
	go gcTimerInf.Run(ctx)
	go gcNotifierInf.Run(ctx)
	go gcProcInf.Run(ctx)
	go execInf.Run(ctx)

	if err := execInf.WaitForSync(ctx); err != nil && !errors.Is(err, context.Canceled) {
		store.Close()
		return fmt.Errorf("execd informer sync: %w", err)
	}
	execMgr.Bootstrap(ctx)

	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		if err := dcRunner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("daemon controller failed", "component", "daemon-controller", "error", err)
		}
	}()
	timerDone := make(chan struct{})
	go func() {
		defer close(timerDone)
		if err := tcRunner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("timer controller failed", "component", "timer-controller", "error", err)
		}
	}()
	notifierDone := make(chan struct{})
	go func() {
		defer close(notifierDone)
		if err := ncRunner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("notifier controller failed", "component", "notifier-controller", "error", err)
		}
	}()
	gcDone := make(chan struct{})
	go func() {
		defer close(gcDone)
		if err := gcRunner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("gc controller failed", "component", "gc", "error", err)
		}
	}()
	ttlDone := make(chan struct{})
	go func() {
		defer close(ttlDone)
		if err := ttlCtl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("eventttl failed", "component", "eventttl", "error", err)
		}
	}()

	slog.Info("impd started",
		"version", version, "commit", commit,
		"socket", socketPath, "dataDir", dataDir, "manifestDir", manifestDir,
		"cgroupRoot", cgroupRoot,
		"killProcsOnShutdown", killProcsOnShutdown,
		"metricsAddr", metricsBound,
		"eventTTL", eventTTL.String())

	select {
	case <-ctx.Done():
		slog.Info("shutting down", "reason", "signal")
	case err := <-serveErr:
		store.Close()
		return fmt.Errorf("api-server: %w", err)
	}

	// Ordered shutdown: manifest → controllers (daemon, timer, notifier, gc, eventttl) → execd → metrics → store → HTTP.
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
	case <-timerDone:
	case <-time.After(5 * time.Second):
		slog.Warn("timer controller did not stop in time")
	}
	select {
	case <-notifierDone:
	case <-time.After(5 * time.Second):
		slog.Warn("notifier controller did not stop in time")
	}
	select {
	case <-gcDone:
	case <-time.After(5 * time.Second):
		slog.Warn("gc controller did not stop in time")
	}
	select {
	case <-ttlDone:
	case <-time.After(5 * time.Second):
		slog.Warn("eventttl did not stop in time")
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

	select {
	case <-scraperDone:
	case <-time.After(2 * time.Second):
		slog.Warn("metrics scraper did not stop in time")
	}
	mctx, mcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer mcancel()
	if err := metricsShutdown(mctx); err != nil {
		slog.Warn("metrics shutdown incomplete", "error", err)
	}

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
