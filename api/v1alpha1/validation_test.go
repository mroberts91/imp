// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
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
			mutate:     func(d *Daemon) { d.Spec.UpdateStrategy.Type = "Canary" },
			wantFields: []string{"spec.updateStrategy.type"},
		},
		{
			name: "rollingUpdate ok",
			mutate: func(d *Daemon) {
				d.Spec.UpdateStrategy.Type = UpdateStrategyRollingUpdate
				d.Spec.UpdateStrategy.RollingUpdate = &RollingUpdateDaemonStrategy{Partition: new(int32(1))}
			},
			wantFields: nil,
		},
		{
			name: "rollingUpdate on Recreate",
			mutate: func(d *Daemon) {
				d.Spec.UpdateStrategy.Type = UpdateStrategyRecreate
				d.Spec.UpdateStrategy.RollingUpdate = &RollingUpdateDaemonStrategy{}
			},
			wantFields: []string{"spec.updateStrategy.rollingUpdate"},
		},
		{
			name: "negative partition",
			mutate: func(d *Daemon) {
				d.Spec.UpdateStrategy.Type = UpdateStrategyRollingUpdate
				d.Spec.UpdateStrategy.RollingUpdate = &RollingUpdateDaemonStrategy{Partition: new(int32(-1))}
			},
			wantFields: []string{"spec.updateStrategy.rollingUpdate.partition"},
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
		{
			name: "valid resources and probes",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Resources = ResourceRequirements{
					Limits: ResourceLimits{
						Memory:    "256Mi",
						CPUWeight: new(int64(100)),
						Pids:      new(int64(64)),
					},
				}
				d.Spec.Template.Spec.LivenessProbe = &Probe{
					Exec:             &ExecAction{Command: []string{"true"}},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 1,
					FailureThreshold: 3,
				}
				d.Spec.Template.Spec.ReadinessProbe = &Probe{
					HTTPGet: &HTTPGetAction{
						Path:   "/readyz",
						Port:   8080,
						Host:   "127.0.0.1",
						Scheme: URISchemeHTTP,
					},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 1,
					FailureThreshold: 3,
				}
			},
		},
		{
			name: "bad memory quantity",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Resources.Limits.Memory = "256M"
			},
			wantFields: []string{"spec.template.spec.resources.limits.memory"},
		},
		{
			name: "cpuWeight out of range",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Resources.Limits.CPUWeight = new(int64(0))
			},
			wantFields: []string{"spec.template.spec.resources.limits.cpuWeight"},
		},
		{
			name: "pids below 1",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Resources.Limits.Pids = new(int64(0))
			},
			wantFields: []string{"spec.template.spec.resources.limits.pids"},
		},
		{
			name: "probe missing handler",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.ReadinessProbe = &Probe{
					TimeoutSeconds: 1, PeriodSeconds: 10, SuccessThreshold: 1, FailureThreshold: 3,
				}
			},
			wantFields: []string{"spec.template.spec.readinessProbe"},
		},
		{
			name: "probe two handlers",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.ReadinessProbe = &Probe{
					Exec:             &ExecAction{Command: []string{"true"}},
					TCPSocket:        &TCPSocketAction{Port: 8080},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 1,
					FailureThreshold: 3,
				}
			},
			wantFields: []string{"spec.template.spec.readinessProbe"},
		},
		{
			name: "httpGet bad port",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.ReadinessProbe = &Probe{
					HTTPGet:          &HTTPGetAction{Port: 0},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 1,
					FailureThreshold: 3,
				}
			},
			wantFields: []string{"spec.template.spec.readinessProbe.httpGet.port"},
		},
		{
			name: "liveness successThreshold must be 1",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.LivenessProbe = &Probe{
					Exec:             &ExecAction{Command: []string{"true"}},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 2,
					FailureThreshold: 3,
				}
			},
			wantFields: []string{"spec.template.spec.livenessProbe.successThreshold"},
		},
		{
			name: "exec probe empty command",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.ReadinessProbe = &Probe{
					Exec:             &ExecAction{},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 1,
					FailureThreshold: 3,
				}
			},
			wantFields: []string{"spec.template.spec.readinessProbe.exec.command"},
		},
		{
			name: "bad cpu quantity",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Resources.Limits.CPU = "0.5"
			},
			wantFields: []string{"spec.template.spec.resources.limits.cpu"},
		},
		{
			name: "cpu millicores ok",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Resources.Limits.CPU = "500m"
			},
			wantFields: nil,
		},
		{
			name: "startup probe successThreshold must be 1",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.StartupProbe = &Probe{
					Exec:             &ExecAction{Command: []string{"true"}},
					TimeoutSeconds:   1,
					PeriodSeconds:    10,
					SuccessThreshold: 2,
					FailureThreshold: 3,
				}
			},
			wantFields: []string{"spec.template.spec.startupProbe.successThreshold"},
		},
		{
			name: "rlimits ok",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{
					{Resource: "nofile", Soft: new(int64(65536))},
					{Resource: "core", Soft: new(int64(0)), Hard: new(RlimitInfinity)},
				}
			},
			wantFields: nil,
		},
		{
			name: "rlimit unknown resource",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{{Resource: "NOFILE", Soft: new(int64(1))}}
			},
			wantFields: []string{"spec.template.spec.rlimits[0].resource"},
		},
		{
			name: "rlimit duplicate resource",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{
					{Resource: "nofile", Soft: new(int64(1))},
					{Resource: "nofile", Soft: new(int64(2))},
				}
			},
			wantFields: []string{"spec.template.spec.rlimits[1].resource"},
		},
		{
			name: "rlimit no values",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{{Resource: "nofile"}}
			},
			wantFields: []string{"spec.template.spec.rlimits[0]"},
		},
		{
			name: "rlimit soft above hard",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{
					{Resource: "nofile", Soft: new(int64(100)), Hard: new(int64(50))},
				}
			},
			wantFields: []string{"spec.template.spec.rlimits[0].soft"},
		},
		{
			name: "rlimit unlimited soft over finite hard",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{
					{Resource: "nofile", Soft: new(RlimitInfinity), Hard: new(int64(50))},
				}
			},
			wantFields: []string{"spec.template.spec.rlimits[0].soft"},
		},
		{
			name: "rlimit below -1",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Rlimits = []Rlimit{{Resource: "nofile", Soft: new(int64(-2))}}
			},
			wantFields: []string{"spec.template.spec.rlimits[0].soft"},
		},
		{
			name:       "nice out of range",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Nice = new(int32(20)) },
			wantFields: []string{"spec.template.spec.nice"},
		},
		{
			name:       "nice ok",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Nice = new(int32(-20)) },
			wantFields: nil,
		},
		{
			name:       "oomScoreAdjust out of range",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.OOMScoreAdjust = new(int32(1001)) },
			wantFields: []string{"spec.template.spec.oomScoreAdjust"},
		},
		{
			name:       "umask not octal",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Umask = new("088") },
			wantFields: []string{"spec.template.spec.umask"},
		},
		{
			name:       "umask too short",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Umask = new("07") },
			wantFields: []string{"spec.template.spec.umask"},
		},
		{
			name:       "umask ok",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Umask = new("0022") },
			wantFields: nil,
		},
		{
			name: "capabilities ok",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{
					Bounding: []string{"net_bind_service", "chown"},
					Ambient:  []string{"net_bind_service"},
				}
			},
			wantFields: nil,
		},
		{
			name: "capabilities ambient only ok",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{
					Ambient: []string{"net_bind_service"},
				}
			},
			wantFields: nil,
		},
		{
			name: "capabilities empty struct",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{}
			},
			wantFields: []string{"spec.template.spec.capabilities"},
		},
		{
			name: "capabilities explicit empty bounding",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{
					Bounding: []string{},
					Ambient:  []string{"net_bind_service"},
				}
			},
			wantFields: []string{"spec.template.spec.capabilities.bounding"},
		},
		{
			name: "capability unknown name",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{
					Bounding: []string{"CAP_NET_BIND_SERVICE"},
				}
			},
			wantFields: []string{"spec.template.spec.capabilities.bounding[0]"},
		},
		{
			name: "capability duplicate",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{
					Ambient: []string{"chown", "chown"},
				}
			},
			wantFields: []string{"spec.template.spec.capabilities.ambient[1]"},
		},
		{
			name: "ambient outside bounding",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Capabilities = &Capabilities{
					Bounding: []string{"net_bind_service"},
					Ambient:  []string{"chown"},
				}
			},
			wantFields: []string{"spec.template.spec.capabilities.ambient[0]"},
		},
		{
			name:       "config ref ok",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "app"}, {Name: "shared"}} },
			wantFields: nil,
		},
		{
			name:       "config ref empty",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Configs = []ConfigRef{{Name: ""}} },
			wantFields: []string{"spec.template.spec.configs[0]"},
		},
		{
			name:       "config ref uppercase",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "App"}} },
			wantFields: []string{"spec.template.spec.configs[0]"},
		},
		{
			name:       "config ref duplicate",
			mutate:     func(d *Daemon) { d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "app"}, {Name: "app"}} },
			wantFields: []string{"spec.template.spec.configs[1]"},
		},
		{
			name: "config ref path ok",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "app", Path: new("/etc/app")}}
			},
			wantFields: nil,
		},
		{
			name: "config ref path relative",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "app", Path: new("etc/app")}}
			},
			wantFields: []string{"spec.template.spec.configs[0].path"},
		},
		{
			name: "config ref path unclean",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "app", Path: new("/etc/app/")}}
			},
			wantFields: []string{"spec.template.spec.configs[0].path"},
		},
		{
			name: "config ref path root",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Configs = []ConfigRef{{Name: "app", Path: new("/")}}
			},
			wantFields: []string{"spec.template.spec.configs[0].path"},
		},
		{
			name: "config ref path collision",
			mutate: func(d *Daemon) {
				d.Spec.Template.Spec.Configs = []ConfigRef{
					{Name: "app", Path: new("/etc/shared")},
					{Name: "other", Path: new("/etc/shared")},
				}
			},
			wantFields: []string{"spec.template.spec.configs[1].path"},
		},
		{
			name:       "negative minReadySeconds",
			mutate:     func(d *Daemon) { d.Spec.MinReadySeconds = -1 },
			wantFields: []string{"spec.minReadySeconds"},
		},
		{
			name: "progressDeadline not above minReady",
			mutate: func(d *Daemon) {
				d.Spec.MinReadySeconds = 30
				d.Spec.ProgressDeadlineSeconds = new(int32(30))
			},
			wantFields: []string{"spec.progressDeadlineSeconds"},
		},
		{
			name: "minReady with deadline ok",
			mutate: func(d *Daemon) {
				d.Spec.MinReadySeconds = 5
				d.Spec.ProgressDeadlineSeconds = new(int32(600))
			},
			wantFields: nil,
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

func validConfig() *Config {
	return &Config{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindConfig},
		Metadata: ObjectMeta{Name: "app"},
		Spec:     ConfigSpec{Data: map[string]string{"app.conf": "listen 8080\n"}},
	}
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Config)
		wantFields []string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{
			name:       "missing name",
			mutate:     func(c *Config) { c.Metadata.Name = "" },
			wantFields: []string{"metadata.name"},
		},
		{
			name:       "nil data",
			mutate:     func(c *Config) { c.Spec.Data = nil },
			wantFields: []string{"spec.data"},
		},
		{
			name:       "empty data map",
			mutate:     func(c *Config) { c.Spec.Data = map[string]string{} },
			wantFields: []string{"spec.data"},
		},
		{
			name:       "filename with parent traversal",
			mutate:     func(c *Config) { c.Spec.Data = map[string]string{"../evil": "x"} },
			wantFields: []string{"spec.data[../evil]"},
		},
		{
			name:       "filename with slash",
			mutate:     func(c *Config) { c.Spec.Data = map[string]string{"sub/app.conf": "x"} },
			wantFields: []string{"spec.data[sub/app.conf]"},
		},
		{
			name:       "filename dot",
			mutate:     func(c *Config) { c.Spec.Data = map[string]string{".": "x"} },
			wantFields: []string{"spec.data[.]"},
		},
		{
			name: "too many files",
			mutate: func(c *Config) {
				c.Spec.Data = map[string]string{}
				for i := range maxConfigFiles + 1 {
					c.Spec.Data[fmt.Sprintf("f%d", i)] = "x"
				}
			},
			wantFields: []string{"spec.data"},
		},
		{
			name: "content too large",
			mutate: func(c *Config) {
				c.Spec.Data = map[string]string{"big.conf": strings.Repeat("a", maxConfigTotalSize+1)}
			},
			wantFields: []string{"spec.data"},
		},
		{
			name:       "bad mode",
			mutate:     func(c *Config) { c.Spec.Mode = new("0888") },
			wantFields: []string{"spec.mode"},
		},
		{
			name:       "mode special bits rejected",
			mutate:     func(c *Config) { c.Spec.Mode = new("2640") }, // setgid — silently dropped otherwise
			wantFields: []string{"spec.mode"},
		},
		{
			name:       "mode without owner read rejected",
			mutate:     func(c *Config) { c.Spec.Mode = new("0044") }, // proc could never read its own config
			wantFields: []string{"spec.mode"},
		},
		{
			name:       "mode ok",
			mutate:     func(c *Config) { c.Spec.Mode = new("0600") },
			wantFields: nil,
		},
		{
			name:       "mode 3-digit ok",
			mutate:     func(c *Config) { c.Spec.Mode = new("644") },
			wantFields: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			errs := ValidateConfig(c)
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
		})
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

func validTimer() *Timer {
	tm := &Timer{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindTimer},
		Metadata: ObjectMeta{Name: "backup"},
		Spec: TimerSpec{
			Schedule: "*/5 * * * *",
			Template: ProcTemplate{
				Spec: ProcTemplateSpec{Command: []string{"/usr/bin/backup"}},
			},
		},
	}
	DefaultTimer(tm)
	return tm
}

func TestValidateTimer(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Timer)
		wantFields []string
	}{
		{
			name:   "valid",
			mutate: func(*Timer) {},
		},
		{
			name:   "descriptor schedule",
			mutate: func(tm *Timer) { tm.Spec.Schedule = "@every 10s" },
		},
		{
			name:       "missing schedule",
			mutate:     func(tm *Timer) { tm.Spec.Schedule = "" },
			wantFields: []string{"spec.schedule"},
		},
		{
			name:       "unparseable schedule",
			mutate:     func(tm *Timer) { tm.Spec.Schedule = "every day at noon" },
			wantFields: []string{"spec.schedule"},
		},
		{
			name:       "six-field schedule rejected",
			mutate:     func(tm *Timer) { tm.Spec.Schedule = "0 0 12 * * *" },
			wantFields: []string{"spec.schedule"},
		},
		{
			name:       "unknown concurrency policy",
			mutate:     func(tm *Timer) { tm.Spec.ConcurrencyPolicy = "Queue" },
			wantFields: []string{"spec.concurrencyPolicy"},
		},
		{
			name:       "negative starting deadline",
			mutate:     func(tm *Timer) { tm.Spec.StartingDeadlineSeconds = new(int64(-1)) },
			wantFields: []string{"spec.startingDeadlineSeconds"},
		},
		{
			name: "negative history limits",
			mutate: func(tm *Timer) {
				tm.Spec.SuccessfulHistoryLimit = new(int32(-1))
				tm.Spec.FailedHistoryLimit = new(int32(-2))
			},
			wantFields: []string{"spec.successfulHistoryLimit", "spec.failedHistoryLimit"},
		},
		{
			name:       "restartPolicy Always rejected",
			mutate:     func(tm *Timer) { tm.Spec.Template.Spec.RestartPolicy = RestartPolicyAlways },
			wantFields: []string{"spec.template.spec.restartPolicy"},
		},
		{
			name:   "restartPolicy OnFailure ok",
			mutate: func(tm *Timer) { tm.Spec.Template.Spec.RestartPolicy = RestartPolicyOnFailure },
		},
		{
			name:       "missing command",
			mutate:     func(tm *Timer) { tm.Spec.Template.Spec.Command = nil },
			wantFields: []string{"spec.template.spec.command"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := validTimer()
			tc.mutate(tm)
			errs := ValidateTimer(tm)
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

func TestValidateLogRetention(t *testing.T) {
	d := validDaemon()
	d.Spec.Template.Spec.LogRetention = &LogRetention{
		MaxSizeMB:  new(int32(0)),
		MaxBackups: new(int32(-1)),
		MaxAgeDays: new(int32(-1)),
	}
	errs := ValidateDaemon(d)
	want := []string{
		"spec.template.spec.logRetention.maxSizeMB",
		"spec.template.spec.logRetention.maxBackups",
		"spec.template.spec.logRetention.maxAgeDays",
	}
	if len(errs) != len(want) {
		t.Fatalf("got %d errors, want %d: %v", len(errs), len(want), errs.ToAggregate())
	}
	for i, w := range want {
		if errs[i].Field != w {
			t.Errorf("error[%d].Field = %q, want %q", i, errs[i].Field, w)
		}
	}

	d = validDaemon()
	d.Spec.Template.Spec.LogRetention = &LogRetention{MaxSizeMB: new(int32(1))}
	if errs := ValidateDaemon(d); errs.ToAggregate() != nil {
		t.Errorf("valid logRetention rejected: %v", errs.ToAggregate())
	}
}

func validNotifier() *Notifier {
	n := &Notifier{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindNotifier},
		Metadata: ObjectMeta{Name: "default"},
		Spec: NotifierSpec{
			Template: ProcTemplate{
				Spec: ProcTemplateSpec{Command: []string{"/usr/local/bin/notify"}},
			},
		},
	}
	DefaultNotifier(n)
	return n
}

func TestValidateNotifier(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*Notifier)
		wantFields []string
	}{
		{
			name:   "valid",
			mutate: func(*Notifier) {},
		},
		{
			name:   "valid selector",
			mutate: func(n *Notifier) { n.Spec.Selector = "app=web,tier!=db" },
		},
		{
			name:       "malformed selector",
			mutate:     func(n *Notifier) { n.Spec.Selector = "app" },
			wantFields: []string{"spec.selector"},
		},
		{
			name:       "selector with empty key",
			mutate:     func(n *Notifier) { n.Spec.Selector = "=web" },
			wantFields: []string{"spec.selector"},
		},
		{
			name:       "negative cooldown",
			mutate:     func(n *Notifier) { n.Spec.CooldownSeconds = new(int32(-1)) },
			wantFields: []string{"spec.cooldownSeconds"},
		},
		{
			name:       "zero minRestarts",
			mutate:     func(n *Notifier) { n.Spec.MinRestarts = new(int32(0)) },
			wantFields: []string{"spec.minRestarts"},
		},
		{
			name:       "zero historyLimit",
			mutate:     func(n *Notifier) { n.Spec.HistoryLimit = new(int32(0)) },
			wantFields: []string{"spec.historyLimit"},
		},
		{
			// Unlike Timer, OnFailure is also rejected: the cooldown expiring
			// is the retry, never the run itself (M10-a2).
			name:       "restartPolicy OnFailure rejected",
			mutate:     func(n *Notifier) { n.Spec.Template.Spec.RestartPolicy = RestartPolicyOnFailure },
			wantFields: []string{"spec.template.spec.restartPolicy"},
		},
		{
			name:       "restartPolicy Always rejected",
			mutate:     func(n *Notifier) { n.Spec.Template.Spec.RestartPolicy = RestartPolicyAlways },
			wantFields: []string{"spec.template.spec.restartPolicy"},
		},
		{
			name:       "missing command",
			mutate:     func(n *Notifier) { n.Spec.Template.Spec.Command = nil },
			wantFields: []string{"spec.template.spec.command"},
		},
		{
			name:       "bad template label",
			mutate:     func(n *Notifier) { n.Spec.Template.Metadata.Labels = map[string]string{"-bad": "x"} },
			wantFields: []string{"spec.template.metadata.labels[-bad]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := validNotifier()
			tc.mutate(n)
			errs := ValidateNotifier(n)
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

func TestValidateFilesystemPolicy(t *testing.T) {
	const fsField = "spec.template.spec.filesystem"
	cases := []struct {
		name       string
		policy     *FilesystemPolicy
		wantFields []string
	}{
		{
			name: "strict with carve-outs",
			policy: &FilesystemPolicy{
				ReadOnlyRoot:   new(true),
				ReadWritePaths: []string{"/var/lib/app", "/var/log/app"},
			},
		},
		{
			name:   "protectHome only",
			policy: &FilesystemPolicy{ProtectHome: new(true)},
		},
		{
			name:       "empty block confines nothing",
			policy:     &FilesystemPolicy{},
			wantFields: []string{fsField},
		},
		{
			name:       "both explicitly false confines nothing",
			policy:     &FilesystemPolicy{ReadOnlyRoot: new(false), ProtectHome: new(false)},
			wantFields: []string{fsField},
		},
		{
			name: "readWritePaths without readOnlyRoot",
			policy: &FilesystemPolicy{
				ProtectHome:    new(true),
				ReadWritePaths: []string{"/var/lib/app"},
			},
			wantFields: []string{fsField + ".readWritePaths"},
		},
		{
			name: "relative path",
			policy: &FilesystemPolicy{
				ReadOnlyRoot:   new(true),
				ReadWritePaths: []string{"var/lib/app"},
			},
			wantFields: []string{fsField + ".readWritePaths[0]"},
		},
		{
			name: "unclean path",
			policy: &FilesystemPolicy{
				ReadOnlyRoot:   new(true),
				ReadWritePaths: []string{"/var/lib/../lib/app"},
			},
			wantFields: []string{fsField + ".readWritePaths[0]"},
		},
		{
			name: "root as carve-out",
			policy: &FilesystemPolicy{
				ReadOnlyRoot:   new(true),
				ReadWritePaths: []string{"/"},
			},
			wantFields: []string{fsField + ".readWritePaths[0]"},
		},
		{
			name: "duplicate carve-out",
			policy: &FilesystemPolicy{
				ReadOnlyRoot:   new(true),
				ReadWritePaths: []string{"/var/lib/app", "/var/lib/app"},
			},
			wantFields: []string{fsField + ".readWritePaths[1]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDaemon()
			d.Spec.Template.Spec.Filesystem = tc.policy
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
		})
	}
}
