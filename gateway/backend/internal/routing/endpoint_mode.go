// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"fmt"
	"strings"
)

// EndpointMode is the three-state per-endpoint control that replaced the old
// native_responses / native_messages booleans: for the Codex /v1/responses and
// Claude Code /v1/messages endpoints, whether the gateway disables the endpoint,
// translates the body to /v1/chat/completions, or proxies it raw (pass-through)
// to the upstream's native path. Serialized as its lowercase string, stored text.
type EndpointMode string

const (
	EndpointModeDisabled    EndpointMode = "disabled"
	EndpointModeTranslate   EndpointMode = "translate"
	EndpointModePassthrough EndpointMode = "passthrough"

	// DefaultEndpointMode is the value a fresh application/spec gets when the
	// flavor is enabled: pass-through, because every supported upstream now
	// serves both native endpoints (design §6).
	DefaultEndpointMode = EndpointModePassthrough
)

func (m EndpointMode) String() string { return string(m) }

// Valid reports whether m is one of the three defined modes.
func (m EndpointMode) Valid() bool {
	switch m {
	case EndpointModeDisabled, EndpointModeTranslate, EndpointModePassthrough:
		return true
	default:
		return false
	}
}

// OrDefault resolves an unset ("") mode to DefaultEndpointMode; every other
// value is returned untouched. The read-time default a zero-value in-memory
// Application still needs even though the migration backfills a non-empty value.
func (m EndpointMode) OrDefault() EndpointMode {
	if m == "" {
		return DefaultEndpointMode
	}
	return m
}

// ParseEndpointMode trims + lowercases, maps "" to DefaultEndpointMode, and
// rejects any unrecognized value (a stable validation failure at the DTO edge).
func ParseEndpointMode(s string) (EndpointMode, error) {
	switch m := EndpointMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return DefaultEndpointMode, nil
	case EndpointModeDisabled, EndpointModeTranslate, EndpointModePassthrough:
		return m, nil
	default:
		return "", fmt.Errorf("routing: invalid endpoint mode %q", s)
	}
}
