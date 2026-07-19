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
)

func DefaultDaemon(d *Daemon) {
	if d.Spec.Replicas == nil {
		d.Spec.Replicas = new(defaultReplicas)
	}
	if d.Spec.UpdateStrategy.Type == "" {
		d.Spec.UpdateStrategy.Type = UpdateStrategyRecreate
	}
	defaultProcTemplateSpec(&d.Spec.Template.Spec)
}

func DefaultProc(p *Proc) {
	defaultProcTemplateSpec(&p.Spec)
}

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
