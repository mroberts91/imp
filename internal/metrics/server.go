// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ListenAndServe starts an HTTP server on addr exposing GET /metrics from
// reg. Empty addr is a no-op (returns "", a nil-op shutdown). The returned
// bound address is the actual listen address (useful when addr uses :0).
func ListenAndServe(addr string, reg *Registry) (bound string, shutdown func(context.Context) error, err error) {
	if addr == "" {
		return "", func(context.Context) error { return nil }, nil
	}
	if reg == nil {
		return "", nil, fmt.Errorf("metrics: nil registry")
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg.Gatherer(), promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	}))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("metrics listen %q: %w", addr, err)
	}
	bound = ln.Addr().String()

	srv := &http.Server{Handler: mux}
	log := slog.With("component", "metrics", "addr", bound)
	go func() {
		log.Info("metrics listening")
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server failed", "error", err)
		}
	}()

	return bound, func(ctx context.Context) error {
		log.Info("metrics shutting down")
		return srv.Shutdown(ctx)
	}, nil
}
