// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package etcl

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// openTest opens a store on a per-test database file.
func openTest(t *testing.T, opts *Options) *Store {
	t.Helper()
	return openTestAt(t, filepath.Join(t.TempDir(), "etcl.db"), opts)
}

func openTestAt(t *testing.T, path string, opts *Options) *Store {
	t.Helper()
	s, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func decode(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decoding body %s: %v", body, err)
	}
	return m
}

func metaOf(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	m, ok := decode(t, body)["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("body has no metadata: %s", body)
	}
	return m
}

func rvOf(t *testing.T, body json.RawMessage) int64 {
	t.Helper()
	rv, err := resourceVersionOf(body)
	if err != nil {
		t.Fatalf("resourceVersionOf: %v", err)
	}
	return rv
}

func genOf(t *testing.T, body json.RawMessage) int64 {
	t.Helper()
	g, ok := metaOf(t, body)["generation"].(float64)
	if !ok {
		t.Fatalf("body has no generation: %s", body)
	}
	return int64(g)
}

func obj(name string, spec, status string) []byte {
	b := fmt.Sprintf(`{"metadata":{"name":%q}`, name)
	if spec != "" {
		b += `,"spec":` + spec
	}
	if status != "" {
		b += `,"status":` + status
	}
	return []byte(b + `}`)
}

func mustCreate(t *testing.T, s *Store, kind string, body []byte) json.RawMessage {
	t.Helper()
	out, err := s.Create(kind, body)
	if err != nil {
		t.Fatalf("Create(%s, %s): %v", kind, body, err)
	}
	return out
}

func TestCreateAssignsIdentity(t *testing.T) {
	s := openTest(t, nil)
	out := mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))

	meta := metaOf(t, out)
	if meta["uid"] == "" || meta["uid"] == nil {
		t.Error("uid not assigned")
	}
	if meta["creationTimestamp"] == nil {
		t.Error("creationTimestamp not assigned")
	}
	if rvOf(t, out) != 1 {
		t.Errorf("resourceVersion = %d, want 1", rvOf(t, out))
	}
	if genOf(t, out) != 1 {
		t.Errorf("generation = %d, want 1", genOf(t, out))
	}

	got, err := s.Get("Widget", "a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(out) {
		t.Errorf("Get returned %s, Create returned %s", got, out)
	}
}

func TestCreateErrors(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("a", "", ""))

	if _, err := s.Create("Widget", obj("a", "", "")); !errors.Is(err, v1alpha1.ErrAlreadyExists) {
		t.Errorf("duplicate create: err = %v, want ErrAlreadyExists", err)
	}
	if _, err := s.Create("Widget", []byte(`{"spec":{}}`)); !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("nameless create: err = %v, want ErrInvalid", err)
	}
	if _, err := s.Get("Widget", "missing"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("get missing: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateCAS(t *testing.T) {
	s := openTest(t, nil)
	created := mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))
	rv := rvOf(t, created)

	if _, err := s.Update("Widget", "a", obj("a", `{"n":2}`, ""), rv+999); !errors.Is(err, v1alpha1.ErrConflict) {
		t.Errorf("stale rv: err = %v, want ErrConflict", err)
	}
	updated, err := s.Update("Widget", "a", obj("a", `{"n":2}`, ""), rv)
	if err != nil {
		t.Fatalf("Update with correct rv: %v", err)
	}
	if rvOf(t, updated) <= rv {
		t.Errorf("rv did not advance: %d -> %d", rv, rvOf(t, updated))
	}
	// expectedRV 0 skips the check.
	if _, err := s.Update("Widget", "a", obj("a", `{"n":3}`, ""), 0); err != nil {
		t.Fatalf("blind update: %v", err)
	}
	if _, err := s.Update("Widget", "missing", obj("missing", `{}`, ""), 0); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("update missing: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Update("Widget", "a", obj("OTHER", `{}`, ""), 0); !errors.Is(err, v1alpha1.ErrInvalid) {
		t.Errorf("name mismatch: err = %v, want ErrInvalid", err)
	}
}

func TestUpdateGenerationAndStatusSemantics(t *testing.T) {
	s := openTest(t, nil)
	created := mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))
	uid := metaOf(t, created)["uid"]
	ct := metaOf(t, created)["creationTimestamp"]

	// Seed a status through the status path.
	withStatus, err := s.UpdateStatus("Widget", "a", obj("a", "", `{"phase":"Running"}`), 0)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if genOf(t, withStatus) != 1 {
		t.Errorf("status write bumped generation to %d", genOf(t, withStatus))
	}

	// A spec change bumps generation, preserves status and identity.
	specChanged, err := s.Update("Widget", "a", obj("a", `{"n":2}`, `{"phase":"IGNORED"}`), 0)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if genOf(t, specChanged) != 2 {
		t.Errorf("generation = %d, want 2", genOf(t, specChanged))
	}
	env := decode(t, specChanged)
	if status := env["status"].(map[string]any); status["phase"] != "Running" {
		t.Errorf("spec-path update touched status: %v", env["status"])
	}
	meta := metaOf(t, specChanged)
	if meta["uid"] != uid || meta["creationTimestamp"] != ct {
		t.Errorf("identity not preserved: %v", meta)
	}

	// A metadata-only change does not bump generation.
	labeled, err := s.Update("Widget", "a", []byte(`{"metadata":{"name":"a","labels":{"x":"y"}},"spec":{"n":2}}`), 0)
	if err != nil {
		t.Fatalf("Update labels: %v", err)
	}
	if genOf(t, labeled) != 2 {
		t.Errorf("metadata-only update bumped generation to %d", genOf(t, labeled))
	}

	// The status path ignores spec and metadata in the caller's body.
	statusOnly, err := s.UpdateStatus("Widget", "a", []byte(`{"metadata":{"name":"a","labels":{"evil":"yes"}},"spec":{"n":99},"status":{"phase":"Failed"}}`), 0)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	env = decode(t, statusOnly)
	if spec := env["spec"].(map[string]any); spec["n"] != float64(2) {
		t.Errorf("status-path update touched spec: %v", env["spec"])
	}
	if labels := metaOf(t, statusOnly)["labels"].(map[string]any); labels["evil"] != nil {
		t.Errorf("status-path update touched labels: %v", labels)
	}
	if status := env["status"].(map[string]any); status["phase"] != "Failed" {
		t.Errorf("status not updated: %v", env["status"])
	}
	if genOf(t, statusOnly) != 2 {
		t.Errorf("UpdateStatus bumped generation to %d", genOf(t, statusOnly))
	}
}

func TestUpdateNoOpCausesNoRVChurn(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))
	first, err := s.Update("Widget", "a", obj("a", `{"n":1}`, ""), 0)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, listRV, err := s.List("Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Re-applying the identical body must not move anything.
	second, err := s.Update("Widget", "a", obj("a", `{"n":1}`, ""), 0)
	if err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	if rvOf(t, second) != rvOf(t, first) {
		t.Errorf("no-op update churned rv: %d -> %d", rvOf(t, first), rvOf(t, second))
	}
	_, listRV2, err := s.List("Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if listRV2 != listRV {
		t.Errorf("no-op update advanced the counter: %d -> %d", listRV, listRV2)
	}
}

func TestDelete(t *testing.T) {
	s := openTest(t, nil)
	created := mustCreate(t, s, "Widget", obj("a", "", ""))
	rv := rvOf(t, created)

	if err := s.Delete("Widget", "a", rv+999); !errors.Is(err, v1alpha1.ErrConflict) {
		t.Errorf("stale rv delete: err = %v, want ErrConflict", err)
	}
	if err := s.Delete("Widget", "a", rv); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("Widget", "a"); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("get after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete("Widget", "a", 0); !errors.Is(err, v1alpha1.ErrNotFound) {
		t.Errorf("double delete: err = %v, want ErrNotFound", err)
	}
}

func TestListSnapshot(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("b", "", ""))
	mustCreate(t, s, "Widget", obj("a", "", ""))
	mustCreate(t, s, "Gadget", obj("c", "", ""))

	objs, listRV, err := s.List("Widget")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("List returned %d objects, want 2", len(objs))
	}
	// Sorted by name.
	if metaOf(t, objs[0])["name"] != "a" || metaOf(t, objs[1])["name"] != "b" {
		t.Errorf("List not sorted by name: %v, %v", metaOf(t, objs[0])["name"], metaOf(t, objs[1])["name"])
	}
	if listRV != 3 {
		t.Errorf("listRV = %d, want 3", listRV)
	}
}

func TestGuaranteedUpdateConcurrent(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("counter", `{"n":0}`, ""))

	const goroutines, increments = 8, 25
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range increments {
				_, err := s.GuaranteedUpdate("Widget", "counter", func(current json.RawMessage) ([]byte, error) {
					var env map[string]any
					if err := json.Unmarshal(current, &env); err != nil {
						return nil, err
					}
					spec := env["spec"].(map[string]any)
					spec["n"] = spec["n"].(float64) + 1
					return json.Marshal(env)
				})
				if err != nil {
					t.Errorf("GuaranteedUpdate: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	final, err := s.Get("Widget", "counter")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	n := decode(t, final)["spec"].(map[string]any)["n"].(float64)
	if int(n) != goroutines*increments {
		t.Errorf("n = %d, want %d (lost updates)", int(n), goroutines*increments)
	}
	// Every increment was a spec change: generation counted them all.
	if genOf(t, final) != int64(goroutines*increments)+1 {
		t.Errorf("generation = %d, want %d", genOf(t, final), goroutines*increments+1)
	}
}

func TestGuaranteedUpdateNoOp(t *testing.T) {
	s := openTest(t, nil)
	created := mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))

	out, err := s.GuaranteedUpdate("Widget", "a", func(current json.RawMessage) ([]byte, error) {
		return current, nil
	})
	if err != nil {
		t.Fatalf("GuaranteedUpdate: %v", err)
	}
	if rvOf(t, out) != rvOf(t, created) {
		t.Errorf("identity closure churned rv: %d -> %d", rvOf(t, created), rvOf(t, out))
	}
}

func TestGuaranteedUpdateCanWriteStatus(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("a", `{"n":1}`, `{"count":1}`))

	// The recorder's count++ pattern: a status write through the full path.
	out, err := s.GuaranteedUpdate("Widget", "a", func(current json.RawMessage) ([]byte, error) {
		var env map[string]any
		if err := json.Unmarshal(current, &env); err != nil {
			return nil, err
		}
		status := env["status"].(map[string]any)
		status["count"] = status["count"].(float64) + 1
		return json.Marshal(env)
	})
	if err != nil {
		t.Fatalf("GuaranteedUpdate: %v", err)
	}
	if count := decode(t, out)["status"].(map[string]any)["count"].(float64); count != 2 {
		t.Errorf("count = %v, want 2", count)
	}
	if genOf(t, out) != 1 {
		t.Errorf("status-only GuaranteedUpdate bumped generation to %d", genOf(t, out))
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etcl.db")
	s := openTestAt(t, path, nil)
	mustCreate(t, s, "Widget", obj("a", `{"n":1}`, ""))
	mustCreate(t, s, "Widget", obj("b", `{"n":2}`, ""))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2 := openTestAt(t, path, nil)
	got, err := s2.Get("Widget", "a")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if decode(t, got)["spec"].(map[string]any)["n"] != float64(1) {
		t.Errorf("object corrupted across reopen: %s", got)
	}

	// The counter continues monotonically - no rv reuse after restart.
	third := mustCreate(t, s2, "Widget", obj("c", "", ""))
	if rvOf(t, third) != 3 {
		t.Errorf("rv after reopen = %d, want 3", rvOf(t, third))
	}

	// The changelog also survived: a watch from 0 replays everything.
	events, cancel, err := s2.Watch("Widget", 0)
	if err != nil {
		t.Fatalf("Watch after reopen: %v", err)
	}
	defer cancel()
	for _, want := range []string{"a", "b", "c"} {
		ev := recvEvent(t, events)
		if ev.Type != v1alpha1.WatchAdded || metaOf(t, ev.Object)["name"] != want {
			t.Errorf("replay event = %s %v, want ADDED %q", ev.Type, metaOf(t, ev.Object)["name"], want)
		}
	}
}

func TestOperationsAfterClose(t *testing.T) {
	s := openTest(t, nil)
	mustCreate(t, s, "Widget", obj("a", "", ""))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Get("Widget", "a"); err == nil {
		t.Error("Get after Close succeeded")
	}
	if _, err := s.Create("Widget", obj("b", "", "")); err == nil {
		t.Error("Create after Close succeeded")
	}
	if _, _, err := s.Watch("Widget", 0); err == nil {
		t.Error("Watch after Close succeeded")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestOpenRefusesLockedDataDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etcl.db")

	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	// A second store on the same data dir — the split-brain a socket-level
	// guard cannot see — must be refused while the first is open.
	if _, err := Open(path, nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open err = %v, want ErrLocked", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close releases the claim: reopening succeeds.
	again, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	again.Close()
}
