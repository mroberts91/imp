// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"regexp"
	"testing"
)

func templateForHash() *ProcTemplate {
	return &ProcTemplate{
		Metadata: TemplateMeta{
			Labels: map[string]string{"app": "web", "tier": "frontend", "zone": "a"},
		},
		Spec: ProcTemplateSpec{
			Command:                       []string{"/usr/bin/serve", "--port", "8080"},
			Env:                           []EnvVar{{Name: "MODE", Value: "prod"}},
			RestartPolicy:                 RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(DefaultTerminationGracePeriodSeconds),
		},
	}
}

func TestHashProcTemplateDeterminism(t *testing.T) {
	first := HashProcTemplate(templateForHash())
	for range 100 {
		if got := HashProcTemplate(templateForHash()); got != first {
			t.Fatalf("hash not deterministic: %q != %q", got, first)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(first) {
		t.Errorf("hash %q is not 8 lowercase hex characters", first)
	}
}

func TestHashProcTemplateGolden(t *testing.T) {
	const golden = "8233565f"
	if got := HashProcTemplate(templateForHash()); got != golden {
		t.Errorf("HashProcTemplate = %q, want %q (template serialization changed?)", got, golden)
	}
}

func TestHashProcTemplateSensitivity(t *testing.T) {
	base := HashProcTemplate(templateForHash())

	changed := templateForHash()
	changed.Spec.Command = append(changed.Spec.Command, "--verbose")
	if HashProcTemplate(changed) == base {
		t.Error("command change did not change hash")
	}

	relabeled := templateForHash()
	relabeled.Metadata.Labels["app"] = "api"
	if HashProcTemplate(relabeled) == base {
		t.Error("label change did not change hash")
	}

	regraced := templateForHash()
	*regraced.Spec.TerminationGracePeriodSeconds = 60
	if HashProcTemplate(regraced) == base {
		t.Error("grace period change did not change hash")
	}

	withProbe := templateForHash()
	withProbe.Spec.ReadinessProbe = &Probe{
		TCPSocket:        &TCPSocketAction{Port: 8080},
		TimeoutSeconds:   1,
		PeriodSeconds:    10,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	if HashProcTemplate(withProbe) == base {
		t.Error("readinessProbe addition did not change hash")
	}

	withLimits := templateForHash()
	withLimits.Spec.Resources.Limits.Memory = "128Mi"
	if HashProcTemplate(withLimits) == base {
		t.Error("resources.limits.memory addition did not change hash")
	}

	withRlimits := templateForHash()
	withRlimits.Spec.Rlimits = []Rlimit{{Resource: "nofile", Soft: new(int64(65536))}}
	if HashProcTemplate(withRlimits) == base {
		t.Error("rlimits addition did not change hash")
	}

	withNice := templateForHash()
	withNice.Spec.Nice = new(int32(5))
	if HashProcTemplate(withNice) == base {
		t.Error("nice addition did not change hash")
	}

	withStartup := templateForHash()
	withStartup.Spec.StartupProbe = &Probe{
		Exec:             &ExecAction{Command: []string{"/bin/true"}},
		TimeoutSeconds:   1,
		PeriodSeconds:    10,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	if HashProcTemplate(withStartup) == base {
		t.Error("startupProbe addition did not change hash")
	}
}

// TestHashProcTemplateM6NilFieldsStable pins that a template leaving every
// M6 field nil hashes exactly as it did before M6 existed (the golden in
// TestHashProcTemplateGolden). This is the upgrade guarantee: pre-M6 Daemons
// must not roll when impd is upgraded. If this fails, an M6 field leaked
// into the canonical serialization (a default materialized, or omitempty
// was dropped).
func TestHashProcTemplateM6NilFieldsStable(t *testing.T) {
	tpl := templateForHash()
	if tpl.Spec.StartupProbe != nil || tpl.Spec.Rlimits != nil || tpl.Spec.Nice != nil ||
		tpl.Spec.OOMScoreAdjust != nil || tpl.Spec.Umask != nil {
		t.Fatal("fixture must leave M6 fields nil")
	}
	defaultProcTemplateSpec(&tpl.Spec) // defaulting must not materialize them
	if tpl.Spec.StartupProbe != nil || tpl.Spec.Rlimits != nil || tpl.Spec.Nice != nil ||
		tpl.Spec.OOMScoreAdjust != nil || tpl.Spec.Umask != nil {
		t.Fatal("defaulting materialized an M6 template field — this rolls every pre-M6 Daemon on upgrade")
	}
	const golden = "8233565f"
	if got := HashProcTemplate(tpl); got != golden {
		t.Errorf("HashProcTemplate = %q, want %q", got, golden)
	}
}
