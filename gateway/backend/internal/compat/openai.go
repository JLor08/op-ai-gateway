// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package compat

import (
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"strings"
	"time"
)

// Error codes/messages shared by more than one OpenAI request-parsing path
// below (Chat Completions and Responses). A message that is specific to a
// single call site stays inline.
const (
	codeOpenAIInputRequired         = "openai.input_required"
	msgOpenAIResponsesInputRequired = "openai responses input is required"

	codeOpenAIContentRequired = "openai.content_required"
	msgOpenAIContentRequired  = "openai content is required"

	codeOpenAIContentUnsupported       = "openai.content_unsupported"
	msgOpenAIContentUnsupportedGeneric = "openai content must be a string or content blocks"
)

type openAIChatRequest struct {
	Model         string              `json:"model"`
	Messages      []openAIChatMessage `json:"messages"`
	Tools         []openAITool        `json:"tools"`
	ToolChoice    any                 `json:"tool_choice"`
	Stream        bool                `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Temperature *float64 `json:"temperature"`
	MaxTokens   int      `json:"max_tokens"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	// Name is an optional participant name (e.g. on a tool message).
	Name string `json:"name"`
	// ToolCallID links a tool-result message (role:"tool") back to the assistant
	// tool call it answers.
	ToolCallID string `json:"tool_call_id"`
	// ToolCalls holds the function/tool calls an assistant message makes. Such a
	// message may carry null/empty Content (a pure tool-call turn) — a coding agent
	// replays it verbatim on the next turn.
	ToolCalls []openAIChatToolCall `json:"tool_calls"`
}

// openAIChatToolCall is a Chat Completions tool call: the function name/arguments
// nest under "function" (unlike the flat Responses shape).
type openAIChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func ParseOpenAIChatCompletions(raw []byte) (inference.Request, error) {
	var input openAIChatRequest
	if err := json.Unmarshal(raw, &input); err != nil {
		return inference.Request{}, err
	}
	req := inference.Request{
		APIFlavor:   "openai_chat_completions",
		Model:       input.Model,
		Stream:      input.Stream,
		MaxTokens:   input.MaxTokens,
		Temperature: input.Temperature,
		Messages:    make([]inference.Message, 0, len(input.Messages)),
		Tools:       make([]inference.Tool, 0, len(input.Tools)),
		ToolChoice:  input.ToolChoice,
	}
	if input.StreamOptions != nil {
		req.IncludeUsage = input.StreamOptions.IncludeUsage
	}
	for _, msg := range input.Messages {
		role := inference.Role(msg.Role)
		out := inference.Message{Role: role, Name: msg.Name}
		for _, tc := range msg.ToolCalls {
			// Tolerant: only function tool calls are modeled; skip any other type.
			if tc.Type != "" && tc.Type != "function" {
				continue
			}
			out.ToolCalls = append(out.ToolCalls, inference.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if role == inference.RoleTool {
			out.ToolCallID = msg.ToolCallID
		}
		// Content is required only for an ordinary message. An assistant tool-call
		// turn (tool_calls present) or a tool-result message (role:"tool") may carry
		// null/empty content — this is the multi-turn shape a coding agent replays.
		contentOptional := len(out.ToolCalls) > 0 || role == inference.RoleTool
		parts, err := parseChatMessageContent(msg.Content, contentOptional)
		if err != nil {
			return inference.Request{}, err
		}
		out.Content = parts
		req.Messages = append(req.Messages, out)
	}
	for _, tool := range input.Tools {
		if tool.Type != "function" {
			return inference.Request{}, &inference.Error{Code: "openai.tool_type_unsupported", Message: "unsupported OpenAI tool type: " + tool.Type}
		}
		if strings.TrimSpace(tool.Function.Name) == "" {
			return inference.Request{}, &inference.Error{Code: "openai.tool_name_required", Message: "function tool name is required"}
		}
		req.Tools = append(req.Tools, inference.Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	if vErr := req.Validate(); vErr != nil {
		return inference.Request{}, vErr
	}
	return req, nil
}

type openAIResponsesRequest struct {
	Model           string                `json:"model"`
	Input           any                   `json:"input"`
	Instructions    string                `json:"instructions"`
	Tools           []openAIResponsesTool `json:"tools"`
	ToolChoice      any                   `json:"tool_choice"`
	Stream          bool                  `json:"stream"`
	Temperature     *float64              `json:"temperature"`
	MaxOutputTokens int                   `json:"max_output_tokens"`
	// Reasoning carries the reasoning config a reasoning model (Codex) sends; only
	// `effort` is forwarded upstream (as `reasoning_effort`). Tolerant of absence.
	Reasoning *openAIResponsesReasoning `json:"reasoning"`
}

type openAIResponsesReasoning struct {
	Effort string `json:"effort"`
}

// openAIResponsesTool is a Responses-API tool. Function tools are FLAT (name at
// the top level), unlike the Chat Completions nesting under "function".
type openAIResponsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func ParseOpenAIResponses(raw []byte) (inference.Request, error) {
	var input openAIResponsesRequest
	if err := json.Unmarshal(raw, &input); err != nil {
		return inference.Request{}, err
	}
	messages, err := parseOpenAIResponsesInput(input.Input)
	if err != nil {
		return inference.Request{}, err
	}
	// `instructions` is the Responses system prompt: prepend it as a system message.
	if strings.TrimSpace(input.Instructions) != "" {
		messages = append([]inference.Message{{
			Role:    inference.RoleSystem,
			Content: []inference.ContentPart{{Type: inference.ContentText, Text: input.Instructions}},
		}}, messages...)
	}
	req := inference.Request{
		APIFlavor:   "openai_responses",
		Model:       input.Model,
		Stream:      input.Stream,
		Temperature: input.Temperature,
		MaxTokens:   input.MaxOutputTokens,
		Messages:    messages,
		ToolChoice:  input.ToolChoice,
	}
	if input.Reasoning != nil {
		req.ReasoningEffort = strings.TrimSpace(input.Reasoning.Effort)
	}
	for _, tool := range input.Tools {
		// Only function tools can be forwarded to the Chat Completions upstream;
		// built-in tool types (web_search, file_search, local_shell, …) are skipped.
		if tool.Type != "function" || strings.TrimSpace(tool.Name) == "" {
			continue
		}
		req.Tools = append(req.Tools, inference.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		})
	}
	if vErr := req.Validate(); vErr != nil {
		return inference.Request{}, vErr
	}
	return req, nil
}

func parseOpenAIResponsesInput(value any) ([]inference.Message, error) {
	switch typed := value.(type) {
	case string:
		text, err := requireContent(typed, codeOpenAIInputRequired, msgOpenAIResponsesInputRequired)
		if err != nil {
			return nil, err
		}
		return []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: text}}}}, nil
	case []any:
		return parseOpenAIResponsesInputArray(typed)
	case nil:
		return nil, &inference.Error{Code: codeOpenAIInputRequired, Message: msgOpenAIResponsesInputRequired}
	default:
		return nil, unsupportedContent(codeOpenAIContentUnsupported, "openai content must be a string, text content blocks, or message input items")
	}
}

// parseOpenAIResponsesInputArray converts a Responses `input` array into internal
// messages. It handles the full multi-turn shape a coding agent (Codex) sends —
// message / function_call / function_call_output items — and is deliberately
// TOLERANT of item types it does not model (reasoning, built-in tool items, …):
// those are skipped rather than rejected, so an evolving client is never blocked.
func parseOpenAIResponsesInputArray(items []any) ([]inference.Message, error) {
	messages := make([]inference.Message, 0, len(items))
	flatParts := make([]string, 0, len(items))

	appendFlatParts := func() {
		if len(flatParts) == 0 {
			return
		}
		messages = append(messages, inference.Message{
			Role:    inference.RoleUser,
			Content: []inference.ContentPart{{Type: inference.ContentText, Text: strings.Join(flatParts, "\n")}},
		})
		flatParts = flatParts[:0]
	}

	// appendMessage adds a message unless it is empty (no text and no tool calls) —
	// an image-only or echoed-empty-assistant turn is skipped, never rejected.
	// reasoning (assistant only, when non-empty) is threaded to the upstream as
	// reasoning_content to preserve the model's chain-of-thought across turns.
	appendMessage := func(role inference.Role, text string, reasoning string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		messages = append(messages, inference.Message{
			Role:      role,
			Content:   []inference.ContentPart{{Type: inference.ContentText, Text: text}},
			Reasoning: reasoning,
		})
	}

	// pendingReasoning holds the text of a replayed `reasoning` item until the
	// assistant output it precedes (a function_call or message) consumes it. A
	// tool result or a fresh user message clears it (that reasoning belonged to the
	// prior assistant turn and has already been attached to its output).
	var pendingReasoning string
	takeReasoning := func() string { r := pendingReasoning; pendingReasoning = ""; return r }

	for _, item := range items {
		part, ok := item.(map[string]any)
		if !ok {
			// A non-object array element (unexpected) — skip tolerantly.
			continue
		}
		itemType, _ := part["type"].(string)
		switch itemType {
		case "function_call":
			// The assistant's prior tool call replayed in history. call_id is the
			// correlation key with the matching function_call_output.
			appendFlatParts()
			tc := inference.ToolCall{
				ID:        responsesCallID(part),
				Name:      stringField(part, "name"),
				Arguments: stringifyResponsesArguments(part["arguments"]),
			}
			// Merge consecutive function_call items into ONE assistant message with a
			// tool_calls array (parallel tool calls), so the upstream chat history is
			// the strict assistant(tool_calls…) → tool… shape.
			if n := len(messages); n > 0 && messages[n-1].Role == inference.RoleAssistant && len(messages[n-1].ToolCalls) > 0 && len(messages[n-1].Content) == 0 {
				// Parallel call merged into the existing assistant message; its
				// reasoning is already attached to the first call.
				messages[n-1].ToolCalls = append(messages[n-1].ToolCalls, tc)
			} else {
				messages = append(messages, inference.Message{Role: inference.RoleAssistant, ToolCalls: []inference.ToolCall{tc}, Reasoning: takeReasoning()})
			}
		case "function_call_output":
			// The tool result the client feeds back -> a tool-role message.
			appendFlatParts()
			pendingReasoning = "" // the reasoning (if any) was the assistant's, already attached to its call
			messages = append(messages, inference.Message{
				Role:       inference.RoleTool,
				ToolCallID: responsesCallID(part),
				Content:    []inference.ContentPart{{Type: inference.ContentText, Text: stringifyResponsesOutput(part["output"])}},
			})
		case "reasoning":
			// The assistant turn's chain-of-thought. It has no Chat Completions
			// item of its own; buffer it and thread it onto the assistant output
			// (function_call / message) it precedes, as reasoning_content, so the
			// model keeps continuity across the tool loop (matching llama.cpp).
			if t := extractReasoningText(part); t != "" {
				if pendingReasoning != "" {
					pendingReasoning += "\n"
				}
				pendingReasoning += t
			}
			continue
		case "input_text", "output_text", "text":
			// A bare typed text content block at the top level of `input`.
			if text := blockText(part); strings.TrimSpace(text) != "" {
				flatParts = append(flatParts, text)
			}
		case "message", "":
			// A message item, or a bare untyped content block.
			_, hasContent := part["content"]
			if !hasContent && part["role"] == nil {
				if text := blockText(part); strings.TrimSpace(text) != "" {
					flatParts = append(flatParts, text)
				}
				continue
			}
			appendFlatParts()
			role, _ := part["role"].(string)
			nr := normalizeResponsesRole(role)
			reasoning := ""
			if nr == inference.RoleAssistant {
				reasoning = takeReasoning()
			} else {
				pendingReasoning = "" // a fresh user turn drops any dangling reasoning
			}
			// Tolerant: non-text content blocks (images, audio, …) are skipped, and
			// an empty message is dropped — never reject the whole request.
			appendMessage(nr, extractResponsesText(part["content"]), reasoning)
		default:
			// Unknown structural item type: keep its message content if any, else skip
			// (tolerant — never reject a whole request over an unmodeled item type).
			if _, hasContent := part["content"]; hasContent {
				appendFlatParts()
				role, _ := part["role"].(string)
				appendMessage(normalizeResponsesRole(role), extractResponsesText(part["content"]), "")
			}
		}
	}

	appendFlatParts()
	if len(messages) == 0 {
		return nil, &inference.Error{Code: codeOpenAIInputRequired, Message: msgOpenAIResponsesInputRequired}
	}
	return messages, nil
}

// extractReasoningText pulls the text of a replayed Responses `reasoning` item:
// the `content` array of {type:"reasoning_text", text} blocks (llama.cpp's shape),
// falling back to the `summary` array of {type:"summary_text", text} blocks. Both
// are tolerant — a non-text/odd block is skipped. Returns "" when there is none.
func extractReasoningText(part map[string]any) string {
	join := func(value any) string {
		arr, ok := value.([]any)
		if !ok {
			return ""
		}
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := block["text"].(string); ok && strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		}
		return strings.Join(out, "\n")
	}
	if t := join(part["content"]); t != "" {
		return t
	}
	return join(part["summary"])
}

// extractResponsesText pulls the concatenated text from a Responses message
// `content` (a string, or an array of content blocks). Non-text blocks (images,
// audio, …) are skipped, never rejected; returns "" when there is no text.
func extractResponsesText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text := blockText(block); strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// blockText returns the text of a Responses content block if it is a text block
// (text / input_text / output_text), else "" — never an error.
func blockText(part map[string]any) string {
	if contentType, ok := part["type"].(string); ok {
		switch contentType {
		case "text", "input_text", "output_text":
		default:
			return ""
		}
	}
	text, _ := part["text"].(string)
	return text
}

// normalizeResponsesRole maps a Responses message role to an internal role,
// defaulting an empty role to user and folding "developer" into system.
func normalizeResponsesRole(role string) inference.Role {
	switch strings.TrimSpace(strings.ToLower(role)) {
	case "", "user":
		return inference.RoleUser
	case "assistant":
		return inference.RoleAssistant
	case "system", "developer":
		return inference.RoleSystem
	case "tool":
		return inference.RoleTool
	default:
		return inference.Role(role)
	}
}

// responsesCallID returns the tool-call correlation id: `call_id` if present
// (the id shared between a function_call and its function_call_output), else `id`.
func responsesCallID(part map[string]any) string {
	if v := stringField(part, "call_id"); v != "" {
		return v
	}
	return stringField(part, "id")
}

func stringField(part map[string]any, key string) string {
	v, _ := part[key].(string)
	return v
}

// stringifyResponsesArguments returns function-call arguments as a raw JSON
// string. The wire form is already a string; an object (lenient clients) is
// marshaled; anything else becomes "{}".
func stringifyResponsesArguments(value any) string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return "{}"
		}
		return typed
	case nil:
		return "{}"
	default:
		if b, err := json.Marshal(typed); err == nil {
			return string(b)
		}
		return "{}"
	}
}

// stringifyResponsesOutput flattens a function_call_output `output` (a string, or
// an array of content blocks / an object in newer specs) into text.
func stringifyResponsesOutput(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := block["text"].(string); ok && t != "" {
				parts = append(parts, t)
			} else if o, ok := block["output"].(string); ok && o != "" {
				parts = append(parts, o)
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		if b, err := json.Marshal(typed); err == nil {
			return string(b)
		}
		return ""
	}
}

// parseChatMessageContent parses a chat message "content" into content parts.
// When contentOptional is true (an assistant tool-call turn or a tool-result
// message), a missing/empty content is allowed and yields no parts; a genuinely
// malformed content (unsupported block) is still rejected.
func parseChatMessageContent(value any, contentOptional bool) ([]inference.ContentPart, error) {
	parts, err := parseOpenAIChatContent(value)
	if err != nil {
		if infErr, ok := err.(*inference.Error); ok && contentOptional && infErr.Code == codeOpenAIContentRequired {
			return nil, nil
		}
		return nil, err
	}
	return parts, nil
}

// parseOpenAIChatContent yields text and image content parts from an OpenAI
// chat message "content" (a string or an array of typed blocks).
func parseOpenAIChatContent(value any) ([]inference.ContentPart, error) {
	switch typed := value.(type) {
	case string:
		text, err := requireContent(typed, codeOpenAIContentRequired, msgOpenAIContentRequired)
		if err != nil {
			return nil, err
		}
		return []inference.ContentPart{{Type: inference.ContentText, Text: text}}, nil
	case []any:
		parts := make([]inference.ContentPart, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				return nil, unsupportedContent(codeOpenAIContentUnsupported, msgOpenAIContentUnsupportedGeneric)
			}
			switch block["type"] {
			case "text":
				text, ok := block["text"].(string)
				if !ok {
					return nil, unsupportedContent(codeOpenAIContentUnsupported, "openai text block must have a string text")
				}
				if strings.TrimSpace(text) != "" {
					parts = append(parts, inference.ContentPart{Type: inference.ContentText, Text: text})
				}
			case "image_url":
				img, _ := block["image_url"].(map[string]any)
				url, _ := img["url"].(string)
				if strings.TrimSpace(url) == "" {
					return nil, unsupportedContent(codeOpenAIContentUnsupported, "image_url.url must be a non-empty string")
				}
				parts = append(parts, inference.ContentPart{Type: inference.ContentImage, ImageURL: url})
			default:
				return nil, unsupportedContent(codeOpenAIContentUnsupported, msgOpenAIContentUnsupportedGeneric)
			}
		}
		if len(parts) == 0 {
			return nil, &inference.Error{Code: codeOpenAIContentRequired, Message: msgOpenAIContentRequired}
		}
		return parts, nil
	case nil:
		return nil, &inference.Error{Code: codeOpenAIContentRequired, Message: msgOpenAIContentRequired}
	default:
		return nil, unsupportedContent(codeOpenAIContentUnsupported, msgOpenAIContentUnsupportedGeneric)
	}
}

func requireContent(text string, requiredCode string, requiredMessage string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", &inference.Error{Code: requiredCode, Message: requiredMessage}
	}
	return text, nil
}

func unsupportedContent(code string, message string) error {
	return &inference.Error{
		Code:    code,
		Message: message,
	}
}

type OpenAIChatCompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int              `json:"index"`
	Message      OpenAIMessageOut `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type OpenAIMessageOut struct {
	Role      string              `json:"role"`
	Content   string              `json:"content"`
	ToolCalls []OpenAIToolCallOut `json:"tool_calls,omitempty"`
}

// OpenAIToolCallOut is a Chat Completions tool call in a response message.
type OpenAIToolCallOut struct {
	ID       string                    `json:"id"`
	Type     string                    `json:"type"`
	Function OpenAIToolCallFunctionOut `json:"function"`
}

type OpenAIToolCallFunctionOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func OpenAIChatResponse(id string, model string, text string, toolCalls []inference.ToolCall, usage inference.Usage) OpenAIChatCompletionResponse {
	msg := OpenAIMessageOut{Role: "assistant", Content: text}
	finish := "stop"
	for _, tc := range toolCalls {
		msg.ToolCalls = append(msg.ToolCalls, OpenAIToolCallOut{
			ID:       tc.ID,
			Type:     "function",
			Function: OpenAIToolCallFunctionOut{Name: tc.Name, Arguments: argumentsOrEmpty(tc.Arguments)},
		})
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	return OpenAIChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChoice{
			{Index: 0, Message: msg, FinishReason: finish},
		},
		Usage: OpenAIUsage{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      usage.TotalTokens,
		},
	}
}

// argumentsOrEmpty guarantees function arguments serialize as a non-empty JSON
// string ("{}" for none), as required by clients that parse them as JSON.
func argumentsOrEmpty(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

type OpenAIResponsesResponseBody struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	CreatedAt  int64  `json:"created_at"`
	Status     string `json:"status"`
	Model      string `json:"model"`
	OutputText string `json:"output_text"`
	// Output is heterogeneous: assistant "message" items (OpenAIResponsesMessage)
	// and "function_call" items (OpenAIResponsesFunctionCall).
	Output []any                `json:"output"`
	Usage  OpenAIResponsesUsage `json:"usage"`
}

type OpenAIResponsesMessage struct {
	Type    string                        `json:"type"`
	ID      string                        `json:"id"`
	Status  string                        `json:"status"`
	Role    string                        `json:"role"`
	Content []OpenAIResponsesContentBlock `json:"content"`
}

// OpenAIResponsesFunctionCall is a Responses "function_call" output item. call_id
// is the correlation key the client echoes in the follow-up function_call_output;
// arguments is a JSON string.
type OpenAIResponsesFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// ResponsesFunctionCallItem builds a function_call output item from an internal
// tool call: call_id is the tool call's id (the correlation key), and the item id
// is a distinct "fc_"-prefixed id.
func ResponsesFunctionCallItem(tc inference.ToolCall) OpenAIResponsesFunctionCall {
	callID := tc.ID
	if callID == "" {
		callID = "call_" + tc.Name
	}
	return OpenAIResponsesFunctionCall{
		Type:      "function_call",
		ID:        "fc_" + callID,
		CallID:    callID,
		Name:      tc.Name,
		Arguments: argumentsOrEmpty(tc.Arguments),
		Status:    "completed",
	}
}

type OpenAIResponsesContentBlock struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// OpenAIResponsesReasoning is a Responses "reasoning" output item (the model's
// chain-of-thought), matching llama.cpp's shape: an rs_-prefixed id, an empty
// summary, reasoning_text content blocks, and an empty encrypted_content.
type OpenAIResponsesReasoning struct {
	Type             string                         `json:"type"`
	ID               string                         `json:"id"`
	Summary          []any                          `json:"summary"`
	Content          []OpenAIResponsesReasoningText `json:"content"`
	EncryptedContent string                         `json:"encrypted_content"`
}

type OpenAIResponsesReasoningText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type OpenAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func OpenAIResponsesResponse(id string, model string, text string, reasoning string, toolCalls []inference.ToolCall, usage inference.Usage) OpenAIResponsesResponseBody {
	output := make([]any, 0, 2+len(toolCalls))
	// The reasoning item (if any) comes first, matching the streaming order and
	// llama.cpp, so a client can record + replay the chain-of-thought.
	if reasoning != "" {
		output = append(output, OpenAIResponsesReasoning{
			Type:    "reasoning",
			ID:      "rs_" + id,
			Summary: []any{},
			Content: []OpenAIResponsesReasoningText{{Type: "reasoning_text", Text: reasoning}},
		})
	}
	// Include the assistant message item when there is text, or when there are no
	// tool calls at all (a plain text turn — keep the message item present).
	if text != "" || len(toolCalls) == 0 {
		output = append(output, OpenAIResponsesMessage{
			Type:   "message",
			ID:     id + "_msg",
			Status: "completed",
			Role:   "assistant",
			Content: []OpenAIResponsesContentBlock{
				{Type: "output_text", Text: text, Annotations: []any{}},
			},
		})
	}
	for _, tc := range toolCalls {
		output = append(output, ResponsesFunctionCallItem(tc))
	}
	return OpenAIResponsesResponseBody{
		ID:         id,
		Object:     "response",
		CreatedAt:  time.Now().Unix(),
		Status:     "completed",
		Model:      model,
		OutputText: text,
		Output:     output,
		Usage: OpenAIResponsesUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
		},
	}
}
