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

const (
	// anthropicVersionHeader / anthropicAPIVersion: api.anthropic.com requires an
	// explicit API version on every call. It is intrinsic to the wire format this
	// client renders and parses, so the client always sets it; the credential
	// header (x-api-key, or a bearer) is the caller's, applied by
	// applyUpstreamAuth.
	anthropicVersionHeader = "anthropic-version"
	anthropicAPIVersion    = "2023-06-01"
	// anthropicMessagesPath is the one endpoint the client calls.
	anthropicMessagesPath = "/v1/messages"
	// anthropicDefaultMaxTokens stands in when the neutral request leaves
	// MaxTokens unset: Anthropic, unlike OpenAI, REQUIRES max_tokens.
	anthropicDefaultMaxTokens = 4096
	// anthropicMaxTemperature is Anthropic's upper bound for temperature (OpenAI
	// allows up to 2, which Anthropic rejects with a 400).
	anthropicMaxTemperature = 1.0
	// claudeCodeSystemPrompt is the EXACT first system block the Claude-Code
	// masquerade prepends on the subscription (OAuth) Messages path, matching what
	// the Claude Code CLI sends. api.anthropic.com's OAuth bearer path expects this
	// as the first system block, so it must stay BYTE-FOR-BYTE identical.
	// REVERSE-ENGINEERED / VERIFY LIVE.
	claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
)

// AnthropicClient is the native Anthropic Messages API (/v1/messages) client for
// a vendor-account target (api.anthropic.com). It serves two paths:
//
//   - TRANSLATE (Client / StreamingClient): it renders the provider-neutral
//     inference.Request into a Messages body and parses the Messages response /
//     SSE stream back into neutral values.
//   - NATIVE PASSTHROUGH (NativeProxyClient): ProxyNative relays an inbound
//     /v1/messages body to the same endpoint, the lossless Claude Code path, via
//     doNativeProxyWithDefaults. The body is verbatim except that a subscription
//     (Masquerade) target gets the Claude-Code system block injected first (see
//     ProxyNative); an api-key target's body is never touched.
//
// It deliberately has no model lister or prober.
//
// The render and parse live HERE rather than in internal/compat because
// internal/provider may not import internal/compat (pinned by
// internal/archtest); compat/anthropic.go only holds the opposite directions
// (Anthropic client request -> neutral, neutral -> Anthropic client response).
type AnthropicClient struct {
	http *http.Client
}

func NewAnthropicClient(httpClient *http.Client) *AnthropicClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AnthropicClient{http: httpClient}
}

var (
	_ Client            = (*AnthropicClient)(nil)
	_ StreamingClient   = (*AnthropicClient)(nil)
	_ NativeProxyClient = (*AnthropicClient)(nil)
)

// ProxyNative forwards the raw inbound /v1/messages body to the upstream's own
// endpoint path and returns the upstream response, Body still open, for the
// gateway to relay byte-for-byte. The body is relayed VERBATIM (the gateway has
// already set the upstream model name) with one exception: for a subscription
// (OAuth) target, Target.Masquerade == MasqueradeClaudeCode, the Claude-Code
// system block is injected as the first `system` block, exactly as the translate
// path renders it (see injectClaudeCodeSystemBlock). An api-key target (empty
// Masquerade) is never touched, so its body stays byte-identical.
//
// api.anthropic.com REQUIRES anthropic-version on every call, so ProxyNative
// guarantees it ITSELF (as the default set passed to doNativeProxyWithDefaults)
// instead of relying on the caller's target carrying it: the header can never go
// missing whatever resolved the target. A ctx-carried anthropic-version (the
// target's ExtraHeaders) still overrides the default, exactly as on the translate
// path. The inbound client's own anthropic-version is not forwarded. The
// credential (x-api-key, or a bearer) is the ctx's, applied by applyUpstreamAuth.
//
// Redirects are not followed: net/http strips only Authorization, not x-api-key,
// from a request that follows a redirect off the host, so following one could
// carry the credential elsewhere. api.anthropic.com does not redirect; a 3xx is
// returned to the caller as the upstream's answer.
func (c *AnthropicClient) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	if target.Masquerade == routing.MasqueradeClaudeCode {
		body = injectClaudeCodeSystemBlock(body)
	}
	return doNativeProxyWithDefaults(ctx, withoutRedirects(c.http), target, path, body, map[string]string{anthropicVersionHeader: anthropicAPIVersion})
}

// injectClaudeCodeSystemBlock returns body with the Claude-Code system block
// (claudeCodeSystemPrompt) as the first `system` block, the shape
// anthropicSystemField renders on the translate path and the one api.anthropic.com's
// OAuth bearer path expects. It is a pure function of its argument: body is never
// written to, and an edited body is always a FRESH slice from json.Marshal.
//
// The edit depends on the client's `system`:
//
//   - an array already starting with the exact block (the real Claude Code client):
//     body is returned UNCHANGED, so the injection is idempotent;
//   - any other array: the block is prepended, every existing block kept as
//     written, cache_control included;
//   - a non-empty string s: [block, {"type":"text","text":s}];
//   - absent, null or "": [block].
//
// The body is decoded with UseNumber and re-marshalled, so every value (a large
// integer, a float's exact spelling) is relayed losslessly; only key order and
// insignificant whitespace/escaping change. It fails OPEN: a body that is not a
// single JSON object, or whose `system` is of a type the Messages API does not
// accept (an object, a number, a bool), is returned unchanged for the upstream to
// reject itself.
func injectClaudeCodeSystemBlock(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return body
	}
	// Decode stops after the first value; trailing data means this is not the
	// single object the upstream would parse, so leave the body alone.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return body
	}
	system, ok := claudeCodeSystemFor(obj["system"])
	if !ok {
		return body
	}
	obj["system"] = system
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// claudeCodeSystemFor maps a client's decoded `system` value to the one carrying
// the Claude-Code block first. ok is false when the value needs no edit (the block
// is already first) or is of a type this does not edit.
func claudeCodeSystemFor(system any) (any, bool) {
	switch s := system.(type) {
	case nil:
		return anthropicSystemField("", routing.MasqueradeClaudeCode), true
	case string:
		return anthropicSystemField(s, routing.MasqueradeClaudeCode), true
	case []any:
		if startsWithClaudeCodeBlock(s) {
			return nil, false
		}
		blocks := make([]any, 0, len(s)+1)
		blocks = append(blocks, anthropicSystemBlock{Type: "text", Text: claudeCodeSystemPrompt})
		return append(blocks, s...), true
	default:
		return nil, false
	}
}

// startsWithClaudeCodeBlock reports whether blocks' first element is a text block
// whose text is exactly claudeCodeSystemPrompt. Other fields on it (cache_control)
// do not matter.
func startsWithClaudeCodeBlock(blocks []any) bool {
	if len(blocks) == 0 {
		return false
	}
	first, ok := blocks[0].(map[string]any)
	if !ok {
		return false
	}
	typ, _ := first["type"].(string)
	text, _ := first["text"].(string)
	return typ == "text" && text == claudeCodeSystemPrompt
}

// withoutRedirects returns a copy of c that hands a 3xx answer back instead of
// following it. The caller's client (shared across every provider) is left
// untouched.
func withoutRedirects(c *http.Client) *http.Client {
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

func (c *AnthropicClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	body, err := anthropicRequestBody(target, req, false)
	if err != nil {
		return Response{}, err
	}
	httpResp, err := c.post(ctx, target, body)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()
	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, ErrTimeout
		}
		return Response{}, fmt.Errorf("%w: read response: %v", ErrInvalidResponse, err)
	}
	CaptureSinkFrom(ctx).WriteResponse(respBytes)
	return parseAnthropicResponse(respBytes)
}

// CompleteStream streams a Messages response, translating the upstream's SSE into
// this codebase's StreamEvents. Like the other clients it applies NO target
// timeout: a stream may legitimately run long, and the gateway's idle watchdog
// governs a stalled one.
func (c *AnthropicClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	body, err := anthropicRequestBody(target, req, true)
	if err != nil {
		return err
	}
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
	st := &anthropicStreamState{tools: map[int]*anthropicStreamTool{}}
	if err := c.scanStream(ctx, streamReader, st, emit); err != nil {
		return err
	}
	// A stream that ends without its terminal event was cut off (a dropped
	// connection, a proxy closing early). Reporting it as Completed would hand the
	// client a partial answer with partial usage as if it were whole.
	if !st.stopped && st.stopReason == "" {
		return fmt.Errorf("%w: stream ended before message_stop", ErrUnavailable)
	}
	if err := st.emitToolCalls(emit); err != nil {
		return err
	}
	var usage *inference.Usage
	if st.sawUsage {
		u := st.usage.canonical()
		usage = &u
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: usage, FinishReason: anthropicFinishReason(st.stopReason)})
}

// scanStream reads the upstream SSE line by line, applying each event to st until
// the stream stops or ends. Only `data:` lines matter: every Anthropic event's
// JSON carries its own `type`, so the preceding `event:` line is redundant.
// Comment lines (keepalives) are reported to the activity hook by streamLineData.
// A scanner error is mapped to the canonical provider error (ErrTimeout when ctx's
// deadline elapsed, ErrUnavailable otherwise).
func (c *AnthropicClient) scanStream(ctx context.Context, r io.Reader, st *anthropicStreamState, emit StreamEmit) error {
	activity := StreamActivityFrom(ctx)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
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
	return nil
}

// post sends the rendered body to the Messages endpoint and returns the 2xx
// response (the caller owns closing its Body). Every failure is already mapped to
// a provider error: ErrTimeout when ctx's deadline elapsed, ErrUnavailable for a
// transport error, and unavailableStatus for a non-2xx status. It records the
// outbound request and, on success, the response headers with the context's
// capture sink.
func (c *AnthropicClient) post(ctx context.Context, target routing.Target, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, anthropicMessagesPath), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	httpReq.Header.Set(anthropicVersionHeader, anthropicAPIVersion)
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

// ---- request render (neutral -> Messages) ----

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
	// System is the Messages `system` field. It is a plain string for the ordinary
	// path (nil when empty, so the field is omitted) and an ARRAY of text blocks
	// for the Claude-Code masquerade, whose first block is exactly
	// claudeCodeSystemPrompt. The type is `any` so one field carries both shapes;
	// omitempty omits it only when nil, so an empty system is set to nil, not "".
	System        any                `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    map[string]any     `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one request content block: text, image, tool_use or
// tool_result, discriminated by Type (the other fields are omitted when unset).
type anthropicBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   string                `json:"content,omitempty"`
	// CacheControl marks this block as a prompt-cache breakpoint. Set only on the
	// last block of the last turn, and only when the request carries an enabled
	// PromptCacheDirective; nil (omitted) otherwise.
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl is the Messages `cache_control` breakpoint marker.
type anthropicCacheControl struct {
	Type string `json:"type"`          // always "ephemeral"
	TTL  string `json:"ttl,omitempty"` // reserved for "1h"; never set in v1
}

// ephemeralCacheControl returns the breakpoint marker for a cache directive. v1
// supports the 5-minute ephemeral default only, so the directive's TTL is not
// emitted (a `ttl` field is left unset); "1h" is reserved for a later change.
func ephemeralCacheControl(_ string) *anthropicCacheControl {
	return &anthropicCacheControl{Type: "ephemeral"}
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicRequestBody renders the neutral request as a Messages body. stream is
// set by the calling method (Complete / CompleteStream), not read from
// req.Stream, so the body always matches the parser that will read the reply.
func anthropicRequestBody(target routing.Target, req inference.Request, stream bool) ([]byte, error) {
	system, messages := anthropicMessages(req.Messages)
	body := anthropicRequest{
		Model:     providerModel(target, req),
		MaxTokens: req.MaxTokens,
		Stream:    stream,
		System:    anthropicSystemField(system, target.Masquerade),
		Messages:  messages,
	}
	// Prompt caching: only when the gateway directed it. The two breakpoints are the
	// last system block and the last turn's last content block, which together
	// cover the stable prefix (system + prior turns) and the growing conversation
	// tail. With no directive (or Enabled=false) nothing below runs and the render
	// is byte-identical to the non-caching one.
	if req.PromptCache != nil && req.PromptCache.Enabled {
		cc := ephemeralCacheControl(req.PromptCache.TTL)
		body.System = anthropicSystemFieldCached(system, target.Masquerade, cc)
		if n := len(body.Messages); n > 0 {
			m := &body.Messages[n-1]
			if c := len(m.Content); c > 0 {
				m.Content[c-1].CacheControl = cc
			}
		}
	}
	if body.MaxTokens <= 0 {
		body.MaxTokens = anthropicDefaultMaxTokens
	}
	if tools := anthropicTools(req.Tools); len(tools) > 0 {
		body.Tools = tools
		// Anthropic rejects a tool_choice without tools, so it rides with them.
		body.ToolChoice = anthropicToolChoiceFor(req.ToolChoice)
	}
	if req.Temperature != nil {
		t := min(*req.Temperature, anthropicMaxTemperature)
		body.Temperature = &t
	}
	for _, s := range req.Stop {
		if s != "" {
			body.StopSequences = append(body.StopSequences, s)
		}
	}
	// ReasoningEffort has no Messages equivalent (extended thinking is a token
	// budget, not an effort level) and is dropped.
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	return raw, nil
}

// anthropicSystemBlock is one element of the Messages `system` array — the block
// form used for the Claude-Code masquerade (the API accepts `system` as either a
// plain string or an array of typed text blocks).
type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicSystemField builds the Messages `system` field from the joined system
// text and the target's masquerade:
//
//   - No masquerade: the plain joined string, or nil when empty so `system` is
//     omitted (byte-identical to the pre-masquerade behaviour).
//   - MasqueradeClaudeCode: an ARRAY whose FIRST block is exactly
//     claudeCodeSystemPrompt and whose second block (when the caller supplied any
//     system text) is that text — the shape the Claude Code CLI sends on the OAuth
//     Messages path. The exact first block is what makes the OAuth bearer accepted.
func anthropicSystemField(system, masquerade string) any {
	if masquerade == routing.MasqueradeClaudeCode {
		blocks := []anthropicSystemBlock{{Type: "text", Text: claudeCodeSystemPrompt}}
		if system != "" {
			blocks = append(blocks, anthropicSystemBlock{Type: "text", Text: system})
		}
		return blocks
	}
	if system == "" {
		return nil
	}
	return system
}

// anthropicSystemFieldCached renders the system field as a block array with
// cache_control on the LAST block. For the masquerade the array already exists
// (first block = claudeCodeSystemPrompt, left untouched); for the plain path it
// wraps the joined string as one block. Returns nil when there is no system text
// and no masquerade (no block to mark — the caller still marks the last turn).
func anthropicSystemFieldCached(system, masquerade string, cc *anthropicCacheControl) any {
	field := anthropicSystemField(system, masquerade) // reuse the non-caching logic
	switch v := field.(type) {
	case []anthropicSystemBlock:
		if len(v) == 0 {
			return nil // never emit "system":[]
		}
		v[len(v)-1].CacheControl = cc
		return v
	case string:
		return []anthropicSystemBlock{{Type: "text", Text: v, CacheControl: cc}}
	default: // nil: empty system, no masquerade
		return field
	}
}

// anthropicMessages splits the neutral messages into Anthropic's top-level system
// string (system + developer text, blank-line separated) and its user/assistant
// turns. Consecutive same-role turns are merged into one, which is what the API
// would do itself and keeps a tool_result ahead of the user text that follows it
// in a single user turn. Turns left with no representable block are dropped
// (Anthropic rejects empty content).
//
// Assistant Reasoning is dropped: a thinking block must carry the upstream's
// signature, which the neutral model does not keep.
func anthropicMessages(messages []inference.Message) (string, []anthropicMessage) {
	var system []string
	out := make([]anthropicMessage, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case inference.RoleSystem, inference.RoleDeveloper:
			if text := msg.Text(); text != "" {
				system = append(system, text)
			}
		case inference.RoleTool:
			block := anthropicBlock{Type: "tool_result", ToolUseID: msg.ToolCallID, Content: msg.Text()}
			out = appendAnthropicTurn(out, "user", []anthropicBlock{block})
		case inference.RoleAssistant:
			out = appendAnthropicTurn(out, "assistant", anthropicAssistantBlocks(msg))
		default:
			out = appendAnthropicTurn(out, "user", anthropicUserBlocks(msg))
		}
	}
	return strings.Join(system, "\n\n"), out
}

func appendAnthropicTurn(turns []anthropicMessage, role string, blocks []anthropicBlock) []anthropicMessage {
	if len(blocks) == 0 {
		return turns
	}
	if n := len(turns); n > 0 && turns[n-1].Role == role {
		turns[n-1].Content = append(turns[n-1].Content, blocks...)
		return turns
	}
	return append(turns, anthropicMessage{Role: role, Content: blocks})
}

func anthropicUserBlocks(msg inference.Message) []anthropicBlock {
	blocks := make([]anthropicBlock, 0, len(msg.Content))
	for _, p := range msg.Content {
		switch p.Type {
		case inference.ContentText:
			if p.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
			}
		case inference.ContentImage:
			if src := anthropicImageSourceFor(p.ImageURL); src != nil {
				blocks = append(blocks, anthropicBlock{Type: "image", Source: src})
			}
		}
	}
	return blocks
}

// anthropicAssistantBlocks renders an assistant turn: its text, then one tool_use
// block per tool call. (Assistant turns cannot carry images in this API.)
func anthropicAssistantBlocks(msg inference.Message) []anthropicBlock {
	blocks := make([]anthropicBlock, 0, len(msg.Content)+len(msg.ToolCalls))
	for _, p := range msg.Content {
		if p.Type == inference.ContentText && p.Text != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: anthropicToolInput(tc.Arguments)})
	}
	return blocks
}

// anthropicToolInput turns a call's JSON-string arguments into the JSON OBJECT a
// tool_use block's input must be. Empty or non-object arguments (a model's
// malformed JSON replayed from history) become {}: Anthropic rejects anything
// but an object, and one bad past call must not make the whole turn unsendable.
func anthropicToolInput(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}

// anthropicImageSourceFor maps an image URL to an Anthropic image source: a
// base64 data: URI becomes an inline base64 source, an http(s) URL a url source.
// Anything else (a non-base64 data URI, file:, a relative path) yields nil and
// the image is dropped rather than failing the request.
func anthropicImageSourceFor(url string) *anthropicImageSource {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		// The media type is everything up to the FIRST ";" (a URI may carry extra
		// parameters, e.g. "image/png;charset=utf-8;base64", that Anthropic would
		// reject inside media_type); the trailing "base64" marker is matched
		// case-insensitively.
		mediaType, params, _ := strings.Cut(meta, ";")
		marker := params
		if i := strings.LastIndex(params, ";"); i >= 0 {
			marker = params[i+1:]
		}
		isBase64 := strings.EqualFold(strings.TrimSpace(marker), "base64")
		if !found || !isBase64 || mediaType == "" || data == "" {
			return nil
		}
		return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		return &anthropicImageSource{Type: "url", URL: url}
	}
	return nil
}

func anthropicTools(tools []inference.Tool) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: anthropicInputSchema(t.Parameters)})
	}
	return out
}

// anthropicInputSchema returns the tool's JSON schema, which Anthropic requires
// to be an object schema; a tool with no (or a type-less) schema gets the empty
// object one.
func anthropicInputSchema(parameters map[string]any) map[string]any {
	if len(parameters) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if _, ok := parameters["type"]; ok {
		return parameters
	}
	schema := make(map[string]any, len(parameters)+1)
	for k, v := range parameters {
		schema[k] = v
	}
	schema["type"] = "object"
	return schema
}

// anthropicToolChoiceFor maps the neutral tool_choice (the OpenAI form:
// "auto" / "required" / "none" / {"type":"function","function":{"name":...}}, or
// the Responses flat {"type":"function","name":...}) to
// Anthropic's {type: auto|any|none|tool, name?}. An already Anthropic-shaped
// object passes through. Anything unrecognised yields nil (the upstream default,
// which for Anthropic is auto).
func anthropicToolChoiceFor(choice any) map[string]any {
	switch c := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		kind, _ := c["type"].(string)
		switch kind {
		case "function":
			// Chat nests the name under "function"; the Responses API's forced-tool
			// form is FLAT ({"type":"function","name":"x"}), so fall back to it
			// rather than silently downgrading a forced tool to auto.
			fn, _ := c["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" {
				name, _ = c["name"].(string)
			}
			if name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		case "tool":
			if name, _ := c["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		case "auto", "any", "none":
			return map[string]any{"type": kind}
		}
	}
	return nil
}

// ---- response parse (Messages -> neutral) ----

// anthropicUsage is a Messages usage object. Anthropic reports the cache subsets
// SEPARATELY from input_tokens; canonical() folds them in (the OpenAI convention
// the neutral inference.Usage uses).
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) canonical() inference.Usage {
	input := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return inference.Usage{
		InputTokens:      input,
		OutputTokens:     u.OutputTokens,
		TotalTokens:      input + u.OutputTokens,
		CachedTokens:     u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// merge overlays the non-zero fields of o onto u. A stream reports usage in two
// places (message_start, then a cumulative message_delta) and a zero in the later
// one means "not restated", not "now zero".
func (u *anthropicUsage) merge(o anthropicUsage) {
	if o.InputTokens > 0 {
		u.InputTokens = o.InputTokens
	}
	if o.OutputTokens > 0 {
		u.OutputTokens = o.OutputTokens
	}
	if o.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = o.CacheReadInputTokens
	}
	if o.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = o.CacheCreationInputTokens
	}
}

// anthropicFinishReason maps a Messages stop_reason to the OpenAI finish_reason
// the neutral Response/StreamEvent carry. An empty stop_reason stays empty (the
// upstream reported none); an unrecognised one is a plain stop.
func anthropicFinishReason(stopReason string) string {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "":
		return ""
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn, anything newer
		return "stop"
	}
}

// parseAnthropicResponse decodes a non-stream Messages response. Text blocks are
// concatenated into Text, thinking blocks into Reasoning, tool_use blocks become
// ToolCalls; every other block type (redacted_thinking, server tool results, ...)
// is ignored.
func parseAnthropicResponse(raw []byte) (Response, error) {
	var decoded struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	// An empty content array WITH a stop_reason is a valid (if odd) reply and its
	// usage was billed; a body with neither is not a Messages response at all.
	if len(decoded.Content) == 0 && decoded.StopReason == "" {
		return Response{}, fmt.Errorf("%w: missing message content", ErrInvalidResponse)
	}
	var text, reasoning strings.Builder
	var toolCalls []inference.ToolCall
	for _, block := range decoded.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			toolCalls = append(toolCalls, inference.ToolCall{ID: block.ID, Name: block.Name, Arguments: compactToolInput(block.Input)})
		}
	}
	return Response{
		Text:         text.String(),
		Usage:        decoded.Usage.canonical(),
		ToolCalls:    toolCalls,
		Reasoning:    reasoning.String(),
		FinishReason: anthropicFinishReason(decoded.StopReason),
	}, nil
}

// compactToolInput renders a tool_use block's input object as the compact JSON
// string the neutral ToolCall carries; an absent or null input is the empty
// object.
func compactToolInput(input json.RawMessage) string {
	var buf bytes.Buffer
	if len(input) == 0 || json.Compact(&buf, input) != nil || buf.String() == "null" {
		return "{}"
	}
	return buf.String()
}

// ---- stream parse (SSE -> neutral events) ----

// anthropicStreamEvent is the union of the Messages SSE event payloads this
// client reads; each event populates only the fields its type defines.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type     string `json:"type"`
		ID       string `json:"id"`
		Name     string `json:"name"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicStreamTool accumulates one streamed tool_use block: id and name arrive
// in content_block_start, the arguments as input_json_delta fragments.
type anthropicStreamTool struct {
	id, name string
	args     strings.Builder
}

// anthropicStreamState is what one scan of a Messages SSE body accumulates: the
// merged usage, the stop reason, whether the stream reached its end, and the
// in-flight tool calls keyed by content block index.
type anthropicStreamState struct {
	usage      anthropicUsage
	sawUsage   bool
	stopReason string
	// stopped is set by message_stop, the stream's terminal event.
	stopped bool
	tools   map[int]*anthropicStreamTool
	order   []int
}

// apply decodes one SSE `data:` payload and applies it, emitting text and
// reasoning deltas as they arrive. Tool calls are only accumulated here and
// emitted whole by emitToolCalls, after the last delta. An undecodable payload or
// an event of an unknown type (ping, content_block_stop, signature_delta, ...) is
// skipped; an `error` event aborts with ErrUnavailable.
func (st *anthropicStreamState) apply(data string, emit StreamEmit) error {
	var ev anthropicStreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	switch ev.Type {
	case "message_start":
		st.mergeUsage(ev.Message.Usage)
	case "content_block_start":
		return st.startBlock(ev, emit)
	case "content_block_delta":
		return st.applyDelta(ev, emit)
	case "message_delta":
		if ev.Delta.StopReason != "" {
			st.stopReason = ev.Delta.StopReason
		}
		st.mergeUsage(ev.Usage)
	case "message_stop":
		st.stopped = true
	case "error":
		if ev.Error != nil {
			return fmt.Errorf("%w: upstream stream error: %s: %s", ErrUnavailable, ev.Error.Type, ev.Error.Message)
		}
		return fmt.Errorf("%w: upstream stream error", ErrUnavailable)
	}
	return nil
}

func (st *anthropicStreamState) mergeUsage(u anthropicUsage) {
	st.usage.merge(u)
	st.sawUsage = true
}

// startBlock handles content_block_start: a tool_use block opens an accumulator,
// and text a block already carries at its start is emitted like a delta.
func (st *anthropicStreamState) startBlock(ev anthropicStreamEvent, emit StreamEmit) error {
	switch ev.ContentBlock.Type {
	case "tool_use":
		if _, ok := st.tools[ev.Index]; !ok {
			st.order = append(st.order, ev.Index)
		}
		st.tools[ev.Index] = &anthropicStreamTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
	case "text":
		if ev.ContentBlock.Text != "" {
			return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: ev.ContentBlock.Text})
		}
	case "thinking":
		if ev.ContentBlock.Thinking != "" {
			return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: ev.ContentBlock.Thinking})
		}
	}
	return nil
}

// applyDelta handles content_block_delta. signature_delta (the thinking block's
// signature, which the neutral model does not keep) and unknown delta types are
// ignored.
func (st *anthropicStreamState) applyDelta(ev anthropicStreamEvent, emit StreamEmit) error {
	switch ev.Delta.Type {
	case "text_delta":
		if ev.Delta.Text != "" {
			return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: ev.Delta.Text})
		}
	case "thinking_delta":
		if ev.Delta.Thinking != "" {
			return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: ev.Delta.Thinking})
		}
	case "input_json_delta":
		if tool, ok := st.tools[ev.Index]; ok {
			// Bound the accumulation so a misbehaving upstream cannot grow memory
			// without limit; real tool arguments are far below this.
			if room := maxToolArgumentsBytes - tool.args.Len(); room > 0 {
				tool.args.WriteString(ev.Delta.PartialJSON[:min(room, len(ev.Delta.PartialJSON))])
			}
		}
	}
	return nil
}

// emitToolCalls emits each assembled tool call, in the order its content block
// opened, before the terminal Completed event. A call that streamed no argument
// fragments takes the empty object.
func (st *anthropicStreamState) emitToolCalls(emit StreamEmit) error {
	for _, idx := range st.order {
		tool := st.tools[idx]
		args := tool.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		call := &inference.ToolCall{ID: tool.id, Name: tool.name, Arguments: args}
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: call}); err != nil {
			return err
		}
	}
	return nil
}
