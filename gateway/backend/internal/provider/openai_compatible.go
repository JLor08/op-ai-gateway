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
	// liveProgress memoizes the upstreams that REJECTED the two advisory
	// live-progress request parameters -- negative verdicts only, see
	// liveProgressMemo (live_progress.go). Held per client rather than as a package
	// global so its lifetime is the client's: one process-wide client in
	// production, an isolated one per test.
	liveProgress *liveProgressMemo
}

func NewOpenAICompatibleClient(httpClient *http.Client) *OpenAICompatibleClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &OpenAICompatibleClient{http: httpClient, liveProgress: newLiveProgressMemo()}
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
		return Response{}, unavailableStatus(httpResp.StatusCode)
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
			DraftN             int     `json:"draft_n"`
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
		usage.DraftTokens = decoded.Timings.DraftN
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
		return nil, unavailableStatus(httpResp.StatusCode)
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

// streamRequestBody builds the Chat Completions STREAMING body. withLiveProgress
// adds the two advisory live-progress parameters (see live_progress.go); without
// it the body is exactly what this client sent before that feature existed.
//
// Note that dropping them puts `stream_options` back to exactly
// `{"include_usage": true}` rather than removing the key: include_usage is what
// makes the terminal usage chunk arrive at all, and every completed-request
// figure (tokens, cost, the recorded rate) depends on it. Losing an advisory
// mid-stream number must not cost the authoritative final one.
//
// Deliberately a pure function of its arguments, so CompleteStream's retry
// rebuilds the body from scratch instead of mutating the first attempt's map.
func streamRequestBody(target routing.Target, req inference.Request, withLiveProgress bool) ([]byte, error) {
	body := map[string]any{
		"model":          providerModel(target, req),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       openAIMessages(req.Messages),
	}
	openAIToolFields(body, req)
	openAISamplingFields(body, req)
	if withLiveProgress {
		// Ask for an EXACT running output-token count mid-stream. llama.cpp then
		// attaches its timings object (predicted_n + predicted_per_second) to every
		// partial; vLLM puts its running completion_tokens on every chunk.
		// continuous_usage_stats is inert without include_usage, which is set above
		// and must stay set. See live_progress.go for why this is only a hint.
		body["timings_per_token"] = true
		body["stream_options"] = map[string]any{
			"include_usage":          true,
			"continuous_usage_stats": true,
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	return raw, nil
}

// schemaRejectionStatus reports whether status is one an upstream uses to refuse a
// body it could not accept: 400 Bad Request or 422 Unprocessable Entity, and
// nothing else. 503 is excluded ON PURPOSE -- unavailableStatus maps it to
// ErrUpstreamStarting, which the load runner consumes as "still warming up", and
// re-issuing the request would both destroy that signal and hit an upstream that
// is not ready. Every other status keeps its existing meaning too.
func schemaRejectionStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

// CompleteStream streams a chat completion, translating the upstream's SSE into
// this codebase's StreamEvents.
//
// The two live-progress parameters are ADVISORY: no live figure may fail, delay or
// alter a request. So a schema rejection is made a NON-EVENT rather than predicted
// -- the shape clause of live_progress.go's three-layer rule is only a
// performance hint -- and an
// upstream that refuses them is re-asked once without them. The retry is invisible
// to the client because all three of its guards must hold:
//
//  1. the parameters were actually sent (liveProgress). A request that never
//     carried them is never retried, so an ordinary 400 keeps its meaning.
//  2. the failure is of the schema-rejection class: a 400/422 status
//     (schemaRejectionStatus) or an in-stream error frame. Never 503, never any
//     other status.
//  3. nothing has been emitted yet -- an explicit boolean set on the first
//     SUCCESSFUL emit, so the invariant is CHECKED rather than inferred from where
//     the code happens to sit.
//
// With (3) holding, the already-sent 200 + text/event-stream headers stop being a
// liability: there is no failure to report to the client, so the second attempt
// just proceeds, minus one advisory number, and the portal renders the "never
// measured" em-dash it already has for that case.
//
// Payload capture across a retry: CaptureSink.RecordRequest ASSIGNS, so the
// captured request body is the one that actually ran. WriteResponse appends, but a
// rejected attempt never reaches it -- the status check precedes both
// RecordResponseHeaders and the response tee -- so only the rarer in-stream-error
// retry leaves the failed attempt's frames ahead of the served ones, which is a
// faithful record of what the gateway did.
func (c *OpenAICompatibleClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	// Guard (1): wantsLiveProgress's three-layer rule says yes (a persisted
	// verdict, else the shape clause) AND this route is not already known to
	// reject the parameters. An empty memo means "send them" (live_progress.go).
	liveProgress := wantsLiveProgress(target) && !c.liveProgress.rejects(target.RouteID)
	// Guard (3): flipped by the wrapper below on the first emit that RETURNED nil,
	// i.e. the first event the client may already have seen.
	emitted := false
	tracked := func(ev inference.StreamEvent) error {
		if err := emit(ev); err != nil {
			return err
		}
		emitted = true
		return nil
	}
	schemaRejected, err := c.completeStreamAttempt(ctx, target, req, liveProgress, tracked)
	if err == nil || !liveProgress || emitted || !schemaRejected {
		return err
	}
	// Deliberately NOT a loop: exactly one retry, bounded by construction rather
	// than by remembering to clear a flag. The second attempt carries none of the
	// advisory parameters, so whatever it reports is the upstream's real answer and
	// belongs to the client unchanged.
	c.liveProgress.recordRejection(target.RouteID)
	_, err = c.completeStreamAttempt(ctx, target, req, false, tracked)
	return err
}

// completeStreamAttempt performs ONE upstream streaming request and translates its
// SSE into emit calls. Its first result reports whether the attempt failed the way
// an upstream that cannot accept the live-progress parameters fails -- a 400/422
// status, or an in-stream error frame. It is advice, not a verdict: CompleteStream
// acts on it only under its own three guards, so it can never affect a request
// that did not carry the parameters or that has already emitted.
func (c *OpenAICompatibleClient) completeStreamAttempt(ctx context.Context, target routing.Target, req inference.Request, liveProgress bool, emit StreamEmit) (bool, error) {
	raw, err := streamRequestBody(target, req, liveProgress)
	if err != nil {
		return false, err
	}
	httpResp, retryable, err := c.startStreamRequest(ctx, target, raw)
	if err != nil {
		return retryable, err
	}
	defer httpResp.Body.Close()

	// Tool calls arrive incrementally across deltas (id/name in the first chunk,
	// arguments in fragments), keyed by index. Accumulate, then emit one
	// StreamEventToolCall per assembled call at the end (before Completed).
	st := &streamChunkState{toolAcc: map[int]*inference.ToolCall{}}
	// Tee the raw upstream SSE into the capture sink (bounded) as the scanner reads
	// it, so the translated upstream response is captured byte-for-byte. When not
	// capturing, ResponseWriter() is nil and the body is read directly.
	var streamReader io.Reader = httpResp.Body
	if rw := CaptureSinkFrom(ctx).ResponseWriter(); rw != nil {
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
		retryable, err := applyStreamChunk(data, st, emit)
		if err != nil {
			return retryable, err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, ErrTimeout
		}
		return false, fmt.Errorf("%w: read stream: %v", ErrUnavailable, err)
	}
	if err := emitToolCalls(st, emit); err != nil {
		return false, err
	}
	return false, emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: st.usage, FinishReason: st.finishReason})
}

// startStreamRequest builds and sends completeStreamAttempt's upstream HTTP
// request, recording the request (and, on a 2xx status, the response headers)
// with the context's capture sink. This is completeStreamAttempt's original
// inline request-building/sending code, extracted verbatim.
//
// Its second result mirrors completeStreamAttempt's own retry signal: true
// when the response status is in the schema-rejection class
// (schemaRejectionStatus). A non-nil *http.Response is returned only on a 2xx
// status, and only then -- the caller owns closing its Body, exactly as
// completeStreamAttempt's own `defer httpResp.Body.Close()` did before this
// was split out: on every other path (a request/transport error, or a
// non-2xx status), that defer would never have been reached, and here the
// response body is closed inline instead, at the same point (this function
// returning) before completeStreamAttempt ever sees it.
func (c *OpenAICompatibleClient) startStreamRequest(ctx context.Context, target routing.Target, raw []byte) (*http.Response, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, "/v1/chat/completions"), bytes.NewReader(raw))
	if err != nil {
		return nil, false, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	sink := CaptureSinkFrom(ctx)
	sink.RecordRequest(httpReq.Header, raw)
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, false, ErrTimeout
		}
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		httpResp.Body.Close()
		return nil, schemaRejectionStatus(httpResp.StatusCode), unavailableStatus(httpResp.StatusCode)
	}
	sink.RecordResponseHeaders(httpResp.Header)
	return httpResp, false, nil
}

// emitToolCalls emits each of st's fully-assembled tool calls, in the order
// their index first appeared, before the terminal Completed event.
func emitToolCalls(st *streamChunkState, emit StreamEmit) error {
	for _, idx := range st.toolOrder {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: st.toolAcc[idx]}); err != nil {
			return err
		}
	}
	return nil
}

// streamChunkState accumulates completeStreamAttempt's per-chunk results
// across one scan of the upstream SSE body: the last usage frame seen, the
// last non-empty finish_reason (it arrives on the final content chunk, and is
// forwarded on the terminal Completed event so the Anthropic edge can map it
// to a stop_reason), and the incrementally assembled tool calls.
type streamChunkState struct {
	usage        *inference.Usage
	finishReason string
	toolAcc      map[int]*inference.ToolCall
	toolOrder    []int
}

// streamChunk is one decoded OpenAI-compatible chat-completions SSE chunk.
// Named (rather than declared inline in applyStreamChunk, as it originally
// was) purely so applyStreamChunk's helpers below can share the type -- same
// fields, same JSON tags, same zero values as the original inline struct.
type streamChunk struct {
	Choices []streamChunkChoice `json:"choices"`
	Usage   *streamChunkUsage   `json:"usage"`
	Timings *streamChunkTimings `json:"timings"`
	Error   *streamChunkError   `json:"error"`
}

type streamChunkChoice struct {
	Delta        streamChunkDelta `json:"delta"`
	FinishReason string           `json:"finish_reason"`
}

type streamChunkDelta struct {
	Content          string                `json:"content"`
	ReasoningContent string                `json:"reasoning_content"`
	Reasoning        string                `json:"reasoning"`
	ToolCalls        []streamChunkToolCall `json:"tool_calls"`
}

type streamChunkToolCall struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id"`
	Function streamChunkToolFunction `json:"function"`
}

type streamChunkToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type streamChunkUsage struct {
	PromptTokens        int                         `json:"prompt_tokens"`
	CompletionTokens    int                         `json:"completion_tokens"`
	TotalTokens         int                         `json:"total_tokens"`
	PromptTokensDetails streamChunkUsageCacheDetail `json:"prompt_tokens_details"`
}

type streamChunkUsageCacheDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

type streamChunkTimings struct {
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
	PredictedN         int     `json:"predicted_n"`
	DraftN             int     `json:"draft_n"`
}

type streamChunkError struct {
	Message string `json:"message"`
}

// applyStreamChunk decodes one SSE `data:` line's JSON payload and applies it
// to st, emitting any resulting stream events: decode -> compute progress ->
// terminal usage -> emit deltas -> accumulate tool calls (mergeChunkUsage,
// chunkProgress and applyChunkChoice below). This is completeStreamAttempt's
// per-chunk body, extracted verbatim; the scanner loop, its guards, and the
// retry decision all stay in completeStreamAttempt.
//
// Its first result mirrors completeStreamAttempt's own: true when this chunk
// carried an in-stream error frame (the schema-rejection class -- see
// CompleteStream's doc comment, guard 2). A non-nil error (whether from that
// frame or from a failed emit) must be returned by the caller immediately,
// exactly as the original inline code did; a nil error means "continue
// scanning", regardless of the decode outcome.
func applyStreamChunk(data string, st *streamChunkState, emit StreamEmit) (bool, error) {
	var chunk streamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, nil
	}
	if chunk.Error != nil {
		// An in-stream error frame. Reported as retryable too: guard (3) means the
		// client has seen nothing yet, and some OpenAI-compatible proxies (LiteLLM,
		// OpenRouter -- reachable behind a llama_swap `peer`) surface a rejected body
		// as an error EVENT after a 200 rather than as a 400 status.
		return true, fmt.Errorf("%w: upstream stream error: %s", ErrUnavailable, chunk.Error.Message)
	}
	mergeChunkUsage(st, chunk)
	progress := chunkProgress(chunk)
	if len(chunk.Choices) > 0 {
		if err := applyChunkChoice(chunk.Choices[0], progress, st, emit); err != nil {
			return false, err
		}
	}
	return false, nil
}

// mergeChunkUsage applies one decoded chunk's usage/timings fields (if
// present) onto st.usage. Each chunk that carries a usage object OVERWRITES
// st.usage wholesale (not a running merge like mergePassthroughUsage) --
// exactly what the original inline code did, since the OpenAI-compatible
// stream's usage object is already cumulative per chunk.
func mergeChunkUsage(st *streamChunkState, chunk streamChunk) {
	if chunk.Usage != nil {
		total := chunk.Usage.TotalTokens
		if total == 0 {
			total = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
		}
		st.usage = &inference.Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  total,
			CachedTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
		}
		if chunk.Timings != nil {
			st.usage.PromptPerSecond = chunk.Timings.PromptPerSecond
			st.usage.TokensPerSecond = chunk.Timings.PredictedPerSecond
			st.usage.DraftTokens = chunk.Timings.DraftN
		}
	}
}

// chunkProgress computes the running, upstream-reported progress for one
// chunk. Computed OUTSIDE the usage branch on purpose: llama.cpp attaches
// timings to partial chunks that carry no usage object, which is why this
// used to be dropped. Never derived here -- a chunk that reports no exact
// count produces no progress at all.
func chunkProgress(chunk streamChunk) *inference.StreamProgress {
	switch {
	case chunk.Timings != nil && (chunk.Timings.PredictedN > 0 || chunk.Timings.PredictedPerSecond > 0):
		return &inference.StreamProgress{
			OutputTokens:    chunk.Timings.PredictedN,
			TokensPerSecond: chunk.Timings.PredictedPerSecond,
		}
	case chunk.Usage != nil && chunk.Usage.CompletionTokens > 0:
		// vLLM's continuous usage: an exact running count, no rate.
		return &inference.StreamProgress{OutputTokens: chunk.Usage.CompletionTokens}
	}
	return nil
}

// applyChunkChoice handles chunk.Choices[0] for the (single-choice) streaming
// case: records the finish_reason, emits a text/reasoning delta event (if
// either is present, tagged with progress), and accumulates any tool-call
// fragments.
func applyChunkChoice(choice streamChunkChoice, progress *inference.StreamProgress, st *streamChunkState, emit StreamEmit) error {
	if fr := choice.FinishReason; fr != "" {
		st.finishReason = fr
	}
	d := choice.Delta
	reasoning := d.ReasoningContent
	if reasoning == "" {
		reasoning = d.Reasoning
	}
	if d.Content != "" || reasoning != "" {
		if err := emit(inference.StreamEvent{
			Type:      inference.StreamEventTextDelta,
			Text:      d.Content,
			Reasoning: reasoning,
			Progress:  progress,
		}); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		accumulateToolCall(st, tc)
	}
	return nil
}

// accumulateToolCall folds one tool-call delta fragment (id/name in the first
// chunk, arguments in fragments) into st, keyed by index.
func accumulateToolCall(st *streamChunkState, tc streamChunkToolCall) {
	acc, ok := st.toolAcc[tc.Index]
	if !ok {
		acc = &inference.ToolCall{}
		st.toolAcc[tc.Index] = acc
		st.toolOrder = append(st.toolOrder, tc.Index)
	}
	if tc.ID != "" {
		acc.ID = tc.ID
	}
	if tc.Function.Name != "" {
		acc.Name = tc.Function.Name
	}
	// Bound accumulated arguments so a misbehaving upstream can't grow memory
	// without limit; real function arguments are far below this.
	if len(acc.Arguments) < maxToolArgumentsBytes {
		acc.Arguments += tc.Function.Arguments
	}
}
