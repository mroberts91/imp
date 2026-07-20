// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Label-selector parsing and matching (M9-i syntax). Moved here from
// internal/apiserver in M10: the selector wire syntax is API semantics, and
// NotifierSpec.Selector needs the same parser at validation time. The
// apiserver's ?labelSelector= handling and the NotifierController's target
// scoping both build on these.

import (
	"fmt"
	"strings"
)

// LabelSelectorTerm is one equality requirement from a label selector.
// Negate is the k!=v form; otherwise it is k=v / k==v.
type LabelSelectorTerm struct {
	Key    string
	Value  string
	Negate bool
}

// Matches reports whether labels satisfy the term, with k8s
// equality-selector semantics for a missing key: k=v fails, k!=v succeeds.
func (t LabelSelectorTerm) Matches(labels map[string]string) bool {
	v, ok := labels[t.Key]
	if t.Negate {
		return !ok || v != t.Value
	}
	return ok && v == t.Value
}

// LabelsMatch reports whether labels satisfy every term (AND). No terms
// matches everything.
func LabelsMatch(labels map[string]string, terms []LabelSelectorTerm) bool {
	for _, t := range terms {
		if !t.Matches(labels) {
			return false
		}
	}
	return true
}

// SelectorError describes a malformed selector term. Callers surface
// BadValue and Detail on whichever field carried the selector.
type SelectorError struct {
	BadValue string
	Detail   string
}

func (e *SelectorError) Error() string {
	return fmt.Sprintf("invalid selector term %q: %s", e.BadValue, e.Detail)
}

// ParseLabelSelector parses a comma-joined list of equality terms (k=v,
// k==v, k!=v) into requirements ANDed together. An empty selector yields no
// terms (matches everything). The error, when non-nil, is a *SelectorError.
func ParseLabelSelector(sel string) ([]LabelSelectorTerm, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, nil
	}
	var terms []LabelSelectorTerm
	for raw := range strings.SplitSeq(sel, ",") {
		term := strings.TrimSpace(raw)
		var t LabelSelectorTerm
		var k, v string
		switch {
		case term == "":
			return nil, &SelectorError{BadValue: raw, Detail: "empty selector term"}
		case strings.Contains(term, "!="):
			k, v, _ = strings.Cut(term, "!=")
			t.Negate = true
		case strings.Contains(term, "=="):
			k, v, _ = strings.Cut(term, "==")
		case strings.Contains(term, "="):
			k, v, _ = strings.Cut(term, "=")
		default:
			return nil, &SelectorError{BadValue: term, Detail: "must be an equality term (k=v, k==v, or k!=v)"}
		}
		t.Key = strings.TrimSpace(k)
		t.Value = strings.TrimSpace(v)
		if t.Key == "" {
			return nil, &SelectorError{BadValue: term, Detail: "term key must not be empty"}
		}
		terms = append(terms, t)
	}
	return terms, nil
}
