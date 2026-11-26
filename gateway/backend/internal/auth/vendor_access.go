// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auth

// VendorAccessEntry is one opted-in vendor account on a token, with its optional
// per-token model-name prefix override. OverrideEnabled==true means OverridePrefix
// REPLACES the account's own ModelPrefix for this token (an empty OverridePrefix
// then serves the account's models under their bare upstream names).
type VendorAccessEntry struct {
	AccountID       string
	OverrideEnabled bool
	OverridePrefix  string
}

// VendorAccess is a token's vendor-account (Anbieter) access policy, carried on
// Token and persisted as api_tokens.vendor_provider_access JSON.
//   - All==true: every active owner account under its own ModelPrefix (Accounts
//     ignored; future accounts auto-included).
//   - All==false: only Accounts; an empty slice means NO vendor access (strict
//     opt-in default).
type VendorAccess struct {
	All bool
	// IncludeGroups==true (with All) additionally covers every vendor account
	// group the token's user owns, under the group's own ModelPrefix.
	IncludeGroups bool
	Accounts      []VendorAccessEntry
	// Groups names individual vendor account groups, with the same optional
	// prefix override as an account entry.
	Groups []VendorAccessGroupEntry
}

// VendorAccessGroupEntry is one vendor account group named on a token, with its
// optional per-token model-name prefix override (same semantics as
// VendorAccessEntry). A group id that no longer exists simply matches nothing.
type VendorAccessGroupEntry struct {
	GroupID         string
	OverrideEnabled bool
	OverridePrefix  string
}

// cloneVendorAccess deep-copies the policy so a stored or returned Token never
// aliases the caller's slices. Entries are value types (scalars only), so a slice
// copy is a full copy.
func cloneVendorAccess(v VendorAccess) VendorAccess {
	out := VendorAccess{All: v.All, IncludeGroups: v.IncludeGroups}
	if len(v.Accounts) > 0 {
		out.Accounts = append([]VendorAccessEntry(nil), v.Accounts...)
	}
	if len(v.Groups) > 0 {
		out.Groups = append([]VendorAccessGroupEntry(nil), v.Groups...)
	}
	return out
}
