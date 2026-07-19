// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package eventttl

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/pkg/client"
)

func startClient(t *testing.T) *client.Client {
	t.Helper()
	dir := t.TempDir()
	store, err := etcl.Open(filepath.Join(dir, "etcl.db"), nil)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	socket := filepath.Join(dir, "impd.sock")
	srv := apiserver.New(apiserver.Config{Store: store})
	ln, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})
	return client.New(socket)
}

func TestSweepDeletesExpired(t *testing.T) {
	cl := startClient(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))

	oldTS := v1alpha1.NewTime(clk.Now().Add(-48 * time.Hour))
	freshTS := v1alpha1.NewTime(clk.Now().Add(-time.Hour))

	if _, err := cl.ApplyEvent(ctx, &v1alpha1.Event{
		Metadata:       v1alpha1.ObjectMeta{Name: "old.ev"},
		Regarding:      v1alpha1.ObjectRef{Kind: v1alpha1.KindDaemon, Name: "web"},
		Type:           v1alpha1.EventTypeNormal,
		Reason:         "Old",
		Message:        "expired",
		Count:          1,
		FirstTimestamp: oldTS,
		LastTimestamp:  oldTS,
	}); err != nil {
		t.Fatalf("ApplyEvent old: %v", err)
	}
	if _, err := cl.ApplyEvent(ctx, &v1alpha1.Event{
		Metadata:       v1alpha1.ObjectMeta{Name: "fresh.ev"},
		Regarding:      v1alpha1.ObjectRef{Kind: v1alpha1.KindDaemon, Name: "web"},
		Type:           v1alpha1.EventTypeNormal,
		Reason:         "Fresh",
		Message:        "kept",
		Count:          1,
		FirstTimestamp: freshTS,
		LastTimestamp:  freshTS,
	}); err != nil {
		t.Fatalf("ApplyEvent fresh: %v", err)
	}

	c := New(cl, 24*time.Hour, clk)
	c.tick = time.Hour // unused in direct sweep
	c.sweep(ctx)

	events, _, err := cl.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if events[0].Metadata.Name != "fresh.ev" {
		t.Errorf("surviving event = %q, want fresh.ev", events[0].Metadata.Name)
	}
}

func TestSweepKeepsRecent(t *testing.T) {
	cl := startClient(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))

	ts := v1alpha1.NewTime(clk.Now().Add(-time.Hour))
	if _, err := cl.ApplyEvent(ctx, &v1alpha1.Event{
		Metadata:       v1alpha1.ObjectMeta{Name: "recent.ev"},
		Regarding:      v1alpha1.ObjectRef{Kind: v1alpha1.KindProc, Name: "web-0"},
		Type:           v1alpha1.EventTypeWarning,
		Reason:         v1alpha1.ReasonBackOff,
		Message:        "backing off",
		Count:          3,
		FirstTimestamp: ts,
		LastTimestamp:  ts,
	}); err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}

	c := New(cl, 72*time.Hour, clk)
	c.sweep(ctx)

	events, _, err := cl.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}
