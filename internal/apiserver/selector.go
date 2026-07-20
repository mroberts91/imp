// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"encoding/json"
	"errors"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// Selector parsing and matching moved to api/v1alpha1 in M10 (the wire
// syntax is API semantics; NotifierSpec.Selector shares it). This file keeps
// the HTTP-facing shaping: the labelSelector-field error and the raw-item
// filter for list responses.

// parseLabelSelector parses a ?labelSelector= value (M9-i). A malformed term
// is an ErrInvalid on the labelSelector field.
func parseLabelSelector(sel string) ([]v1alpha1.LabelSelectorTerm, error) {
	terms, err := v1alpha1.ParseLabelSelector(sel)
	if err != nil {
		if selErr, ok := errors.AsType[*v1alpha1.SelectorError](err); ok {
			return nil, &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
				Type: v1alpha1.ErrorTypeInvalid, Field: "labelSelector",
				BadValue: selErr.BadValue, Detail: selErr.Detail,
			}}}
		}
		return nil, err
	}
	return terms, nil
}

// filterByLabels keeps only items whose labels satisfy every term (AND). Items
// are partial-decoded for their metadata.labels; an unreadable item is dropped
// (the store holds only validated objects, so this is defensive).
func filterByLabels(items []json.RawMessage, terms []v1alpha1.LabelSelectorTerm) []json.RawMessage {
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
		if v1alpha1.LabelsMatch(meta.Metadata.Labels, terms) {
			out = append(out, raw)
		}
	}
	return out
}
