// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package manifest

// fsnotify on the manifest directory plus a periodic full
// re-scan. Fork of kubelet's file
// config source (pkg/kubelet/config/file, Copyright The Kubernetes Authors,
// Apache-2.0).
//
// The manifest directory is declaratively authoritative for everything it
// ever created: every applied object is annotated impd.sh/managed-by:
// manifest, and the sweep deletes exactly the annotated objects whose
// (kind, name) no longer appears in any manifest. Objects without the
// annotation (impctl-applied) are never touched.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/recorder"
	"github.com/mroberts91/imp/pkg/client"
)

const (
	debounceWindow        = 100 * time.Millisecond
	defaultRescanInterval = 20 * time.Second
)

type Options struct {
	RescanInterval time.Duration
}

type Watcher struct {
	client      *client.Client
	dir         string
	rescanEvery time.Duration
	recorder    *recorder.Recorder
	log         *slog.Logger
}

func NewWatcher(c *client.Client, dir string, opts *Options, rec *recorder.Recorder) *Watcher {
	interval := defaultRescanInterval
	if opts != nil && opts.RescanInterval > 0 {
		interval = opts.RescanInterval
	}
	if rec == nil {
		rec = recorder.New(c, "manifest", nil)
	}
	return &Watcher{
		client:      c,
		dir:         dir,
		rescanEvery: interval,
		recorder:    rec,
		log:         slog.With("component", "manifest"),
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("manifest: creating %s: %w", w.dir, err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("manifest: starting fsnotify: %w", err)
	}
	defer fsw.Close()
	if err := fsw.Add(w.dir); err != nil {
		return fmt.Errorf("manifest: watching %s: %w", w.dir, err)
	}

	w.scanAndLog(ctx)

	ticker := time.NewTicker(w.rescanEvery)
	defer ticker.Stop()
	var pending <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-fsw.Events:
			if isManifestFile(ev.Name) {
				pending = time.After(debounceWindow)
			}
		case err := <-fsw.Errors:
			w.log.Warn("fsnotify error; the periodic re-scan still covers changes", "error", err)
		case <-pending:
			pending = nil
			w.scanAndLog(ctx)
		case <-ticker.C:
			w.scanAndLog(ctx)
		}
	}
}

func (w *Watcher) scanAndLog(ctx context.Context) {
	if err := w.scan(ctx); err != nil && ctx.Err() == nil {
		w.log.Error("manifest scan failed", "dir", w.dir, "error", err)
	}
}

type identity struct {
	Kind, Name string
}

// scan is one full pass. parse everything, apply everything, then sweep.
func (w *Watcher) scan(ctx context.Context) error {
	files, err := listManifestFiles(w.dir)
	if err != nil {
		return err
	}

	desired := map[identity][]byte{}   // annotated bodies to apply
	sources := map[identity]string{}   // winning relpath per identity
	failedSources := map[string]bool{} // files that did not parse this pass

	for _, path := range files {
		rel, err := filepath.Rel(w.dir, path)
		if err != nil {
			rel = path
		}
		objs, err := ParseFile(path)
		if err != nil {
			failedSources[rel] = true
			w.log.Warn("skipping unparseable manifest", "file", rel, "error", err)
			continue
		}
		for _, obj := range objs {
			id := identity{obj.Kind, obj.Name}
			if winner, dup := sources[id]; dup {
				w.log.Warn("duplicate identity; first file wins",
					"kind", id.Kind, "name", id.Name, "kept", winner, "ignored", rel)
				w.emitDuplicateEvent(ctx, id, winner, rel)
				continue
			}
			body, err := annotate(obj.Body, map[string]string{
				v1alpha1.AnnotationManagedBy:  v1alpha1.ManagedByManifest,
				v1alpha1.AnnotationSourcePath: rel,
			})
			if err != nil {
				w.log.Warn("annotating object failed", "kind", id.Kind, "name", id.Name, "error", err)
				continue
			}
			desired[id] = body
			sources[id] = rel
		}
	}

	// Apply phase. Failures are per-object
	ids := slices.SortedFunc(maps.Keys(desired), func(a, b identity) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Name, b.Name))
	})
	for _, id := range ids {
		if _, err := w.client.Apply(ctx, id.Kind, id.Name, desired[id]); err != nil {
			w.log.Warn("apply failed", "kind", id.Kind, "name", id.Name, "file", sources[id], "error", err)
		}
	}

	// Sweep phase. delete what this watcher created and the directory no
	// longer defines.
	for _, kind := range []string{v1alpha1.KindDaemon, v1alpha1.KindProc, v1alpha1.KindEvent} {
		list, err := w.client.ListRaw(ctx, kind)
		if err != nil {
			w.log.Warn("sweep skipped for kind", "kind", kind, "error", err)
			continue
		}
		for _, raw := range list.Items {
			var peek struct {
				Metadata v1alpha1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &peek); err != nil {
				continue
			}
			ann := peek.Metadata.Annotations
			if ann[v1alpha1.AnnotationManagedBy] != v1alpha1.ManagedByManifest {
				continue
			}
			if _, ok := desired[identity{kind, peek.Metadata.Name}]; ok {
				continue
			}
			if failedSources[ann[v1alpha1.AnnotationSourcePath]] {
				w.log.Warn("keeping object whose source file did not parse",
					"kind", kind, "name", peek.Metadata.Name, "file", ann[v1alpha1.AnnotationSourcePath])
				continue
			}
			if err := w.client.Delete(ctx, kind, peek.Metadata.Name); err != nil && !errors.Is(err, v1alpha1.ErrNotFound) {
				w.log.Warn("delete failed", "kind", kind, "name", peek.Metadata.Name, "error", err)
				continue
			}
			w.log.Info("deleted object no longer in manifests", "kind", kind, "name", peek.Metadata.Name)
		}
	}
	return nil
}

// emitDuplicateEvent records a duplicate-identity Warning. The recorder
// aggregates identical messages, so rescans bump count rather than creating
// a new Event each pass.
func (w *Watcher) emitDuplicateEvent(ctx context.Context, id identity, winner, loser string) {
	w.recorder.Eventf(ctx, v1alpha1.ObjectRef{Kind: id.Kind, Name: id.Name},
		v1alpha1.EventTypeWarning, v1alpha1.ReasonFailedValidation,
		"duplicate identity %s/%s: kept the definition in %s, ignored the one in %s",
		strings.ToLower(id.Kind), id.Name, winner, loser)
}

// annotate returns the body with the given annotations merged in.
func annotate(body []byte, kv map[string]string) ([]byte, error) {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	meta, ok := env["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		env["metadata"] = meta
	}
	ann, ok := meta["annotations"].(map[string]any)
	if !ok {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	for k, v := range kv {
		ann[k] = v
	}
	return json.Marshal(env)
}

func listManifestFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && isManifestFile(e.Name()) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(files)
	return files, nil
}

// isManifestFile accepts *.yaml and *.yml
func isManifestFile(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") || strings.HasSuffix(base, "~") {
		return false
	}
	ext := filepath.Ext(base)
	return ext == ".yaml" || ext == ".yml"
}
