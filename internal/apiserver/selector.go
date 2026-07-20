// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"encoding/json"
	"strings"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// labelTerm is one equality requirement from a labelSelector. negate is the
// k!=v form; otherwise it is k=v / k==v.
type labelTerm struct {
	key    string
	value  string
	negate bool
}

// matches reports whether labels satisfy the term, with k8s equality-selector
// semantics for a missing key: k=v fails, k!=v succeeds.
func (t labelTerm) matches(labels map[string]string) bool {
	v, ok := labels[t.key]
	if t.negate {
		return !ok || v != t.value
	}
	return ok && v == t.value
}

// parseLabelSelector parses a comma-joined list of equality terms (k=v, k==v,
// k!=v) into requirements ANDed together (M9-i). An empty selector yields no
// terms (matches everything). A malformed term is an ErrInvalid on the
// labelSelector field.
func parseLabelSelector(sel string) ([]labelTerm, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, nil
	}
	var terms []labelTerm
	for raw := range strings.SplitSeq(sel, ",") {
		term := strings.TrimSpace(raw)
		var t labelTerm
		var k, v string
		switch {
		case term == "":
			return nil, selectorInvalid(raw, "empty selector term")
		case strings.Contains(term, "!="):
			k, v, _ = strings.Cut(term, "!=")
			t.negate = true
		case strings.Contains(term, "=="):
			k, v, _ = strings.Cut(term, "==")
		case strings.Contains(term, "="):
			k, v, _ = strings.Cut(term, "=")
		default:
			return nil, selectorInvalid(term, "must be an equality term (k=v, k==v, or k!=v)")
		}
		t.key = strings.TrimSpace(k)
		t.value = strings.TrimSpace(v)
		if t.key == "" {
			return nil, selectorInvalid(term, "term key must not be empty")
		}
		terms = append(terms, t)
	}
	return terms, nil
}

func selectorInvalid(badValue, detail string) error {
	return &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
		Type: v1alpha1.ErrorTypeInvalid, Field: "labelSelector", BadValue: badValue, Detail: detail,
	}}}
}

// filterByLabels keeps only items whose labels satisfy every term (AND). Items
// are partial-decoded for their metadata.labels; an unreadable item is dropped
// (the store holds only validated objects, so this is defensive).
func filterByLabels(items []json.RawMessage, terms []labelTerm) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		var meta struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		keep := true
		for _, t := range terms {
			if !t.matches(meta.Metadata.Labels) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, raw)
		}
	}
	return out
}
