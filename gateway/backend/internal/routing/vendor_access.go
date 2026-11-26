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
//     (and so a portal session whose stored chat policy is unset or all:true)
//     does not auto-borrow one.
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

// TokenOwnsVendorGroup reports whether g is one of token's OWN vendor account
// groups. Groups are personal, so unlike accounts there is no shared case: a token
// with no user, or a blank owner id, owns nothing.
func TokenOwnsVendorGroup(token auth.Token, g VendorAccountGroup) bool {
	return token.UserID != "" && g.OwnerUserID == token.UserID
}

// TokenUsesVendorGroups reports whether token can use any vendor account group at
// all: a token with a user that has all + include_groups, or lists a group. The
// resolver's group step and the listing's group half read the groups only for such
// a token, so a token that can use none neither pays for the read nor fails on it.
// Whether a listed group is the user's own and active is still decided per group
// (TokenVendorGroupPrefix).
func TokenUsesVendorGroups(token auth.Token) bool {
	access := token.VendorAccess
	return token.UserID != "" && ((access.All && access.IncludeGroups) || len(access.Groups) > 0)
}

// TokenVendorGroupPrefix reports the public model-name prefix a token serves group
// g under, and whether g is allowed for the token at all. A group the token's user
// does not own is never allowed, even when listed.
//
//   - All && IncludeGroups: allowed under the group's own ModelPrefix; a listed
//     entry's override is ignored (all wins, as for own accounts).
//   - A listed entry: its override prefix when enabled, else the group's ModelPrefix.
//   - Otherwise denied.
func TokenVendorGroupPrefix(token auth.Token, g VendorAccountGroup) (prefix string, allowed bool) {
	if !TokenOwnsVendorGroup(token, g) {
		return "", false
	}
	access := token.VendorAccess
	if access.All && access.IncludeGroups {
		return g.ModelPrefix, true
	}
	for _, e := range access.Groups {
		if e.GroupID != g.ID {
			continue
		}
		if e.OverrideEnabled {
			return e.OverridePrefix, true
		}
		return g.ModelPrefix, true
	}
	return "", false
}
