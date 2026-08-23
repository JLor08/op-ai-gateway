// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package inference

import "strings"

type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ContentType string

const (
	ContentText       ContentType = "text"
	ContentToolResult ContentType = "tool_result"
	ContentImage      ContentType = "image"
)

type ContentPart struct {
	Type       ContentType    `json:"type"`
	Text       string         `json:"text,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	JSON       map[string]any `json:"json,omitempty"`
	ImageURL   string         `json:"image_url,omitempty"`
}

type Message struct {
	Role    Role          `json:"role"`
	Content []ContentPart `json:"content"`
	Name    string        `json:"name,omitempty"`
	// ToolCallID links a tool-result message (Role == RoleTool) back to the
	// assistant tool call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls holds the tool/function calls an assistant message makes. Such a
	// message may have empty Content (a pure tool-call turn).
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// Reasoning is the assistant turn's chain-of-thought (the harmony "analysis"
	// channel), replayed from a client's prior `reasoning` output item. It is
	// forwarded to the upstream as `reasoning_content` on the assistant message so a
	// reasoning model keeps its train of thought across a multi-turn agent loop
	// (matching what llama.cpp does natively). Set only on assistant messages.
	Reasoning string `json:"reasoning,omitempty"`
}

func (m Message) Text() string {
	parts := make([]string, 0, len(m.Content))
	for _, part := range m.Content {
		if part.Type == ContentText && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments is the raw JSON-string arguments exactly as produced by the model
	// / sent by the client. Both the OpenAI Chat Completions and Responses wire
	// formats carry function arguments as a JSON string, so keeping it as a string
	// round-trips losslessly (no reparse/reserialize).
	Arguments string `json:"arguments,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// CachedTokens are prompt tokens served from the upstream's prompt cache (cache
	// READ). It is a subset of the OpenAI-canonical InputTokens above (which INCLUDES
	// both cache reads and cache-creation writes).
	CachedTokens int `json:"cached_tokens,omitempty"`
	// CacheWriteTokens are prompt tokens WRITTEN to the cache this turn (Anthropic
	// cache_creation_input_tokens). Only the Anthropic format reports this — OpenAI /
	// Responses have no cache-write count, so it stays 0 there. It too is a subset of
	// the canonical InputTokens.
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	PromptPerSecond  float64 `json:"prompt_per_second,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

type Request struct {
	ID        string `json:"id,omitempty"`
	APIFlavor string `json:"api_flavor"`
	Model     string `json:"model"`
	// RequestedModel is the model name exactly as the client sent it, before
	// any token model override rewrote Model. Recorded on the usage event so
	// the activity list can show the pre-override name (issue #7).
	RequestedModel string `json:"-"`
	SessionID      string `json:"session_id,omitempty"`
	// ClientSessionID is the best-effort session id EXTRACTED from the client's
	// natural signal (explicit X-OP-AI-Gateway-Session-ID header, Codex session_id
	// header / prompt_cache_key, Claude Code x-claude-code-session-id, portal chat
	// id, generic prompt_cache_key/user/metadata.user_id). Always populated for
	// display; drives route affinity in the "client_session" mode. SessionID above
	// stays = the explicit header only (legacy affinity input).
	ClientSessionID string `json:"client_session_id,omitempty"`
	// SessionSource labels where ClientSessionID came from: header|chat|codex|
	// claude-code|openai|anthropic (empty when none).
	SessionSource string `json:"session_source,omitempty"`
	// AgentID is the Claude Code subagent id (x-claude-code-agent-id), empty otherwise.
	AgentID  string    `json:"agent_id,omitempty"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	// ToolChoice is forwarded to the upstream verbatim ("auto"/"none"/"required"
	// or a specific-function selector); empty means the upstream default.
	ToolChoice any  `json:"tool_choice,omitempty"`
	Stream     bool `json:"stream"`
	// IncludeUsage requests that a streaming response emit a final usage chunk
	// (OpenAI `stream_options.include_usage`). Only consumed by the chat-completions
	// streaming path; empty means no usage chunk.
	IncludeUsage bool     `json:"include_usage,omitempty"`
	MaxTokens    int      `json:"max_tokens,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`
	// Stop holds stop sequences forwarded to the upstream verbatim; empty means none.
	Stop []string `json:"stop,omitempty"`
	// ReasoningEffort is the requested reasoning effort ("low"/"medium"/"high",
	// from the Responses `reasoning.effort`); forwarded to the upstream as
	// `reasoning_effort`. Empty means the upstream default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ServerOverrideID, when non-empty, forces routing.Resolver to serve this request
	// from exactly this AI-server id, bypassing resource-group provisioning, route
	// affinity, and the maintenance-status routing exclusion — the gateway handler is
	// responsible for authorizing the principal against this specific server BEFORE
	// setting this field; the resolver only re-checks that the server actually offers
	// the requested model and is enabled + reachable (unless ServerOverrideForceUnreachable).
	ServerOverrideID string `json:"server_override_id,omitempty"`
	// ServerOverrideForceUnreachable, when true alongside ServerOverrideID, routes to the
	// named server even if it is currently unhealthy/unreachable (still refused if the
	// server is disabled). Ignored when ServerOverrideID is empty.
	ServerOverrideForceUnreachable bool `json:"server_override_force_unreachable,omitempty"`
}

func (r Request) Validate() *Error {
	if r.Model == "" {
		return &Error{Code: "request.model_required", Message: "model is required"}
	}
	if len(r.Messages) == 0 {
		return &Error{Code: "request.messages_required", Message: "at least one message is required"}
	}
	for _, msg := range r.Messages {
		if msg.Role == "" {
			return &Error{Code: "request.role_required", Message: "message role is required"}
		}
		// A pure tool-call turn (assistant) may carry tool calls with no text
		// content, so content is required only when there are no tool calls.
		if len(msg.Content) == 0 && len(msg.ToolCalls) == 0 {
			return &Error{Code: "request.content_required", Message: "message content is required"}
		}
	}
	return nil
}

type StreamEventType string

const (
	StreamEventTextDelta StreamEventType = "text_delta"
	StreamEventToolCall  StreamEventType = "tool_call"
	StreamEventCompleted StreamEventType = "completed"
	StreamEventError     StreamEventType = "error"
)

type StreamEvent struct {
	Type         StreamEventType `json:"type"`
	Text         string          `json:"text,omitempty"`
	Reasoning    string          `json:"reasoning,omitempty"`
	ToolCall     *ToolCall       `json:"tool_call,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Error        *Error          `json:"error,omitempty"`
}
