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
