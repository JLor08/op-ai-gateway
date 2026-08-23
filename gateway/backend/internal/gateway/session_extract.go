// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// sessionHeaderName is the explicit gateway session override header. The portal
// chat loopback sets it to the chat id; external integrations may set it too.
const sessionHeaderName = "X-OP-AI-Gateway-Session-ID"

// maxSessionIDLen caps a stored session id (prompt_cache_key can be long). Display
// + affinity-key only; the request body is forwarded upstream unchanged.
const maxSessionIDLen = 200

// sessionEndpoint identifies the client protocol/endpoint so the extractor reads
// the right natural signal. Codex (/v1/responses) and generic OpenAI
// (/v1/chat/completions) share api_flavor "openai", so the endpoint — not the
// flavor — is the discriminator.
type sessionEndpoint int

const (
	endpointChat      sessionEndpoint = iota // /v1/chat/completions (generic OpenAI)
	endpointResponses                        // /v1/responses (Codex)
	endpointMessages                         // /v1/messages (Claude Code / Anthropic)
)

// sessionInfo is the extractor result. ExplicitHeader is the raw
// X-OP-AI-Gateway-Session-ID value (legacy affinity input); ClientSession is the
// full best-effort id used for display + client_session affinity.
type sessionInfo struct {
	ExplicitHeader string
	ClientSession  string
	Source         string
	AgentID        string
}

func capSession(s string) string {
	if len(s) > maxSessionIDLen {
		return s[:maxSessionIDLen]
	}
	return s
}

// extractClientSession derives the session id from the request, flavor-/endpoint-
// gated. Priority: explicit header > per-endpoint header > per-endpoint body field.
// Body parsing is tolerant + minimal and only runs when the headers gave nothing.
func extractClientSession(h http.Header, raw []byte, endpoint sessionEndpoint) sessionInfo {
	info := sessionInfo{}
	explicit := strings.TrimSpace(h.Get(sessionHeaderName))
	info.ExplicitHeader = capSession(explicit)
	if endpoint == endpointMessages {
		info.AgentID = capSession(strings.TrimSpace(h.Get("x-claude-code-agent-id")))
	}

	// 1. Explicit override header (highest priority; portal chat sets it).
	if explicit != "" {
		info.ClientSession = capSession(explicit)
		info.Source = "header"
		if h.Get(internalAuthHeaderName) != "" {
			info.Source = "chat"
		}
		return info
	}

	// 2. Per-endpoint header signals (no body parse needed).
	switch endpoint {
	case endpointResponses:
		if v := strings.TrimSpace(h.Get("session_id")); v != "" {
			info.ClientSession, info.Source = capSession(v), "codex"
			return info
		}
	case endpointMessages:
		if v := strings.TrimSpace(h.Get("x-claude-code-session-id")); v != "" {
			info.ClientSession, info.Source = capSession(v), "claude-code"
			return info
		}
	}

	// 3. Per-endpoint body fallback (only reached when headers were empty).
	if id, src := sessionFromBody(raw, endpoint); id != "" {
		info.ClientSession, info.Source = capSession(id), src
	}
	return info
}

func sessionFromBody(raw []byte, endpoint sessionEndpoint) (id, source string) {
	if len(raw) == 0 {
		return "", ""
	}
	switch endpoint {
	case endpointResponses:
		var b struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		_ = json.Unmarshal(raw, &b)
		if v := strings.TrimSpace(b.PromptCacheKey); v != "" {
			return v, "codex"
		}
	case endpointChat:
		var b struct {
			PromptCacheKey string `json:"prompt_cache_key"`
			User           string `json:"user"`
		}
		_ = json.Unmarshal(raw, &b)
		if v := strings.TrimSpace(b.PromptCacheKey); v != "" {
			return v, "openai"
		}
		if v := strings.TrimSpace(b.User); v != "" {
			return v, "openai"
		}
	case endpointMessages:
		var b struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(raw, &b)
		if v := strings.TrimSpace(b.Metadata.UserID); v != "" {
			return v, "anthropic"
		}
	}
	return "", ""
}
