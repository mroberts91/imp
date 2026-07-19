// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ResourceRequirements declares compute limits for a Proc. Limits-only in M3
// (no requests). Deliberately a typed struct rather than a free-form
// ResourceList — fields map 1:1 to cgroup v2 controllers we write.
type ResourceRequirements struct {
	Limits ResourceLimits `json:"limits,omitzero"`
}

// ResourceLimits maps to cgroup v2 memory.max / cpu.max / cpu.weight /
// pids.max.
type ResourceLimits struct {
	// Memory is a byte quantity: bare decimal, or Ki/Mi/Gi (binary 1024).
	Memory string `json:"memory,omitempty"`
	// CPU is a hard quota: millicores ("500m") or whole cores ("2"),
	// mapped to cgroup v2 cpu.max over a fixed 100ms period. Empty = no
	// quota (cpu.max stays "max").
	CPU string `json:"cpu,omitempty"`
	// CPUWeight is cgroup v2 cpu.weight in [1, 10000]. Nil = kernel default (100).
	CPUWeight *int64 `json:"cpuWeight,omitempty"`
	// Pids is cgroup v2 pids.max. Nil = max (unlimited).
	Pids *int64 `json:"pids,omitempty"`
}

// Empty reports whether no limit fields are set.
func (in ResourceLimits) Empty() bool {
	return in.Memory == "" && in.CPU == "" && in.CPUWeight == nil && in.Pids == nil
}

// CPUMaxPeriodUsec is the fixed cpu.max period: 100ms, the kernel default.
const CPUMaxPeriodUsec = int64(100000)

// ParseCPUMax parses a CPU quota quantity — millicores ("500m") or whole
// cores ("2") — into the cpu.max quota in microseconds per CPUMaxPeriodUsec
// period. Fractional-core decimals are rejected; millicores exist for that.
func ParseCPUMax(s string) (quotaUsec int64, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	num, milli := strings.CutSuffix(s, "m")
	n, perr := strconv.ParseInt(num, 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("invalid quantity %q (want millicores like \"500m\" or whole cores like \"2\"): %w", s, perr)
	}
	if n <= 0 {
		return 0, fmt.Errorf("quantity must be greater than 0")
	}
	millicores := n
	if !milli {
		if n > (1<<63-1)/1000 {
			return 0, fmt.Errorf("quantity %q overflows", s)
		}
		millicores = n * 1000
	}
	if millicores > (1<<63-1)/100 {
		return 0, fmt.Errorf("quantity %q overflows", s)
	}
	// 1000m = one full core = one full period.
	return millicores * (CPUMaxPeriodUsec / 1000), nil
}

// ParseMemoryBytes parses a memory quantity into bytes.
// Supports bare decimal integers and binary suffixes Ki, Mi, Gi (case-sensitive).
func ParseMemoryBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	var mult uint64 = 1
	num := s
	switch {
	case strings.HasSuffix(s, "Ki"):
		mult = 1024
		num = strings.TrimSuffix(s, "Ki")
	case strings.HasSuffix(s, "Mi"):
		mult = 1024 * 1024
		num = strings.TrimSuffix(s, "Mi")
	case strings.HasSuffix(s, "Gi"):
		mult = 1024 * 1024 * 1024
		num = strings.TrimSuffix(s, "Gi")
	default:
		// Reject trailing letters that look like accidental SI suffixes (M, G, …).
		if len(s) > 0 {
			last := rune(s[len(s)-1])
			if unicode.IsLetter(last) {
				return 0, fmt.Errorf("unknown suffix in %q (want bare bytes, Ki, Mi, or Gi)", s)
			}
		}
	}
	if num == "" {
		return 0, fmt.Errorf("missing numeric part in %q", s)
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q: %w", s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("quantity must be greater than 0")
	}
	if mult > 1 && n > ^uint64(0)/mult {
		return 0, fmt.Errorf("quantity %q overflows", s)
	}
	return n * mult, nil
}
