// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
)

// validDaemon returns a minimal Daemon that passes
// validation after defaulting.
func validDaemon() *Daemon {
	d := &Daemon{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindDaemon},
		Metadata: ObjectMeta{Name: "web"},
		Spec: DaemonSpec{
			Template: ProcTemplate{
				Spec: ProcTemplateSpec{Command: []string{"/usr/bin/serve"}},
			},
		},
	}
	DefaultDaemon(d)
	return d
}

func TestValidateDaemon(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Daemon)
		wantFields []string
	}{
		{
			name:   "valid",
			mutate: func(*Daemon) {},
		},
		{
			name:       "missing name",
			mutate:     func(d *Daemon) { d.Metadata.Name = "" },
			wantFields: []string{"metadata.name"},
		},
		{
			name:       "uppercase name",
			mutate:     func(d *Daemon) { d.Metadata.Name = "Web" },
			wantFields: []string{"metadata.name"},
		},
		{
			name:       "name too long",
			mutate:     func(d *Daemon) { d.Metadata.Name = strings.Repeat("a", 254) },
			wantFields: []string{"metadata.name"},
		},
		{
			name:       "name with trailing dash",
			mutate:     func(d *Daemon) { d.Metadata.Name = "web-" },
			wantFields: []string{"metadata.name"},
		},
		{
			name:       "dotted name ok",
			mutate:     func(d *Daemon) { d.Metadata.Name = "web.example.com" },
			wantFields: nil,
		},
		{
			name:       "bad metadata label key",
			mutate:     func(d *Daemon) { d.Metadata.Labels = map[string]string{"-bad": "x"} },
			wantFields: []string{"metadata.labels[-bad]"},
		},
		{
			name:       "bad metadata label value",
			mutate:     func(d *Daemon) { d.Metadata.Labels = map[string]string{"app": "bad value"} },
			wantFields: []string{"metadata.labels[app]"},
		},
		{
			name:       "prefixed label key ok",
			mutate:     func(d *Daemon) { d.Metadata.Labels = map[string]string{"impd.sh/daemon-name": "web"} },
			wantFields: nil,
		},
		{
			name:       "label key with empty prefix",
			mutate:     func(d *Daemon) { d.Metadata.Labels = map[string]string{"/name": "x"} },
			wantFields: []string{"metadata.labels[/name]"},
		},
		{
			name:       "label key with two slashes",
			mutate:     func(d *Daemon) { d.Metadata.Labels = map[string]string{"a.b/c/d": "x"} },
			wantFields: []string{"metadata.labels[a.b/c/d]"},
		},
		{
			name:       "nil replicas",
			mutate:     func(d *Daemon) { d.Spec.Replicas = nil },
			wantFields: []string{"spec.replicas"},
		},
		{
			name:       "negative replicas",
			mutate:     func(d *Daemon) { d.Spec.Replicas = new(int32(-1)) },
			wantFields: []string{"spec.replicas"},
		},
		{
			name:       "zero replicas ok",
			mutate:     func(d *Daemon) { d.Spec.Replicas = new(int32(0)) },
			wantFields: nil,
		},
		{
			name:       "unknown update strategy",
			mutate:     func(d *Daemon) { d.Spec.UpdateStrategy.Type = "RollingUpdate" },
			wantFields: []string{"spec.updateStrategy.type"},
		},
		{
			name:       "empty command",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Command = nil },
			wantFields: []string{"spec.template.spec.command"},
		},
		{
			name:       "empty executable",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Command = []string{""} },
			wantFields: []string{"spec.template.spec.command[0]"},
		},
		{
			name: "env without name",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Env = []EnvVar{{Name: "OK"}, {Value: "orphan"}}
			},
			wantFields: []string{"spec.template.spec.env[1].name"},
		},
		{
			name:       "unknown restart policy",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.RestartPolicy = "Sometimes" },
			wantFields: []string{"spec.template.spec.restartPolicy"},
		},
		{
			name:       "unknown stop signal",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.StopSignal = "BOGUS" },
			wantFields: []string{"spec.template.spec.stopSignal"},
		},
		{
			name:       "SIG-prefixed stop signal ok",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.StopSignal = "SIGINT" },
			wantFields: nil,
		},
		{
			name: "negative grace period",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.TerminationGracePeriodSeconds = new(int64(-1))
			},
			wantFields: []string{"spec.template.spec.terminationGracePeriodSeconds"},
		},
		{
			name:       "bad template label",
			mutate:     func(d *Daemon) { d.Spec.Template.Metadata.Labels = map[string]string{"bad key": "x"} },
			wantFields: []string{"spec.template.metadata.labels[bad key]"},
		},
		{
			name: "multiple errors accumulate",
			mutate: func(d *Daemon) {
				d.Metadata.Name = "Bad"
				d.Spec.Replicas = new(int32(-2))
				d.Spec.Template.Spec.Command = nil
			},
			wantFields: []string{"metadata.name", "spec.replicas", "spec.template.spec.command"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDaemon()
			tc.mutate(d)
			errs := ValidateDaemon(d)
			var gotFields []string
			for _, e := range errs {
				gotFields = append(gotFields, e.Field)
			}
			if len(gotFields) != len(tc.wantFields) {
				t.Fatalf("got %d errors %v, want fields %v\nerrors: %v",
					len(errs), gotFields, tc.wantFields, errs.ToAggregate())
			}
			for i, want := range tc.wantFields {
				if gotFields[i] != want {
					t.Errorf("error[%d].Field = %q, want %q", i, gotFields[i], want)
				}
			}
			if len(tc.wantFields) == 0 && errs.ToAggregate() != nil {
				t.Errorf("ToAggregate() = %v, want nil", errs.ToAggregate())
			}
		})
	}
}

func TestValidateProc(t *testing.T) {
	p := &Proc{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindProc},
		Metadata: ObjectMeta{Name: "web-0-9f86d081"},
		Spec:     ProcSpec{Command: []string{"/usr/bin/serve"}},
	}
	DefaultProc(p)
	if errs := ValidateProc(p); len(errs) != 0 {
		t.Errorf("valid Proc rejected: %v", errs.ToAggregate())
	}

	p.Spec.Command = nil
	p.Metadata.Name = ""
	errs := ValidateProc(p)
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want 2: %v", len(errs), errs.ToAggregate())
	}
	if errs[0].Field != "metadata.name" || errs[1].Field != "spec.command" {
		t.Errorf("fields = %q, %q", errs[0].Field, errs[1].Field)
	}
}

func TestValidateEvent(t *testing.T) {
	e := fullEvent()
	if errs := ValidateEvent(e); len(errs) != 0 {
		t.Errorf("valid Event rejected: %v", errs.ToAggregate())
	}

	bad := &Event{Metadata: ObjectMeta{Name: "x"}, Type: "Fatal"}
	errs := ValidateEvent(bad)
	wantFields := []string{"regarding.kind", "regarding.name", "type", "reason"}
	if len(errs) != len(wantFields) {
		t.Fatalf("got %d errors, want %d: %v", len(errs), len(wantFields), errs.ToAggregate())
	}
	for i, want := range wantFields {
		if errs[i].Field != want {
			t.Errorf("error[%d].Field = %q, want %q", i, errs[i].Field, want)
		}
	}
}

func TestFieldErrorRendering(t *testing.T) {
	cases := []struct {
		err  *FieldError
		want string
	}{
		{
			requiredErr(NewPath("spec").Child("command"), ""),
			"spec.command: Required value",
		},
		{
			invalidErr(NewPath("spec").Child("replicas"), int32(-1), "must be greater than or equal to 0"),
			`spec.replicas: Invalid value: "-1": must be greater than or equal to 0`,
		},
		{
			notSupportedErr(NewPath("spec").Child("restartPolicy"), "Sometimes", []string{"Always", "OnFailure", "Never"}),
			`spec.restartPolicy: Unsupported value: "Sometimes": supported values: Always, OnFailure, Never`,
		},
		{
			invalidErr(NewPath("spec").Child("env").Index(2).Child("name"), "", "detail"),
			`spec.env[2].name: Invalid value: "": detail`,
		},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
