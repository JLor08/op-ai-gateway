// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"strings"
)

// vendor_provider_access wire shape. Kept local; auth.VendorAccess is the
// in-memory type (store imports auth, auth cannot import store).
type vendorAccessWire struct {
	All      bool                    `json:"all,omitempty"`
	Accounts []vendorAccessEntryWire `json:"accounts,omitempty"`
}
type vendorAccessEntryWire struct {
	AccountID      string              `json:"account_id"`
	PrefixOverride *prefixOverrideWire `json:"prefix_override,omitempty"`
}
type prefixOverrideWire struct {
	Enabled bool   `json:"enabled"`
	Value   string `json:"value"`
}

// DecodeVendorAccess parses api_tokens.vendor_provider_access. Blank or malformed
// yields the strict default (All=false, no accounts) — a bad row never breaks
// token resolution. Entries with a blank account_id are dropped, and a
// prefix_override object only counts when its enabled flag is set.
func DecodeVendorAccess(s string) auth.VendorAccess {
	if strings.TrimSpace(s) == "" {
		return auth.VendorAccess{}
	}
	var w vendorAccessWire
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return auth.VendorAccess{}
	}
	out := auth.VendorAccess{All: w.All}
	for _, e := range w.Accounts {
		id := strings.TrimSpace(e.AccountID)
		if id == "" {
			continue
		}
		entry := auth.VendorAccessEntry{AccountID: id}
		// presence AND enabled <=> override on: a stored {"enabled":false,...}
		// object is a switched-off override, not an active one.
		if e.PrefixOverride != nil && e.PrefixOverride.Enabled {
			entry.OverrideEnabled = true
			entry.OverridePrefix = e.PrefixOverride.Value
		}
		out.Accounts = append(out.Accounts, entry)
	}
	return out
}

// EncodeVendorAccess serializes the policy. The strict default encodes to "" (the
// column default) so "no access" round-trips as the empty string, not "{}".
func EncodeVendorAccess(v auth.VendorAccess) string {
	if !v.All && len(v.Accounts) == 0 {
		return ""
	}
	w := vendorAccessWire{All: v.All}
	for _, e := range v.Accounts {
		ew := vendorAccessEntryWire{AccountID: e.AccountID}
		if e.OverrideEnabled {
			ew.PrefixOverride = &prefixOverrideWire{Enabled: true, Value: e.OverridePrefix}
		}
		w.Accounts = append(w.Accounts, ew)
	}
	b, err := json.Marshal(w)
	if err != nil {
		return ""
	}
	return string(b)
}
