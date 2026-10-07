// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package vendorauth implements the OAuth mechanics for connecting a consumer
// SUBSCRIPTION account (Anthropic Claude Pro/Max, OpenAI ChatGPT/Codex) as an
// external vendor account: PKCE and state generation, authorize-URL
// construction, authorization-code and device-code token exchange, token
// refresh, and the sealed TokenSet credential blob.
//
// WARNING: this whole path is REVERSE-ENGINEERED from the official vendor CLIs
// (Claude Code, Codex CLI). It is UNDOCUMENTED, ToS-sensitive and
// EXPERIMENTAL: the vendors do not publish or support these endpoints for
// third-party use, and every value in constants.go may be stale, rotated or
// blocked. VERIFY LIVE before relying on any of it; the flows read all vendor
// parameters through Endpoints so they can be pointed at a corrected endpoint
// (or at httptest servers in tests) without code changes.
//
// The package is a pure library: it performs no persistence and serves no HTTP
// routes. Callers own session state (the pending PKCE verifier and state
// between "begin" and "complete"), splitting of the pasted "code#state" string
// Anthropic's callback page displays, and re-sealing a refreshed TokenSet. It
// imports only internal/capture (for the at-rest secret scheme) and the
// standard library, so internal/portal and internal/gateway can depend on it
// without an import cycle.
package vendorauth
