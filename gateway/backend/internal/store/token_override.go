// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"encoding/json"
	"strings"
)

// DecodeModelOverrideMap parses the api_tokens.model_override_map JSON string into
// a requested-model -> gateway-model map. An empty/blank/malformed value yields
// nil (no per-model overrides) — malformed is treated as "none" rather than an
// error so a bad row never breaks token resolution.
func DecodeModelOverrideMap(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// EncodeModelOverrideMap serializes a requested-model -> gateway-model map into the
// JSON string stored in api_tokens.model_override_map. An empty/nil map yields ""
// (the column default) so "no entries" round-trips as the empty string, not "{}".
func EncodeModelOverrideMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
