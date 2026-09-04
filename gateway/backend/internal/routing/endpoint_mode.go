// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// EndpointMode is the three-state per-endpoint control that replaced the old
// native_responses / native_messages booleans: for the Codex /v1/responses and
// Claude Code /v1/messages endpoints, whether the gateway disables the endpoint,
// translates the body to /v1/chat/completions, or proxies it raw (pass-through)
// to the upstream's native path. Serialized as its lowercase string, stored text.
//
// Production validates a raw string against these three values at the DTO edge
// (portal.validEndpointMode, service_applications.go) rather than through a
// method or parser on this type — that validation is intentionally stricter
// (case-sensitive, rejects "") than a permissive helper would be, so keep any
// future parsing/validation helper for this type there, not here.
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"
)
