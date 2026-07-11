// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func (c *Client) GetDaemon(ctx context.Context, name string) (*v1alpha1.Daemon, error) {
	return get[v1alpha1.Daemon](ctx, c, v1alpha1.KindDaemon, name)
}

func (c *Client) ListDaemons(ctx context.Context) ([]v1alpha1.Daemon, string, error) {
	return list[v1alpha1.Daemon](ctx, c, v1alpha1.KindDaemon)
}

func (c *Client) ApplyDaemon(ctx context.Context, d *v1alpha1.Daemon) (*v1alpha1.Daemon, error) {
	return apply(ctx, c, v1alpha1.KindDaemon, d)
}

func (c *Client) UpdateDaemonStatus(ctx context.Context, d *v1alpha1.Daemon) (*v1alpha1.Daemon, error) {
	return updateStatus(ctx, c, v1alpha1.KindDaemon, d)
}

func (c *Client) DeleteDaemon(ctx context.Context, name string) error {
	return c.Delete(ctx, v1alpha1.KindDaemon, name)
}

func (c *Client) GetProc(ctx context.Context, name string) (*v1alpha1.Proc, error) {
	return get[v1alpha1.Proc](ctx, c, v1alpha1.KindProc, name)
}

func (c *Client) ListProcs(ctx context.Context) ([]v1alpha1.Proc, string, error) {
	return list[v1alpha1.Proc](ctx, c, v1alpha1.KindProc)
}

func (c *Client) ApplyProc(ctx context.Context, p *v1alpha1.Proc) (*v1alpha1.Proc, error) {
	return apply(ctx, c, v1alpha1.KindProc, p)
}

func (c *Client) UpdateProcStatus(ctx context.Context, p *v1alpha1.Proc) (*v1alpha1.Proc, error) {
	return updateStatus(ctx, c, v1alpha1.KindProc, p)
}

func (c *Client) DeleteProc(ctx context.Context, name string) error {
	return c.Delete(ctx, v1alpha1.KindProc, name)
}

func (c *Client) GetEvent(ctx context.Context, name string) (*v1alpha1.Event, error) {
	return get[v1alpha1.Event](ctx, c, v1alpha1.KindEvent, name)
}

func (c *Client) ListEvents(ctx context.Context) ([]v1alpha1.Event, string, error) {
	return list[v1alpha1.Event](ctx, c, v1alpha1.KindEvent)
}

func (c *Client) ApplyEvent(ctx context.Context, e *v1alpha1.Event) (*v1alpha1.Event, error) {
	return apply(ctx, c, v1alpha1.KindEvent, e)
}

func (c *Client) DeleteEvent(ctx context.Context, name string) error {
	return c.Delete(ctx, v1alpha1.KindEvent, name)
}

type object interface {
	v1alpha1.Daemon | v1alpha1.Proc | v1alpha1.Event
}

func get[T object](ctx context.Context, c *Client, kind, name string) (*T, error) {
	raw, err := c.GetRaw(ctx, kind, name)
	if err != nil {
		return nil, err
	}
	return decodeInto[T](raw)
}

func list[T object](ctx context.Context, c *Client, kind string) ([]T, string, error) {
	l, err := c.ListRaw(ctx, kind)
	if err != nil {
		return nil, "", err
	}
	items := make([]T, 0, len(l.Items))
	for _, raw := range l.Items {
		obj, err := decodeInto[T](raw)
		if err != nil {
			return nil, "", err
		}
		items = append(items, *obj)
	}
	return items, l.ResourceVersion, nil
}

func apply[T object](ctx context.Context, c *Client, kind string, obj *T) (*T, error) {
	stamped := *obj
	name := stampTypeMeta(&stamped, kind)
	body, err := json.Marshal(&stamped)
	if err != nil {
		return nil, fmt.Errorf("client: encoding %s: %w", kind, err)
	}
	raw, err := c.Apply(ctx, kind, name, body)
	if err != nil {
		return nil, err
	}
	return decodeInto[T](raw)
}

func updateStatus[T object](ctx context.Context, c *Client, kind string, obj *T) (*T, error) {
	stamped := *obj
	name := stampTypeMeta(&stamped, kind)
	body, err := json.Marshal(&stamped)
	if err != nil {
		return nil, fmt.Errorf("client: encoding %s: %w", kind, err)
	}
	raw, err := c.UpdateStatusRaw(ctx, kind, name, body)
	if err != nil {
		return nil, err
	}
	return decodeInto[T](raw)
}

// stampTypeMeta sets the canonical apiVersion/kind and returns the object's
// name.
func stampTypeMeta[T object](obj *T, kind string) string {
	switch o := any(obj).(type) {
	case *v1alpha1.Daemon:
		o.APIVersion, o.Kind = v1alpha1.APIVersion, kind
		return o.Metadata.Name
	case *v1alpha1.Proc:
		o.APIVersion, o.Kind = v1alpha1.APIVersion, kind
		return o.Metadata.Name
	case *v1alpha1.Event:
		o.APIVersion, o.Kind = v1alpha1.APIVersion, kind
		return o.Metadata.Name
	default:
		panic("unreachable: object constraint covers all kinds")
	}
}

func decodeInto[T object](raw json.RawMessage) (*T, error) {
	obj := new(T)
	if err := json.Unmarshal(raw, obj); err != nil {
		return nil, fmt.Errorf("client: decoding object: %w", err)
	}
	return obj, nil
}
