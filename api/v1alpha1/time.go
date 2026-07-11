// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"time"
)

// Time wraps time.Time to serialize as RFC3339 at second precision,
// mirroring Kubernetes' metav1.Time rendering. The zero value marshals as
// null.
type Time struct {
	time.Time
}

func NewTime(t time.Time) Time {
	return Time{t.Truncate(time.Second)}
}

func (t Time) Equal(u Time) bool {
	return t.Time.Equal(u.Time)
}

// MarshalJSON renders RFC3339 in UTC at second precision, or null for the
// zero value.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339))
}

// UnmarshalJSON parses RFC3339; null yields the zero value.
func (t *Time) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}
