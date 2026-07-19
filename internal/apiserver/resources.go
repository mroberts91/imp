// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/mroberts91/imp/api/v1alpha1"
)

const maxBodyBytes = 1 << 20

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	kind, err := resolveKind(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if r.URL.Query().Get("watch") == "true" {
		s.handleWatch(w, r, kind)
		return
	}
	items, listRV, err := s.store.List(kind)
	if err != nil {
		writeError(w, err)
		return
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, v1alpha1.ObjectList{
		ResourceVersion: strconv.FormatInt(listRV, 10),
		Items:           items,
	})
}

// handleWatch upgrades a list request to an ndjson stream: one WatchEvent
// per line, flushed per line.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, kind string) {
	sinceRV, err := parseRV(r.URL.Query().Get("resourceVersion"))
	if err != nil {
		writeError(w, err)
		return
	}
	events, cancel, err := s.store.Watch(kind, sinceRV)
	if err != nil {
		writeError(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	kind, err := resolveKind(r)
	if err != nil {
		writeError(w, err)
		return
	}
	body, err := s.store.Get(kind, r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// handleApply is the simplified server-side apply: default, validate, then
// create or replace spec. On replace, metadata identity and status are
// preserved (etcl's spec-path update). Removing a field from the manifest
// removes it from the live object.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	kind, err := resolveKind(r)
	if err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	data, err := readBody(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	body, expectedRV, err := s.prepare(kind, name, data, false)
	if err != nil {
		writeError(w, err)
		return
	}

	for range 3 {
		out, created, err := s.applyOnce(kind, name, body, expectedRV)
		switch {
		case err == nil && created:
			writeRaw(w, http.StatusCreated, out)
			return
		case err == nil:
			writeRaw(w, http.StatusOK, out)
			return
		case errors.Is(err, v1alpha1.ErrNotFound) || errors.Is(err, v1alpha1.ErrAlreadyExists):
			continue
		default:
			writeError(w, err)
			return
		}
	}
	writeError(w, fmt.Errorf("apiserver: apply of %s %q kept racing concurrent writes: %w", kind, name, v1alpha1.ErrConflict))
}

func (s *Server) applyOnce(kind, name string, body []byte, expectedRV int64) (json.RawMessage, bool, error) {
	out, err := s.store.Create(kind, body)
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, v1alpha1.ErrAlreadyExists) {
		return nil, false, err
	}

	// Per-kind update rule: a Proc's spec is immutable after creation -
	// template changes arrive as new Procs under a new template hash, never
	// as mutations of a live one.
	if kind == v1alpha1.KindProc {
		current, err := s.store.Get(kind, name)
		if err != nil {
			return nil, false, err
		}
		same, err := specsEqual(current, body)
		if err != nil {
			return nil, false, err
		}
		if !same {
			return nil, false, &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
				Type: v1alpha1.ErrorTypeInvalid, Field: "spec",
				Detail: "Proc spec is immutable; template changes arrive as new Procs",
			}}}
		}
	}

	out, err = s.store.Update(kind, name, body, expectedRV)
	return out, false, err
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	kind, err := resolveKind(r)
	if err != nil {
		writeError(w, err)
		return
	}
	expectedRV, err := parseRV(r.URL.Query().Get("resourceVersion"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.Delete(kind, r.PathValue("name"), expectedRV); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStatus is the status sub-resource: only the body's status subtree is
// taken (etcl enforces that), and the write is always Compare and Store.
// Status writers reconcile from observed state, so they always know which rv they read.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	kind, err := resolveKind(r)
	if err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	data, err := readBody(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	body, expectedRV, err := s.prepare(kind, name, data, true)
	if err != nil {
		writeError(w, err)
		return
	}
	if expectedRV == 0 {
		writeError(w, &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
			Type: v1alpha1.ErrorTypeRequired, Field: "metadata.resourceVersion",
			Detail: "status updates are compare-and-swap",
		}}})
		return
	}
	out, err := s.store.UpdateStatus(kind, name, body, expectedRV)
	if err != nil {
		writeError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, out)
}

// prepare decodes, checks, and canonicalizes an incoming object: strict
// per-kind decode, TypeMeta enforcement and stamping, URL/body name
// agreement, and for the apply path, defaulting then validation Returns the
// canonical body and the rv from the body's metadata (0 if absent).
func (s *Server) prepare(kind, urlName string, data []byte, forStatus bool) ([]byte, int64, error) {
	switch kind {
	case v1alpha1.KindDaemon:
		var obj v1alpha1.Daemon
		if err := strictUnmarshal(data, &obj); err != nil {
			return nil, 0, invalidBody(err)
		}
		if err := checkIdentity(&obj.TypeMeta, &obj.Metadata, kind, urlName); err != nil {
			return nil, 0, err
		}
		rv, err := parseRV(obj.Metadata.ResourceVersion)
		if err != nil {
			return nil, 0, err
		}
		if !forStatus {
			obj.Status = v1alpha1.DaemonStatus{}
			v1alpha1.DefaultDaemon(&obj)
			if errs := v1alpha1.ValidateDaemon(&obj); len(errs) > 0 {
				return nil, 0, &v1alpha1.InvalidError{Errs: errs}
			}
		}
		body, err := json.Marshal(&obj)
		return body, rv, err

	case v1alpha1.KindProc:
		var obj v1alpha1.Proc
		if err := strictUnmarshal(data, &obj); err != nil {
			return nil, 0, invalidBody(err)
		}
		if err := checkIdentity(&obj.TypeMeta, &obj.Metadata, kind, urlName); err != nil {
			return nil, 0, err
		}
		rv, err := parseRV(obj.Metadata.ResourceVersion)
		if err != nil {
			return nil, 0, err
		}
		if !forStatus {
			obj.Status = v1alpha1.ProcStatus{}
			v1alpha1.DefaultProc(&obj)
			if errs := v1alpha1.ValidateProc(&obj); len(errs) > 0 {
				return nil, 0, &v1alpha1.InvalidError{Errs: errs}
			}
		}
		body, err := json.Marshal(&obj)
		return body, rv, err

	case v1alpha1.KindTimer:
		var obj v1alpha1.Timer
		if err := strictUnmarshal(data, &obj); err != nil {
			return nil, 0, invalidBody(err)
		}
		if err := checkIdentity(&obj.TypeMeta, &obj.Metadata, kind, urlName); err != nil {
			return nil, 0, err
		}
		rv, err := parseRV(obj.Metadata.ResourceVersion)
		if err != nil {
			return nil, 0, err
		}
		if !forStatus {
			obj.Status = v1alpha1.TimerStatus{}
			v1alpha1.DefaultTimer(&obj)
			if errs := v1alpha1.ValidateTimer(&obj); len(errs) > 0 {
				return nil, 0, &v1alpha1.InvalidError{Errs: errs}
			}
		}
		body, err := json.Marshal(&obj)
		return body, rv, err

	case v1alpha1.KindEvent:
		var obj v1alpha1.Event
		if err := strictUnmarshal(data, &obj); err != nil {
			return nil, 0, invalidBody(err)
		}
		if err := checkIdentity(&obj.TypeMeta, &obj.Metadata, kind, urlName); err != nil {
			return nil, 0, err
		}
		rv, err := parseRV(obj.Metadata.ResourceVersion)
		if err != nil {
			return nil, 0, err
		}
		if !forStatus {
			if errs := v1alpha1.ValidateEvent(&obj); len(errs) > 0 {
				return nil, 0, &v1alpha1.InvalidError{Errs: errs}
			}
		}
		body, err := json.Marshal(&obj)
		return body, rv, err

	default:
		return nil, 0, fmt.Errorf("apiserver: no resource type for kind %q: %w", kind, v1alpha1.ErrNotFound)
	}
}

// checkIdentity enforces TypeMeta if present, stamps the canonical values,
// and reconciles the body's name with the URL's.
func checkIdentity(tm *v1alpha1.TypeMeta, meta *v1alpha1.ObjectMeta, kind, urlName string) error {
	if tm.APIVersion != "" && tm.APIVersion != v1alpha1.APIVersion {
		return &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
			Type: v1alpha1.ErrorTypeNotSupported, Field: "apiVersion", BadValue: tm.APIVersion,
			Detail: "supported values: " + v1alpha1.APIVersion,
		}}}
	}
	if tm.Kind != "" && tm.Kind != kind {
		return &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
			Type: v1alpha1.ErrorTypeInvalid, Field: "kind", BadValue: tm.Kind,
			Detail: fmt.Sprintf("does not match the request path (expected %s)", kind),
		}}}
	}
	tm.APIVersion, tm.Kind = v1alpha1.APIVersion, kind

	if meta.Name == "" {
		meta.Name = urlName
	}
	if meta.Name != urlName {
		return &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
			Type: v1alpha1.ErrorTypeInvalid, Field: "metadata.name", BadValue: meta.Name,
			Detail: fmt.Sprintf("does not match the request path (expected %q)", urlName),
		}}}
	}
	return nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, invalidBody(err)
	}
	return data, nil
}

// strictUnmarshal rejects unknown fields
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the object")
	}
	return nil
}

func invalidBody(err error) error {
	return &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
		Type: v1alpha1.ErrorTypeInvalid, Field: "<body>", Detail: err.Error(),
	}}}
}

func parseRV(rv string) (int64, error) {
	if rv == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(rv, 10, 64)
	if err != nil || n < 0 {
		return 0, &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
			Type: v1alpha1.ErrorTypeInvalid, Field: "metadata.resourceVersion", BadValue: rv,
			Detail: "not a resourceVersion this server issued",
		}}}
	}
	return n, nil
}

// specsEqual compares the spec subtrees of two serialized
// objects by JSON.
func specsEqual(a, b []byte) (bool, error) {
	var ae, be struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(a, &ae); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &be); err != nil {
		return false, err
	}
	var av, bv any
	if len(ae.Spec) > 0 {
		if err := json.Unmarshal(ae.Spec, &av); err != nil {
			return false, err
		}
	}
	if len(be.Spec) > 0 {
		if err := json.Unmarshal(be.Spec, &bv); err != nil {
			return false, err
		}
	}
	am, err := json.Marshal(av)
	if err != nil {
		return false, err
	}
	bm, err := json.Marshal(bv)
	if err != nil {
		return false, err
	}
	return bytes.Equal(am, bm), nil
}
