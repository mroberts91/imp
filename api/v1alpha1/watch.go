// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "encoding/json"

// The watch event vocabulary mirrors Kubernetes'
// staging/src/k8s.io/apimachinery/pkg/watch/watch.go (Copyright The
// Kubernetes Authors, Apache-2.0).

type WatchEventType string

const (
	WatchAdded    WatchEventType = "ADDED"
	WatchModified WatchEventType = "MODIFIED"
	WatchDeleted  WatchEventType = "DELETED"
	WatchError    WatchEventType = "ERROR"
)

type WatchEvent struct {
	Type   WatchEventType  `json:"type"`
	Object json.RawMessage `json:"object"`
}
