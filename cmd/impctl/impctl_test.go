// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// End-to-end: real cobra commands against a real impd API over a real
// socket - the M1 step-4 acceptance ("get/apply/delete/version against the
// live socket").
package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/apiserver"
	"github.com/mroberts91/imp/internal/etcl"
)

// startServer brings up store + api-server on a socket and returns the
// socket path.
func startServer(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "imp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	store, err := etcl.Open(filepath.Join(dir, "etcl.db"), nil)
	if err != nil {
		t.Fatalf("etcl.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := apiserver.New(apiserver.Config{
		Store:   store,
		Version: v1alpha1.VersionInfo{Version: "v0.0.0-test"},
	})
	socket := filepath.Join(dir, "impd.sock")
	l, err := apiserver.Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(l) //nolint:errcheck // ends with Close
	t.Cleanup(func() { hs.Close() })
	return socket
}

// impctl runs one command line against the socket, returning stdout.
func impctl(t *testing.T, socket string, wantErr bool, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--socket", socket}, args...))
	err := root.ExecuteContext(t.Context())
	if wantErr && err == nil {
		t.Fatalf("impctl %v succeeded, expected failure\nstdout: %s", args, out.String())
	}
	if !wantErr && err != nil {
		t.Fatalf("impctl %v: %v\nstderr: %s", args, err, errOut.String())
	}
	combined := out.String() + errOut.String()
	if err != nil {
		// SilenceErrors is on (main.printError owns rendering), so include
		// the error text the user would have seen.
		combined += err.Error()
	}
	return combined
}

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	return path
}

const webManifest = `
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  template:
    spec:
      command: ["/usr/bin/serve", "--port", "8080"]
---
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: worker
spec:
  replicas: 2
  template:
    spec:
      command: ["/usr/bin/work"]
`

func TestApplyGetDeleteFlow(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, webManifest)

	// Apply: both documents created.
	out := impctl(t, socket, false, "apply", "-f", manifest)
	if !strings.Contains(out, "daemon/web created") || !strings.Contains(out, "daemon/worker created") {
		t.Errorf("apply output:\n%s", out)
	}

	// Re-apply: unchanged, byte for byte.
	out = impctl(t, socket, false, "apply", "-f", manifest)
	if !strings.Contains(out, "daemon/web unchanged") || !strings.Contains(out, "daemon/worker unchanged") {
		t.Errorf("re-apply output:\n%s", out)
	}

	// Edit: configured.
	edited := writeManifest(t, strings.Replace(webManifest, "8080", "9090", 1))
	out = impctl(t, socket, false, "apply", "-f", edited)
	if !strings.Contains(out, "daemon/web configured") || !strings.Contains(out, "daemon/worker unchanged") {
		t.Errorf("edited apply output:\n%s", out)
	}

	// Get table: both daemons with READY x/y columns.
	out = impctl(t, socket, false, "get", "daemons")
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "web") || !strings.Contains(out, "0/2") {
		t.Errorf("get daemons output:\n%s", out)
	}

	// Get single as YAML round-trips through the server.
	out = impctl(t, socket, false, "get", "daemon", "web", "-o", "yaml")
	if !strings.Contains(out, "kind: Daemon") || !strings.Contains(out, "name: web") || !strings.Contains(out, "\"9090\"") {
		t.Errorf("get -o yaml output:\n%s", out)
	}

	// Delete by kind/name, then by manifest.
	out = impctl(t, socket, false, "delete", "daemon", "web")
	if !strings.Contains(out, "daemon/web deleted") {
		t.Errorf("delete output:\n%s", out)
	}
	out = impctl(t, socket, false, "get", "daemons")
	if strings.Contains(out, "web ") {
		t.Errorf("web still listed after delete:\n%s", out)
	}
	// Deleting via manifest now half-fails (web is gone): exit is an error
	// but worker is still deleted.
	out = impctl(t, socket, true, "delete", "-f", manifest)
	if !strings.Contains(out, "daemon/worker deleted") {
		t.Errorf("delete -f output:\n%s", out)
	}
}

func TestApplyValidationOutput(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, `
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: broken
spec:
  replicas: -3
  template:
    spec:
      command: []
`)
	out := impctl(t, socket, true, "apply", "-f", manifest)
	// Field-path errors verbatim from the server.
	if !strings.Contains(out, "spec.replicas") || !strings.Contains(out, "spec.template.spec.command") {
		t.Errorf("validation output missing field paths:\n%s", out)
	}
}

func TestGetEmptyAndUnknownKind(t *testing.T) {
	socket := startServer(t)
	out := impctl(t, socket, false, "get", "procs")
	if !strings.Contains(out, "No resources found") {
		t.Errorf("empty get output:\n%s", out)
	}
	impctl(t, socket, true, "get", "gizmos")
}

func TestVersion(t *testing.T) {
	socket := startServer(t)
	out := impctl(t, socket, false, "version")
	if !strings.Contains(out, "Client:") || !strings.Contains(out, "Server: v0.0.0-test") {
		t.Errorf("version output:\n%s", out)
	}
}

func TestLogsResolvesDaemonWithNoProcs(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, `
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  template:
    spec:
      command: ["/usr/bin/serve"]
`)
	impctl(t, socket, false, "apply", "-f", manifest)
	// No controller running yet, so the daemon has no procs - the error
	// must say so rather than claim the daemon doesn't exist.
	out := impctl(t, socket, true, "logs", "web")
	if !strings.Contains(out, "no procs yet") {
		t.Errorf("logs output:\n%s", out)
	}
}

func TestDescribeDaemonShowsSections(t *testing.T) {
	socket := startServer(t)
	manifest := writeManifest(t, `
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  replicas: 1
  template:
    spec:
      command: ["/bin/sleep", "60"]
`)
	impctl(t, socket, false, "apply", "-f", manifest)
	out := impctl(t, socket, false, "describe", "daemon", "web")
	for _, want := range []string{
		"Name:\tweb",
		"Conditions:",
		"Events:",
		"ObservedGeneration:",
		"Command:\t/bin/sleep 60",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("describe missing %q:\n%s", want, out)
		}
	}
}

func TestEventsForFilter(t *testing.T) {
	socket := startServer(t)
	out := impctl(t, socket, false, "events")
	if !strings.Contains(out, "No resources found") {
		t.Errorf("events empty output:\n%s", out)
	}
	impctl(t, socket, true, "events", "--for", "not-a-ref")
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    string
		want string
	}{
		{"45s", "45s"},
		{"119s", "119s"},
		{"3m30s", "3m30s"},
		{"5m0s", "5m"},
		{"45m", "45m"},
		{"5h20m", "5h20m"},
		{"30h", "30h"},
		{"73h", "3d1h"},
		{"240h", "10d"},
	}
	for _, tc := range cases {
		d, err := time.ParseDuration(tc.d)
		if err != nil {
			t.Fatalf("bad case %q: %v", tc.d, err)
		}
		if got := humanDuration(d); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
