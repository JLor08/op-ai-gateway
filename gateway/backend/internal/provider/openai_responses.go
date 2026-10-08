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

// openaiResponsesPath is the one endpoint this client POSTs to. The target's
// Endpoint already carries the ChatGPT backend's .../backend-api/codex prefix
// (routing.vendorSubscriptionOpenAITarget), so the path is the BARE /responses —
// the Codex CLI's own path, NOT the OpenAI-platform /v1/responses. It matches the
// path endpointModeFor hands the native-passthrough layer, so the translate and
// passthrough legs of the SAME target reach the identical upstream URL.
// REVERSE-ENGINEERED / VERIFY-LIVE.
const openaiResponsesPath = "/responses"

// OpenAIResponsesClient is the outbound client for an OpenAI SUBSCRIPTION target
// (the reverse-engineered ChatGPT backend, Responses protocol only). It serves
// BOTH paths of such a target:
//
//   - TRANSLATE (Client/StreamingClient): it renders the provider-neutral
//     inference.Request into a Responses body, POSTs it to {endpoint}/responses,
//     and parses the Responses SSE stream back into neutral values — so a
//     chat/completions (portal-chat) client, which the ChatGPT backend cannot
//     speak directly, is served over the one protocol it does. The backend is
//     stream-only (VERIFY-LIVE), so Complete streams internally and aggregates.
//   - NATIVE PASSTHROUGH (NativeProxyClient): it forwards an inbound /v1/responses
//     (Codex) body verbatim via doNativeProxy, the lossless M5b behaviour, now
//     under this one client so a single registration serves both legs.
//
// The render and parse live HERE rather than in internal/compat because
// internal/provider may not import internal/compat (pinned by internal/archtest);
// compat/openai.go only holds the opposite directions (an inbound Responses
// request -> neutral, neutral -> a client-facing Responses response). The bearer
// and the static Codex headers (OpenAI-Beta, originator, chatgpt-account-id) are
// NOT this client's concern: the gateway attaches them to ctx via
// subscriptionAuthCtx, and applyUpstreamAuth sets them on the request.
//
// EXPERIMENTAL / ToS-sensitive. The constants it depends on are VERIFY-LIVE.
type OpenAIResponsesClient struct {
	http *http.Client
}

func NewOpenAIResponsesClient(httpClient *http.Client) *OpenAIResponsesClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &OpenAIResponsesClient{http: httpClient}
}

var (
	_ Client            = (*OpenAIResponsesClient)(nil)
	_ StreamingClient   = (*OpenAIResponsesClient)(nil)
	_ NativeProxyClient = (*OpenAIResponsesClient)(nil)
)

// Complete drives the stream INTERNALLY (the ChatGPT backend is stream-only —
// VERIFY-LIVE) and aggregates the text, reasoning, tool calls and usage into a
// provider.Response. Unlike CompleteStream it applies target.Timeout, matching the
// other clients' buffered-completion deadline policy.
func (c *OpenAIResponsesClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	body, err := openaiResponsesRequestBody(target, req)
	if err != nil {
		return Response{}, err
	}
	var agg responsesAggregator
	if err := c.stream(ctx, target, body, agg.emit); err != nil {
		return Response{}, err
	}
	return agg.response(), nil
}

// CompleteStream streams a Responses reply, translating the upstream SSE into this
// codebase's StreamEvents. Like the other clients it applies NO target timeout: a
// stream may legitimately run long, and the gateway's idle watchdog governs a
// stalled one.
func (c *OpenAIResponsesClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	body, err := openaiResponsesRequestBody(target, req)
	if err != nil {
		return err
	}
	return c.stream(ctx, target, body, emit)
}

// ProxyNative forwards the raw inbound /v1/responses body to the ChatGPT backend's
// /responses path verbatim (the lossless Codex passthrough). It reuses
// doNativeProxy so this ONE client serves both the passthrough and the translate
// paths of an OpenAI subscription target.
func (c *OpenAIResponsesClient) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	return doNativeProxy(ctx, c.http, target, path, body)
}

// stream POSTs body and runs the Responses SSE parse, emitting neutral events. It
// is shared by CompleteStream (emit is the caller's) and Complete (emit is the
// aggregator's).
func (c *OpenAIResponsesClient) stream(ctx context.Context, target routing.Target, body []byte, emit StreamEmit) error {
	httpResp, err := c.post(ctx, target, body)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	// Tee the raw upstream SSE into the capture sink (bounded) as the scanner reads
	// it. When not capturing, ResponseWriter() is nil and the body is read directly.
	var streamReader io.Reader = httpResp.Body
	if rw := CaptureSinkFrom(ctx).ResponseWriter(); rw != nil {
		streamReader = io.TeeReader(httpResp.Body, rw)
	}
	activity := StreamActivityFrom(ctx)
	st := &openaiResponsesStreamState{tools: map[int]*openaiResponsesStreamTool{}}
	scanner := bufio.NewScanner(streamReader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Only `data:` lines carry an event; each payload's own `type` discriminates
		// it, so the preceding `event:` line is redundant. Comment lines (keepalives)
		// are reported to the activity hook by streamLineData.
		data, ok := streamLineData(strings.TrimSpace(scanner.Text()), activity)
		if !ok {
			continue
		}
		if err := st.apply(data, emit); err != nil {
			return err
		}
		if st.stopped {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: read stream: %v", ErrUnavailable, err)
	}
	// A stream that ends without a terminal event was cut off (a dropped
	// connection, a proxy closing early). Reporting it as Completed would hand the
	// client a partial answer with partial usage as if it were whole.
	if !st.stopped {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: stream ended before response.completed", ErrUnavailable)
	}
	if err := st.emitToolCalls(emit); err != nil {
		return err
	}
	var usage *inference.Usage
	if st.sawUsage {
		u := st.usage
		usage = &u
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: usage, FinishReason: st.finishReason()})
}

// post sends the rendered body to {endpoint}/responses and returns the 2xx
// response (the caller owns closing its Body). Every failure is already mapped to
// a provider error: ErrTimeout when ctx's deadline elapsed, ErrUnavailable for a
// transport error, and unavailableStatus for a non-2xx status (401/403 ->
// ErrAuthRejected, 503 -> ErrUpstreamStarting). It records the outbound request
// and, on success, the response headers with the context's capture sink. The
// credential + the static Codex headers ride on ctx (subscriptionAuthCtx) and are
// set by applyUpstreamAuth; this client sets only Content-Type.
func (c *OpenAIResponsesClient) post(ctx context.Context, target routing.Target, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, openaiResponsesPath), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	sink := CaptureSinkFrom(ctx)
	sink.RecordRequest(httpReq.Header, body)
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		httpResp.Body.Close()
		return nil, unavailableStatus(httpResp.StatusCode)
	}
	sink.RecordResponseHeaders(httpResp.Header)
	return httpResp, nil
}

// ---- request render (neutral -> Responses body) ----

type openaiResponsesRequest struct {
	Model string `json:"model"`
	// Stream is ALWAYS true: the ChatGPT backend is stream-only (VERIFY-LIVE), and
	// Complete streams internally and aggregates. It is a literal, not read from
	// req.Stream, so the body always matches the SSE parser that reads the reply.
	Stream bool `json:"stream"`
	// Instructions is the Responses system prompt: the joined system + developer
	// text. Omitted when empty.
	Instructions    string                     `json:"instructions,omitempty"`
	Input           []openaiResponsesInputItem `json:"input"`
	Tools           []openaiResponsesTool      `json:"tools,omitempty"`
	ToolChoice      any                        `json:"tool_choice,omitempty"`
	Reasoning       *openaiResponsesReasoning  `json:"reasoning,omitempty"`
	MaxOutputTokens int                        `json:"max_output_tokens,omitempty"`
}

// openaiResponsesInputItem is one `input` array item, discriminated by Type:
// "message" (role + content blocks), "function_call" (call_id/name/arguments) or
// "function_call_output" (call_id/output). The unused fields are omitted.
type openaiResponsesInputItem struct {
	Type      string                       `json:"type"`
	Role      string                       `json:"role,omitempty"`
	Content   []openaiResponsesContentPart `json:"content,omitempty"`
	CallID    string                       `json:"call_id,omitempty"`
	Name      string                       `json:"name,omitempty"`
	Arguments string                       `json:"arguments,omitempty"`
	Output    string                       `json:"output,omitempty"`
}

// openaiResponsesContentPart is one content block inside a message item: a user
// turn's text is input_text and an image is input_image (image_url a data: URI or
// an http(s) URL); an assistant turn's text is output_text.
type openaiResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// openaiResponsesTool is a flat Responses function tool (name/description/
// parameters at the top level, unlike the Chat Completions nesting under
// "function").
type openaiResponsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openaiResponsesReasoning struct {
	Effort string `json:"effort"`
}

// openaiResponsesRequestBody renders the neutral request as a Responses body. The
// stream flag is always true (see openaiResponsesRequest.Stream).
func openaiResponsesRequestBody(target routing.Target, req inference.Request) ([]byte, error) {
	instructions, input := openaiResponsesInput(req.Messages)
	body := openaiResponsesRequest{
		Model:        providerModel(target, req),
		Stream:       true,
		Instructions: instructions,
		Input:        input,
	}
	if body.Input == nil {
		// The backend requires a non-null input array; an all-system conversation
		// (no user/assistant/tool turn) sends an empty one rather than JSON null.
		body.Input = []openaiResponsesInputItem{}
	}
	if tools := openaiResponsesTools(req.Tools); len(tools) > 0 {
		body.Tools = tools
		// tool_choice rides WITH tools only: sending it without any tool risks a 400,
		// and it is meaningless with none.
		body.ToolChoice = openaiResponsesToolChoiceFor(req.ToolChoice)
	}
	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
		body.Reasoning = &openaiResponsesReasoning{Effort: effort}
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = req.MaxTokens
	}
	// Temperature and Stop are deliberately NOT forwarded: the ChatGPT backend
	// rejects a Responses body carrying sampling overrides it does not accept for a
	// subscription model (VERIFY-LIVE). Keeping the body minimal is the tolerant
	// choice until a live probe says otherwise.
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	return raw, nil
}

// openaiResponsesInput splits the neutral messages into the Responses `instructions`
// string (system + developer text, blank-line separated) and the `input` items.
// Assistant Reasoning is dropped: a replayable Responses reasoning item needs the
// upstream's encrypted_content, which the neutral model does not keep.
func openaiResponsesInput(messages []inference.Message) (string, []openaiResponsesInputItem) {
	var instructions []string
	var input []openaiResponsesInputItem
	for _, msg := range messages {
		switch msg.Role {
		case inference.RoleSystem, inference.RoleDeveloper:
			if text := msg.Text(); text != "" {
				instructions = append(instructions, text)
			}
		case inference.RoleTool:
			input = append(input, openaiResponsesInputItem{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: msg.Text(),
			})
		case inference.RoleAssistant:
			input = append(input, openaiResponsesAssistantItems(msg)...)
		default:
			if item, ok := openaiResponsesUserItem(msg); ok {
				input = append(input, item)
			}
		}
	}
	return strings.Join(instructions, "\n\n"), input
}

// openaiResponsesUserItem renders a user turn as a message item whose content is
// input_text blocks and input_image blocks. ok is false when the turn has no
// representable content (an image-only turn with an undecodable URL, or an empty
// turn), so it is skipped rather than sent empty.
func openaiResponsesUserItem(msg inference.Message) (openaiResponsesInputItem, bool) {
	content := make([]openaiResponsesContentPart, 0, len(msg.Content))
	for _, p := range msg.Content {
		switch p.Type {
		case inference.ContentText:
			if p.Text != "" {
				content = append(content, openaiResponsesContentPart{Type: "input_text", Text: p.Text})
			}
		case inference.ContentImage:
			if url := openaiResponsesImageURL(p.ImageURL); url != "" {
				content = append(content, openaiResponsesContentPart{Type: "input_image", ImageURL: url})
			}
		}
	}
	if len(content) == 0 {
		return openaiResponsesInputItem{}, false
	}
	return openaiResponsesInputItem{Type: "message", Role: "user", Content: content}, true
}

// openaiResponsesAssistantItems renders an assistant turn: a message item carrying
// its output_text (when it has any), then one function_call item per tool call.
func openaiResponsesAssistantItems(msg inference.Message) []openaiResponsesInputItem {
	var items []openaiResponsesInputItem
	var content []openaiResponsesContentPart
	for _, p := range msg.Content {
		if p.Type == inference.ContentText && p.Text != "" {
			content = append(content, openaiResponsesContentPart{Type: "output_text", Text: p.Text})
		}
	}
	if len(content) > 0 {
		items = append(items, openaiResponsesInputItem{Type: "message", Role: "assistant", Content: content})
	}
	for _, tc := range msg.ToolCalls {
		items = append(items, openaiResponsesInputItem{
			Type:      "function_call",
			CallID:    tc.ID,
			Name:      tc.Name,
			Arguments: openaiResponsesToolArguments(tc.Arguments),
		})
	}
	return items
}

// openaiResponsesToolArguments returns a function_call's arguments as the JSON
// STRING the Responses item carries. Empty arguments become "{}" so a pure
// tool-call turn replayed from history is never sent an empty string the backend
// might reject.
func openaiResponsesToolArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return "{}"
	}
	return arguments
}

// openaiResponsesImageURL passes through a base64 data: URI or an http(s) URL (the
// two shapes input_image accepts) and drops anything else (file:, a relative path)
// by returning "", rather than failing the request.
func openaiResponsesImageURL(url string) string {
	if strings.HasPrefix(url, "data:") || strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		return url
	}
	return ""
}

func openaiResponsesTools(tools []inference.Tool) []openaiResponsesTool {
	out := make([]openaiResponsesTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, openaiResponsesTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// openaiResponsesToolChoiceFor maps the neutral tool_choice (the OpenAI Chat form:
// "auto" / "required" / "none" / {"type":"function","function":{"name":...}}) to
// the Responses form, which takes the strings verbatim and a specific tool as the
// FLAT {"type":"function","name":...}. An already Responses-shaped object passes
// through; anything unrecognised yields nil (the upstream default, auto).
func openaiResponsesToolChoiceFor(choice any) any {
	switch c := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "auto", "required", "none":
			return strings.ToLower(strings.TrimSpace(c))
		}
	case map[string]any:
		kind, _ := c["type"].(string)
		switch kind {
		case "function":
			if fn, ok := c["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					return map[string]any{"type": "function", "name": name}
				}
			}
			if name, _ := c["name"].(string); name != "" { // already flat Responses shape
				return map[string]any{"type": "function", "name": name}
			}
		case "auto", "required", "none":
			return kind
		}
	}
	return nil
}

// ---- stream parse (Responses SSE -> neutral events) ----

// openaiResponsesStreamEvent is the union of the Responses SSE payloads this client
// reads; each event populates only the fields its type defines.
type openaiResponsesStreamEvent struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	// Delta carries the incremental text for response.output_text.delta,
	// response.reasoning_text.delta / response.reasoning_summary_text.delta, and the
	// argument fragment for response.function_call_arguments.delta.
	Delta string `json:"delta"`
	// Arguments is the whole argument string on response.function_call_arguments.done.
	Arguments string `json:"arguments"`
	// Item is the output item on response.output_item.added / .done (a function_call
	// carries call_id/name, and .done may carry the full arguments).
	Item *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	// Response is the terminal event's response object (response.completed /
	// .incomplete), carrying the usage.
	Response *struct {
		Status string                `json:"status"`
		Usage  *openaiResponsesUsage `json:"usage"`
	} `json:"response"`
	Error *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// openaiResponsesUsage is the usage object on a terminal event's response. OpenAI
// semantics: input_tokens ALREADY includes the cached subset (reported separately
// under input_tokens_details.cached_tokens).
type openaiResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u openaiResponsesUsage) canonical() inference.Usage {
	return inference.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.InputTokens + u.OutputTokens,
		CachedTokens: u.InputTokensDetails.CachedTokens,
	}
}

// openaiResponsesStreamTool accumulates one streamed function_call: call_id and
// name arrive in response.output_item.added, the arguments as
// response.function_call_arguments.delta fragments.
type openaiResponsesStreamTool struct {
	callID, name string
	args         strings.Builder
}

// openaiResponsesStreamState is what one scan of a Responses SSE body accumulates:
// the terminal usage, whether a terminal event arrived (stopped) and which kind
// (incomplete), and the in-flight tool calls keyed by output_index.
type openaiResponsesStreamState struct {
	usage      inference.Usage
	sawUsage   bool
	stopped    bool
	incomplete bool
	tools      map[int]*openaiResponsesStreamTool
	order      []int
}

// finishReason derives the terminal OpenAI finish_reason from the accumulated
// state: "length" for an incomplete (truncated) stream, "tool_calls" when any tool
// call was assembled, else "stop".
func (st *openaiResponsesStreamState) finishReason() string {
	switch {
	case st.incomplete:
		return "length"
	case len(st.order) > 0:
		return "tool_calls"
	default:
		return "stop"
	}
}

// apply decodes one SSE `data:` payload and applies it, emitting text and
// reasoning deltas as they arrive. Tool calls are only accumulated here and emitted
// whole by emitToolCalls, after the terminal event. An undecodable payload or an
// event of an unmodeled type (response.created, response.in_progress,
// response.output_item.added for a message, response.content_part.*,
// response.output_text.done, codex.rate_limits — M6 scrapes those from headers) is
// skipped; response.failed and an `error` event abort with ErrUnavailable.
func (st *openaiResponsesStreamState) apply(data string, emit StreamEmit) error {
	var ev openaiResponsesStreamEvent
	if json.Unmarshal([]byte(data), &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != "" {
			return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: ev.Delta})
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if ev.Delta != "" {
			return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: ev.Delta})
		}
	case "response.output_item.added":
		st.startItem(ev)
	case "response.function_call_arguments.delta":
		st.appendArgs(ev.OutputIndex, ev.Delta)
	case "response.function_call_arguments.done":
		st.setArgsIfEmpty(ev.OutputIndex, ev.Arguments)
	case "response.output_item.done":
		st.finishItem(ev)
	case "response.completed":
		st.applyTerminal(ev)
		st.stopped = true
	case "response.incomplete":
		st.applyTerminal(ev)
		st.incomplete = true
		st.stopped = true
	case "response.failed":
		return responsesStreamError(ev)
	case "error":
		return responsesStreamError(ev)
	}
	return nil
}

// startItem handles response.output_item.added: a function_call item opens an
// accumulator keyed by its output_index. Any other item type (message, reasoning)
// is ignored — its text arrives through the *_text.delta events above.
func (st *openaiResponsesStreamState) startItem(ev openaiResponsesStreamEvent) {
	if ev.Item == nil || ev.Item.Type != "function_call" {
		return
	}
	if _, ok := st.tools[ev.OutputIndex]; !ok {
		st.order = append(st.order, ev.OutputIndex)
	}
	tool := &openaiResponsesStreamTool{callID: openaiResponsesCallID(ev.Item.CallID, ev.Item.ID), name: ev.Item.Name}
	if ev.Item.Arguments != "" {
		tool.args.WriteString(ev.Item.Arguments)
	}
	st.tools[ev.OutputIndex] = tool
}

// appendArgs appends an argument fragment to the tool at index, bounding the
// accumulation so a misbehaving upstream cannot grow memory without limit.
func (st *openaiResponsesStreamState) appendArgs(index int, fragment string) {
	tool, ok := st.tools[index]
	if !ok || fragment == "" {
		return
	}
	if room := maxToolArgumentsBytes - tool.args.Len(); room > 0 {
		tool.args.WriteString(fragment[:min(room, len(fragment))])
	}
}

// setArgsIfEmpty records the whole-arguments string an upstream may send on
// response.function_call_arguments.done, used only when no delta fragments were
// accumulated (a backend that sends the arguments once rather than streaming them).
func (st *openaiResponsesStreamState) setArgsIfEmpty(index int, arguments string) {
	if tool, ok := st.tools[index]; ok && tool.args.Len() == 0 && arguments != "" {
		tool.args.WriteString(arguments[:min(maxToolArgumentsBytes, len(arguments))])
	}
}

// finishItem handles response.output_item.done: it backfills a function_call's
// call_id / name / arguments from the completed item when the streamed events did
// not carry them (a backend that omits output_item.added, or streams no argument
// deltas). A non-function_call done item is ignored.
func (st *openaiResponsesStreamState) finishItem(ev openaiResponsesStreamEvent) {
	if ev.Item == nil || ev.Item.Type != "function_call" {
		return
	}
	tool, ok := st.tools[ev.OutputIndex]
	if !ok {
		st.order = append(st.order, ev.OutputIndex)
		tool = &openaiResponsesStreamTool{}
		st.tools[ev.OutputIndex] = tool
	}
	if tool.callID == "" {
		tool.callID = openaiResponsesCallID(ev.Item.CallID, ev.Item.ID)
	}
	if tool.name == "" {
		tool.name = ev.Item.Name
	}
	if tool.args.Len() == 0 && ev.Item.Arguments != "" {
		tool.args.WriteString(ev.Item.Arguments[:min(maxToolArgumentsBytes, len(ev.Item.Arguments))])
	}
}

// applyTerminal records the usage carried by a terminal event's response object.
func (st *openaiResponsesStreamState) applyTerminal(ev openaiResponsesStreamEvent) {
	if ev.Response != nil && ev.Response.Usage != nil {
		st.usage = ev.Response.Usage.canonical()
		st.sawUsage = true
	}
}

// emitToolCalls emits each assembled tool call, in the order its item opened,
// before the terminal Completed event. A call that streamed no argument fragments
// takes the empty object.
func (st *openaiResponsesStreamState) emitToolCalls(emit StreamEmit) error {
	for _, idx := range st.order {
		tool := st.tools[idx]
		args := tool.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		call := &inference.ToolCall{ID: tool.callID, Name: tool.name, Arguments: args}
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: call}); err != nil {
			return err
		}
	}
	return nil
}

// openaiResponsesCallID returns the function call's correlation id: call_id when
// present (the key the follow-up function_call_output echoes), else the item id.
func openaiResponsesCallID(callID, itemID string) string {
	if strings.TrimSpace(callID) != "" {
		return callID
	}
	return itemID
}

// responsesStreamError builds the ErrUnavailable a response.failed / `error` event
// aborts the stream with, naming the upstream error when one is carried.
func responsesStreamError(ev openaiResponsesStreamEvent) error {
	if ev.Error != nil && (ev.Error.Message != "" || ev.Error.Type != "" || ev.Error.Code != "") {
		return fmt.Errorf("%w: upstream stream error: %s %s: %s", ErrUnavailable, ev.Error.Type, ev.Error.Code, ev.Error.Message)
	}
	return fmt.Errorf("%w: upstream stream %s", ErrUnavailable, ev.Type)
}

// ---- Complete aggregation (neutral events -> Response) ----

// responsesAggregator collapses the neutral StreamEvents that stream emits into a
// single provider.Response, so Complete can drive the stream-only backend
// internally.
type responsesAggregator struct {
	text      strings.Builder
	reasoning strings.Builder
	toolCalls []inference.ToolCall
	usage     inference.Usage
	finish    string
}

func (a *responsesAggregator) emit(ev inference.StreamEvent) error {
	switch ev.Type {
	case inference.StreamEventTextDelta:
		a.text.WriteString(ev.Text)
		a.reasoning.WriteString(ev.Reasoning)
	case inference.StreamEventToolCall:
		if ev.ToolCall != nil {
			a.toolCalls = append(a.toolCalls, *ev.ToolCall)
		}
	case inference.StreamEventCompleted:
		if ev.Usage != nil {
			a.usage = *ev.Usage
		}
		a.finish = ev.FinishReason
	}
	return nil
}

func (a *responsesAggregator) response() Response {
	return Response{
		Text:         a.text.String(),
		Usage:        a.usage,
		ToolCalls:    a.toolCalls,
		Reasoning:    a.reasoning.String(),
		FinishReason: a.finish,
	}
}
