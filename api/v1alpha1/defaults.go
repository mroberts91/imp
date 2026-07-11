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
}
