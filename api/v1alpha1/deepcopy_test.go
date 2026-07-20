// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// snapshot is the JSON-round-trip correctness oracle: mutate the copy, then
// prove the original's serialized form did not change.
func snapshot(t *testing.T, obj any) string {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestDaemonDeepCopy(t *testing.T) {
	orig := fullDaemon()
	before := snapshot(t, orig)

	cp := orig.DeepCopy()
	if diff := cmp.Diff(orig, cp); diff != "" {
		t.Fatalf("copy differs from original (-orig +copy):\n%s", diff)
	}

	// Mutate every reference-typed field of the copy.
	cp.Metadata.Labels["app"] = "mutated"
	cp.Metadata.Annotations["new"] = "mutated"
	cp.Metadata.OwnerReferences = append(cp.Metadata.OwnerReferences, OwnerReference{Name: "x"})
	*cp.Spec.Replicas = 99
	cp.Spec.Template.Metadata.Labels["app"] = "mutated"
	cp.Spec.Template.Spec.Command[0] = "/mutated"
	cp.Spec.Template.Spec.Env[0].Value = "mutated"
	*cp.Spec.Template.Spec.TerminationGracePeriodSeconds = 999
	*cp.Spec.Template.Spec.Resources.Limits.CPUWeight = 1
	cp.Spec.Template.Spec.LivenessProbe.Exec.Command[0] = "/mutated"
	cp.Spec.Template.Spec.ReadinessProbe.HTTPGet.HTTPHeaders[0].Value = "mutated"
	*cp.Spec.ProgressDeadlineSeconds = 1
	cp.Spec.Template.Spec.StartupProbe.Exec.Command[0] = "/mutated"
	*cp.Spec.Template.Spec.Rlimits[0].Soft = 1
	*cp.Spec.Template.Spec.Rlimits[0].Hard = 1
	*cp.Spec.Template.Spec.Nice = -1
	*cp.Spec.Template.Spec.OOMScoreAdjust = 1
	*cp.Spec.Template.Spec.Umask = "0777"
	*cp.Spec.Template.Spec.NoNewPrivileges = false
	cp.Spec.Template.Spec.Capabilities.Bounding[0] = "mutated"
	cp.Spec.Template.Spec.Capabilities.Ambient[0] = "mutated"
	*cp.Spec.Template.Spec.PrivateTmp = false
	cp.Spec.Template.Spec.Configs[0] = ConfigRef{Name: "mutated"}
	*cp.Spec.Template.Spec.Configs[1].Path = "/mutated" // deep-copied Path pointer
	cp.Status.Conditions[0].Status = ConditionTrue

	if after := snapshot(t, orig); after != before {
		t.Errorf("mutating the copy changed the original:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestProcDeepCopy(t *testing.T) {
	orig := fullProc()
	before := snapshot(t, orig)

	cp := orig.DeepCopy()
	if diff := cmp.Diff(orig, cp); diff != "" {
		t.Fatalf("copy differs from original (-orig +copy):\n%s", diff)
	}

	cp.Metadata.Labels[LabelReplicaIndex] = "9"
	cp.Metadata.OwnerReferences[0].Name = "mutated"
	cp.Spec.Command[0] = "/mutated"
	*cp.Spec.TerminationGracePeriodSeconds = 999
	cp.Status.State.Running.PID = 1
	cp.Status.Conditions[0].Status = ConditionFalse

	if after := snapshot(t, orig); after != before {
		t.Errorf("mutating the copy changed the original:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestConfigDeepCopy(t *testing.T) {
	orig := fullConfig()
	before := snapshot(t, orig)

	cp := orig.DeepCopy()
	if diff := cmp.Diff(orig, cp); diff != "" {
		t.Fatalf("copy differs from original (-orig +copy):\n%s", diff)
	}

	cp.Metadata.Labels["app"] = "mutated"
	cp.Spec.Data["app.conf"] = "mutated"
	cp.Spec.BinaryData["cert.der"][0] = 0xff
	cp.Spec.Modes["app.conf"] = "0777"
	*cp.Spec.Mode = "0777"

	if after := snapshot(t, orig); after != before {
		t.Errorf("mutating the copy changed the original:\nbefore: %s\nafter:  %s", before, after)
	}

	// Nil Mode stays nil rather than materializing.
	minimal := &Config{Metadata: ObjectMeta{Name: "x"}, Spec: ConfigSpec{Data: map[string]string{"a": "b"}}}
	if minimal.DeepCopy().Spec.Mode != nil {
		t.Error("nil Mode materialized by DeepCopy")
	}
	var nilCfg *Config
	if nilCfg.DeepCopy() != nil {
		t.Error("nil Config DeepCopy != nil")
	}
}

func TestProcStateDeepCopyTerminated(t *testing.T) {
	orig := &ProcStatus{
		Phase: ProcPhaseFailed,
		State: ProcState{Terminated: &ProcStateTerminated{
			ExitCode: new(int(3)),
			Message:  "exited",
		}},
	}
	cp := orig.DeepCopy()
	*cp.State.Terminated.ExitCode = 7
	cp.State.Terminated.Message = "mutated"
	if *orig.State.Terminated.ExitCode != 3 || orig.State.Terminated.Message != "exited" {
		t.Errorf("mutating the copy changed the original: %+v", orig.State.Terminated)
	}
}

func TestEventDeepCopy(t *testing.T) {
	orig := fullEvent()
	before := snapshot(t, orig)

	cp := orig.DeepCopy()
	if diff := cmp.Diff(orig, cp); diff != "" {
		t.Fatalf("copy differs from original (-orig +copy):\n%s", diff)
	}

	cp.Count = 100
	cp.Regarding.Name = "mutated"

	if after := snapshot(t, orig); after != before {
		t.Errorf("mutating the copy changed the original:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestDeepCopyNilHandling(t *testing.T) {
	var d *Daemon
	if d.DeepCopy() != nil {
		t.Error("nil Daemon DeepCopy != nil")
	}
	// Nil maps and slices stay nil rather than becoming empty.
	minimal := &Daemon{Metadata: ObjectMeta{Name: "x"}}
	cp := minimal.DeepCopy()
	if cp.Metadata.Labels != nil || cp.Metadata.OwnerReferences != nil || cp.Spec.Replicas != nil {
		t.Errorf("nil fields materialized: %+v", cp)
	}
}
