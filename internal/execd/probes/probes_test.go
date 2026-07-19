// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package probes

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/execd/cgroups"
)

type stubRunner struct {
	code  int
	err   error
	calls int
}

func (s *stubRunner) RunIn(_ context.Context, _ string, _ []string, _ cgroups.RunOptions) (int, error) {
	s.calls++
	return s.code, s.err
}

func waitEvent(t *testing.T, ch <-chan ResultEvent, timeout time.Duration) ResultEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for probe event")
		return ResultEvent{}
	}
}

func TestReadinessInitialFailureThenSuccess(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0).UTC())
	events := make(chan ResultEvent, 8)
	runner := &stubRunner{code: 0}
	m := NewManager(runner, clk, func(ev ResultEvent) { events <- ev })

	spec := &v1alpha1.Probe{
		Exec:                &v1alpha1.ExecAction{Command: []string{"true"}},
		InitialDelaySeconds: 0,
		PeriodSeconds:       1,
		TimeoutSeconds:      1,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
	m.Start("Proc/web", "/cg", clk.Now(), nil, spec)
	defer m.Stop("Proc/web")

	ev := waitEvent(t, events, time.Second)
	if ev.Result != ResultFailure || ev.ProbeType != ProbeReadiness {
		t.Fatalf("initial: got %+v", ev)
	}

	waitWaiters(t, clk, 1)
	clk.Step(time.Second)
	ev = waitEvent(t, events, time.Second)
	if ev.Result != ResultSuccess {
		t.Fatalf("after success: got %+v", ev)
	}
	if runner.calls < 1 {
		t.Fatalf("expected exec calls, got %d", runner.calls)
	}
}

func TestLivenessFailureThreshold(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0).UTC())
	events := make(chan ResultEvent, 8)
	runner := &stubRunner{code: 1}
	m := NewManager(runner, clk, func(ev ResultEvent) { events <- ev })

	spec := &v1alpha1.Probe{
		Exec:             &v1alpha1.ExecAction{Command: []string{"false"}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		SuccessThreshold: 1,
		FailureThreshold: 2,
	}
	m.Start("Proc/web", "/cg", clk.Now(), spec, nil)
	defer m.Stop("Proc/web")

	ev := waitEvent(t, events, time.Second)
	if ev.Result != ResultSuccess || ev.ProbeType != ProbeLiveness {
		t.Fatalf("initial liveness: got %+v", ev)
	}

	waitWaiters(t, clk, 1)
	clk.Step(time.Second) // first failure — below threshold
	select {
	case ev := <-events:
		t.Fatalf("unexpected event below threshold: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	clk.Step(time.Second) // second failure — cross threshold
	ev = waitEvent(t, events, time.Second)
	if ev.Result != ResultFailure {
		t.Fatalf("want Failure, got %+v", ev)
	}
}

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	portN, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	port := int32(portN)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, msg := runHTTP(ctx, &v1alpha1.HTTPGetAction{
		Host: "127.0.0.1",
		Port: port,
		Path: "/",
	})
	if res != ResultSuccess {
		t.Fatalf("http probe: %s %s", res, msg)
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	portN, _ := strconv.Atoi(portStr)
	port := int32(portN)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, msg := runTCP(ctx, &v1alpha1.TCPSocketAction{Host: "127.0.0.1", Port: port})
	if res != ResultSuccess {
		t.Fatalf("tcp probe: %s %s", res, msg)
	}
}

func waitWaiters(t *testing.T, clk *clock.Fake, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clk.Waiters() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d clock waiters (have %d)", want, clk.Waiters())
}
