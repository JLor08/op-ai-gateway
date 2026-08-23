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
// (non-HTTP) caller cannot bypass authorization entirely -- see
// docs/superpowers/plans (PT-2 Part 2 / Part 2b). Mapped to 403 in
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
