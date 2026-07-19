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

func TestParseCPUMax(t *testing.T) {
	cases := []struct {
		in      string
		want    int64 // quota µs per 100000µs period
		wantErr bool
	}{
		{"500m", 50000, false},
		{"1000m", 100000, false},
		{"1", 100000, false},
		{"2", 200000, false},
		{"250m", 25000, false},
		{"1m", 100, false},
		{" 2 ", 200000, false},
		{"", 0, true},
		{"0", 0, true},
		{"0m", 0, true},
		{"-500m", 0, true},
		{"0.5", 0, true}, // fractional cores rejected; that's what millicores are for
		{"500M", 0, true},
		{"m", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseCPUMax(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCPUMax(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCPUMax(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCPUMax(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRlimitValues(t *testing.T) {
	cases := []struct {
		name         string
		r            Rlimit
		wantS, wantH int64
	}{
		{"both", Rlimit{Soft: new(int64(100)), Hard: new(int64(200))}, 100, 200},
		{"soft only sets both", Rlimit{Soft: new(int64(100))}, 100, 100},
		{"hard only sets both", Rlimit{Hard: new(int64(200))}, 200, 200},
		{"infinity", Rlimit{Soft: new(RlimitInfinity), Hard: new(RlimitInfinity)}, -1, -1},
	}
	for _, tc := range cases {
		s, h := tc.r.Values()
		if s != tc.wantS || h != tc.wantH {
			t.Errorf("%s: Values() = (%d, %d), want (%d, %d)", tc.name, s, h, tc.wantS, tc.wantH)
		}
	}
}
