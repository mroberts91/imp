// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/cache"
	"github.com/mroberts91/imp/internal/queue"
	"github.com/mroberts91/imp/pkg/client"
)

// revision is a Daemon's current Proc identity (M8-g). template is the pure
// impd.sh/template-hash; config is the combined hash of resolved Config
// content ("" when the template references no Configs); suffix is what a Proc
// name ends with and the impd.sh/config-hash label carries — the config hash
// when present, else the template hash. A no-config Daemon therefore produces
// byte-identical Proc names and labels to pre-M8 (config == "" everywhere).
type revision struct {
	template string
	config   string
	suffix   string
}

// reasonConfigMissing is the Progressing condition reason while a Daemon holds
// on an absent Config (shares the string with the Event reason).
const reasonConfigMissing = v1alpha1.ReasonConfigMissing

// resolveRevision computes the Daemon's Proc identity. When the template names
// Configs, each is resolved from the config informer and hashed into the
// combined revision; missing names the first unresolved Config (empty ⇒ all
// resolved). A missing Config is not an error here — the caller holds (M8-h)
// and self-heals when the Config appears.
func (c *Controller) resolveRevision(d *v1alpha1.Daemon) (rev revision, missing string) {
	templateHash := v1alpha1.HashProcTemplate(&d.Spec.Template)
	refs := d.Spec.Template.Spec.Configs
	if len(refs) == 0 {
		return revision{template: templateHash, config: "", suffix: templateHash}, ""
	}
	specs := make([]*v1alpha1.ConfigSpec, len(refs))
	for i, ref := range refs {
		raw, ok := c.configs.GetByKey(v1alpha1.KindConfig + "/" + ref.Name)
		if !ok {
			return revision{}, ref.Name
		}
		var cfg v1alpha1.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			// The apiserver validated what it stored; unreadable bytes are
			// wire corruption. Treat as missing so a resync heals it.
			c.log.Warn("skipping unreadable config in cache", "kind", v1alpha1.KindConfig, "key", ref.Name, "error", err)
			return revision{}, ref.Name
		}
		specs[i] = &cfg.Spec
	}
	configHash := v1alpha1.HashDaemonRevision(templateHash, refs, specs)
	return revision{template: templateHash, config: configHash, suffix: configHash}, ""
}

// rollReason classifies why the stale set is stale: a template-hash mismatch
// (TemplateChanged) takes precedence; otherwise the roll is config-only
// (ConfigChanged, M8). Only meaningful when stale is non-empty.
func rollReason(stale []v1alpha1.Proc, rev revision) string {
	for i := range stale {
		if stale[i].Metadata.Labels[v1alpha1.LabelTemplateHash] != rev.template {
			return v1alpha1.ReasonTemplateChanged
		}
	}
	return v1alpha1.ReasonConfigChanged
}

// holdForMissingConfig writes the Daemon's status while a referenced Config is
// absent (M8-h): observed Procs are reported honestly, but Progressing is
// True/ConfigMissing and nothing is created or deleted this pass. The hold
// self-heals when the Config appears — its watch event re-enqueues this Daemon
// (EnqueueReferencingDaemons). updatedReplicas is 0: without a resolvable
// revision the controller cannot claim any Proc is up to date.
func (c *Controller) holdForMissingConfig(ctx context.Context, d *v1alpha1.Daemon, missing string) error {
	procs := c.allProcsFor(d.Metadata.Name)
	nowT := c.clock.Now()
	now := v1alpha1.NewTime(nowT)
	replicas := *d.Spec.Replicas
	gen := d.Metadata.Generation

	var ready, available int32
	for i := range procs {
		if procReady(&procs[i]) {
			ready++
		}
		if ok, _ := procAvailable(&procs[i], d.Spec.MinReadySeconds, nowT); ok {
			available++
		}
	}

	avail := availableCondition(available, replicas, gen, now)
	prog := v1alpha1.Condition{
		Type:               v1alpha1.ConditionTypeProgressing,
		Status:             v1alpha1.ConditionTrue,
		Reason:             reasonConfigMissing,
		Message:            fmt.Sprintf("waiting for config %q to exist", missing),
		ObservedGeneration: gen,
		LastTransitionTime: now,
		LastUpdateTime:     now,
	}

	err := client.RetryOnConflict(func() error {
		fresh, err := c.client.GetDaemon(ctx, d.Metadata.Name)
		if err != nil {
			return err
		}
		if fresh.Metadata.UID != d.Metadata.UID {
			// Deleted and recreated mid-pass; this observation is the dead
			// incarnation's. The new one's watch events drive fresh passes.
			return nil
		}
		fresh.Status.ObservedGeneration = gen
		fresh.Status.Replicas = int32(len(procs))
		fresh.Status.UpdatedReplicas = 0
		fresh.Status.ReadyReplicas = ready
		fresh.Status.AvailableReplicas = available
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, avail)
		v1alpha1.SetStatusCondition(&fresh.Status.Conditions, prog)
		_, err = c.client.UpdateDaemonStatus(ctx, fresh)
		return err
	})
	if errors.Is(err, v1alpha1.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("updating status of %s/%s: %w", v1alpha1.KindDaemon, d.Metadata.Name, err)
	}
	return nil
}

// allProcsFor returns every Proc labeled with the Daemon's name, regardless of
// revision — used by the missing-config hold, which cannot partition into
// current/stale without a resolvable revision.
func (c *Controller) allProcsFor(daemonName string) []v1alpha1.Proc {
	var procs []v1alpha1.Proc
	for _, raw := range c.procs.List() {
		var p v1alpha1.Proc
		if err := json.Unmarshal(raw, &p); err != nil {
			c.log.Warn("skipping unreadable proc in cache", "kind", v1alpha1.KindProc, "error", err)
			continue
		}
		if p.Metadata.Labels[v1alpha1.LabelDaemonName] == daemonName {
			procs = append(procs, p)
		}
	}
	return procs
}

// EnqueueReferencingDaemons returns a Config-informer handler that enqueues
// every Daemon whose template references the fired Config (M8 §4.3). A Config
// create/edit/delete thus re-reconciles its referencing Daemons promptly: an
// edit rolls them, an appearance heals a ConfigMissing hold. Deletion memory
// is unnecessary (unlike EnqueueOwner) — the reference lives on the Daemon, so
// the same scan finds referencing Daemons whether the Config exists or not.
// O(daemons) per Config event, fine on a single host.
func EnqueueReferencingDaemons(daemons *cache.Store, q queue.RateLimitingInterface) func(key string) {
	log := slog.With("component", componentName, "kind", v1alpha1.KindConfig)
	return func(key string) {
		kind, name, ok := strings.Cut(key, "/")
		if !ok || kind != v1alpha1.KindConfig {
			return
		}
		for _, raw := range daemons.List() {
			// Configs decodes as []ConfigRef so the union's custom unmarshal
			// handles both bare-string and object-form refs — an object-form
			// ref must not silently fail to decode, which would break
			// roll-on-change for daemons that use a path: ref (M9-o).
			var d struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Template struct {
						Spec struct {
							Configs []v1alpha1.ConfigRef `json:"configs"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				log.Warn("skipping daemon with unreadable spec", "error", err)
				continue
			}
			if slices.ContainsFunc(d.Spec.Template.Spec.Configs, func(r v1alpha1.ConfigRef) bool {
				return r.Name == name
			}) {
				q.Add(v1alpha1.KindDaemon + "/" + d.Metadata.Name)
			}
		}
	}
}
