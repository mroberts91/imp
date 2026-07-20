// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package configfiles materializes a Proc's referenced Config content onto
// disk before spawn (M8) and removes it on teardown. Path-less refs land in a
// per-proc directory rooted at {data-dir}/configs (execd injects
// IMP_CONFIG_DIR pointing there); a ref with a path: lands its files in that
// absolute directory instead (M9-b). It is the only place a Config's content
// touches the filesystem.
package configfiles

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/execd/childsetup"
)

// defaultFileMode is the mode of a materialized file when the Config sets no
// mode (M8-e). Never defaulted in the API, so a Config's content hash stays
// stable across upgrades.
const defaultFileMode os.FileMode = 0o644

// pathsSubdir under root holds the per-proc written-paths records — impd's
// private bookkeeping of which absolute path: files each Proc materialized, so
// teardown can clean them up and the conflict check can tell "ours" from
// "someone else's". Proc names are DNS labels, so ".paths" never collides.
const pathsSubdir = ".paths"

// ConfigGetter resolves a Config by name. execd resolves fresh at spawn (M8-k)
// rather than holding a Config informer; *client.Client satisfies this.
type ConfigGetter interface {
	GetConfig(ctx context.Context, name string) (*v1alpha1.Config, error)
}

// Store materializes Config content under root ({data-dir}/configs) using
// getter to resolve Configs. It is safe for the single-writer-per-proc use in
// execd (each Proc's directory is written only by that Proc's worker).
type Store struct {
	root   string
	getter ConfigGetter
	log    *slog.Logger
}

// New returns a Store rooted at root (e.g. {data-dir}/configs), resolving
// Configs through getter.
func New(root string, getter ConfigGetter) *Store {
	return &Store{root: root, getter: getter, log: slog.With("component", "execd", "subsystem", "configfiles")}
}

// Dir is the per-proc config directory — the value of IMP_CONFIG_DIR. It is
// deterministic from the Proc name, so a D1-adopted Proc's inherited
// IMP_CONFIG_DIR stays valid without re-materialization.
func (s *Store) Dir(proc string) string {
	return filepath.Join(s.root, proc)
}

func (s *Store) pathRecordFile(proc string) string {
	return filepath.Join(s.root, pathsSubdir, proc+".json")
}

// Materialize writes every file of every Config referenced by p (M8-k
// version-skew note: a Proc labeled revision H may transiently get content H′
// if the Config changed since reconcile — the DaemonController then observes
// H′ ≠ H and rolls again, converging; this is the level-triggered self-heal).
//
// A path-less ref writes under Dir(proc)/<configName>/. A ref with a path:
// writes into that absolute directory (M9-b): the directory is created 0755 if
// absent (world-traversable for a shared /etc/myapp) but an existing directory
// is left untouched, and a file imp did not write is never overwritten —
// materialization fails with ErrConfigPathConflict instead. The absolute paths
// written are recorded so teardown can remove them and a restart can rewrite
// its own files freely.
//
// Ownership (M8-e/M8-l): when the Proc drops to user:/group:, the per-proc
// dir, per-config dirs, and files are chowned to the resolved uid/gid; a shared
// path: directory is not chowned (it may serve several procs) but its files
// are. A no-refs Proc writes nothing. Called on every spawn — a restart heals
// hand-edited drift.
func (s *Store) Materialize(ctx context.Context, p *v1alpha1.Proc) (err error) {
	if len(p.Spec.Configs) == 0 {
		return nil
	}
	uid, gid, chown, err := childsetup.ChownIDs(p.Spec.User, p.Spec.Group)
	if err != nil {
		return err
	}
	procDir := s.Dir(p.Metadata.Name)

	// Root traversable-but-not-listable so any dropped-privilege process can
	// reach its own (private) per-proc directory. Never chowned or listable.
	if err := ensureDir(s.root, 0o711, -1, -1, false); err != nil {
		return err
	}
	// Private bookkeeping dir for path records (impd-only, procs never read it).
	if err := ensureDir(filepath.Join(s.root, pathsSubdir), 0o700, -1, -1, false); err != nil {
		return err
	}

	// The path: files this Proc wrote last time — allowed to be overwritten by
	// this pass, and the discriminator for the conflict check.
	prevPaths := s.loadPathRecord(p.Metadata.Name)
	prevSet := make(map[string]bool, len(prevPaths))
	for _, path := range prevPaths {
		prevSet[path] = true
	}

	// Persist the written-paths record on EVERY exit, success or failure: a
	// file this pass wrote must be recorded as ours, or a retry after a
	// mid-pass failure would reject imp's own leftover file as a foreign
	// ConfigPathConflict and wedge the Daemon permanently. On failure we keep
	// the prior record too (its files were not cleaned up below).
	var written []string // absolute path: files written this pass
	done := false
	defer func() {
		record := written
		if !done {
			record = unionPaths(prevPaths, written)
		}
		if saveErr := s.savePathRecord(p.Metadata.Name, record); saveErr != nil && err == nil {
			err = saveErr
		}
	}()

	// Fresh per-proc dir each spawn so a file a Config no longer declares does
	// not linger from a previous revision.
	if err := os.RemoveAll(procDir); err != nil {
		return fmt.Errorf("clearing config dir %s: %w", procDir, err)
	}
	if err := ensureDir(procDir, 0o700, uid, gid, chown); err != nil {
		return err
	}

	for _, ref := range p.Spec.Configs {
		cfg, err := s.getter.GetConfig(ctx, ref.Name)
		if err != nil {
			return fmt.Errorf("resolving config %q: %w", ref.Name, err)
		}
		files := collectFiles(&cfg.Spec)

		if ref.Path == nil {
			cfgDir := filepath.Join(procDir, ref.Name)
			if err := ensureDir(cfgDir, 0o700, uid, gid, chown); err != nil {
				return err
			}
			if err := s.writeAll(&cfg.Spec, files, cfgDir, ref.Name, uid, gid, chown, nil, nil); err != nil {
				return err
			}
			continue
		}

		destDir := *ref.Path
		if err := ensureSharedDir(destDir); err != nil {
			return err
		}
		if err := s.writeAll(&cfg.Spec, files, destDir, ref.Name, uid, gid, chown, prevSet, &written); err != nil {
			return err
		}
	}

	// Success: remove path: files a previous revision wrote but this one no
	// longer does, and let the deferred save record exactly `written`.
	for _, old := range prevPaths {
		if !slices.Contains(written, old) {
			if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
				s.log.Warn("removing stale config path file", "path", old, "error", err)
			}
		}
	}
	done = true
	return nil
}

// unionPaths returns a and b concatenated with duplicates removed, order
// preserved (a first).
func unionPaths(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, p := range list {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// writeAll writes files into destDir with per-file modes. When record is
// non-nil (a path: ref) it enforces the no-overwrite rule against ownedSet and
// appends every written path to *record.
func (s *Store) writeAll(spec *v1alpha1.ConfigSpec, files map[string][]byte, destDir, cfgName string, uid, gid int, chown bool, ownedSet map[string]bool, record *[]string) error {
	// Sorted for determinism (no functional dependence on order).
	for _, file := range slices.Sorted(maps.Keys(files)) {
		mode, err := resolveFileMode(spec, file)
		if err != nil {
			return fmt.Errorf("config %q: %w", cfgName, err)
		}
		dst := filepath.Join(destDir, file)
		if record != nil {
			// Refuse to overwrite a file imp did not write for this Proc.
			if _, statErr := os.Lstat(dst); statErr == nil && !ownedSet[dst] {
				return fmt.Errorf("config %q would overwrite %s: %w", cfgName, dst, v1alpha1.ErrConfigPathConflict)
			}
		}
		if err := writeFile(dst, files[file], mode, uid, gid, chown); err != nil {
			return fmt.Errorf("writing %s: %w", dst, err)
		}
		if record != nil {
			*record = append(*record, dst)
		}
	}
	return nil
}

// Remove deletes a Proc's per-proc config directory and any absolute path:
// files it wrote (files only, never their shared directories), then the path
// record. Teardown on Proc-object deletion. Idempotent.
func (s *Store) Remove(proc string) error {
	for _, path := range s.loadPathRecord(proc) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.log.Warn("removing config path file on teardown", "path", path, "error", err)
		}
	}
	if err := os.Remove(s.pathRecordFile(proc)); err != nil && !os.IsNotExist(err) {
		s.log.Warn("removing config path record on teardown", "proc", proc, "error", err)
	}
	return os.RemoveAll(s.Dir(proc))
}

// loadPathRecord reads the Proc's recorded path: files. A missing or unreadable
// record is treated as empty (the next Materialize rewrites it).
func (s *Store) loadPathRecord(proc string) []string {
	data, err := os.ReadFile(s.pathRecordFile(proc))
	if err != nil {
		return nil
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		s.log.Warn("unreadable config path record; treating as empty", "proc", proc, "error", err)
		return nil
	}
	return paths
}

// savePathRecord writes the Proc's path: files atomically; an empty list
// removes the record entirely.
func (s *Store) savePathRecord(proc string, paths []string) error {
	file := s.pathRecordFile(proc)
	if len(paths) == 0 {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// collectFiles merges a Config's text (Data) and binary (BinaryData) files into
// one filename → bytes map. Validation guarantees the key sets are disjoint.
func collectFiles(spec *v1alpha1.ConfigSpec) map[string][]byte {
	out := make(map[string][]byte, len(spec.Data)+len(spec.BinaryData))
	for name, content := range spec.Data {
		out[name] = []byte(content)
	}
	maps.Copy(out, spec.BinaryData)
	return out
}

// ensureDir creates path with exactly mode (chmod defeats the umask) and,
// when chown is set, gives it to uid/gid.
func ensureDir(path string, mode os.FileMode, uid, gid int, chown bool) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if chown {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}
	}
	return nil
}

// ensureSharedDir creates a path: destination directory 0755 if absent, but
// leaves an existing directory untouched (it may be an admin-managed /etc dir
// serving several procs — do not weaken its mode or flip its owner). Every
// directory component imp *creates* is chmod'd to 0755 to defeat the umask, so
// a dropped-privilege proc can traverse even a multi-level brand-new path;
// pre-existing ancestors are never touched.
func ensureSharedDir(path string) error {
	// Collect the components that don't yet exist (deepest first), stopping at
	// the deepest existing ancestor.
	var missing []string
	for p := path; ; {
		if _, err := os.Stat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p { // reached the filesystem root
			break
		}
		p = parent
	}
	if len(missing) == 0 {
		return nil // already exists — untouched
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	for _, p := range missing {
		if err := os.Chmod(p, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}

// writeFile writes content to path atomically (temp file in the same dir, then
// rename), applying mode and, when chown is set, uid/gid — before the rename,
// so no reader ever sees the file at the wrong ownership or mode.
func writeFile(path string, content []byte, mode os.FileMode, uid, gid int, chown bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if chown {
		if err := tmp.Chown(uid, gid); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// resolveFileMode picks a file's mode: its per-file Modes entry, else the
// Config's Mode, else the default (M9-c resolution order).
func resolveFileMode(spec *v1alpha1.ConfigSpec, filename string) (os.FileMode, error) {
	if m, ok := spec.Modes[filename]; ok {
		return parseMode(m)
	}
	return resolveMode(spec.Mode)
}

// resolveMode turns a Config's octal mode string (validated at apply) into a
// file mode; nil means the default (M8-e).
func resolveMode(mode *string) (os.FileMode, error) {
	if mode == nil {
		return defaultFileMode, nil
	}
	return parseMode(*mode)
}

func parseMode(mode string) (os.FileMode, error) {
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", mode, err)
	}
	return os.FileMode(m), nil
}
