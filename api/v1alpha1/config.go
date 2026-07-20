// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Config is declared file content for processes (k8s analog: ConfigMap,
// consumed at spawn; participates in rollout identity first-class). A Daemon
// or Timer template references Configs by name via ProcTemplateSpec.Configs;
// execd materializes their files under IMP_CONFIG_DIR before spawn, and the
// referenced content joins the Proc's revision identity so a config edit rolls
// the Daemon exactly like a template edit (M8-b).
type Config struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     ConfigSpec `json:"spec"`
	// No status: nothing observes into a Config (M8-i).
}

// ConfigSpec is the desired state of a Config.
type ConfigSpec struct {
	// Data maps filename → inline file content. Each filename is a single
	// path component (no separators, not "." or ".."). Content is UTF-8 text
	// by YAML's nature; binary payloads are out of scope for M8.
	Data map[string]string `json:"data"`
	// Mode is the octal file mode applied to every materialized file, e.g.
	// "0600". Nil means 0644, resolved by execd at materialization — never
	// defaulted here, so a Config's content hash is stable across upgrades
	// that change defaults (same posture as ProcTemplateSpec.LogRetention).
	Mode *string `json:"mode,omitempty"`
}
