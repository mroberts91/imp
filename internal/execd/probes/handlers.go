// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package probes

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/execd/cgroups"
)

// Runner executes a command inside a Proc cgroup (D4). *cgroups.Manager
// satisfies this.
type Runner interface {
	RunIn(ctx context.Context, path string, command []string, opts cgroups.RunOptions) (exitCode int, err error)
}

func runProbe(ctx context.Context, runner Runner, cgroupPath string, spec *v1alpha1.Probe) (Result, string) {
	switch {
	case spec.Exec != nil:
		return runExec(ctx, runner, cgroupPath, spec.Exec)
	case spec.HTTPGet != nil:
		return runHTTP(ctx, spec.HTTPGet)
	case spec.TCPSocket != nil:
		return runTCP(ctx, spec.TCPSocket)
	default:
		return ResultUnknown, "no probe handler configured"
	}
}

func runExec(ctx context.Context, runner Runner, cgroupPath string, exec *v1alpha1.ExecAction) (Result, string) {
	if runner == nil {
		return ResultFailure, "exec probe: no cgroup runner"
	}
	code, err := runner.RunIn(ctx, cgroupPath, exec.Command, cgroups.RunOptions{})
	if err != nil {
		if ctx.Err() != nil {
			return ResultFailure, "exec probe timed out"
		}
		return ResultFailure, fmt.Sprintf("exec probe error: %v", err)
	}
	if code == 0 {
		return ResultSuccess, "exec probe succeeded"
	}
	return ResultFailure, fmt.Sprintf("exec probe exit code %d", code)
}

func runHTTP(ctx context.Context, get *v1alpha1.HTTPGetAction) (Result, string) {
	host := get.Host
	if host == "" {
		host = "127.0.0.1"
	}
	scheme := strings.ToLower(string(get.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	path := get.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := fmt.Sprintf("%s://%s:%d%s", scheme, host, get.Port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ResultFailure, err.Error()
	}
	for _, h := range get.HTTPHeaders {
		req.Header.Add(h.Name, h.Value)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ResultFailure, "http probe timed out"
		}
		return ResultFailure, fmt.Sprintf("http probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return ResultSuccess, fmt.Sprintf("http probe %d", resp.StatusCode)
	}
	return ResultFailure, fmt.Sprintf("http probe status %d", resp.StatusCode)
}

func runTCP(ctx context.Context, sock *v1alpha1.TCPSocketAction) (Result, string) {
	host := sock.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(sock.Port)))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return ResultFailure, "tcp probe timed out"
		}
		return ResultFailure, fmt.Sprintf("tcp probe: %v", err)
	}
	_ = conn.Close()
	return ResultSuccess, "tcp probe succeeded"
}
