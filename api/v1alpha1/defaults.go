// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Defaulting functions are invoked only by the api-server, always before
// validation, so every write path gets identical treatment. The controller
// copies a Daemon's already-defaulted template into the ProcSpecs it creates.
// execd never defaults anything.

const (
	DefaultStopSignal                    = "TERM"
	DefaultTerminationGracePeriodSeconds = int64(30)
	defaultReplicas                      = int32(1)

	// Probe defaults match kubelet SetDefaults_Probe
	// (pkg/apis/core/v1/defaults.go).
	DefaultProbeTimeoutSeconds   int32 = 1
	DefaultProbePeriodSeconds    int32 = 10
	DefaultProbeSuccessThreshold int32 = 1
	DefaultProbeFailureThreshold int32 = 3

	DefaultHTTPGetHost   = "127.0.0.1"
	DefaultTCPSocketHost = "127.0.0.1"

	// DefaultProgressDeadlineSeconds matches the Deployment default.
	// Materialized (DaemonSpec is outside the hashed template).
	DefaultProgressDeadlineSeconds = int32(600)

	// Log retention built-ins (doc 08 M5-d). Resolved by execd when a
	// Proc's logRetention (or one of its fields) is nil — deliberately NOT
	// materialized by defaulting, so pre-M5 template hashes stay stable.
	DefaultLogMaxSizeMB  = int32(10)
	DefaultLogMaxBackups = int32(3)
	DefaultLogMaxAgeDays = int32(0)

	defaultSuccessfulHistoryLimit = int32(3)
	defaultFailedHistoryLimit     = int32(1)

	// Notifier defaults (M10-a1). Cooldown matches ft's 30-minute
	// per-event alert cooldown; minRestarts 3 means a crash-loop pages on
	// a loop, not a blip (and absorbs boot-time network races).
	defaultNotifierCooldownSeconds = int32(1800)
	defaultNotifierMinRestarts     = int32(3)
	defaultNotifierHistoryLimit    = int32(20)
)

func DefaultDaemon(d *Daemon) {
	if d.Spec.Replicas == nil {
		d.Spec.Replicas = new(defaultReplicas)
	}
	if d.Spec.UpdateStrategy.Type == "" {
		d.Spec.UpdateStrategy.Type = UpdateStrategyRecreate
	}
	if d.Spec.UpdateStrategy.Type == UpdateStrategyRollingUpdate {
		if d.Spec.UpdateStrategy.RollingUpdate == nil {
			d.Spec.UpdateStrategy.RollingUpdate = &RollingUpdateDaemonStrategy{}
		}
		if d.Spec.UpdateStrategy.RollingUpdate.Partition == nil {
			d.Spec.UpdateStrategy.RollingUpdate.Partition = new(int32(0))
		}
	}
	if d.Spec.ProgressDeadlineSeconds == nil {
		d.Spec.ProgressDeadlineSeconds = new(DefaultProgressDeadlineSeconds)
	}
	defaultProcTemplateSpec(&d.Spec.Template.Spec)
}

func DefaultProc(p *Proc) {
	defaultProcTemplateSpec(&p.Spec)
}

func DefaultTimer(t *Timer) {
	if t.Spec.Suspend == nil {
		t.Spec.Suspend = new(false)
	}
	if t.Spec.ConcurrencyPolicy == "" {
		t.Spec.ConcurrencyPolicy = ConcurrencyForbid
	}
	if t.Spec.SuccessfulHistoryLimit == nil {
		t.Spec.SuccessfulHistoryLimit = new(defaultSuccessfulHistoryLimit)
	}
	if t.Spec.FailedHistoryLimit == nil {
		t.Spec.FailedHistoryLimit = new(defaultFailedHistoryLimit)
	}
	// Timer runs are run-to-completion: the daemon-shaped Always default
	// is wrong here, so pick Never before the shared defaulting runs.
	if t.Spec.Template.Spec.RestartPolicy == "" {
		t.Spec.Template.Spec.RestartPolicy = RestartPolicyNever
	}
	defaultProcTemplateSpec(&t.Spec.Template.Spec)
}

func DefaultNotifier(n *Notifier) {
	if n.Spec.CooldownSeconds == nil {
		n.Spec.CooldownSeconds = new(defaultNotifierCooldownSeconds)
	}
	if n.Spec.MinRestarts == nil {
		n.Spec.MinRestarts = new(defaultNotifierMinRestarts)
	}
	if n.Spec.HistoryLimit == nil {
		n.Spec.HistoryLimit = new(defaultNotifierHistoryLimit)
	}
	// A notification is one shot: if the failure level persists, the
	// cooldown expiring re-fires it — that is the retry (M10-a2). Pick
	// Never before the shared defaulting's daemon-shaped Always runs.
	if n.Spec.Template.Spec.RestartPolicy == "" {
		n.Spec.Template.Spec.RestartPolicy = RestartPolicyNever
	}
	defaultProcTemplateSpec(&n.Spec.Template.Spec)
}

// DefaultConfig applies no defaults (M8-e): a Config's content and mode are
// taken verbatim. Mode resolution happens in execd at materialization, never
// here, so a Config's content hash stays stable across upgrades that change
// defaults. Present for symmetry with the other Default* functions and the
// apiserver's per-kind dispatch.
func DefaultConfig(c *Config) {}

func defaultProcTemplateSpec(s *ProcTemplateSpec) {
	if s.RestartPolicy == "" {
		s.RestartPolicy = RestartPolicyAlways
	}
	if s.StopSignal == "" {
		s.StopSignal = DefaultStopSignal
	}
	if s.TerminationGracePeriodSeconds == nil {
		s.TerminationGracePeriodSeconds = new(DefaultTerminationGracePeriodSeconds)
	}
	if s.LivenessProbe != nil {
		DefaultProbe(s.LivenessProbe)
	}
	if s.ReadinessProbe != nil {
		DefaultProbe(s.ReadinessProbe)
	}
	// StartupProbe inner defaults materialize only when the probe is set —
	// such a Daemon rolls anyway. Absent stays nil (hash stability).
	if s.StartupProbe != nil {
		DefaultProbe(s.StartupProbe)
	}
}

// DefaultProbe fills zero timing fields and http/tcp host/scheme defaults.
// Timing defaults match kubelet SetDefaults_Probe.
func DefaultProbe(p *Probe) {
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = DefaultProbeTimeoutSeconds
	}
	if p.PeriodSeconds == 0 {
		p.PeriodSeconds = DefaultProbePeriodSeconds
	}
	if p.SuccessThreshold == 0 {
		p.SuccessThreshold = DefaultProbeSuccessThreshold
	}
	if p.FailureThreshold == 0 {
		p.FailureThreshold = DefaultProbeFailureThreshold
	}
	if p.HTTPGet != nil {
		if p.HTTPGet.Host == "" {
			p.HTTPGet.Host = DefaultHTTPGetHost
		}
		if p.HTTPGet.Scheme == "" {
			p.HTTPGet.Scheme = URISchemeHTTP
		}
	}
	if p.TCPSocket != nil && p.TCPSocket.Host == "" {
		p.TCPSocket.Host = DefaultTCPSocketHost
	}
}
