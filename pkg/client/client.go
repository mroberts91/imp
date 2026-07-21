// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package client is the typed client for impd's API
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/mroberts91/imp/api/v1alpha1"
)

const DefaultSocketPath = "/run/imp/impd.sock"

const apiPrefix = "/apis/impd.sh/v1alpha1/"

var pluralByKind = map[string]string{
	v1alpha1.KindDaemon:   "daemons",
	v1alpha1.KindProc:     "procs",
	v1alpha1.KindEvent:    "events",
	v1alpha1.KindTimer:    "timers",
	v1alpha1.KindConfig:   "configs",
	v1alpha1.KindNotifier: "notifiers",
}

type Client struct {
	hc   *http.Client
	base string
}

func New(socketPath string) *Client {
	return &Client{
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
		// The host is a placeholder. Transport always dials the socket.
		base: "http://impd",
	}
}

func resourcePath(kind, name string) (string, error) {
	plural, ok := pluralByKind[kind]
	if !ok {
		return "", fmt.Errorf("client: unknown kind %q", kind)
	}
	if name == "" {
		return apiPrefix + plural, nil
	}
	return apiPrefix + plural + "/" + url.PathEscape(name), nil
}

func (c *Client) GetRaw(ctx context.Context, kind, name string) (json.RawMessage, error) {
	path, err := resourcePath(kind, name)
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodGet, path, nil)
}

// ListOption customizes a list request's query string. Options are list-only;
// watch ignores them (informers filter client-side, M9-i).
type ListOption func(url.Values)

// WithLabelSelector filters a list to objects whose labels satisfy sel — a
// comma-joined set of equality terms (k=v, k==v, k!=v). Empty sel is a no-op.
func WithLabelSelector(sel string) ListOption {
	return func(q url.Values) {
		if sel != "" {
			q.Set("labelSelector", sel)
		}
	}
}

func (c *Client) ListRaw(ctx context.Context, kind string, opts ...ListOption) (*v1alpha1.ObjectList, error) {
	path, err := resourcePath(kind, "")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, opt := range opts {
		opt(q)
	}
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	raw, err := c.doJSON(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.ObjectList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("client: decoding list response: %w", err)
	}
	return &list, nil
}

// Apply PUTs the body as the object's new manifest
func (c *Client) Apply(ctx context.Context, kind, name string, body []byte) (json.RawMessage, error) {
	path, err := resourcePath(kind, name)
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodPut, path, body)
}

// UpdateStatusRaw PUTs to the status sub-resource.
func (c *Client) UpdateStatusRaw(ctx context.Context, kind, name string, body []byte) (json.RawMessage, error) {
	path, err := resourcePath(kind, name)
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodPut, path+"/status", body)
}

func (c *Client) Delete(ctx context.Context, kind, name string) error {
	path, err := resourcePath(kind, name)
	if err != nil {
		return err
	}
	_, err = c.doJSON(ctx, http.MethodDelete, path, nil)
	return err
}

func (c *Client) Watch(ctx context.Context, kind, sinceRV string) (<-chan v1alpha1.WatchEvent, func(), error) {
	path, err := resourcePath(kind, "")
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{"watch": {"true"}}
	if sinceRV != "" {
		q.Set("resourceVersion", sinceRV)
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		defer cancel()
		return nil, nil, readError(resp)
	}

	events := make(chan v1alpha1.WatchEvent)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var ev v1alpha1.WatchEvent
			if err := dec.Decode(&ev); err != nil {
				return // EOF, cancellation, or a broken stream: caller relists
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, cancel, nil
}

type LogOptions struct {
	Follow     bool
	TailLines  int
	Timestamps bool
}

func (c *Client) ProcLogs(ctx context.Context, name string, opts LogOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if opts.Follow {
		q.Set("follow", "true")
	}
	if opts.TailLines > 0 {
		q.Set("tailLines", fmt.Sprint(opts.TailLines))
	}
	if opts.Timestamps {
		q.Set("timestamps", "true")
	}
	u := c.base + apiPrefix + "procs/" + url.PathEscape(name) + "/log"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp.Body, nil
}

func (c *Client) ServerVersion(ctx context.Context) (*v1alpha1.VersionInfo, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return nil, err
	}
	var v v1alpha1.VersionInfo
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("client: decoding version: %w", err)
	}
	return &v, nil
}

// ServerInfo returns the running impd's effective configuration
// (GET /info): resolved flag values plus live facts like the bound
// metrics address and cgroup enforcement. ErrNotFound means the serving
// impd predates the endpoint.
func (c *Client) ServerInfo(ctx context.Context) (*v1alpha1.ServerInfo, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/info", nil)
	if err != nil {
		return nil, err
	}
	var info v1alpha1.ServerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("client: decoding info: %w", err)
	}
	return &info, nil
}

// Stats returns point-in-time resource observations for running Procs
// (the data behind impctl top). 501 when the serving impd has no stats
// provider wired.
func (c *Client) Stats(ctx context.Context) ([]v1alpha1.ProcStat, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, apiPrefix+"stats", nil)
	if err != nil {
		return nil, err
	}
	var l v1alpha1.StatsList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("client: decoding stats: %w", err)
	}
	return l.Items, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, readError(resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("client: reading response: %w", err)
	}
	return data, nil
}

// readError turns a non-2xx response back into an error matching the
// original via the APIError wire document.
func readError(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("client: HTTP %d (unreadable body: %v)", resp.StatusCode, err)
	}
	var apiErr v1alpha1.APIError
	if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.Reason != "" {
		return apiErr.Err()
	}
	return fmt.Errorf("client: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}
