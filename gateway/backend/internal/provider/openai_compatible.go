// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
)

// maxToolArgumentsBytes bounds the per-tool-call streamed argument accumulation.
const maxToolArgumentsBytes = 1 << 20 // 1 MiB

type OpenAICompatibleClient struct {
	http *http.Client
}

func NewOpenAICompatibleClient(httpClient *http.Client) *OpenAICompatibleClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &OpenAICompatibleClient{http: httpClient}
}

func (c *OpenAICompatibleClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	reqBody := map[string]any{
		"model":    providerModel(target, req),
		"stream":   false,
		"messages": openAIMessages(req.Messages),
	}
	openAIToolFields(reqBody, req)
	openAISamplingFields(reqBody, req)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	sink := CaptureSinkFrom(ctx)
	sink.RecordRequest(httpReq.Header, body)
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, ErrTimeout
		}
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("%w: upstream status %d", ErrUnavailable, httpResp.StatusCode)
	}
	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, ErrTimeout
		}
		return Response{}, fmt.Errorf("%w: read response: %v", ErrInvalidResponse, err)
	}
	sink.RecordResponseHeaders(httpResp.Header)
	sink.WriteResponse(respBytes)
	var decoded struct {
		Choices []struct {
			Message struct {
				Content          string           `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				Reasoning        string           `json:"reasoning"`
				ToolCalls        []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(respBytes, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	if len(decoded.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: missing choice", ErrInvalidResponse)
	}
	choice := decoded.Choices[0]
	toolCalls := toolCallsFrom(choice.Message.ToolCalls)
	reasoning := choice.Message.ReasoningContent
	if reasoning == "" {
		reasoning = choice.Message.Reasoning
	}
	// A tool-call-only OR reasoning-only response has empty content but is valid: a
	// reasoning model can emit only its analysis channel (e.g. truncated at
	// max_tokens), which the streaming path also accepts — reject only a genuinely
	// empty choice (no content, no tool calls, no reasoning).
	if choice.Message.Content == "" && len(toolCalls) == 0 && reasoning == "" {
		return Response{}, fmt.Errorf("%w: missing choice content", ErrInvalidResponse)
	}
	totalTokens := decoded.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = decoded.Usage.PromptTokens + decoded.Usage.CompletionTokens
	}
	usage := inference.Usage{
		InputTokens:  decoded.Usage.PromptTokens,
		OutputTokens: decoded.Usage.CompletionTokens,
		TotalTokens:  totalTokens,
		CachedTokens: decoded.Usage.PromptTokensDetails.CachedTokens,
	}
	if decoded.Timings != nil {
		usage.PromptPerSecond = decoded.Timings.PromptPerSecond
		usage.TokensPerSecond = decoded.Timings.PredictedPerSecond
	}
	return Response{
		Text:         choice.Message.Content,
		Usage:        usage,
		ToolCalls:    toolCalls,
		Reasoning:    reasoning,
		FinishReason: choice.FinishReason,
	}, nil
}

// openAIToolCall is a Chat Completions tool call (used in both the non-stream
// response and — accumulated across deltas — the stream).
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func toolCallsFrom(calls []openAIToolCall) []inference.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]inference.ToolCall, 0, len(calls))
	for _, tc := range calls {
		out = append(out, inference.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out
}

func (c *OpenAICompatibleClient) ListModels(ctx context.Context, target routing.Target) ([]string, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, "/v1/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, httpReq)
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: upstream status %d", ErrUnavailable, httpResp.StatusCode)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	models := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

var _ Prober = (*OpenAICompatibleClient)(nil)

// Probe issues a GET against the application's health path; a 2xx is reachable.
func (c *OpenAICompatibleClient) Probe(ctx context.Context, target routing.Target, path string) error {
	return httpProbe(ctx, c.http, target, path)
}

var _ LoadedModelLister = (*OpenAICompatibleClient)(nil)

// LoadedModels GETs the configured status endpoint and parses the loaded model
// names (llama-swap /running, /v1/models, llama.cpp /props — see parseLoadedModels).
func (c *OpenAICompatibleClient) LoadedModels(ctx context.Context, target routing.Target, statusPath, format string) ([]string, error) {
	return fetchLoadedModels(ctx, c.http, target, statusPath, format)
}

var _ ModelUnloader = (*OpenAICompatibleClient)(nil)

// UnloadModel implements ModelUnloader via llama-swap's unload endpoint. On a non-llama-swap
// OpenAI-compatible upstream (plain llama.cpp, vLLM) the endpoint 404s => (false, nil).
func (c *OpenAICompatibleClient) UnloadModel(ctx context.Context, target routing.Target, model string) (bool, error) {
	if strings.TrimSpace(model) == "" {
		return false, nil
	}
	return postUnloadLlamaSwap(ctx, c.http, target, model)
}

var _ ModelInfoProber = (*OpenAICompatibleClient)(nil)

// ProbeModelInfo GETs the configured probe path (llama.cpp /props) and parses the
// loaded model's name + context size.
func (c *OpenAICompatibleClient) ProbeModelInfo(ctx context.Context, target routing.Target, probePath string) ([]ModelInfo, error) {
	return fetchModelInfo(ctx, c.http, target, probePath)
}

var _ MemoryProber = (*OpenAICompatibleClient)(nil)

// ProbeServerMemory GETs the configured capacity-probe path and parses the upstream
// saturation signal (llama.cpp /metrics Prometheus or /props|/slots JSON).
func (c *OpenAICompatibleClient) ProbeServerMemory(ctx context.Context, target routing.Target, probePath, format string) (ServerMemory, error) {
	return fetchServerMemory(ctx, c.http, target, probePath, format)
}

// openAIMessages builds OpenAI chat messages: a tool-result message becomes
// {role:"tool", tool_call_id, content}; an assistant message with tool calls adds
// a tool_calls array; a message with image parts uses array content with
// image_url blocks; otherwise plain string content.
func openAIMessages(messages []inference.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		m := map[string]any{"role": string(msg.Role)}

		// Tool-result message: {role:"tool", tool_call_id, content}.
		if msg.Role == inference.RoleTool {
			m["content"] = msg.Text()
			if msg.ToolCallID != "" {
				m["tool_call_id"] = msg.ToolCallID
			}
			out = append(out, m)
			continue
		}

		hasImage := false
		for _, p := range msg.Content {
			if p.Type == inference.ContentImage {
				hasImage = true
				break
			}
		}
		if hasImage {
			blocks := make([]map[string]any, 0, len(msg.Content))
			for _, p := range msg.Content {
				switch p.Type {
				case inference.ContentText:
					if p.Text != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
					}
				case inference.ContentImage:
					blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL}})
				}
			}
			m["content"] = blocks
		} else {
			m["content"] = msg.Text()
		}

		// Assistant tool/function calls.
		if len(msg.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				args := tc.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				calls = append(calls, map[string]any{
					"id":       tc.ID,
					"type":     "function",
					"function": map[string]any{"name": tc.Name, "arguments": args},
				})
			}
			m["tool_calls"] = calls
		}
		// Thread the assistant turn's chain-of-thought back to the upstream as
		// reasoning_content so a reasoning model (harmony/gpt-oss) keeps continuity
		// across the tool loop. Non-standard but honored by llama.cpp; other servers
		// ignore an unknown field.
		if msg.Role == inference.RoleAssistant && msg.Reasoning != "" {
			m["reasoning_content"] = msg.Reasoning
		}
		out = append(out, m)
	}
	return out
}

// openAITools converts internal tool definitions into the Chat Completions
// (nested) tool shape: {type:"function", function:{name, description, parameters}}.
func openAITools(tools []inference.Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if t.Parameters != nil {
			fn["parameters"] = t.Parameters
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// openAIToolFields adds tools + tool_choice to the request body when present.
func openAIToolFields(body map[string]any, req inference.Request) {
	if tools := openAITools(req.Tools); len(tools) > 0 {
		body["tools"] = tools
		if req.ToolChoice != nil {
			body["tool_choice"] = req.ToolChoice
		}
	}
}

// openAISamplingFields adds temperature/max_tokens/stop/reasoning_effort to the
// request body when set.
func openAISamplingFields(body map[string]any, req inference.Request) {
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
}

var _ NativeProxyClient = (*OpenAICompatibleClient)(nil)

// ProxyNative forwards the raw client body to the upstream's own endpoint path
// (native passthrough for Codex /v1/responses and Claude Code /v1/messages).
func (c *OpenAICompatibleClient) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	return doNativeProxy(ctx, c.http, target, path, body)
}

var _ StreamingClient = (*OpenAICompatibleClient)(nil)

func (c *OpenAICompatibleClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	body := map[string]any{
		"model":          providerModel(target, req),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       openAIMessages(req.Messages),
	}
	openAIToolFields(body, req)
	openAISamplingFields(body, req)
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, "/v1/chat/completions"), bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	sink := CaptureSinkFrom(ctx)
	sink.RecordRequest(httpReq.Header, raw)
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("%w: upstream status %d", ErrUnavailable, httpResp.StatusCode)
	}
	sink.RecordResponseHeaders(httpResp.Header)

	var usage *inference.Usage
	// The last non-empty finish_reason seen across chunks (it arrives on the final
	// content chunk). Forwarded on the terminal Completed event so the Anthropic
	// edge can map it to a stop_reason.
	var finishReason string
	// Tool calls arrive incrementally across deltas (id/name in the first chunk,
	// arguments in fragments), keyed by index. Accumulate, then emit one
	// StreamEventToolCall per assembled call at the end (before Completed).
	toolAcc := map[int]*inference.ToolCall{}
	var toolOrder []int
	// Tee the raw upstream SSE into the capture sink (bounded) as the scanner reads
	// it, so the translated upstream response is captured byte-for-byte. When not
	// capturing, ResponseWriter() is nil and the body is read directly.
	var streamReader io.Reader = httpResp.Body
	if rw := sink.ResponseWriter(); rw != nil {
		streamReader = io.TeeReader(httpResp.Body, rw)
	}
	scanner := bufio.NewScanner(streamReader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				TotalTokens         int `json:"total_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Timings *struct {
				PromptPerSecond    float64 `json:"prompt_per_second"`
				PredictedPerSecond float64 `json:"predicted_per_second"`
			} `json:"timings"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return fmt.Errorf("%w: upstream stream error: %s", ErrUnavailable, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			total := chunk.Usage.TotalTokens
			if total == 0 {
				total = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
			}
			usage = &inference.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  total,
				CachedTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
			}
			if chunk.Timings != nil {
				usage.PromptPerSecond = chunk.Timings.PromptPerSecond
				usage.TokensPerSecond = chunk.Timings.PredictedPerSecond
			}
		}
		if len(chunk.Choices) > 0 {
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				finishReason = fr
			}
			d := chunk.Choices[0].Delta
			reasoning := d.ReasoningContent
			if reasoning == "" {
				reasoning = d.Reasoning
			}
			if d.Content != "" || reasoning != "" {
				if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: d.Content, Reasoning: reasoning}); err != nil {
					return err
				}
			}
			for _, tc := range d.ToolCalls {
				acc, ok := toolAcc[tc.Index]
				if !ok {
					acc = &inference.ToolCall{}
					toolAcc[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				// Bound accumulated arguments so a misbehaving upstream can't grow
				// memory without limit; real function arguments are far below this.
				if len(acc.Arguments) < maxToolArgumentsBytes {
					acc.Arguments += tc.Function.Arguments
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: read stream: %v", ErrUnavailable, err)
	}
	// Emit each fully-assembled tool call before the terminal Completed event.
	for _, idx := range toolOrder {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: toolAcc[idx]}); err != nil {
			return err
		}
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: usage, FinishReason: finishReason})
}
