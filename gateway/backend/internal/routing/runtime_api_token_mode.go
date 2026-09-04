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
