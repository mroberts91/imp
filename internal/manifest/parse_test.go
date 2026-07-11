// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"strings"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
)

func TestParseMultiDocument(t *testing.T) {
	input := `# leading comment document

---
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: web
spec:
  template:
    spec:
      command: ["/usr/bin/serve"]
---
# a comments-only document between real ones
---
apiVersion: impd.sh/v1alpha1
kind: Daemon
metadata:
  name: worker
spec:
  replicas: 2
  template:
    spec:
      command: ["/usr/bin/work"]
---
`
	objs, err := Parse([]byte(input), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("parsed %d objects, want 2", len(objs))
	}
	if objs[0].Kind != v1alpha1.KindDaemon || objs[0].Name != "web" || objs[0].Source != "test.yaml" {
		t.Errorf("objs[0] = %+v", objs[0])
	}
	if objs[1].Name != "worker" {
		t.Errorf("objs[1] = %+v", objs[1])
	}
	if !strings.Contains(string(objs[1].Body), `"replicas":2`) {
		t.Errorf("body not converted to JSON: %s", objs[1].Body)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "wrong apiVersion",
			input:   "apiVersion: apps/v1\nkind: Daemon\nmetadata:\n  name: web\n",
			wantErr: "apiVersion",
		},
		{
			name:    "unknown kind",
			input:   "apiVersion: impd.sh/v1alpha1\nkind: Deployment\nmetadata:\n  name: web\n",
			wantErr: "unknown kind",
		},
		{
			name:    "missing name",
			input:   "apiVersion: impd.sh/v1alpha1\nkind: Daemon\nmetadata: {}\n",
			wantErr: "metadata.name",
		},
		{
			name:    "broken yaml",
			input:   "apiVersion: [unclosed\n",
			wantErr: "document 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input), "bad.yaml")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want mention of %q", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "bad.yaml") {
				t.Errorf("error does not name the source file: %v", err)
			}
		})
	}
}

func TestParseEmptyStream(t *testing.T) {
	for _, input := range []string{"", "---\n---\n", "# only comments\n"} {
		objs, err := Parse([]byte(input), "empty.yaml")
		if err != nil || len(objs) != 0 {
			t.Errorf("Parse(%q) = %v, %v; want empty, nil", input, objs, err)
		}
	}
}
