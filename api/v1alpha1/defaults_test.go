// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

func TestDefaultDaemon(t *testing.T) {
	d := &Daemon{
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			Template: ProcTemplate{Spec: ProcTemplateSpec{Command: []string{"/bin/true"}}},
		},
	}
	DefaultDaemon(d)

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("Replicas = %v, want 1", d.Spec.Replicas)
	}
	if d.Spec.UpdateStrategy.Type != UpdateStrategyRecreate {
		t.Errorf("UpdateStrategy.Type = %q, want %q", d.Spec.UpdateStrategy.Type, UpdateStrategyRecreate)
	}
	tpl := d.Spec.Template.Spec
	if tpl.RestartPolicy != RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want %q", tpl.RestartPolicy, RestartPolicyAlways)
	}
	if tpl.StopSignal != DefaultStopSignal {
		t.Errorf("StopSignal = %q, want %q", tpl.StopSignal, DefaultStopSignal)
	}
	if tpl.TerminationGracePeriodSeconds == nil || *tpl.TerminationGracePeriodSeconds != DefaultTerminationGracePeriodSeconds {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", tpl.TerminationGracePeriodSeconds, DefaultTerminationGracePeriodSeconds)
	}
}

func TestDefaultDaemonPreservesSetValues(t *testing.T) {
	d := &Daemon{
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			Replicas:       new(int32(0)),
			UpdateStrategy: UpdateStrategy{Type: UpdateStrategyRecreate},
			Template: ProcTemplate{Spec: ProcTemplateSpec{
				Command:                       []string{"/bin/true"},
				RestartPolicy:                 RestartPolicyNever,
				StopSignal:                    "INT",
				TerminationGracePeriodSeconds: new(int64(0)),
			}},
		},
	}
	DefaultDaemon(d)

	if *d.Spec.Replicas != 0 {
		t.Errorf("explicit 0 replicas overwritten to %d", *d.Spec.Replicas)
	}
	tpl := d.Spec.Template.Spec
	if tpl.RestartPolicy != RestartPolicyNever || tpl.StopSignal != "INT" || *tpl.TerminationGracePeriodSeconds != 0 {
		t.Errorf("set values overwritten: %+v", tpl)
	}
}

func TestDefaultProc(t *testing.T) {
	p := &Proc{
		Metadata: ObjectMeta{Name: "web-0-abc"},
		Spec:     ProcSpec{Command: []string{"/bin/true"}},
	}
	DefaultProc(p)

	if p.Spec.RestartPolicy != RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want %q", p.Spec.RestartPolicy, RestartPolicyAlways)
	}
	if p.Spec.StopSignal != DefaultStopSignal {
		t.Errorf("StopSignal = %q, want %q", p.Spec.StopSignal, DefaultStopSignal)
	}
	if p.Spec.TerminationGracePeriodSeconds == nil || *p.Spec.TerminationGracePeriodSeconds != DefaultTerminationGracePeriodSeconds {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", p.Spec.TerminationGracePeriodSeconds, DefaultTerminationGracePeriodSeconds)
	}
}
