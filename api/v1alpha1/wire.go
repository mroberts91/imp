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

// ProcStat is one Proc's point-in-time resource observation, served by
// GET /apis/impd.sh/v1alpha1/stats for `impctl top`. Observations, not
// objects: they never touch the store. CPUUsageUsec is cumulative — rates
// are computed by the consumer from two samples.
type ProcStat struct {
	Proc               string    `json:"proc"`
	Owner              ObjectRef `json:"owner,omitzero"`
	CPUUsageUsec       uint64    `json:"cpuUsageUsec"`
	MemoryCurrentBytes uint64    `json:"memoryCurrentBytes"`
	PidsCurrent        uint64    `json:"pidsCurrent"`
	// NrThrottled / ThrottledUsec are cumulative cpu.stat throttling counters
	// (M9-k): how many enforcement periods the cgroup was throttled, and for
	// how long in total. Best-effort — 0 when the kernel omits the lines.
	NrThrottled   uint64 `json:"nrThrottled"`
	ThrottledUsec uint64 `json:"throttledUsec"`
	SampledAt     Time   `json:"sampledAt,omitzero"`
}

// StatsList is the wire shape of the /stats response.
type StatsList struct {
	Items []ProcStat `json:"items"`
}
