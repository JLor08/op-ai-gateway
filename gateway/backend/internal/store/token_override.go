// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"strings"
)

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
//
// PER ROW it writes the NARROWEST shape that can carry the row: a row with both
// listing switches false is written in the LEGACY string form ("name":"target"),
// and only a row that actually uses a switch gets the object form.
//
// That is a rollback contract, not a cosmetic choice. The pre-branch decoder
// (DecodeModelOverrideMap) unmarshals the column into map[string]string and
// returns nil the moment ANY value is an object — not just for that row, for
// the WHOLE map. Writing objects unconditionally would therefore turn a
// downgrade into silent data loss: every token saved under the new binary would
// lose ALL its per-model overrides, and the next save under the old binary would
// write "" over the column, making the loss permanent. Emitting the legacy form
// for rows that need nothing more keeps a deployment that never touches the new
// switches fully rollback-compatible.
//
// The residual is deliberate and narrow: a token with at least ONE row using
// `offer` or `hide_target` still loses its entire override map on a rollback,
// because the old decoder's all-or-nothing rejection is in the old binary and
// cannot be fixed from here. That case is opt-in — an operator who flipped a
// switch this branch introduced — where the unswitched case is everybody.
//
// Both shapes decode here (decodeOverrideRule), so this is invisible on the
// read side; only the bytes in the column change.
func EncodeModelOverrideRules(m map[string]ModelOverrideRule) string {
	if len(m) == 0 {
		return ""
	}
	out := make(map[string]any, len(m))
	for name, rule := range m {
		if !rule.Offer && !rule.HideTarget {
			out[name] = rule.To
			continue
		}
		out[name] = rule
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// AuthModelOverrideRules converts decoded store rules into auth.ModelOverrideRule,
// the mirror type carried on auth.Token.ModelOverrideRules. The two types are
// duplicated (identical shape, no shared definition) rather than auth.Token
// referencing ModelOverrideRule directly, because this package (store) already
// imports auth (for auth.Token itself, used by SQLStore.LookupBearer below) —
// having auth import store the other way would be an import cycle. This is the
// one place that bridges the two: every call site that builds an auth.Token
// from a store.TokenRecord's decoded rules goes through here.
//
// The conversion is a TYPE CONVERSION (auth.ModelOverrideRule(rule)), not a
// field-by-field literal: the two types have an identical underlying
// structure (same field names, types, and order — struct tags are ignored
// for conversion purposes since Go 1.8), so the conversion is legal today.
// If either struct later gains/loses/reorders a field without the other
// following, the types stop being identical and this line fails to
// COMPILE — turning drift between the two duplicated types into a build
// error instead of a silently-dropped field.
func AuthModelOverrideRules(rules map[string]ModelOverrideRule) map[string]auth.ModelOverrideRule {
	if len(rules) == 0 {
		return nil
	}
	out := make(map[string]auth.ModelOverrideRule, len(rules))
	for name, rule := range rules {
		out[name] = auth.ModelOverrideRule(rule)
	}
	return out
}
