// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "op-ai-gateway/internal/auth"

// TokenVendorPrefix reports the public model-name prefix a token serves `acc`
// under, and whether `acc` is allowed for the token at all. It is the single
// source both the listing overlay and the resolver use, so advertised names
// always equal routable names. The effective public name of a model `m` is
// prefix + m.UpstreamModel.
//
//   - All==true: every account allowed, under its own ModelPrefix (native).
//   - An entry with OverrideEnabled==false: allowed, native ModelPrefix.
//   - An entry with OverrideEnabled==true: allowed, the override prefix (which may
//     be "" => bare upstream names).
//   - Not All and not listed: denied.
func TokenVendorPrefix(access auth.VendorAccess, acc VendorAccount) (prefix string, allowed bool) {
	if access.All {
		return acc.ModelPrefix, true
	}
	for _, e := range access.Accounts {
		if e.AccountID != acc.ID {
			continue
		}
		if e.OverrideEnabled {
			return e.OverridePrefix, true
		}
		return acc.ModelPrefix, true
	}
	return "", false
}
