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
	if d.Spec.ProgressDeadlineSeconds == nil || *d.Spec.ProgressDeadlineSeconds != DefaultProgressDeadlineSeconds {
		t.Errorf("ProgressDeadlineSeconds = %v, want %d", d.Spec.ProgressDeadlineSeconds, DefaultProgressDeadlineSeconds)
	}
	if d.Spec.MinReadySeconds != 0 {
		t.Errorf("MinReadySeconds = %d, want 0 (no default)", d.Spec.MinReadySeconds)
	}
	// M6 template fields must stay nil (hash stability — pre-M6 Daemons
	// must not roll on upgrade).
	if tpl.StartupProbe != nil || tpl.Rlimits != nil || tpl.Nice != nil ||
		tpl.OOMScoreAdjust != nil || tpl.Umask != nil {
		t.Error("defaulting materialized an M6 template field")
	}
}

func TestDefaultDaemonStartupProbe(t *testing.T) {
	d := &Daemon{
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			Template: ProcTemplate{Spec: ProcTemplateSpec{
				Command:      []string{"/bin/true"},
				StartupProbe: &Probe{Exec: &ExecAction{Command: []string{"/bin/started"}}},
			}},
		},
	}
	DefaultDaemon(d)
	sp := d.Spec.Template.Spec.StartupProbe
	if sp.PeriodSeconds != DefaultProbePeriodSeconds || sp.FailureThreshold != DefaultProbeFailureThreshold {
		t.Errorf("startupProbe inner defaults not applied: period=%d failureThreshold=%d", sp.PeriodSeconds, sp.FailureThreshold)
	}
}

func TestDefaultDaemonRollingUpdate(t *testing.T) {
	d := &Daemon{
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			UpdateStrategy: UpdateStrategy{Type: UpdateStrategyRollingUpdate},
			Template:       ProcTemplate{Spec: ProcTemplateSpec{Command: []string{"/bin/true"}}},
		},
	}
	DefaultDaemon(d)
	ru := d.Spec.UpdateStrategy.RollingUpdate
	if ru == nil || ru.Partition == nil || *ru.Partition != 0 {
		t.Errorf("RollingUpdate = %+v, want partition 0", ru)
	}

	d2 := &Daemon{
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			UpdateStrategy: UpdateStrategy{
				Type:          UpdateStrategyRollingUpdate,
				RollingUpdate: &RollingUpdateDaemonStrategy{Partition: new(int32(2))},
			},
			Template: ProcTemplate{Spec: ProcTemplateSpec{Command: []string{"/bin/true"}}},
		},
	}
	DefaultDaemon(d2)
	if *d2.Spec.UpdateStrategy.RollingUpdate.Partition != 2 {
		t.Errorf("explicit partition overwritten to %d", *d2.Spec.UpdateStrategy.RollingUpdate.Partition)
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

func TestDefaultProbe(t *testing.T) {
	d := &Daemon{
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			Template: ProcTemplate{Spec: ProcTemplateSpec{
				Command: []string{"/bin/true"},
				LivenessProbe: &Probe{
					HTTPGet: &HTTPGetAction{Port: 8080},
				},
				ReadinessProbe: &Probe{
					TCPSocket: &TCPSocketAction{Port: 8080},
				},
			}},
		},
	}
	DefaultDaemon(d)

	lp := d.Spec.Template.Spec.LivenessProbe
	if lp.TimeoutSeconds != DefaultProbeTimeoutSeconds ||
		lp.PeriodSeconds != DefaultProbePeriodSeconds ||
		lp.SuccessThreshold != DefaultProbeSuccessThreshold ||
		lp.FailureThreshold != DefaultProbeFailureThreshold {
		t.Errorf("liveness timing defaults: %+v", lp)
	}
	if lp.HTTPGet.Host != DefaultHTTPGetHost || lp.HTTPGet.Scheme != URISchemeHTTP {
		t.Errorf("httpGet defaults: %+v", lp.HTTPGet)
	}

	rp := d.Spec.Template.Spec.ReadinessProbe
	if rp.TCPSocket.Host != DefaultTCPSocketHost {
		t.Errorf("tcpSocket host = %q, want %q", rp.TCPSocket.Host, DefaultTCPSocketHost)
	}
}

func TestDefaultProbePreservesSetValues(t *testing.T) {
	p := &Probe{
		Exec:                &ExecAction{Command: []string{"true"}},
		InitialDelaySeconds: 5,
		TimeoutSeconds:      2,
		PeriodSeconds:       15,
		SuccessThreshold:    1,
		FailureThreshold:    5,
	}
	DefaultProbe(p)
	if p.InitialDelaySeconds != 5 || p.TimeoutSeconds != 2 || p.PeriodSeconds != 15 ||
		p.SuccessThreshold != 1 || p.FailureThreshold != 5 {
		t.Errorf("set probe values overwritten: %+v", p)
	}
}

func TestDefaultTimer(t *testing.T) {
	tm := &Timer{
		Metadata: ObjectMeta{Name: "backup"},
		Spec: TimerSpec{
			Schedule: "@hourly",
			Template: ProcTemplate{Spec: ProcTemplateSpec{Command: []string{"/usr/bin/backup"}}},
		},
	}
	DefaultTimer(tm)

	if tm.Spec.Suspend == nil || *tm.Spec.Suspend {
		t.Errorf("Suspend = %v, want false", tm.Spec.Suspend)
	}
	if tm.Spec.ConcurrencyPolicy != ConcurrencyForbid {
		t.Errorf("ConcurrencyPolicy = %q, want Forbid", tm.Spec.ConcurrencyPolicy)
	}
	if v := tm.Spec.SuccessfulHistoryLimit; v == nil || *v != 3 {
		t.Errorf("SuccessfulHistoryLimit = %v, want 3", v)
	}
	if v := tm.Spec.FailedHistoryLimit; v == nil || *v != 1 {
		t.Errorf("FailedHistoryLimit = %v, want 1", v)
	}
	if tm.Spec.Template.Spec.RestartPolicy != RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never (not the daemon-shaped Always)", tm.Spec.Template.Spec.RestartPolicy)
	}
	if tm.Spec.Template.Spec.StopSignal != DefaultStopSignal {
		t.Errorf("StopSignal = %q, want %q", tm.Spec.Template.Spec.StopSignal, DefaultStopSignal)
	}
}

func TestDefaultTimerPreservesSetValues(t *testing.T) {
	tm := &Timer{
		Metadata: ObjectMeta{Name: "backup"},
		Spec: TimerSpec{
			Schedule:          "@hourly",
			ConcurrencyPolicy: ConcurrencyAllow,
			Template: ProcTemplate{Spec: ProcTemplateSpec{
				Command:       []string{"/usr/bin/backup"},
				RestartPolicy: RestartPolicyOnFailure,
			}},
		},
	}
	DefaultTimer(tm)
	if tm.Spec.ConcurrencyPolicy != ConcurrencyAllow {
		t.Errorf("explicit ConcurrencyPolicy overwritten to %q", tm.Spec.ConcurrencyPolicy)
	}
	if tm.Spec.Template.Spec.RestartPolicy != RestartPolicyOnFailure {
		t.Errorf("explicit RestartPolicy overwritten to %q", tm.Spec.Template.Spec.RestartPolicy)
	}
}
