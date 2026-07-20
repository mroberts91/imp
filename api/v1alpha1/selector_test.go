// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseLabelSelector(t *testing.T) {
	cases := []struct {
		name       string
		sel        string
		want       []LabelSelectorTerm
		wantDetail string // "" = no error; else the SelectorError detail
	}{
		{name: "empty", sel: ""},
		{name: "whitespace only", sel: "   "},
		{
			name: "single equality",
			sel:  "app=web",
			want: []LabelSelectorTerm{{Key: "app", Value: "web"}},
		},
		{
			name: "double equals",
			sel:  "app==web",
			want: []LabelSelectorTerm{{Key: "app", Value: "web"}},
		},
		{
			name: "negation",
			sel:  "tier!=db",
			want: []LabelSelectorTerm{{Key: "tier", Value: "db", Negate: true}},
		},
		{
			name: "multiple terms with spaces",
			sel:  " app=web , tier!=db ",
			want: []LabelSelectorTerm{
				{Key: "app", Value: "web"},
				{Key: "tier", Value: "db", Negate: true},
			},
		},
		{
			name: "empty value",
			sel:  "app=",
			want: []LabelSelectorTerm{{Key: "app", Value: ""}},
		},
		{name: "bare word", sel: "app", wantDetail: "must be an equality term (k=v, k==v, or k!=v)"},
		{name: "empty term in list", sel: "app=web,,tier=db", wantDetail: "empty selector term"},
		{name: "empty key", sel: "=web", wantDetail: "term key must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLabelSelector(tc.sel)
			if tc.wantDetail != "" {
				selErr, ok := errors.AsType[*SelectorError](err)
				if !ok {
					t.Fatalf("err = %v, want *SelectorError", err)
				}
				if selErr.Detail != tc.wantDetail {
					t.Errorf("Detail = %q, want %q", selErr.Detail, tc.wantDetail)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("terms differ (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLabelSelectorTermMatches(t *testing.T) {
	labels := map[string]string{"app": "web", "tier": "frontend"}
	cases := []struct {
		name string
		term LabelSelectorTerm
		want bool
	}{
		{"equality match", LabelSelectorTerm{Key: "app", Value: "web"}, true},
		{"equality wrong value", LabelSelectorTerm{Key: "app", Value: "db"}, false},
		// k8s equality-selector semantics for a missing key: k=v fails,
		// k!=v succeeds.
		{"equality missing key", LabelSelectorTerm{Key: "zone", Value: "a"}, false},
		{"negate different value", LabelSelectorTerm{Key: "app", Value: "db", Negate: true}, true},
		{"negate equal value", LabelSelectorTerm{Key: "app", Value: "web", Negate: true}, false},
		{"negate missing key", LabelSelectorTerm{Key: "zone", Value: "a", Negate: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.term.Matches(labels); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLabelsMatch(t *testing.T) {
	labels := map[string]string{"app": "web", "tier": "frontend"}
	all := []LabelSelectorTerm{
		{Key: "app", Value: "web"},
		{Key: "tier", Value: "db", Negate: true},
	}
	if !LabelsMatch(labels, all) {
		t.Error("all-satisfied terms did not match")
	}
	oneFails := append(slices.Clone(all), LabelSelectorTerm{Key: "zone", Value: "a"})
	if LabelsMatch(labels, oneFails) {
		t.Error("AND semantics violated: matched with a failing term")
	}
	if !LabelsMatch(labels, nil) {
		t.Error("no terms must match everything")
	}
	if !LabelsMatch(nil, nil) {
		t.Error("no terms must match even nil labels")
	}
}
