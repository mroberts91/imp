// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

func TestParseMemoryBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"1", 1, false},
		{"1024", 1024, false},
		{"1Ki", 1024, false},
		{"256Mi", 256 * 1024 * 1024, false},
		{"2Gi", 2 * 1024 * 1024 * 1024, false},
		{" 64Mi ", 64 * 1024 * 1024, false},
		{"", 0, true},
		{"0", 0, true},
		{"0Mi", 0, true},
		{"256M", 0, true}, // SI rejected
		{"1GiB", 0, true},
		{"Ki", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseMemoryBytes(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMemoryBytes(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemoryBytes(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMemoryBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
