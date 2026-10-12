package geminiwire

import (
	"bytes"
	"encoding/json"
	"op-ai-gateway/internal/inference"
	"strings"
)

// Result is the neutral outcome of a Gemini response or stream chunk. It is
// package-local because geminiwire must not import internal/provider; the
// client copies it into provider.Response.
type Result struct {
	Text         string
	Reasoning    string
	ToolCalls    []inference.ToolCall
	Usage        inference.Usage
	FinishReason string
}

// ParseResponse decodes a buffered generateContent response. It errors only on
// malformed JSON; a prompt-blocked reply (no candidates, promptFeedback set)
// is a valid, billed result with empty text and a content_filter finish.
func ParseResponse(raw []byte) (Result, error) {
	res, _, err := parseGenerateContent(raw)
	return res, err
}

// ParseStreamChunk decodes one SSE data payload. res carries that chunk's text
// and reasoning deltas, any whole function calls in it, and its (cumulative)
// usageMetadata; done is true when the chunk reports a finishReason. A chunk
// that carries a functionCall but no finishReason leaves res.FinishReason empty:
// only the chunk that reports the finishReason maps it, and it reflects
// tool_calls only when that same chunk carries a functionCall, so a caller
// aggregating a stream must remember tool calls seen on earlier chunks.
func ParseStreamChunk(raw []byte) (res Result, done bool, err error) {
	return parseGenerateContent(raw)
}

// parseGenerateContent is the shared decoder behind ParseResponse and
// ParseStreamChunk: a buffered response and a stream chunk are the same wire
// shape. It never indexes candidates[0] unconditionally, because a
// prompt-blocked reply has none.
func parseGenerateContent(raw []byte) (Result, bool, error) {
	var resp GenerateContentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Result{}, false, err
	}
	var res Result
	hasToolCall := false
	finish := ""
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = cand.FinishReason
		if cand.Content != nil {
			res.Text, res.Reasoning, res.ToolCalls = partsToNeutral(cand.Content.Parts)
			hasToolCall = len(res.ToolCalls) > 0
		}
	} else if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		finish = "SAFETY" // a blocked prompt is a content_filter; mapped below
	}
	res.FinishReason = mapFinishReason(finish, hasToolCall)
	if resp.UsageMetadata != nil {
		res.Usage = canonicalUsage(resp.UsageMetadata)
	}
	return res, finish != "", nil
}

func partsToNeutral(parts []Part) (text, reasoning string, calls []inference.ToolCall) {
	var tb, rb strings.Builder
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			calls = append(calls, inference.ToolCall{
				ID:        encodeToolCallID(p.FunctionCall.Name, p.ThoughtSignature),
				Name:      p.FunctionCall.Name,
				Arguments: compactArgs(p.FunctionCall.Args),
			})
		case p.Thought:
			rb.WriteString(p.Text)
		case p.Text != "":
			tb.WriteString(p.Text)
		}
	}
	return tb.String(), rb.String(), calls
}

func compactArgs(args json.RawMessage) string {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "{}"
	}
	return buf.String()
}

// mapFinishReason folds a Gemini finishReason to the OpenAI vocabulary the
// neutral model uses. A present functionCall makes a plain stop a tool_calls.
func mapFinishReason(reason string, hasToolCall bool) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "LANGUAGE",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "IMAGE_OTHER":
		return "content_filter"
	default: // STOP, MALFORMED_FUNCTION_CALL, MISSING_THOUGHT_SIGNATURE, OTHER, newer
		if hasToolCall {
			return "tool_calls"
		}
		return "stop"
	}
}

// canonicalUsage maps Gemini usageMetadata to neutral Usage in the OpenAI
// convention: InputTokens already includes cached; OutputTokens includes
// reasoning (thoughts); input+output stays consistent with the reported total.
func canonicalUsage(u *UsageMetadata) inference.Usage {
	return inference.Usage{
		InputTokens:  u.PromptTokenCount,
		CachedTokens: u.CachedContentTokenCount,
		OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		TotalTokens:  u.TotalTokenCount,
	}
}
