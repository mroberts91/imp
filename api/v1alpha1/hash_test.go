// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestConfigRefUnionMarshal pins the M9-b upgrade guarantee: a bare-name
// ConfigRef marshals to the pre-M9 bare string, so string-form manifests keep
// byte-identical canonical JSON (hence template hash); a ref with a path uses
// the object form. Both forms round-trip through the slice shape templates
// carry.
func TestConfigRefUnionMarshal(t *testing.T) {
	bare, err := json.Marshal(ConfigRef{Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if string(bare) != `"app"` {
		t.Errorf("bare ConfigRef = %s, want \"app\" (pre-M9 byte compatibility)", bare)
	}
	obj, err := json.Marshal(ConfigRef{Name: "app", Path: new("/etc/app")})
	if err != nil {
		t.Fatal(err)
	}
	if string(obj) != `{"name":"app","path":"/etc/app"}` {
		t.Errorf("path ConfigRef = %s, want the object form", obj)
	}
	var refs []ConfigRef
	if err := json.Unmarshal([]byte(`["app",{"name":"shared","path":"/etc/shared"}]`), &refs); err != nil {
		t.Fatal(err)
	}
	want := []ConfigRef{{Name: "app"}, {Name: "shared", Path: new("/etc/shared")}}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("round-trip = %+v, want %+v", refs, want)
	}

	// A typo'd key in the object form is a hard error (strict decode), not a
	// silently-dropped field — matching the apiserver's DisallowUnknownFields.
	var bad ConfigRef
	if err := json.Unmarshal([]byte(`{"name":"web","paths":"/etc/web"}`), &bad); err == nil {
		t.Errorf("object form with unknown field decoded to %+v, want an error", bad)
	}
}

// TestHashProcTemplateStringConfigGolden pins that a template with string-form
// configs hashes to a fixed value AND serializes the bare-string form — the
// byte-for-byte pre-M9 result, so a pre-M9 daemon using `configs: [app]` does
// not roll when impd gains the union type (M9-b).
func TestHashProcTemplateStringConfigGolden(t *testing.T) {
	tpl := templateForHash()
	tpl.Spec.Configs = []ConfigRef{{Name: "app"}}
	const golden = "8386d2b5"
	if got := HashProcTemplate(tpl); got != golden {
		t.Errorf("HashProcTemplate(string-config) = %q, want %q", got, golden)
	}
	b, err := json.Marshal(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"configs":["app"]`) {
		t.Errorf("template JSON = %s, want a bare-string configs array", b)
	}
}

func configWithBinaryForHash() *ConfigSpec {
	return &ConfigSpec{
		Data:       map[string]string{"app.conf": "listen 8080\n"},
		BinaryData: map[string][]byte{"cert.der": {0x00, 0x01, 0x02, 0x03}},
		Mode:       new("0644"),
		Modes:      map[string]string{"app.conf": "0400"},
	}
}

// TestHashConfigSpecBinaryModesGolden pins a Config using binaryData + modes.
// Breaking it rolls every Daemon that references such a Config on upgrade.
func TestHashConfigSpecBinaryModesGolden(t *testing.T) {
	const golden = "6e5f73f5"
	if got := HashConfigSpec(configWithBinaryForHash()); got != golden {
		t.Errorf("HashConfigSpec(binary+modes) = %q, want %q (serialization changed?)", got, golden)
	}
	// binaryData participates.
	noBinary := configWithBinaryForHash()
	noBinary.BinaryData = nil
	if HashConfigSpec(noBinary) == HashConfigSpec(configWithBinaryForHash()) {
		t.Error("binaryData change did not change config hash")
	}
	// modes participates.
	noModes := configWithBinaryForHash()
	noModes.Modes = nil
	if HashConfigSpec(noModes) == HashConfigSpec(configWithBinaryForHash()) {
		t.Error("modes change did not change config hash")
	}
}

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

	withNNP := templateForHash()
	withNNP.Spec.NoNewPrivileges = new(true)
	if HashProcTemplate(withNNP) == base {
		t.Error("noNewPrivileges addition did not change hash")
	}

	withCaps := templateForHash()
	withCaps.Spec.Capabilities = &Capabilities{Ambient: []string{"net_bind_service"}}
	if HashProcTemplate(withCaps) == base {
		t.Error("capabilities addition did not change hash")
	}

	withPrivateTmp := templateForHash()
	withPrivateTmp.Spec.PrivateTmp = new(true)
	if HashProcTemplate(withPrivateTmp) == base {
		t.Error("privateTmp addition did not change hash")
	}

	withConfigs := templateForHash()
	withConfigs.Spec.Configs = []ConfigRef{{Name: "app"}}
	if HashProcTemplate(withConfigs) == base {
		t.Error("configs addition did not change hash")
	}
}

// TestHashProcTemplateNilFieldsStable pins that a template leaving every
// M6 and M7 field nil hashes exactly as it did before those milestones
// existed (the golden in TestHashProcTemplateGolden). This is the upgrade
// guarantee: pre-existing Daemons must not roll when impd is upgraded. If
// this fails, a milestone field leaked into the canonical serialization (a
// default materialized, or omitempty was dropped).
func TestHashProcTemplateNilFieldsStable(t *testing.T) {
	tpl := templateForHash()
	nilFieldsNil := func() bool {
		return tpl.Spec.StartupProbe == nil && tpl.Spec.Rlimits == nil && tpl.Spec.Nice == nil &&
			tpl.Spec.OOMScoreAdjust == nil && tpl.Spec.Umask == nil &&
			tpl.Spec.NoNewPrivileges == nil && tpl.Spec.Capabilities == nil && tpl.Spec.PrivateTmp == nil &&
			tpl.Spec.Configs == nil
	}
	if !nilFieldsNil() {
		t.Fatal("fixture must leave M6/M7/M8 fields nil")
	}
	defaultProcTemplateSpec(&tpl.Spec) // defaulting must not materialize them
	if !nilFieldsNil() {
		t.Fatal("defaulting materialized an M6/M7/M8 template field — this rolls every pre-existing Daemon on upgrade")
	}
	const golden = "8233565f"
	if got := HashProcTemplate(tpl); got != golden {
		t.Errorf("HashProcTemplate = %q, want %q", got, golden)
	}
}

func configForHash() *ConfigSpec {
	return &ConfigSpec{
		Data: map[string]string{
			"app.conf":  "listen 8080\n",
			"logrotate": "daily\n",
		},
		Mode: new("0600"),
	}
}

// TestHashConfigSpecGolden pins the content-hash of a fixture Config. Breaking
// this means the canonical ConfigSpec serialization changed, which rolls every
// Daemon that references a Config on upgrade — change the golden only
// deliberately.
func TestHashConfigSpecGolden(t *testing.T) {
	const golden = "6f28de52"
	if got := HashConfigSpec(configForHash()); got != golden {
		t.Errorf("HashConfigSpec = %q, want %q (ConfigSpec serialization changed?)", got, golden)
	}
	// Mode participates in the hash: dropping it must change the result.
	noMode := configForHash()
	noMode.Mode = nil
	if HashConfigSpec(noMode) == HashConfigSpec(configForHash()) {
		t.Error("mode change did not change config hash")
	}
	// Content participates.
	edited := configForHash()
	edited.Data["app.conf"] = "listen 9090\n"
	if HashConfigSpec(edited) == HashConfigSpec(configForHash()) {
		t.Error("content change did not change config hash")
	}
}

// TestHashConfigRevisionGolden pins the combined revision hash and its
// invariants (M8-g).
func TestHashConfigRevisionGolden(t *testing.T) {
	const templateHash = "8233565f"
	refs := []RevisionRef{{Name: "app", Hash: "1a2b3c4d"}, {Name: "shared", Hash: "5e6f7a8b"}}

	const golden = "90cb9485"
	if got := HashConfigRevision(templateHash, refs); got != golden {
		t.Errorf("HashConfigRevision = %q, want %q (revision serialization changed?)", got, golden)
	}

	// No refs ⇒ the template hash unchanged (the M7 byte-identical guarantee).
	if got := HashConfigRevision(templateHash, nil); got != templateHash {
		t.Errorf("HashConfigRevision(_, nil) = %q, want %q", got, templateHash)
	}

	// Renaming a reference rolls even when content hashes are unchanged.
	renamed := []RevisionRef{{Name: "app2", Hash: "1a2b3c4d"}, {Name: "shared", Hash: "5e6f7a8b"}}
	if HashConfigRevision(templateHash, renamed) == golden {
		t.Error("renaming a config reference did not change the revision")
	}
	// A different template hash rolls even with identical refs.
	if HashConfigRevision("ffffffff", refs) == golden {
		t.Error("template-hash change did not change the revision")
	}
	// Reordering references is a different revision (order is reference order).
	reordered := []RevisionRef{refs[1], refs[0]}
	if HashConfigRevision(templateHash, reordered) == golden {
		t.Error("reordering config references did not change the revision")
	}
}
