// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// The error accumulation in this file is derived from
// Kubernetes (staging/src/k8s.io/apimachinery/pkg/util/validation/field),
// Copyright The Kubernetes Authors, Apache-2.0. Validation never fails fast:
// every error in an object is collected with its JSON-style field path so
// `impctl apply` can report all of them at once.

import (
	"errors"
	"fmt"
	"strings"
)

type ErrorType string

const (
	ErrorTypeRequired     ErrorType = "FieldValueRequired"
	ErrorTypeInvalid      ErrorType = "FieldValueInvalid"
	ErrorTypeNotSupported ErrorType = "FieldValueNotSupported"
)

// FieldError is one validation failure, located by a JSON-style field path
// such as spec.template.spec.command.
type FieldError struct {
	Type     ErrorType `json:"type"`
	Field    string    `json:"field"`
	BadValue any       `json:"badValue,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Error renders the k8s-style one-line form.
func (e *FieldError) Error() string {
	var b strings.Builder
	b.WriteString(e.Field)
	b.WriteString(": ")
	switch e.Type {
	case ErrorTypeRequired:
		b.WriteString("Required value")
	case ErrorTypeInvalid:
		fmt.Fprintf(&b, "Invalid value: %q", fmt.Sprintf("%v", e.BadValue))
	case ErrorTypeNotSupported:
		fmt.Fprintf(&b, "Unsupported value: %q", fmt.Sprintf("%v", e.BadValue))
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

type ErrorList []*FieldError

// ToAggregate returns nil for an empty list, otherwise a single error joining
// every failure.
func (l ErrorList) ToAggregate() error {
	if len(l) == 0 {
		return nil
	}
	errs := make([]error, len(l))
	for i, e := range l {
		errs[i] = e
	}
	return errors.Join(errs...)
}

func requiredErr(p *Path, detail string) *FieldError {
	return &FieldError{Type: ErrorTypeRequired, Field: p.String(), Detail: detail}
}

func invalidErr(p *Path, value any, detail string) *FieldError {
	return &FieldError{Type: ErrorTypeInvalid, Field: p.String(), BadValue: value, Detail: detail}
}

func notSupportedErr(p *Path, value any, valid []string) *FieldError {
	return &FieldError{
		Type:     ErrorTypeNotSupported,
		Field:    p.String(),
		BadValue: value,
		Detail:   "supported values: " + strings.Join(valid, ", "),
	}
}

// Path locates a field, built incrementally as validation descends.
// name renders spec.template.spec.env[2].name style paths.
type Path struct {
	parent *Path
	name   string
	index  int
	kind   pathKind
}

type pathKind int

const (
	pathChild pathKind = iota
	pathIndex
	pathKey
)

func NewPath(name string) *Path {
	return &Path{name: name}
}

func (p *Path) Child(name string) *Path {
	return &Path{parent: p, name: name}
}

func (p *Path) Index(i int) *Path {
	return &Path{parent: p, index: i, kind: pathIndex}
}

func (p *Path) Key(k string) *Path {
	return &Path{parent: p, name: k, kind: pathKey}
}

func (p *Path) String() string {
	var render func(*Path, *strings.Builder)
	render = func(p *Path, b *strings.Builder) {
		if p.parent != nil {
			render(p.parent, b)
		}
		switch p.kind {
		case pathIndex:
			fmt.Fprintf(b, "[%d]", p.index)
		case pathKey:
			fmt.Fprintf(b, "[%s]", p.name)
		default:
			if p.parent != nil {
				b.WriteString(".")
			}
			b.WriteString(p.name)
		}
	}
	var b strings.Builder
	render(p, &b)
	return b.String()
}

// Sentinel errors shared by every layer, mirroring the shape of k8s's
// apimachinery error.
var (
	ErrNotFound      = errors.New("object not found")
	ErrAlreadyExists = errors.New("object already exists")
	ErrConflict      = errors.New("resourceVersion conflict")
	ErrCompacted     = errors.New("resourceVersion already compacted")
	ErrInvalid       = errors.New("invalid object")
)

// InvalidError is a validation failure with the accumulated field
// errors.
type InvalidError struct {
	Errs ErrorList
}

func (e *InvalidError) Error() string {
	if agg := e.Errs.ToAggregate(); agg != nil {
		return "invalid object: " + agg.Error()
	}
	return "invalid object"
}

func (e *InvalidError) Is(target error) bool {
	return target == ErrInvalid
}
