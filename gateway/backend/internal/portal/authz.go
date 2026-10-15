// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
)

// ErrPrincipalForbidden is the generic defense-in-depth sentinel for the
// token-less mutating API methods that gained an internal principal check in
// PT-2 Part 2 (SetServerNetbird, UpdateSystemSettings, SetUserLimits,
// AddUserToAdminGroup, ReassignGroupsOwnedBy, UserTokens) and PT-2 Part 2b
// (ReissueAllCertificates, ReissueEdgeCertificate, RenewCertificateNow,
// RotateCertificateCA, RotateNetbirdToken, SetNetbirdNetwork,
// CreateGatewaySetupKey, EnrollGatewaySidecar, TestNetbird). These methods span
// unrelated domains (server/netbird, system settings, user limits, group
// membership, tokens, certificates) with no single existing per-domain
// forbidden sentinel to reuse, so they share this one instead of inventing
// near-identical ones per domain. The gateway's requireWebScope HTTP gate
// already enforces the exact same scope requirement before any of these are
// reached over HTTP in practice; this sentinel exists so a future internal
// (non-HTTP) caller cannot bypass authorization entirely. Mapped to 403 in
// sharedErrorMap (internal/gateway/error_map.go).
// CodePrincipalForbidden is ErrPrincipalForbidden's API error code. It is
// exported so the gateway's own pre-emptive 403 responses (written directly,
// without going through the sentinel/writeMappedError) can share the exact
// same value instead of re-hardcoding it — see internal/gateway/certificates.go,
// edge_certificates.go, system_settings_endpoints.go, and error_map.go's
// sharedErrorMap row for ErrPrincipalForbidden.
const CodePrincipalForbidden = "portal.principal_forbidden"

var ErrPrincipalForbidden = errors.New(CodePrincipalForbidden)

// isSystem reports whether the token carries the "system" scope
// (system_admin) — the top role tier. A system-scope principal bypasses
// every ownership/admin-group check below it throughout this package.
func isSystem(p auth.Token) bool {
	return p.HasScope("system")
}

// isAdmin reports whether the token carries the "admin" scope, which every
// admin AND system_admin token holds (sessionPrincipal grants "admin" to
// both). Do not confuse with isSystem: an isAdmin-true, isSystem-false token
// is a plain admin, not a system_admin.
func isAdmin(p auth.Token) bool {
	return p.HasScope("admin")
}

// hasGatewayUse reports whether the token carries the base "gateway:use"
// scope — the default, non-privileged scope every gateway-facing token
// needs to be usable at all.
func hasGatewayUse(p auth.Token) bool {
	return p.HasScope("gateway:use")
}

// principalMayManageTokenSetting reports whether the caller may change a token
// setting that is reserved for MANAGEMENT principals — today
// claude_effort_compatibility. The HTTP gate in front of the token routes
// (requireWebScope(gateway:use)) is no help here: every user bearer carries
// gateway:use, so it admits an inference-only token editing its own settings.
// This is the check that tells the two apart, and it lives in the service
// layer so no caller can skip it.
//
// Two kinds of principal qualify:
//   - an authorized portal session — and the gateway's own trusted loopback,
//     which is built the same way. Both come from sessionPrincipal, which
//     leaves auth.Token.ID empty: there is no api_tokens row behind them. The
//     cookie + CSRF check has already happened by the time one reaches here.
//   - a bearer that holds the admin scope. A bearer's principal always carries
//     the token's own non-empty id (store LookupBearer / auth.TokenStore), so a
//     non-empty ID means "authenticated by a token secret"; such a token is
//     management-capable only if it was minted with the admin scope.
//
// An inference-only bearer (gateway:use / llm:invoke, ID != "", no admin)
// never qualifies — including for the token it IS. Do not widen this to
// hasGatewayUse: that is exactly the scope the bearer already holds.
func principalMayManageTokenSetting(owner auth.Token) bool {
	return owner.ID == "" || isAdmin(owner)
}
