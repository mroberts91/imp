// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "encoding/json"

// ObjectList is the wire shape of a list response: the items plus the
// resourceVersion the snapshot is consistent at.
type ObjectList struct {
	ResourceVersion string            `json:"resourceVersion"`
	Items           []json.RawMessage `json:"items"`
}

type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Branch    string `json:"branch,omitempty"`
	BuildTime string `json:"buildTime,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
}
