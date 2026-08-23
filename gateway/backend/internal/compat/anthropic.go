// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"strings"
)

type anthropicMessagesRequest struct {
	Model    string             `json:"model"`
	System   any                `json:"system"` // string OR array of text blocks
	Messages []anthropicMessage `json:"messages"`
	Tools    []anthropicTool    `json:"tools"`
	// ToolChoice is Anthropic's shape ({"type":"auto|any|tool|none","name":...});
	// it is translated to the Chat Completions form for the upstream.
	ToolChoice    *anthropicToolChoice `json:"tool_choice"`
	MaxTokens     int                  `json:"max_tokens"`
	Stream        bool                 `json:"stream"`
	Temperature   *float64             `json:"temperature"`
	StopSequences []string             `json:"stop_sequences"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string OR array of content blocks
}

// anthropicTool is a Claude tool definition. The JSON-Schema field is
// `input_schema` (not `parameters`), which maps to the internal Tool.Parameters.
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ParseAnthropicMessages converts a Claude Code (Anthropic Messages API) request
// into the internal inference.Request. It handles the full multi-turn tool-use
// shape Claude Code sends — text / tool_use / tool_result content blocks, a
// string-or-array `system`, flat `tools` (input_schema), and `tool_choice` — and
// is TOLERANT of block types it does not model (image, thinking, …): those are
// skipped, never rejected, so an evolving client is never blocked.
func ParseAnthropicMessages(raw []byte) (inference.Request, error) {
	var input anthropicMessagesRequest
	if err := json.Unmarshal(raw, &input); err != nil {
		return inference.Request{}, err
	}
	if len(input.Messages) == 0 {
		return inference.Request{}, &inference.Error{Code: "request.messages_required", Message: "at least one message is required"}
	}

	messages := make([]inference.Message, 0, len(input.Messages)+1)
	if sys := anthropicSystemText(input.System); strings.TrimSpace(sys) != "" {
		messages = append(messages, inference.Message{
			Role:    inference.RoleSystem,
			Content: []inference.ContentPart{{Type: inference.ContentText, Text: sys}},
		})
	}
	for _, msg := range input.Messages {
		parsed, err := parseAnthropicContentBlocks(normalizeAnthropicRole(msg.Role), msg.Content)
		if err != nil {
			return inference.Request{}, err
		}
		messages = append(messages, parsed...)
	}

	req := inference.Request{
		APIFlavor:   "anthropic_messages",
		Model:       input.Model,
		Messages:    messages,
		MaxTokens:   input.MaxTokens,
		Stream:      input.Stream,
		Temperature: input.Temperature,
		Stop:        input.StopSequences,
	}
	for _, tool := range input.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		req.Tools = append(req.Tools, inference.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.InputSchema,
		})
	}
	req.ToolChoice = anthropicToolChoiceToChat(input.ToolChoice)
	if vErr := req.Validate(); vErr != nil {
		return inference.Request{}, vErr
	}
	return req, nil
}

// parseAnthropicContentBlocks converts one Anthropic message's `content` (a
// string OR an array of content blocks) into internal messages. A single
// Anthropic message can yield MULTIPLE internal messages: tool_result blocks
// (which Anthropic carries inside a user message) become separate tool-role
// messages emitted BEFORE this message's own text, so the upstream chat history
// is the strict assistant(tool_calls) → tool(result) → user(text) shape.
func parseAnthropicContentBlocks(role inference.Role, content any) ([]inference.Message, error) {
	switch typed := content.(type) {
	case string:
		text, err := requireContent(typed, "anthropic.content_required", "anthropic message content is required")
		if err != nil {
			return nil, err
		}
		return []inference.Message{{Role: role, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}}, nil
	case []any:
		return parseAnthropicBlockArray(role, typed)
	case nil:
		return nil, &inference.Error{Code: "anthropic.content_required", Message: "anthropic message content is required"}
	default:
		return nil, unsupportedContent("anthropic.content_unsupported", "anthropic message content must be a string or content blocks")
	}
}

func parseAnthropicBlockArray(role inference.Role, blocks []any) ([]inference.Message, error) {
	var out []inference.Message
	parts := make([]inference.ContentPart, 0, len(blocks))
	var toolCalls []inference.ToolCall
	var reasoning strings.Builder

	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			// Tolerant: a non-object array element is skipped, never rejected.
			continue
		}
		switch stringField(block, "type") {
		case "text":
			if t, _ := block["text"].(string); strings.TrimSpace(t) != "" {
				parts = append(parts, inference.ContentPart{Type: inference.ContentText, Text: t})
			}
		case "thinking":
			// A replayed extended-thinking block (Claude Code echoes the assistant
			// turn's thinking back in the tool loop). Capture its text as the turn's
			// reasoning so it threads back to the upstream as reasoning_content,
			// keeping the reasoning model's chain-of-thought — the direct analog of
			// the Codex reasoning path, and what llama.cpp does. The `signature` is
			// NOT validated: we never minted a real one, and only api.anthropic.com
			// validates signatures — it is never in the translate path.
			if t, _ := block["thinking"].(string); strings.TrimSpace(t) != "" {
				if reasoning.Len() > 0 {
					reasoning.WriteString("\n")
				}
				reasoning.WriteString(t)
			}
		case "image":
			// Claude Code sends pasted screenshots. Convert to an image content part so
			// the provider forwards it as an OpenAI image_url block to a vision upstream.
			if url := anthropicImageURL(block["source"]); url != "" {
				parts = append(parts, inference.ContentPart{Type: inference.ContentImage, ImageURL: url})
			}
		case "tool_use":
			// An assistant tool call. Anthropic `input` is a JSON object; the internal
			// model + Chat Completions upstream carry arguments as a JSON string.
			toolCalls = append(toolCalls, inference.ToolCall{
				ID:        stringField(block, "id"),
				Name:      stringField(block, "name"),
				Arguments: anthropicInputToArguments(block["input"]),
			})
		case "tool_result":
			// A tool result the client feeds back -> a separate tool-role message,
			// keyed by tool_use_id (the correlation id of the prior tool_use).
			out = append(out, inference.Message{
				Role:       inference.RoleTool,
				ToolCallID: stringField(block, "tool_use_id"),
				Content:    []inference.ContentPart{{Type: inference.ContentText, Text: anthropicToolResultText(block["content"])}},
			})
		default:
			// Tolerant: unmodeled blocks (thinking, …) are skipped, not rejected.
			continue
		}
	}

	// This message's own content (text + images) + tool calls become one message of
	// `role`, appended AFTER any tool_result messages extracted above. The captured
	// reasoning (from any thinking block) rides on it; it is attached only when the
	// message carries content or tool calls, so a degenerate thinking-only turn
	// drops out rather than producing a message that fails Validate.
	if len(parts) > 0 || len(toolCalls) > 0 {
		out = append(out, inference.Message{Role: role, Content: parts, ToolCalls: toolCalls, Reasoning: reasoning.String()})
	}

	// A block array with nothing representable (e.g. an empty array) yields no
	// messages rather than rejecting the whole request — tolerant, like the
	// Responses parser. If every message drops out, the top-level Validate catches
	// the empty conversation.
	return out, nil
}

// anthropicImageURL turns an Anthropic image block `source` into a URL the OpenAI
// image_url content block understands: a base64 source becomes a data: URI
// (data:<media_type>;base64,<data>), a url source is passed through. Returns ""
// (skip the block) when the source is missing/malformed.
func anthropicImageURL(source any) string {
	src, ok := source.(map[string]any)
	if !ok {
		return ""
	}
	switch stringField(src, "type") {
	case "base64":
		data := stringField(src, "data")
		if strings.TrimSpace(data) == "" {
			return ""
		}
		mediaType := stringField(src, "media_type")
		if mediaType == "" {
			mediaType = "image/png"
		}
		return "data:" + mediaType + ";base64," + data
	case "url":
		return stringField(src, "url")
	default:
		return ""
	}
}

// normalizeAnthropicRole maps an Anthropic message role to an internal role.
// Anthropic `messages` carry only "user"/"assistant"; anything else passes
// through so Validate can reject a truly bad role.
func normalizeAnthropicRole(role string) inference.Role {
	switch strings.TrimSpace(strings.ToLower(role)) {
	case "user":
		return inference.RoleUser
	case "assistant":
		return inference.RoleAssistant
	default:
		return inference.Role(role)
	}
}

// anthropicSystemText extracts the system prompt from Anthropic `system` (a
// string OR an array of {type:"text",text,cache_control?} blocks). Non-text
// entries are skipped; cache_control is ignored.
func anthropicSystemText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		return joinBlockText(typed)
	default:
		return ""
	}
}

// anthropicToolResultText flattens an Anthropic tool_result `content` (a string
// OR an array of blocks, most commonly [{type:"text",text:…}]) into text.
// Non-text blocks (images, …) are skipped.
func anthropicToolResultText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		return joinBlockText(typed)
	default:
		return ""
	}
}

// joinBlockText concatenates the `.text` of each object element that carries a
// non-empty string text, newline-joined. Non-object / non-text elements skipped.
func joinBlockText(items []any) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := block["text"].(string); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicInputToArguments serializes an Anthropic tool_use `input` (a JSON
// object) into the raw JSON-string arguments the internal model uses. An
// absent / null / unencodable input yields "{}".
func anthropicInputToArguments(value any) string {
	if value == nil {
		return "{}"
	}
	b, err := json.Marshal(value)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return "{}"
	}
	return string(b)
}

// anthropicToolChoiceToChat maps an Anthropic tool_choice to the OpenAI Chat
// Completions form the upstream understands (the provider forwards it verbatim):
// auto→"auto", any→"required", none→"none",
// tool→{"type":"function","function":{"name":…}}. nil/unknown → nil (upstream
// default, which for Anthropic is "auto").
func anthropicToolChoiceToChat(tc *anthropicToolChoice) any {
	if tc == nil {
		return nil
	}
	switch strings.TrimSpace(strings.ToLower(tc.Type)) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if strings.TrimSpace(tc.Name) == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return nil
	}
}

type AnthropicTokenCount struct {
	InputTokens int `json:"input_tokens"`
}

func CountAnthropicTokens(raw []byte) (AnthropicTokenCount, error) {
	req, err := ParseAnthropicMessages(raw)
	if err != nil {
		return AnthropicTokenCount{}, err
	}
	total := 0
	for _, msg := range req.Messages {
		total += len(strings.Fields(msg.Text()))
	}
	return AnthropicTokenCount{InputTokens: total}, nil
}

type AnthropicMessageResponseBody struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Role string `json:"role"`
	// Content is heterogeneous: text blocks (AnthropicTextBlock) and tool_use
	// blocks (AnthropicToolUseBlock).
	Content      []any          `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        AnthropicUsage `json:"usage"`
}

type AnthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// AnthropicThinkingBlock is an extended-thinking block in a response. The
// `signature` is always the empty string on the translate path (we mint no real
// signature — mirroring llama.cpp, which also emits ""); nothing in this path
// validates it (only api.anthropic.com does, and it is never reached here). It is
// emitted BEFORE the text/tool_use blocks, per the Anthropic ordering rule.
type AnthropicThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

// AnthropicToolUseBlock is a tool call in a response. `input` is a JSON OBJECT
// (Anthropic's shape), reconstructed from the internal JSON-string arguments.
type AnthropicToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheReadInputTokens are the prompt tokens served from the upstream's prompt
	// cache (OpenAI prompt_tokens_details.cached_tokens). Omitted when zero.
	// Anthropic reports cache reads separately from input_tokens, so InputTokens
	// above already EXCLUDES these (see AnthropicInputTokens).
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
}

// AnthropicInputTokens returns the prompt-token count to report as Anthropic
// input_tokens: the upstream prompt tokens MINUS the cached subset. OpenAI's
// prompt_tokens INCLUDES cached tokens, whereas Anthropic reports cache reads
// separately in cache_read_input_tokens, so the two must not double-count. Never
// negative.
func AnthropicInputTokens(u inference.Usage) int {
	n := u.InputTokens - u.CachedTokens
	if n < 0 {
		return 0
	}
	return n
}

// AnthropicStopReason maps an OpenAI Chat Completions finish_reason to the
// Anthropic stop_reason: "length"→"max_tokens"; tool calls (finish_reason
// "tool_calls" OR any tool call present)→"tool_use"; everything else ("stop", "",
// unknown)→"end_turn".
//
// NOTE: Anthropic's "stop_sequence" reason is intentionally NOT produced. Over a
// generic Chat Completions upstream a stop-sequence match surfaces as
// finish_reason "stop" — indistinguishable from a natural end — so the matched
// sequence cannot be recovered on the translate path (verified against llama.cpp,
// whose /v1/chat/completions serializer discards the stopping_word; its native
// /v1/messages recovers it only from raw internal task metadata). Full fidelity
// requires native passthrough.
func AnthropicStopReason(finishReason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}
	switch strings.TrimSpace(strings.ToLower(finishReason)) {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// AnthropicMessageResponse renders a non-streaming Messages response. It emits a
// thinking block first when the model reasoned (mirroring llama.cpp; ordering
// thinking → text → tool_use is an Anthropic requirement), then a text block when
// there is text (or when there are no tool calls at all — a plain text turn always
// carries a text block), then a tool_use block per tool call. stop_reason is
// mapped from finishReason (see AnthropicStopReason).
func AnthropicMessageResponse(id string, model string, text string, reasoning string, toolCalls []inference.ToolCall, finishReason string, usage inference.Usage) AnthropicMessageResponseBody {
	content := make([]any, 0, 2+len(toolCalls))
	if reasoning != "" {
		content = append(content, AnthropicThinkingBlock{Type: "thinking", Thinking: reasoning, Signature: ""})
	}
	if text != "" || len(toolCalls) == 0 {
		content = append(content, AnthropicTextBlock{Type: "text", Text: text})
	}
	for _, tc := range toolCalls {
		content = append(content, AnthropicToolUseBlock{
			Type:  "tool_use",
			ID:    anthropicToolUseID(tc),
			Name:  tc.Name,
			Input: argumentsToObject(tc.Arguments),
		})
	}
	return AnthropicMessageResponseBody{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: AnthropicStopReason(finishReason, len(toolCalls) > 0),
		Usage: AnthropicUsage{
			InputTokens:          AnthropicInputTokens(usage),
			OutputTokens:         usage.OutputTokens,
			CacheReadInputTokens: usage.CachedTokens,
		},
	}
}

// anthropicToolUseID returns the tool_use block id. Anthropic ids are "toolu_"-
// prefixed; when the upstream call has no id, one is synthesized from the name.
// The only hard requirement is that the follow-up tool_result.tool_use_id echoes
// it — which Claude Code does verbatim.
func anthropicToolUseID(tc inference.ToolCall) string {
	if strings.TrimSpace(tc.ID) != "" {
		return tc.ID
	}
	return "toolu_" + tc.Name
}

// argumentsToObject parses raw JSON-string arguments into an object for the
// Anthropic tool_use `input` field (an object, not a string). An empty / invalid
// / non-object payload yields an empty object.
func argumentsToObject(args string) map[string]any {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return map[string]any{}
	}
	return obj
}
