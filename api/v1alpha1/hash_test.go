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
}
