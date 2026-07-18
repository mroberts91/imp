// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// One-wire-both-ends test: the informer over the real client, apiserver,
// and etcl store, across a real Unix socket.
package cache_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/etcl"
	"github.com/mroberts91/imp/pkg/client"
)

func startServer(t *testing.T) *client.Client {
	t.Helper()
	dir := t.TempDir()

	store, err := etcl.Open(filepath.Join(dir, "etcl.db"), nil)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := apiserver.New(apiserver.Config{
		Store:   store,
		Version: v1alpha1.VersionInfo{Version: "test"},
	})
	socket := filepath.Join(dir, "impd.sock")
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(l) //nolint:errcheck // ends with Close
	t.Cleanup(func() { hs.Close() })

	return client.New(socket)
}

func TestInformerRealServerRoundTrip(t *testing.T) {
	c := startServer(t)
	ctx := t.Context()

	keys := make(chan string, 100)
	inf := cache.NewInformer(c, v1alpha1.KindDaemon, func(key string) { keys <- key }, nil)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go inf.Run(runCtx)

	if err := inf.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	if n := len(inf.Store().List()); n != 0 {
		t.Fatalf("store has %d items before anything was applied", n)
	}

	// Create.
	daemon := &v1alpha1.Daemon{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.DaemonSpec{
			Template: v1alpha1.ProcTemplate{
				Spec: v1alpha1.ProcTemplateSpec{Command: []string{"/bin/sleep", "60"}},
			},
		},
	}
	if _, err := c.ApplyDaemon(ctx, daemon); err != nil {
		t.Fatalf("ApplyDaemon: %v", err)
	}
	if got := collectKeys(t, keys, 1); got["Daemon/web"] != 1 {
		t.Fatalf("firings after create = %v, want Daemon/web", got)
	}

	raw, ok := inf.Store().GetByKey("Daemon/web")
	if !ok {
		t.Fatal("Daemon/web missing from store")
	}
	var mirrored v1alpha1.Daemon
	if err := json.Unmarshal(raw, &mirrored); err != nil {
		t.Fatalf("stored object does not decode as Daemon: %v", err)
	}
	if mirrored.Metadata.Name != "web" || mirrored.Metadata.ResourceVersion == "" {
		t.Errorf("mirrored object incomplete: %+v", mirrored.Metadata)
	}

	// Update: the mirror must converge to the new generation.
	updated := mirrored
	updated.Spec.Template.Spec.Command = []string{"/bin/sleep", "120"}
	if _, err := c.ApplyDaemon(ctx, &updated); err != nil {
		t.Fatalf("ApplyDaemon(update): %v", err)
	}
	collectKeys(t, keys, 1)
	waitFor(t, "mirror to converge to the update", func() bool {
		raw, getOK := inf.Store().GetByKey("Daemon/web")
		if !getOK {
			return false
		}
		var d v1alpha1.Daemon
		return json.Unmarshal(raw, &d) == nil && d.Metadata.Generation == 2
	})

	// Delete.
	if err := c.DeleteDaemon(ctx, "web"); err != nil {
		t.Fatalf("DeleteDaemon: %v", err)
	}
	if got := collectKeys(t, keys, 1); got["Daemon/web"] != 1 {
		t.Fatalf("firings after delete = %v, want Daemon/web", got)
	}
	waitFor(t, "store to drop the deleted daemon", func() bool {
		_, stillThere := inf.Store().GetByKey("Daemon/web")
		return !stillThere
	})
}
