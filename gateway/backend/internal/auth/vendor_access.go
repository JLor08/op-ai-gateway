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
	All      bool
	Accounts []VendorAccessEntry
}

// cloneVendorAccess deep-copies the policy so a stored or returned Token never
// aliases the caller's slice. Entries are value types (scalars only), so a slice
// copy is a full copy.
func cloneVendorAccess(v VendorAccess) VendorAccess {
	if len(v.Accounts) == 0 {
		return VendorAccess{All: v.All}
	}
	return VendorAccess{All: v.All, Accounts: append([]VendorAccessEntry(nil), v.Accounts...)}
}
