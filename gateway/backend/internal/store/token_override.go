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

// ModelOverrideRule is one row of a token's model-override map: the gateway
// model a requested name resolves to, plus whether that requested name is
// advertised in the model listing (Offer) and whether the target's own name is
// dropped from it (HideTarget). Both switches affect the LISTING only — the
// listing is a display, never an access control: a hidden target stays callable
// under its real name, exactly as before this feature.
type ModelOverrideRule struct {
	To         string `json:"to"`
	Offer      bool   `json:"offer,omitempty"`
	HideTarget bool   `json:"hide_target,omitempty"`
}

// DecodeModelOverrideRules parses api_tokens.model_override_map. It accepts BOTH
// shapes: the object form written since this feature, and the plain
// requested->model string map written before it. The legacy form yields rules
// with both switches false, so no pre-existing token changes behavior — that is
// why this needs no data migration.
//
// An empty, blank or malformed value yields nil (no per-model overrides);
// malformed is treated as "none" rather than an error so a bad row never breaks
// token resolution. Rules with a blank target are dropped: they could not route
// anywhere, and advertising such an alias would offer a dead name.
func DecodeModelOverrideRules(s string) map[string]ModelOverrideRule {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	out := make(map[string]ModelOverrideRule, len(raw))
	for name, value := range raw {
		rule, ok := decodeOverrideRule(value)
		if !ok || strings.TrimSpace(rule.To) == "" {
			continue
		}
		rule.To = strings.TrimSpace(rule.To)
		out[name] = rule
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeOverrideRule reads one entry in either shape.
func decodeOverrideRule(value json.RawMessage) (ModelOverrideRule, bool) {
	var legacy string
	if err := json.Unmarshal(value, &legacy); err == nil {
		return ModelOverrideRule{To: legacy}, true
	}
	var rule ModelOverrideRule
	if err := json.Unmarshal(value, &rule); err != nil {
		return ModelOverrideRule{}, false
	}
	return rule, true
}

// EncodeModelOverrideRules serializes the rules into the JSON string stored in
// api_tokens.model_override_map. An empty/nil map yields "" (the column
// default) so "no entries" round-trips as the empty string, not "{}".
func EncodeModelOverrideRules(m map[string]ModelOverrideRule) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
