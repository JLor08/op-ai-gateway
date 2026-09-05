// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// RuntimeAPITokenMode is the per-spec choice of WHERE the upstream API token
// the gateway injects into (and sends to) the child model server comes from.
// Default (and every pre-feature row) is "app" = reuse Application.APIToken,
// which preserves today's edge behaviour. Serialized as its lowercase string,
// stored text (mirrors VisibleDevicesMode). Validated at the DTO edge
// (portal.validRuntimeAPITokenMode), not by a method here.
type RuntimeAPITokenMode string

const (
	RuntimeAPITokenModeOff    RuntimeAPITokenMode = "off"
	RuntimeAPITokenModeSet    RuntimeAPITokenMode = "set"
	RuntimeAPITokenModeRandom RuntimeAPITokenMode = "random"
	RuntimeAPITokenModeApp    RuntimeAPITokenMode = "app"
)

// RuntimeAPITokenHeaderSource selects whether the transmission header is
// inherited from the application ("app", the default) or set per-spec ("custom").
type RuntimeAPITokenHeaderSource string

const (
	RuntimeAPITokenHeaderSourceApp    RuntimeAPITokenHeaderSource = "app"
	RuntimeAPITokenHeaderSourceCustom RuntimeAPITokenHeaderSource = "custom"
)

// SpecUpstreamAuth returns the SEALED upstream token and the effective
// transmission header the gateway must attach to requests for one mapping's
// server_agent child, per the spec's api_token_mode. Routing never decrypts —
// the edge (server.go upstreamAuthCtx -> OpenSecret) does. A zero-value spec
// (empty mode) is treated as "app" = today's behaviour.
func SpecUpstreamAuth(spec RuntimeSpec, app Application) (token, header string) {
	mode := spec.APITokenMode
	if mode == "" {
		mode = string(RuntimeAPITokenModeApp)
	}
	switch RuntimeAPITokenMode(mode) {
	case RuntimeAPITokenModeOff:
		return "", ""
	case RuntimeAPITokenModeSet, RuntimeAPITokenModeRandom:
		return spec.APIToken, effectiveAPITokenHeader(spec, app)
	default: // app
		return app.APIToken, effectiveAPITokenHeader(spec, app)
	}
}

// effectiveAPITokenHeader is app.APITokenHeader unless the spec sets a custom one.
func effectiveAPITokenHeader(spec RuntimeSpec, app Application) string {
	if RuntimeAPITokenHeaderSource(spec.APITokenHeaderSource) == RuntimeAPITokenHeaderSourceCustom {
		return spec.APITokenHeader
	}
	return app.APITokenHeader
}
