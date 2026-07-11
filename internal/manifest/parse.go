// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package manifest owns imp's manifest surface: YAML documents whose
// identity is (kind, name).
package manifest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/mroberts91/imp/api/v1alpha1"
)

type Object struct {
	Kind   string
	Name   string
	Body   []byte
	Source string
}

// ParseFile reads and parses one manifest file (multi-document YAML
// supported).
func ParseFile(path string) ([]Object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse splits multi-document YAML and validates each document's identity.
// apiVersion must be impd.sh/v1alpha1, kind must be a known kind, and
// metadata.name must be set. Empty documents are skipped.
func Parse(data []byte, source string) ([]Object, error) {
	var objects []Object
	for i, doc := range splitDocuments(data) {
		jsonBody, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		if isEmptyDocument(jsonBody) {
			continue
		}
		var peek struct {
			v1alpha1.TypeMeta `json:",inline"`
			Metadata          struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(jsonBody, &peek); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		switch {
		case peek.APIVersion != v1alpha1.APIVersion:
			return nil, fmt.Errorf("%s: document %d: apiVersion %q is not %q", source, i+1, peek.APIVersion, v1alpha1.APIVersion)
		case !v1alpha1.IsValidKind(peek.Kind):
			return nil, fmt.Errorf("%s: document %d: unknown kind %q", source, i+1, peek.Kind)
		case peek.Metadata.Name == "":
			return nil, fmt.Errorf("%s: document %d: metadata.name is required", source, i+1)
		}
		objects = append(objects, Object{
			Kind:   peek.Kind,
			Name:   peek.Metadata.Name,
			Body:   jsonBody,
			Source: source,
		})
	}
	return objects, nil
}

// splitDocuments splits a YAML stream on `---` document separators
func splitDocuments(data []byte) [][]byte {
	var docs [][]byte
	var current bytes.Buffer
	flush := func() {
		docs = append(docs, bytes.Clone(current.Bytes()))
		current.Reset()
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		trimmed := bytes.TrimRight(line, " \t")
		if bytes.Equal(trimmed, []byte("---")) || bytes.HasPrefix(trimmed, []byte("--- ")) {
			flush()
			continue
		}
		current.Write(line)
		current.WriteByte('\n')
	}
	flush()
	return docs
}

// isEmptyDocument reports whether the converted JSON carries no content
// YAMLToJSON renders a blank or comments-only document as "null".
func isEmptyDocument(jsonBody []byte) bool {
	return len(bytes.TrimSpace(jsonBody)) == 0 || bytes.Equal(bytes.TrimSpace(jsonBody), []byte("null"))
}
