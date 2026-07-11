// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"fmt"
	"net/http"
)

type ErrorReason string

const (
	ErrorReasonNotFound      ErrorReason = "NotFound"
	ErrorReasonAlreadyExists ErrorReason = "AlreadyExists"
	ErrorReasonConflict      ErrorReason = "Conflict"
	ErrorReasonCompacted     ErrorReason = "Compacted"
	ErrorReasonInvalid       ErrorReason = "Invalid"
	ErrorReasonInternal      ErrorReason = "Internal"
)

// APIError is the JSON error document returned by every non-2xx API
// response.
type APIError struct {
	Reason      ErrorReason `json:"reason"`
	Message     string      `json:"message"`
	FieldErrors ErrorList   `json:"fieldErrors,omitempty"`
}

// APIErrorFrom classifies err into its wire document and HTTP status.
func APIErrorFrom(err error) (*APIError, int) {
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		return &APIError{Reason: ErrorReasonInvalid, Message: err.Error(), FieldErrors: inv.Errs}, http.StatusUnprocessableEntity
	case errors.Is(err, ErrNotFound):
		return &APIError{Reason: ErrorReasonNotFound, Message: err.Error()}, http.StatusNotFound
	case errors.Is(err, ErrAlreadyExists):
		return &APIError{Reason: ErrorReasonAlreadyExists, Message: err.Error()}, http.StatusConflict
	case errors.Is(err, ErrConflict):
		return &APIError{Reason: ErrorReasonConflict, Message: err.Error()}, http.StatusConflict
	case errors.Is(err, ErrCompacted):
		return &APIError{Reason: ErrorReasonCompacted, Message: err.Error()}, http.StatusGone
	default:
		return &APIError{Reason: ErrorReasonInternal, Message: err.Error()}, http.StatusInternalServerError
	}
}

func (e *APIError) Err() error {
	switch e.Reason {
	case ErrorReasonInvalid:
		if len(e.FieldErrors) > 0 {
			return &InvalidError{Errs: e.FieldErrors}
		}
		return &wireError{msg: e.Message, err: ErrInvalid}
	case ErrorReasonNotFound:
		return &wireError{msg: e.Message, err: ErrNotFound}
	case ErrorReasonAlreadyExists:
		return &wireError{msg: e.Message, err: ErrAlreadyExists}
	case ErrorReasonConflict:
		return &wireError{msg: e.Message, err: ErrConflict}
	case ErrorReasonCompacted:
		return &wireError{msg: e.Message, err: ErrCompacted}
	default:
		return fmt.Errorf("server error: %s", e.Message)
	}
}

var _ error = &wireError{}

type wireError struct {
	msg string
	err error
}

func (w *wireError) Error() string { return w.msg }
func (w *wireError) Unwrap() error { return w.err }
