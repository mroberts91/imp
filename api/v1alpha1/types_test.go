// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"
)

// fullDaemon returns a Daemon with every field populated, for round-trip and
// deep-copy tests.
func fullDaemon() *Daemon {
	return &Daemon{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindDaemon},
		Metadata: ObjectMeta{
			Name:              "web",
			UID:               "5c9cb6a1-6069-4e61-9437-1d327a012f0c",
			ResourceVersion:   "42",
			Generation:        3,
			CreationTimestamp: NewTime(time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)),
			Labels:            map[string]string{"app": "web", "tier": "frontend"},
			Annotations:       map[string]string{AnnotationManagedBy: ManagedByManifest, AnnotationSourcePath: "web.yaml"},
		},
		Spec: DaemonSpec{
			Replicas:       new(int32(2)),
			UpdateStrategy: UpdateStrategy{Type: UpdateStrategyRecreate},
			Template: ProcTemplate{
				Metadata: TemplateMeta{
					Labels:      map[string]string{"app": "web"},
					Annotations: map[string]string{"team": "platform"},
				},
				Spec: ProcTemplateSpec{
					Command:                       []string{"/usr/bin/serve", "--port", "8080"},
					Env:                           []EnvVar{{Name: "MODE", Value: "prod"}, {Name: "EMPTY"}},
					WorkingDir:                    "/srv/web",
					User:                          "www-data",
					Group:                         "www-data",
					RestartPolicy:                 RestartPolicyAlways,
					StopSignal:                    "TERM",
					TerminationGracePeriodSeconds: new(int64(10)),
				},
			},
		},
		Status: DaemonStatus{
			ObservedGeneration: 3,
			Replicas:           2,
			ReadyReplicas:      1,
			UpdatedReplicas:    2,
			Conditions: []Condition{{
				Type:               ConditionTypeAvailable,
				Status:             ConditionFalse,
				ObservedGeneration: 3,
				LastTransitionTime: NewTime(time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC)),
				Reason:             "MinimumReplicasUnavailable",
				Message:            "1 of 2 replicas ready",
			}},
		},
	}
}

func fullProc() *Proc {
	return &Proc{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindProc},
		Metadata: ObjectMeta{
			Name:              "web-0-9f86d081",
			UID:               "a2b96d9e-70b3-4a17-8fca-2c7ecb0f66a3",
			ResourceVersion:   "57",
			Generation:        1,
			CreationTimestamp: NewTime(time.Date(2026, 7, 10, 12, 1, 0, 0, time.UTC)),
			Labels: map[string]string{
				LabelDaemonName:   "web",
				LabelTemplateHash: "9f86d081",
				LabelReplicaIndex: "0",
			},
			OwnerReferences: []OwnerReference{{
				APIVersion: APIVersion,
				Kind:       KindDaemon,
				Name:       "web",
				UID:        "5c9cb6a1-6069-4e61-9437-1d327a012f0c",
			}},
		},
		Spec: ProcSpec{
			Command:                       []string{"/usr/bin/serve"},
			RestartPolicy:                 RestartPolicyAlways,
			StopSignal:                    "TERM",
			TerminationGracePeriodSeconds: new(DefaultTerminationGracePeriodSeconds),
		},
		Status: ProcStatus{
			Phase: ProcPhaseRunning,
			State: ProcState{Running: &ProcStateRunning{
				PID:            4242,
				StartedAt:      NewTime(time.Date(2026, 7, 10, 12, 1, 5, 0, time.UTC)),
				ProcStartTicks: 123456789,
			}},
			RestartCount: 2,
			Conditions: []Condition{{
				Type:               ConditionTypeReady,
				Status:             ConditionTrue,
				LastTransitionTime: NewTime(time.Date(2026, 7, 10, 12, 1, 5, 0, time.UTC)),
				Reason:             "Running",
				Message:            "process is running",
			}},
		},
	}
}

func fullEvent() *Event {
	return &Event{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindEvent},
		Metadata: ObjectMeta{
			Name:              "web-0-9f86d081.17a2b3c4",
			UID:               "0b54c1e3-df4b-4b86-b3a7-6a2e4a3720cf",
			ResourceVersion:   "58",
			CreationTimestamp: NewTime(time.Date(2026, 7, 10, 12, 2, 0, 0, time.UTC)),
		},
		Regarding:          ObjectRef{Kind: KindProc, Name: "web-0-9f86d081", UID: "a2b96d9e-70b3-4a17-8fca-2c7ecb0f66a3"},
		Type:               EventTypeWarning,
		Reason:             ReasonBackOff,
		Message:            "back-off restarting failed process",
		Count:              7,
		FirstTimestamp:     NewTime(time.Date(2026, 7, 10, 12, 2, 0, 0, time.UTC)),
		LastTimestamp:      NewTime(time.Date(2026, 7, 10, 12, 9, 0, 0, time.UTC)),
		ReportingComponent: "execd",
	}
}

func TestJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
		out  func() any
	}{
		{"Daemon", fullDaemon(), func() any { return &Daemon{} }},
		{"Proc", fullProc(), func() any { return &Proc{} }},
		{"Event", fullEvent(), func() any { return &Event{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := tc.out()
			if err := json.Unmarshal(data, got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if diff := cmp.Diff(tc.in, got); diff != "" {
				t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTimeMarshaling(t *testing.T) {
	// Sub-second precision is dropped and rendering is UTC.
	loc := time.FixedZone("EST", -5*60*60)
	tm := NewTime(time.Date(2026, 7, 10, 7, 1, 2, 999_999_999, loc))
	data, err := json.Marshal(tm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"2026-07-10T12:01:02Z"`; string(data) != want {
		t.Errorf("marshaled %s, want %s", data, want)
	}

	var back Time
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Equal(tm) {
		t.Errorf("round-trip changed instant: %v != %v", back, tm)
	}

	// Zero value marshals as null and is dropped by omitzero.
	zeroData, err := json.Marshal(Time{})
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if string(zeroData) != "null" {
		t.Errorf("zero Time marshaled %s, want null", zeroData)
	}
	metaData, err := json.Marshal(ObjectMeta{Name: "x"})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if strings.Contains(string(metaData), "creationTimestamp") {
		t.Errorf("zero creationTimestamp not omitted: %s", metaData)
	}
	var meta ObjectMeta
	if err := json.Unmarshal([]byte(`{"name":"x","creationTimestamp":null}`), &meta); err != nil {
		t.Fatalf("unmarshal null timestamp: %v", err)
	}
	if !meta.CreationTimestamp.IsZero() {
		t.Errorf("null timestamp unmarshaled non-zero: %v", meta.CreationTimestamp)
	}
}

func TestYAMLManifest(t *testing.T) {
	manifest := `
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
  labels:
    app: web
spec:
  replicas: 2
  template:
    spec:
      command: ["/usr/bin/serve", "--port", "8080"]
      env:
        - name: MODE
          value: prod
      restartPolicy: OnFailure
`
	var d Daemon
	if err := yaml.Unmarshal([]byte(manifest), &d); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if d.Kind != KindDaemon || d.APIVersion != APIVersion {
		t.Errorf("TypeMeta = %+v", d.TypeMeta)
	}
	if d.Metadata.Name != "web" || d.Metadata.Labels["app"] != "web" {
		t.Errorf("Metadata = %+v", d.Metadata)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Errorf("Replicas = %v, want 2", d.Spec.Replicas)
	}
	if got := d.Spec.Template.Spec.Command; len(got) != 3 || got[0] != "/usr/bin/serve" {
		t.Errorf("Command = %v", got)
	}
	if got := d.Spec.Template.Spec.Env; len(got) != 1 || got[0] != (EnvVar{Name: "MODE", Value: "prod"}) {
		t.Errorf("Env = %v", got)
	}
	if d.Spec.Template.Spec.RestartPolicy != RestartPolicyOnFailure {
		t.Errorf("RestartPolicy = %q", d.Spec.Template.Spec.RestartPolicy)
	}
}
