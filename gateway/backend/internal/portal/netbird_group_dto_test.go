// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"testing"
)

// TestDecodeNetbirdGroupIDs covers the ServerDTO tolerant decode of the opaque
// netbird_group_ids column: valid JSON -> refs; empty/malformed/null -> a
// non-nil empty slice (never an error, serializes as [] not null).
func TestDecodeNetbirdGroupIDs(t *testing.T) {
	t.Run("valid JSON decodes", func(t *testing.T) {
		got := decodeNetbirdGroupIDs(`[{"id":"gA","name":"A"},{"id":"gB","name":"B"}]`)
		if len(got) != 2 || got[0] != (NetbirdGroupRefDTO{ID: "gA", Name: "A"}) || got[1] != (NetbirdGroupRefDTO{ID: "gB", Name: "B"}) {
			t.Fatalf("decode = %+v, want [{gA A} {gB B}]", got)
		}
	})

	for _, tc := range []struct {
		name, raw string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"garbage", "not json"},
		{"null", "null"},
		{"wrong shape", `{"id":"x"}`},
	} {
		t.Run(tc.name+" -> non-nil empty", func(t *testing.T) {
			got := decodeNetbirdGroupIDs(tc.raw)
			if got == nil {
				t.Fatalf("decode(%q) = nil, want non-nil empty slice", tc.raw)
			}
			if len(got) != 0 {
				t.Fatalf("decode(%q) = %+v, want empty", tc.raw, got)
			}
			b, err := json.Marshal(got)
			if err != nil || string(b) != "[]" {
				t.Fatalf("marshal empty = %q (err %v), want []", b, err)
			}
		})
	}
}
