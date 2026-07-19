// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readProcStartTicks returns /proc/<pid>/stat field 22 (starttime). Captured
// at spawn for D1 re-adoption identity checks.
func readProcStartTicks(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// Comm may contain spaces/parens: find the last ')' then split the rest.
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0, fmt.Errorf("proc %d/stat: malformed", pid)
	}
	fields := strings.Fields(s[i+2:])
	// After comm: state is fields[0] (= field 3), starttime is field 22
	// overall → index 22-3 = 19 in the post-comm slice.
	const starttimeIdx = 19
	if len(fields) <= starttimeIdx {
		return 0, fmt.Errorf("proc %d/stat: too few fields", pid)
	}
	return strconv.ParseInt(fields[starttimeIdx], 10, 64)
}
