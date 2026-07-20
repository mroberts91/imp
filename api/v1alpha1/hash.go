// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
)

// HashProcTemplate computes the impd.sh/template-hash value for a template.
func HashProcTemplate(t *ProcTemplate) string {
	b, err := json.Marshal(t)
	if err != nil {
		panic(fmt.Sprintf("v1alpha1: marshaling ProcTemplate: %v", err))
	}
	h := fnv.New32a()
	h.Write(b)
	return fmt.Sprintf("%08x", h.Sum32())
}

// HashConfigSpec computes the content hash of a Config's spec (M8-g). Go
// marshals maps with sorted keys, so the canonical JSON — hence this hash —
// is deterministic. Mode participates: changing the file mode rolls the
// Daemon too, since it changes the bytes execd materializes.
func HashConfigSpec(spec *ConfigSpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		panic(fmt.Sprintf("v1alpha1: marshaling ConfigSpec: %v", err))
	}
	h := fnv.New32a()
	h.Write(b)
	return fmt.Sprintf("%08x", h.Sum32())
}

// RevisionRef pairs a referenced Config's name with the hash of its resolved
// spec (HashConfigSpec), in template-reference order. It is a component of the
// Proc revision hash, distinct from the spec-facing ConfigRef (proc.go) that
// manifests carry — renamed from ConfigRef in M9 to free that name for the
// union type (M9-n). Path never participates here: a ref's path changes the
// template's canonical JSON, so it already rolls the Daemon via the template
// hash.
type RevisionRef struct {
	Name string
	Hash string
}

// HashDaemonRevision composes a Daemon's Proc revision from its template hash
// and the resolved specs of its referenced Configs, in reference order
// (specs[i] corresponds to refs[i]). Only each ref's Name and resolved content
// hash participate — the path is already folded into templateHash. Callers
// resolve each Config's spec from wherever they read it — the controller's
// informer cache or the CLI's API client — so centralizing the composition
// here keeps those callers from drifting on how the revision is built.
func HashDaemonRevision(templateHash string, refs []ConfigRef, specs []*ConfigSpec) string {
	revRefs := make([]RevisionRef, len(refs))
	for i := range refs {
		revRefs[i] = RevisionRef{Name: refs[i].Name, Hash: HashConfigSpec(specs[i])}
	}
	return HashConfigRevision(templateHash, revRefs)
}

// HashConfigRevision combines a template hash with resolved config hashes into
// the revision suffix used for Proc names and the impd.sh/config-hash label
// (M8-g). Names participate, so renaming a reference rolls the Daemon even
// when two Configs share content. With no refs it returns templateHash
// unchanged — the M7 byte-identical guarantee: a daemon that gains no config
// identity keeps its exact pre-M8 Proc names and labels, and does not roll on
// upgrade. (The DaemonController shortcuts the empty case before ever calling
// this; the guard here keeps the function total and the guarantee explicit.)
func HashConfigRevision(templateHash string, refs []RevisionRef) string {
	if len(refs) == 0 {
		return templateHash
	}
	h := fnv.New32a()
	fmt.Fprint(h, templateHash)
	for _, r := range refs {
		// NUL delimiters keep (name, hash) boundaries unambiguous, so no two
		// distinct ref lists can serialize to the same byte stream.
		fmt.Fprintf(h, "\x00%s\x00%s", r.Name, r.Hash)
	}
	return fmt.Sprintf("%08x", h.Sum32())
}
