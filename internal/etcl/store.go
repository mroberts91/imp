// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package etcl is imp's durable object store: SQLite (WAL mode) behind a
// narrow interface - get, list, create/update/delete with optimistic
// concurrency, and watch with resumable positions via a global monotonic
// resourceVersion counter.
//
// Because impd is a single process, watch notification is in-memory push.
// SQLite exists for durability and watch replay/resume, not for change
// detection.
//
// The store deals in raw JSON bodies (json.RawMessage), not typed objects:
// its only direct consumer is the api-server, which validates typed objects
// before writing and streams stored bytes back out without re-decoding.
// etcl's envelope knowledge is limited to metadata
//
// Logical forks (Copyright The Kubernetes Authors / the kine authors,
// Apache-2.0)
package etcl

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/mroberts91/imp/api/v1alpha1"
)

const schema = `
CREATE TABLE IF NOT EXISTS objects (
	kind             TEXT    NOT NULL,
	name             TEXT    NOT NULL,
	uid              TEXT    NOT NULL,
	resource_version INTEGER NOT NULL,
	generation       INTEGER NOT NULL,
	body             TEXT    NOT NULL,
	PRIMARY KEY (kind, name)
);
CREATE TABLE IF NOT EXISTS changelog (
	rv         INTEGER PRIMARY KEY,
	event_type TEXT    NOT NULL,
	kind       TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	body       TEXT    NOT NULL,
	ts         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_changelog_kind_rv ON changelog(kind, rv);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);
`

// Options configures a Store. The zero value means "use defaults".
type Options struct {
	RetainRows   int
	RetainWindow time.Duration
	CompactEvery int
	Now          func() time.Time
}

func (o *Options) withDefaults() Options {
	out := Options{}
	if o != nil {
		out = *o
	}
	if out.RetainRows <= 0 {
		out.RetainRows = 1000
	}
	if out.RetainWindow <= 0 {
		out.RetainWindow = 5 * time.Minute
	}
	if out.CompactEvery <= 0 {
		out.CompactEvery = 100
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

// ErrLocked reports that another process holds the store's data directory.
// The socket-level guard in apiserver.Listen catches a second impd on the
// same socket; this catches the split-brain the 2026-07-11 incident hit —
// a second impd pointed at the SAME data dir through a DIFFERENT socket
// (two writers on one SQLite file corrupt rv monotonicity).
var ErrLocked = errors.New("etcl: data directory is locked by another process")

// Store is the object store
type Store struct {
	db   *sql.DB
	opts Options
	lock *os.File

	mu            sync.RWMutex
	closed        bool
	rv            int64
	compactedRV   int64
	writesSince   int
	watchers      map[int64]*watcher
	nextWatcherID int64
	pumps         sync.WaitGroup
}

func Open(path string, opts *Options) (*Store, error) {
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("etcl: opening %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		lock.Close()
		return nil, fmt.Errorf("etcl: initializing schema: %w", err)
	}
	s := &Store{db: db, opts: opts.withDefaults(), lock: lock, watchers: map[int64]*watcher{}}
	if s.rv, err = s.loadMeta("rv"); err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	if s.compactedRV, err = s.loadMeta("compacted_rv"); err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) loadMeta(key string) (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("etcl: loading meta %q: %w", key, err)
	}
	return v, nil
}

// Close stops all watchers and closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, w := range s.watchers {
		w.signalStop()
	}
	s.watchers = map[int64]*watcher{}
	s.mu.Unlock()

	s.pumps.Wait()
	err := s.db.Close()
	// Releasing the flock last: the data dir stays claimed until the
	// database is actually closed.
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}

// Get returns the current body of the object, or ErrNotFound.
func (s *Store) Get(kind, name string) (json.RawMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errClosed
	}
	var body []byte
	err := s.db.QueryRow(`SELECT body FROM objects WHERE kind = ? AND name = ?`, kind, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(kind, name)
	}
	if err != nil {
		return nil, fmt.Errorf("etcl: get %s %q: %w", kind, name, err)
	}
	return body, nil
}

// List returns a snapshot of every object of the kind (sorted by name) and
// the resourceVersion the snapshot is consistent at.
func (s *Store) List(kind string) ([]json.RawMessage, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, 0, errClosed
	}
	rows, err := s.db.Query(`SELECT body FROM objects WHERE kind = ? ORDER BY name`, kind)
	if err != nil {
		return nil, 0, fmt.Errorf("etcl: list %s: %w", kind, err)
	}
	defer rows.Close()
	var objs []json.RawMessage
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, 0, fmt.Errorf("etcl: list %s: %w", kind, err)
		}
		objs = append(objs, body)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("etcl: list %s: %w", kind, err)
	}
	return objs, s.rv, nil
}

// Create stores a new object, assigning uid, creationTimestamp,
// resourceVersion, and generation=1. Returns ErrAlreadyExists or ErrInvalid.
func (s *Store) Create(kind string, body []byte) (json.RawMessage, error) {
	env, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	name := nameOf(env)
	if name == "" {
		return nil, missingName()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	var one int
	err = s.db.QueryRow(`SELECT 1 FROM objects WHERE kind = ? AND name = ?`, kind, name).Scan(&one)
	if err == nil {
		return nil, fmt.Errorf("etcl: %s %q: %w", kind, name, v1alpha1.ErrAlreadyExists)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("etcl: create %s %q: %w", kind, name, err)
	}

	newRV := s.rv + 1
	meta := metadataOf(env)
	meta["uid"] = uuid.NewString()
	meta["creationTimestamp"] = s.opts.Now().UTC().Format(time.RFC3339)
	meta["generation"] = int64(1)
	meta["resourceVersion"] = strconv.FormatInt(newRV, 10)
	newBody, err := marshalEnvelope(env)
	if err != nil {
		return nil, err
	}

	err = s.writeTxLocked(newRV, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO objects (kind, name, uid, resource_version, generation, body) VALUES (?, ?, ?, ?, ?, ?)`,
			kind, name, meta["uid"], newRV, 1, string(newBody)); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO changelog (rv, event_type, kind, name, body, ts) VALUES (?, ?, ?, ?, ?, ?)`,
			newRV, v1alpha1.WatchAdded, kind, name, string(newBody), s.opts.Now().Unix())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("etcl: create %s %q: %w", kind, name, err)
	}
	s.dispatchLocked(changeEvent{rv: newRV, kind: kind, event: v1alpha1.WatchEvent{Type: v1alpha1.WatchAdded, Object: newBody}})
	s.maybeCompactLocked()
	return newBody, nil
}

// Update replaces the object through the spec path
func (s *Store) Update(kind, name string, body []byte, expectedRV int64) (json.RawMessage, error) {
	return s.update(kind, name, body, expectedRV, updateSpec)
}

// UpdateStatus replaces only the object's status, everything else is
// preserved from the stored object.
func (s *Store) UpdateStatus(kind, name string, body []byte, expectedRV int64) (json.RawMessage, error) {
	return s.update(kind, name, body, expectedRV, updateStatusOnly)
}

// Delete removes the object.
func (s *Store) Delete(kind, name string, expectedRV int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	row, err := s.getRowLocked(kind, name)
	if err != nil {
		return err
	}
	if expectedRV != 0 && expectedRV != row.rv {
		return conflict(kind, name, expectedRV, row.rv)
	}

	newRV := s.rv + 1
	lastEnv, err := parseEnvelope(row.body)
	if err != nil {
		return err
	}
	metadataOf(lastEnv)["resourceVersion"] = strconv.FormatInt(newRV, 10)
	lastBody, err := marshalEnvelope(lastEnv)
	if err != nil {
		return err
	}

	err = s.writeTxLocked(newRV, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM objects WHERE kind = ? AND name = ?`, kind, name); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO changelog (rv, event_type, kind, name, body, ts) VALUES (?, ?, ?, ?, ?, ?)`,
			newRV, v1alpha1.WatchDeleted, kind, name, string(lastBody), s.opts.Now().Unix())
		return err
	})
	if err != nil {
		return fmt.Errorf("etcl: delete %s %q: %w", kind, name, err)
	}
	s.dispatchLocked(changeEvent{rv: newRV, kind: kind, event: v1alpha1.WatchEvent{Type: v1alpha1.WatchDeleted, Object: lastBody}})
	s.maybeCompactLocked()
	return nil
}

// GuaranteedUpdate is the retry loop every in-process read-modify-write goes
// through.
func (s *Store) GuaranteedUpdate(kind, name string, tryUpdate func(current json.RawMessage) ([]byte, error)) (json.RawMessage, error) {
	for {
		current, err := s.Get(kind, name)
		if err != nil {
			return nil, err
		}
		rv, err := resourceVersionOf(current)
		if err != nil {
			return nil, err
		}
		updated, err := tryUpdate(current)
		if err != nil {
			return nil, err
		}
		result, err := s.update(kind, name, updated, rv, updateFull)
		if errors.Is(err, v1alpha1.ErrConflict) {
			continue
		}
		return result, err
	}
}

type updateMode int

const (
	updateSpec updateMode = iota
	updateStatusOnly
	updateFull
)

func (s *Store) update(kind, name string, body []byte, expectedRV int64, mode updateMode) (json.RawMessage, error) {
	env, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	if n := nameOf(env); n != "" && n != name {
		return nil, &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
			Type: v1alpha1.ErrorTypeInvalid, Field: "metadata.name", BadValue: n,
			Detail: fmt.Sprintf("does not match the target name %q", name),
		}}}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	row, err := s.getRowLocked(kind, name)
	if err != nil {
		return nil, err
	}
	if expectedRV != 0 && expectedRV != row.rv {
		return nil, conflict(kind, name, expectedRV, row.rv)
	}
	curEnv, err := parseEnvelope(row.body)
	if err != nil {
		return nil, err
	}
	curMeta := metadataOf(curEnv)

	newEnv := env
	switch mode {
	case updateStatusOnly:
		newEnv = curEnv
		if st, ok := env["status"]; ok {
			newEnv["status"] = st
		} else {
			delete(newEnv, "status")
		}
	case updateSpec:
		if st, ok := curEnv["status"]; ok {
			newEnv["status"] = st
		} else {
			delete(newEnv, "status")
		}
	case updateFull:
		// Caller's body is trusted wholesale.
	}

	meta := metadataOf(newEnv)
	meta["name"] = name
	meta["uid"] = row.uid
	if ct, ok := curMeta["creationTimestamp"]; ok {
		meta["creationTimestamp"] = ct
	} else {
		delete(meta, "creationTimestamp")
	}
	gen := row.generation
	if mode != updateStatusOnly && !jsonEqual(newEnv["spec"], curEnv["spec"]) {
		gen++
	}
	meta["generation"] = gen

	// No-op short-circuit: with the current resourceVersion stamped in, a
	// byte-identical body means an unchanged object.
	meta["resourceVersion"] = strconv.FormatInt(row.rv, 10)
	unchanged, err := marshalEnvelope(newEnv)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(unchanged, row.body) {
		return row.body, nil
	}

	newRV := s.rv + 1
	meta["resourceVersion"] = strconv.FormatInt(newRV, 10)
	newBody, err := marshalEnvelope(newEnv)
	if err != nil {
		return nil, err
	}

	err = s.writeTxLocked(newRV, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE objects SET resource_version = ?, generation = ?, body = ? WHERE kind = ? AND name = ?`,
			newRV, gen, string(newBody), kind, name); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO changelog (rv, event_type, kind, name, body, ts) VALUES (?, ?, ?, ?, ?, ?)`,
			newRV, v1alpha1.WatchModified, kind, name, string(newBody), s.opts.Now().Unix())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("etcl: update %s %q: %w", kind, name, err)
	}
	s.dispatchLocked(changeEvent{rv: newRV, kind: kind, event: v1alpha1.WatchEvent{Type: v1alpha1.WatchModified, Object: newBody}})
	s.maybeCompactLocked()
	return newBody, nil
}

// writeTxLocked runs the statements plus the counter bump in one
// transaction, and advances the in-memory counter only after commit.
func (s *Store) writeTxLocked(newRV int64, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (key, value) VALUES ('rv', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		newRV); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.rv = newRV
	return nil
}

type objectRow struct {
	uid        string
	rv         int64
	generation int64
	body       []byte
}

func (s *Store) getRowLocked(kind, name string) (objectRow, error) {
	var row objectRow
	err := s.db.QueryRow(
		`SELECT uid, resource_version, generation, body FROM objects WHERE kind = ? AND name = ?`,
		kind, name).Scan(&row.uid, &row.rv, &row.generation, &row.body)
	if errors.Is(err, sql.ErrNoRows) {
		return row, notFound(kind, name)
	}
	if err != nil {
		return row, fmt.Errorf("etcl: get %s %q: %w", kind, name, err)
	}
	return row, nil
}

func parseEnvelope(body []byte) (map[string]any, error) {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("etcl: parsing object body: %w", err)
	}
	if env == nil {
		env = map[string]any{}
	}
	return env, nil
}

func marshalEnvelope(env map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("etcl: marshaling object body: %w", err)
	}
	return b, nil
}

func metadataOf(env map[string]any) map[string]any {
	if m, ok := env["metadata"].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	env["metadata"] = m
	return m
}

func nameOf(env map[string]any) string {
	if m, ok := env["metadata"].(map[string]any); ok {
		if n, ok := m["name"].(string); ok {
			return n
		}
	}
	return ""
}

func resourceVersionOf(body json.RawMessage) (int64, error) {
	env, err := parseEnvelope(body)
	if err != nil {
		return 0, err
	}
	rvs, ok := metadataOf(env)["resourceVersion"].(string)
	if !ok {
		return 0, fmt.Errorf("etcl: object has no metadata.resourceVersion")
	}
	rv, err := strconv.ParseInt(rvs, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("etcl: parsing resourceVersion %q: %w", rvs, err)
	}
	return rv, nil
}

// jsonEqual compares two decoded JSON values by serialization
// (encoding/json sorts map keys).
func jsonEqual(a, b any) bool {
	am, aerr := json.Marshal(a)
	bm, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(am, bm)
}

var errClosed = errors.New("etcl: store is closed")

func notFound(kind, name string) error {
	return fmt.Errorf("etcl: %s %q: %w", kind, name, v1alpha1.ErrNotFound)
}

func conflict(kind, name string, expected, actual int64) error {
	return fmt.Errorf("etcl: %s %q: expected resourceVersion %d, have %d: %w",
		kind, name, expected, actual, v1alpha1.ErrConflict)
}

func missingName() error {
	return &v1alpha1.InvalidError{Errs: v1alpha1.ErrorList{{
		Type: v1alpha1.ErrorTypeRequired, Field: "metadata.name",
	}}}
}
