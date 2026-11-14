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
	endpointChat                 sessionEndpoint = iota // /v1/chat/completions (generic OpenAI)
	endpointResponses                                   // /v1/responses (Codex)
	endpointMessages                                    // /v1/messages (Claude Code / Anthropic)
	endpointImages                                      // /v1/images/generations (OpenAI images)
	endpointImagesEdit                                  // /v1/images/edits (OpenAI image edit, multipart)
	endpointSpeech                                      // /v1/audio/speech (OpenAI text-to-speech)
	endpointTranscription                               // /v1/audio/transcriptions (OpenAI speech-to-text, multipart)
	endpointTranscriptionDetails                        // /v1/audio/transcriptions/details (OpenAI speech-to-text with speaker turns, multipart, one-shot)
	endpointSystemOne                                   // /v1/systemone (System One decisions)
	endpointDecisions                                   // /v1/decisions (OpenAI Decisions)
	endpointRealtime                                    // /v1/realtime (OpenAI Realtime WebSocket; openai_audio flavor family)
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
	case endpointImages, endpointImagesEdit:
		// /v1/images/generations and /v1/images/edits have no per-endpoint
		// header signal; see the fuller comment on sessionFromBody's identical
		// case below.
	case endpointSpeech:
		// /v1/audio/speech has no per-endpoint header signal either; see the
		// fuller comment on sessionFromBody's speech case below.
	case endpointSystemOne, endpointDecisions:
		// /v1/systemone and /v1/decisions have no per-endpoint header signal
		// either; see the fuller comment on sessionFromBody's decisions case
		// below.
	case endpointRealtime:
		// /v1/realtime is a WebSocket upgrade: no body to read and no per-endpoint
		// header signal; the explicit session header (step 1) still applies.
	case endpointTranscription, endpointTranscriptionDetails:
		// /v1/audio/transcriptions and its one-shot diarized twin
		// /v1/audio/transcriptions/details have no per-endpoint header signal
		// either; see the fuller comment on sessionFromBody's transcription case
		// below.
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
	case endpointImages, endpointImagesEdit:
		// /v1/images/generations carries no session signal: OpenAI's images
		// request has no prompt_cache_key, no user and no metadata field, and
		// image requests deliberately do not take an affinity pin at all (the
		// affinity key is coarse -- see the write guards in resolver.go). The
		// edits request is no different, and its body is multipart, which this
		// function's JSON decoding could not read a field out of anyway. Stated
		// as a case rather than left to the switch's absent default, so a reader
		// can tell "considered, and there is none" from "nobody thought about it".
	case endpointSpeech:
		// /v1/audio/speech carries no session signal: OpenAI's speech request
		// (model, input, voice, response_format, speed, ...) has no
		// prompt_cache_key, no user and no metadata field, and a speech request
		// is a one-shot synthesis with no conversation to pin an affinity to.
		// Stated as a case rather than left to the switch's absent default, so a
		// reader can tell "considered, and there is none" from "nobody thought
		// about it".
	case endpointSystemOne:
		// /v1/systemone carries no session signal: its request (model, state,
		// questions) has no prompt_cache_key, no user and no metadata field, and a
		// decision request is a one-shot verdict with no conversation to pin an
		// affinity to. Stated as a case rather than left to the switch's absent
		// default, so a reader can tell "considered, and there is none" from
		// "nobody thought about it".
	case endpointDecisions:
		// /v1/decisions is read the same way as /v1/systemone. Its request does
		// carry a prompt_cache_key, but that is a prompt-cache hint (the
		// translate route drops it, ADR-098), not a conversation: a decision is
		// a one-shot verdict like a System One one. The explicit session header
		// (step 1 of extractClientSession) still applies.
	case endpointRealtime:
		// /v1/realtime has no request body at all (a WebSocket upgrade), so there
		// is no body signal to read.
	case endpointTranscription, endpointTranscriptionDetails:
		// /v1/audio/transcriptions (and its /details twin, which takes the same
		// form) carries no session signal: OpenAI's
		// transcription request (file, model, language, prompt, response_format,
		// temperature, ...) has no prompt_cache_key, no user and no metadata
		// field, and a transcription is a one-shot recognition with no
		// conversation to pin an affinity to. Its body is multipart, which this
		// function's JSON decoding could not read a field out of anyway. Stated
		// as a case rather than left to the switch's absent default, so a reader
		// can tell "considered, and there is none" from "nobody thought about
		// it".
	}
	return "", ""
}
