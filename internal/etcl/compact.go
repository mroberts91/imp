// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package etcl

// Compaction bookkeeping in the manner of kine (github.com/k3s-io/kine,
// Apache-2.0): the changelog is pruned to a contiguous suffix and
// compactedRV.
import (
	"database/sql"
	"errors"
	"log/slog"
)

// Compact prunes changelog rows outside both retention rules: rows are kept
// if they are among the newest RetainRows OR younger than RetainWindow.
// Compaction also runs automatically every CompactEvery writes.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	return s.compactLocked()
}

func (s *Store) maybeCompactLocked() {
	s.writesSince++
	if s.writesSince < s.opts.CompactEvery {
		return
	}
	s.writesSince = 0
	if err := s.compactLocked(); err != nil {
		slog.Warn("changelog compaction failed", "component", "etcl", "error", err)
	}
}

func (s *Store) compactLocked() error {
	// byCount: the smallest rv the last-N rule keeps. Fewer than RetainRows
	// rows means nothing is compactable.
	var byCount int64
	err := s.db.QueryRow(
		`SELECT rv FROM changelog ORDER BY rv DESC LIMIT 1 OFFSET ?`,
		s.opts.RetainRows-1).Scan(&byCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// byTime: the smallest rv the age rule keeps. If every row is stale, the
	// count rule alone governs. Taking min() rather than trusting timestamps
	// to be ordered.
	cutoff := s.opts.Now().Add(-s.opts.RetainWindow).Unix()
	byTime := s.rv + 1
	var minRecent sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(rv) FROM changelog WHERE ts >= ?`, cutoff).Scan(&minRecent); err != nil {
		return err
	}
	if minRecent.Valid {
		byTime = minRecent.Int64
	}

	threshold := min(byCount, byTime)
	if threshold-1 <= s.compactedRV {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(`DELETE FROM changelog WHERE rv < ?`, threshold); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (key, value) VALUES ('compacted_rv', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		threshold-1); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.compactedRV = threshold - 1
	return nil
}
