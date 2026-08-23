// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func hdr(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestExtractClientSession(t *testing.T) {
	cases := []struct {
		name       string
		h          http.Header
		raw        string
		endpoint   sessionEndpoint
		wantID     string
		wantSource string
		wantExpl   string
		wantAgent  string
	}{
		{
			name: "explicit header wins over codex header", endpoint: endpointResponses,
			h:      hdr("X-OP-AI-Gateway-Session-ID", "explicit-1", "session_id", "codex-1"),
			wantID: "explicit-1", wantSource: "header", wantExpl: "explicit-1",
		},
		{
			name: "internal loopback marks source=chat", endpoint: endpointChat,
			h:      hdr("X-OP-AI-Gateway-Session-ID", "chat_42", internalAuthHeaderName, "secret"),
			wantID: "chat_42", wantSource: "chat", wantExpl: "chat_42",
		},
		{
			name: "codex session_id header", endpoint: endpointResponses,
			h:      hdr("session_id", "thread_abc"),
			wantID: "thread_abc", wantSource: "codex",
		},
		{
			name: "codex prompt_cache_key body fallback", endpoint: endpointResponses,
			raw:    `{"model":"m","prompt_cache_key":"pck_xyz"}`,
			wantID: "pck_xyz", wantSource: "codex",
		},
		{
			name: "claude code session header + agent id", endpoint: endpointMessages,
			h:      hdr("x-claude-code-session-id", "cc_sess", "x-claude-code-agent-id", "agent_7"),
			wantID: "cc_sess", wantSource: "claude-code", wantAgent: "agent_7",
		},
		{
			name: "anthropic metadata.user_id body fallback", endpoint: endpointMessages,
			raw:    `{"model":"m","metadata":{"user_id":"user_hash_session_uuid"}}`,
			wantID: "user_hash_session_uuid", wantSource: "anthropic",
		},
		{
			name: "openai chat prompt_cache_key", endpoint: endpointChat,
			raw:    `{"model":"m","prompt_cache_key":"pck_chat"}`,
			wantID: "pck_chat", wantSource: "openai",
		},
		{
			name: "openai chat user fallback", endpoint: endpointChat,
			raw:    `{"model":"m","user":"u_1"}`,
			wantID: "u_1", wantSource: "openai",
		},
		{
			name: "generic chat with nothing -> empty", endpoint: endpointChat,
			raw:    `{"model":"m"}`,
			wantID: "", wantSource: "",
		},
		{
			name: "flavor gating: openai user NOT read on responses endpoint", endpoint: endpointResponses,
			raw:    `{"model":"m","user":"u_1"}`,
			wantID: "", wantSource: "",
		},
		{
			name: "length cap at 200", endpoint: endpointResponses,
			h:      hdr("session_id", strings.Repeat("a", 250)),
			wantID: strings.Repeat("a", 200), wantSource: "codex",
		},
		{
			name: "agent id ignored on non-messages endpoint", endpoint: endpointChat,
			h:      hdr("x-claude-code-agent-id", "agent_9"),
			wantID: "", wantSource: "", wantAgent: "",
		},
		{
			name: "length cap on explicit header", endpoint: endpointChat,
			h:      hdr("X-OP-AI-Gateway-Session-ID", strings.Repeat("b", 250)),
			wantID: strings.Repeat("b", 200), wantSource: "header", wantExpl: strings.Repeat("b", 200),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractClientSession(c.h, []byte(c.raw), c.endpoint)
			if got.ClientSession != c.wantID {
				t.Fatalf("ClientSession = %q, want %q", got.ClientSession, c.wantID)
			}
			if got.Source != c.wantSource {
				t.Fatalf("Source = %q, want %q", got.Source, c.wantSource)
			}
			if got.ExplicitHeader != c.wantExpl {
				t.Fatalf("ExplicitHeader = %q, want %q", got.ExplicitHeader, c.wantExpl)
			}
			if got.AgentID != c.wantAgent {
				t.Fatalf("AgentID = %q, want %q", got.AgentID, c.wantAgent)
			}
		})
	}
}
