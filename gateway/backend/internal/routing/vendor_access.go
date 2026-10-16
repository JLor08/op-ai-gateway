// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "op-ai-gateway/internal/auth"

// TokenOwnsVendorAccount reports whether acc is one of token's OWN vendor accounts:
// the token carries a user identity and that user is the account's owner. A service
// token (or any token with no user) owns nothing, and a blank owner id matches nobody,
// so the answer never turns on two empty strings being equal. It is the one place the
// own-versus-shared distinction is made, for routing, the model listing and the
// save-time validation of a token policy alike.
func TokenOwnsVendorAccount(token auth.Token, acc VendorAccount) bool {
	return token.UserID != "" && acc.OwnerUserID == token.UserID
}

// TokenVendorPrefix reports the public model-name prefix a token serves `acc`
// under, and whether `acc` is allowed for the token at all. It is the single
// source both the listing overlay and the resolver use, so advertised names
// always equal routable names. The effective public name of a model `m` is
// prefix + m.UpstreamModel.
//
// It decides USE only. Whether the token's principal may REACH `acc` at all is the
// routing grant's question (VendorAccountEnumerator), asked before this one. For
// an account the principal does not own (a SHARED one) the policy must additionally
// NAME the account (the per-token opt-in, ADR-050): borrowing another owner's
// credential is always a deliberate, named choice.
//
//   - All==true: every account the token's principal OWNS is allowed, under its own
//     ModelPrefix (native). It never covers a shared account: an all:true token
//     (and so a portal session, which is always all:true) does not auto-borrow one.
//   - An entry naming the account (own or shared) with OverrideEnabled==false:
//     allowed, native ModelPrefix.
//   - An entry naming the account with OverrideEnabled==true: allowed, the override
//     prefix (which may be "" => bare upstream names).
//   - Not covered by All and not listed: denied.
//
// For an OWN account All wins over a listed entry (the account list is ignored for
// access under all:true, so its prefix override is not applied): that is the
// behaviour from before sharing and it is unchanged.
func TokenVendorPrefix(token auth.Token, acc VendorAccount) (prefix string, allowed bool) {
	access := token.VendorAccess
	if access.All && TokenOwnsVendorAccount(token, acc) {
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
