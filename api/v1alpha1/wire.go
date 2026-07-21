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

// ServerInfo is the wire shape of GET /info: the running impd's effective
// configuration — resolved flag values plus live facts (the bound metrics
// address, whether the cgroup root is kernel-enforced) that reading the
// unit file cannot answer. impd is flags-only, so this endpoint is the one
// place an operator can ask a live daemon what it is actually using.
type ServerInfo struct {
	Version VersionInfo `json:"version"`

	Socket      string `json:"socket"`
	DataDir     string `json:"dataDir"`
	ManifestDir string `json:"manifestDir"`
	// LogDir is where per-Proc process logs are written (under DataDir).
	LogDir string `json:"logDir"`
	// ConfigDir is the Config materialization base: each Proc's rendered
	// config files live under it, exposed to children as IMP_CONFIG_DIR.
	ConfigDir string `json:"configDir"`

	CgroupRoot string `json:"cgroupRoot"`
	// CgroupKernelEnforced is false when the cgroup root is a plain
	// directory (the ad-hoc/rootless fake root): limits are accepted but
	// not enforced by the kernel.
	CgroupKernelEnforced bool `json:"cgroupKernelEnforced"`

	// MetricsAddr is the bound listen address, empty when disabled.
	MetricsAddr         string `json:"metricsAddr,omitempty"`
	LogLevel            string `json:"logLevel"`
	EventTTLSeconds     int    `json:"eventTTLSeconds"`
	KillProcsOnShutdown bool   `json:"killProcsOnShutdown"`
	SocketGroup         string `json:"socketGroup,omitempty"`

	// Privileged reports whether impd runs as root (per-Proc users,
	// sandboxing, and kernel limits available).
	Privileged bool `json:"privileged"`
	PID        int  `json:"pid"`
	StartedAt  Time `json:"startedAt,omitzero"`
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
