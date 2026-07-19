// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetProcPhaseAndRestarts(t *testing.T) {
	r := New()
	r.SetProcPhase("web-0", "web", "Running")
	r.SetProcPhase("web-0", "web", "Pending")
	r.ObserveRestarts("web-0", "web", 1)
	r.ObserveRestarts("web-0", "web", 3)
	r.ObserveRestarts("web-0", "web", 3) // no-op

	if got := testutil.ToFloat64(r.procPhase.WithLabelValues("web-0", "web", "Running")); got != 0 {
		t.Fatalf("Running gauge = %v, want 0", got)
	}
	if got := testutil.ToFloat64(r.procPhase.WithLabelValues("web-0", "web", "Pending")); got != 1 {
		t.Fatalf("Pending gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.procRestarts.WithLabelValues("web-0", "web")); got != 3 {
		t.Fatalf("restarts = %v, want 3", got)
	}
}

func TestIncProbeAndReconcile(t *testing.T) {
	r := New()
	r.IncProbeResult("web-0", "Readiness", "Success")
	r.ObserveReconcile(ControllerDaemon, 10*time.Millisecond, false)
	r.ObserveReconcile(ControllerDaemon, time.Millisecond, true)

	if got := testutil.ToFloat64(r.probeResults.WithLabelValues("web-0", "Readiness", "Success")); got != 1 {
		t.Fatalf("probe = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.reconcileErrors.WithLabelValues(ControllerDaemon)); got != 1 {
		t.Fatalf("errors = %v, want 1", got)
	}
}

func TestCPUDelta(t *testing.T) {
	r := New()
	r.ObserveProcCPU("web-0", "web", 1.5)
	r.ObserveProcCPU("web-0", "web", 2.0)
	if got := testutil.ToFloat64(r.procCPU.WithLabelValues("web-0", "web")); got != 2.0 {
		t.Fatalf("cpu = %v, want 2.0", got)
	}
}

func TestListenAndServeEmptyDisabled(t *testing.T) {
	_, shutdown, err := ListenAndServe("", New())
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestListenAndServeRoundTrip(t *testing.T) {
	r := New()
	r.SetProcPhase("a-0", "a", "Running")
	r.SetProcMemory("a-0", "a", 4096)

	bound, shutdown, err := ListenAndServe("127.0.0.1:0", r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(t.Context()) }()

	resp, err := http.Get("http://" + bound + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"imp_proc_phase", "imp_proc_memory_bytes", `proc="a-0"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in exposition:\n%s", want, s)
		}
	}
}
