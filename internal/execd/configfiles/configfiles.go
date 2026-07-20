// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package configfiles materializes a Proc's referenced Config content onto
// disk before spawn (M8) and removes it on teardown. Files live under a
// per-proc directory rooted at {data-dir}/configs; execd injects
// IMP_CONFIG_DIR pointing at that directory. It is the only place a Config's
// content touches the filesystem, and it writes only under --data-dir.
package configfiles

import (
	"context"
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

// ConfigGetter resolves a Config by name. execd resolves fresh at spawn (M8-k)
// rather than holding a Config informer; *client.Client satisfies this.
type ConfigGetter interface {
	GetConfig(ctx context.Context, name string) (*v1alpha1.Config, error)
}

// defaultFileMode is the mode of a materialized file when the Config sets no
// mode (M8-e). Never defaulted in the API, so a Config's content hash stays
// stable across upgrades.
const defaultFileMode os.FileMode = 0o644

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

// Materialize writes every file of every Config referenced by p under
// Dir(proc)/<configName>/, resolving current content from the store (M8-k
// version-skew note: a Proc labeled revision H may transiently get content H′
// if the Config changed since reconcile — the DaemonController then observes
// H′ ≠ H and rolls again, converging; this is the level-triggered self-heal).
//
// Ownership (M8-e/M8-l): when the Proc drops to user:/group:, the per-proc
// dir, per-config dirs, and files are chowned to the resolved uid/gid so the
// process can read even a restrictive mode; the process runs as impd
// otherwise and no chown is attempted. The per-proc dir is 0700 (private to
// its owner), and the shared root is 0711 (traversable but not listable) so a
// dropped-privilege process can reach its own dir. A no-refs Proc writes
// nothing. Called on every spawn — a restart heals hand-edited drift.
func (s *Store) Materialize(ctx context.Context, p *v1alpha1.Proc) error {
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
	// Fresh per-proc dir each spawn so a file a Config no longer declares does
	// not linger from a previous revision.
	if err := os.RemoveAll(procDir); err != nil {
		return fmt.Errorf("clearing config dir %s: %w", procDir, err)
	}
	if err := ensureDir(procDir, 0o700, uid, gid, chown); err != nil {
		return err
	}

	for _, name := range p.Spec.Configs {
		cfg, err := s.getter.GetConfig(ctx, name)
		if err != nil {
			return fmt.Errorf("resolving config %q: %w", name, err)
		}
		mode, err := resolveMode(cfg.Spec.Mode)
		if err != nil {
			return fmt.Errorf("config %q: %w", name, err)
		}
		cfgDir := filepath.Join(procDir, name)
		if err := ensureDir(cfgDir, 0o700, uid, gid, chown); err != nil {
			return err
		}
		// Sorted for determinism (no functional dependence on order).
		for _, file := range slices.Sorted(maps.Keys(cfg.Spec.Data)) {
			path := filepath.Join(cfgDir, file)
			if err := writeFile(path, cfg.Spec.Data[file], mode, uid, gid, chown); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}
		}
	}
	return nil
}

// Remove deletes a Proc's per-proc config directory (teardown on Proc-object
// deletion). Idempotent: removing an absent directory is not an error, so it
// is safe to call for Procs that referenced no Configs.
func (s *Store) Remove(proc string) error {
	return os.RemoveAll(s.Dir(proc))
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

// writeFile writes content to path atomically (temp file in the same dir, then
// rename), applying mode and, when chown is set, uid/gid — before the rename,
// so no reader ever sees the file at the wrong ownership or mode.
func writeFile(path, content string, mode os.FileMode, uid, gid int, chown bool) error {
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
	if _, err := tmp.WriteString(content); err != nil {
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

// resolveMode turns a Config's octal mode string (validated 3–4 octal digits
// at apply) into a file mode; nil means the default (M8-e).
func resolveMode(mode *string) (os.FileMode, error) {
	if mode == nil {
		return defaultFileMode, nil
	}
	m, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", *mode, err)
	}
	return os.FileMode(m), nil
}
