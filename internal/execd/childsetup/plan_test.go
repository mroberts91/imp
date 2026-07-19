// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

import (
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mroberts91/imp/api/v1alpha1"
)

var planCmp = cmp.AllowUnexported(step{})

// TestCapNumberTableMatchesAPI pins the shim's capability-number mapping
// against the API's validation vocabulary, both directions (the
// TestRlimitTableMatchesAPI pattern), and pins the numbers as a contiguous
// 0..n range — the kernel's capability numbering.
func TestCapNumberTableMatchesAPI(t *testing.T) {
	for _, name := range v1alpha1.KnownCapabilityNames() {
		if _, ok := capNumbers[name]; !ok {
			t.Errorf("API accepts capability %q but the shim has no number mapping", name)
		}
	}
	seen := make([]bool, len(capNumbers))
	for name, n := range capNumbers {
		if !v1alpha1.IsKnownCapability(name) {
			t.Errorf("shim maps capability %q but API validation rejects it", name)
		}
		if n < 0 || n >= len(capNumbers) {
			t.Errorf("capability %q number %d outside 0..%d", name, n, len(capNumbers)-1)
			continue
		}
		if seen[n] {
			t.Errorf("capability number %d mapped twice", n)
		}
		seen[n] = true
	}
}

// TestSandboxPlanIdentityOnlyIsM6Sequence pins that without Capabilities
// the plan is byte-identical to the M6 identity drop: groups → gid → uid,
// nothing else.
func TestSandboxPlanIdentityOnlyIsM6Sequence(t *testing.T) {
	id := &identity{uid: 1000, gid: 1000, groups: []int{1000, 44}}
	want := []step{
		{opSetgroups, []int{1000, 44}},
		{opSetgid, 1000},
		{opSetuid, 1000},
	}
	if diff := cmp.Diff(want, sandboxPlan(id, nil), planCmp); diff != "" {
		t.Errorf("plan mismatch (-want +got):\n%s", diff)
	}

	// Group-only (uid untouched, supplementary groups cleared — the M6
	// semantics) must also stay exact.
	groupOnly := &identity{uid: -1, gid: 33, groups: []int{}}
	want = []step{
		{opSetgroups, []int{}},
		{opSetgid, 33},
	}
	if diff := cmp.Diff(want, sandboxPlan(groupOnly, nil), planCmp); diff != "" {
		t.Errorf("group-only plan mismatch (-want +got):\n%s", diff)
	}
}

func TestSandboxPlanEmpty(t *testing.T) {
	if got := sandboxPlan(nil, nil); len(got) != 0 {
		t.Errorf("plan for no identity, no capabilities = %v, want empty", got)
	}
}

// TestSandboxPlanIdentityAndAmbient pins the full §4.3 sequence: keep-caps
// bracketing the identity drop, inheritable set before the ambient raises,
// raises strictly after the setuid (a UID change clears the ambient set).
func TestSandboxPlanIdentityAndAmbient(t *testing.T) {
	id := &identity{uid: 33, gid: 33, groups: []int{33}}
	caps := &v1alpha1.Capabilities{Ambient: []string{"net_bind_service", "chown"}}
	want := []step{
		{opKeepcapsOn, nil},
		{opSetgroups, []int{33}},
		{opSetgid, 33},
		{opSetuid, 33},
		{opCapsetInheritable, []string{"net_bind_service", "chown"}},
		{opAmbientRaise, "net_bind_service"},
		{opAmbientRaise, "chown"},
		{opKeepcapsOff, nil},
	}
	if diff := cmp.Diff(want, sandboxPlan(id, caps), planCmp); diff != "" {
		t.Errorf("plan mismatch (-want +got):\n%s", diff)
	}
}

// TestSandboxPlanAmbientWithoutIdentity covers M7-j: ambient with no user:
// is allowed; the plan keeps the same shape minus the identity steps.
func TestSandboxPlanAmbientWithoutIdentity(t *testing.T) {
	caps := &v1alpha1.Capabilities{Ambient: []string{"net_bind_service"}}
	want := []step{
		{opKeepcapsOn, nil},
		{opCapsetInheritable, []string{"net_bind_service"}},
		{opAmbientRaise, "net_bind_service"},
		{opKeepcapsOff, nil},
	}
	if diff := cmp.Diff(want, sandboxPlan(nil, caps), planCmp); diff != "" {
		t.Errorf("plan mismatch (-want +got):\n%s", diff)
	}
}

// TestSandboxPlanBounding pins that every table capability not listed is
// dropped, in capability-number order, before any identity step.
func TestSandboxPlanBounding(t *testing.T) {
	id := &identity{uid: 33, gid: 33, groups: []int{33}}
	caps := &v1alpha1.Capabilities{Bounding: []string{"net_bind_service", "chown"}}
	got := sandboxPlan(id, caps)

	wantDrops := len(capNumbers) - 2
	var drops []string
	for _, s := range got {
		if s.op != opCapBsetDrop {
			break
		}
		drops = append(drops, s.arg.(string))
	}
	if len(drops) != wantDrops {
		t.Fatalf("got %d leading bounding drops, want %d (plan %v)", len(drops), wantDrops, got)
	}
	if slices.Contains(drops, "net_bind_service") || slices.Contains(drops, "chown") {
		t.Errorf("kept capabilities appear in the drop list: %v", drops)
	}
	for i := 1; i < len(drops); i++ {
		if capNumbers[drops[i-1]] >= capNumbers[drops[i]] {
			t.Errorf("drops not in capability-number order: %s (%d) before %s (%d)",
				drops[i-1], capNumbers[drops[i-1]], drops[i], capNumbers[drops[i]])
		}
	}
	// The remainder is exactly the identity-only plan.
	rest := got[len(drops):]
	if diff := cmp.Diff(sandboxPlan(id, nil), rest, planCmp); diff != "" {
		t.Errorf("post-drop plan differs from the identity plan (-want +got):\n%s", diff)
	}
}
