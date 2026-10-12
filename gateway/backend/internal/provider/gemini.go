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
	"net/url"
	"op-ai-gateway/internal/geminiwire"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
)

const (
	// geminiAPIVersion is the path prefix of the Gemini Developer API surface the
	// client calls (generateContent / streamGenerateContent live under /v1beta).
	geminiAPIVersion = "/v1beta"
	// geminiErrorBodyLimit bounds the upstream body read for a non-2xx status so a
	// 400 INVALID_ARGUMENT diagnostic reaches the caller without an unbounded read.
	geminiErrorBodyLimit = 4 << 10
	// geminiStreamLineMaxLen bounds one SSE line: a single chunk can carry a whole
	// functionCall with large arguments.
	geminiStreamLineMaxLen = 1 << 20
)

// GeminiClient speaks Google's native generateContent wire. It translates the
// neutral request/response via internal/geminiwire and owns HTTP, auth
// (x-goog-api-key, applied by applyUpstreamAuth from the ctx), redirect policy,
// timeout, capture and SSE scanning. Like AnthropicClient it deliberately
// implements no ModelLister/Prober (vendor model discovery lives in the portal).
//
// Redirects are never followed on either path (translate or native passthrough):
// net/http strips only Authorization, not a custom credential header such as
// x-goog-api-key, from a request that follows a redirect off the host, so
// following one could carry the key elsewhere. A 3xx is handed back as the
// upstream's answer.
type GeminiClient struct {
	noRedirect *http.Client
}

func NewGeminiClient(httpClient *http.Client) *GeminiClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &GeminiClient{noRedirect: withoutRedirects(httpClient)}
}

var (
	_ Client            = (*GeminiClient)(nil)
	_ StreamingClient   = (*GeminiClient)(nil)
	_ NativeProxyClient = (*GeminiClient)(nil)
)

// geminiModelPath builds the request path. Unlike every other dialect the model
// rides in the URL, not the body, so it is stripped of a leading "models/" (the
// listing form of the name) and path-escaped.
func geminiModelPath(model, action string) string {
	model = strings.TrimPrefix(model, "models/")
	return geminiAPIVersion + "/models/" + url.PathEscape(model) + ":" + action
}

func (c *GeminiClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	body, err := c.renderBody(target, req)
	if err != nil {
		return Response{}, err
	}
	path := geminiModelPath(providerModel(target, req), "generateContent")
	httpResp, err := c.post(ctx, target, path, body)
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
	res, err := geminiwire.ParseResponse(respBytes)
	if err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	if res.Text == "" && res.Reasoning == "" && len(res.ToolCalls) == 0 && res.FinishReason == "" {
		return Response{}, fmt.Errorf("%w: missing response content", ErrInvalidResponse)
	}
	return geminiResponse(res), nil
}

// CompleteStream streams a streamGenerateContent?alt=sse response, translating
// the upstream's SSE into this codebase's StreamEvents. Like the other clients it
// applies NO target timeout: a stream may legitimately run long, and the
// gateway's idle watchdog governs a stalled one. Gemini's SSE carries only
// `data:` frames (no [DONE] sentinel), each a whole generateContent response
// chunk; the stream is complete once a chunk reports a finishReason.
func (c *GeminiClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	body, err := c.renderBody(target, req)
	if err != nil {
		return err
	}
	path := geminiModelPath(providerModel(target, req), "streamGenerateContent") + "?alt=sse"
	httpResp, err := c.post(ctx, target, path, body)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	// Tee the raw upstream SSE into the capture sink (bounded) as the scanner reads
	// it. When not capturing, ResponseWriter() is nil and the body is read directly.
	var reader io.Reader = httpResp.Body
	if rw := CaptureSinkFrom(ctx).ResponseWriter(); rw != nil {
		reader = io.TeeReader(httpResp.Body, rw)
	}

	st := &geminiStreamState{}
	if err := c.scanStream(ctx, reader, st, emit); err != nil {
		return err
	}
	// A stream that ends without a finishReason chunk was cut off (a dropped
	// connection, a proxy closing early). Reporting it as Completed would hand the
	// client a partial answer with partial usage as if it were whole.
	if !st.done {
		return fmt.Errorf("%w: stream ended before a finish reason", ErrUnavailable)
	}
	// geminiwire.ParseStreamChunk maps finishReason per chunk: a functionCall can
	// arrive on an earlier chunk while the terminal chunk reports STOP, which maps
	// to "stop". Upgrade a final "stop" to "tool_calls" when any tool call was
	// buffered, so the completion reflects the whole stream.
	if len(st.toolCalls) > 0 && st.finish == "stop" {
		st.finish = "tool_calls"
	}
	for i := range st.toolCalls {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventToolCall, ToolCall: &st.toolCalls[i]}); err != nil {
			return err
		}
	}
	var usagePtr *inference.Usage
	if st.sawUsage {
		u := st.usage
		usagePtr = &u
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: usagePtr, FinishReason: st.finish})
}

// geminiStreamState accumulates one streamGenerateContent response across its SSE
// chunks: buffered tool calls, the last non-zero usage and the finish reason of
// the chunk that ended the stream.
type geminiStreamState struct {
	toolCalls []inference.ToolCall
	usage     inference.Usage
	sawUsage  bool
	finish    string
	done      bool
}

// scanStream reads the upstream SSE line by line, applying each `data:` frame to
// st until the stream ends. Comment lines (keepalives) are reported to the
// activity hook by streamLineData. A scanner error is mapped to the canonical
// provider error (ErrTimeout when ctx's deadline elapsed, ErrUnavailable
// otherwise).
func (c *GeminiClient) scanStream(ctx context.Context, r io.Reader, st *geminiStreamState, emit StreamEmit) error {
	activity := StreamActivityFrom(ctx)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), geminiStreamLineMaxLen)
	for scanner.Scan() {
		data, ok := streamLineData(strings.TrimSpace(scanner.Text()), activity)
		if !ok {
			continue
		}
		if err := st.apply(data, emit); err != nil {
			return err
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

// apply folds one SSE data frame into st, emitting its text and reasoning deltas
// as they arrive. An in-stream error frame ends the stream as ErrUnavailable with
// a bounded snippet of the frame; a frame that is not valid JSON is tolerated and
// skipped.
func (st *geminiStreamState) apply(data string, emit StreamEmit) error {
	if geminiStreamIsError([]byte(data)) {
		return fmt.Errorf("%w: upstream stream error: %s", ErrUnavailable, readUpstreamErrorSnippet(strings.NewReader(data), geminiErrorBodyLimit))
	}
	res, chunkDone, err := geminiwire.ParseStreamChunk([]byte(data))
	if err != nil {
		return nil // tolerate a non-JSON frame
	}
	if err := emitGeminiDeltas(res, emit); err != nil {
		return err
	}
	st.toolCalls = append(st.toolCalls, res.ToolCalls...)
	// usageMetadata is cumulative per chunk, so the last non-zero one is the
	// answer's usage; a chunk without it must not reset an earlier one.
	if res.Usage != (inference.Usage{}) {
		st.usage, st.sawUsage = res.Usage, true
	}
	if chunkDone {
		st.finish, st.done = res.FinishReason, true
	}
	return nil
}

// emitGeminiDeltas emits the chunk's text and reasoning as separate text-delta
// events, skipping whichever is empty.
func emitGeminiDeltas(res geminiwire.Result, emit StreamEmit) error {
	if res.Text != "" {
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: res.Text}); err != nil {
			return err
		}
	}
	if res.Reasoning != "" {
		return emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Reasoning: res.Reasoning})
	}
	return nil
}

// ProxyNative forwards the raw inbound generateContent body to the upstream's own
// endpoint path and returns the upstream response, Body still open, for the
// gateway to relay byte-for-byte. The credential (x-goog-api-key) is the ctx's,
// applied by applyUpstreamAuth. It uses the no-redirect client; see GeminiClient.
func (c *GeminiClient) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte) (*ProxyResponse, error) {
	return doNativeProxy(ctx, c.noRedirect, target, path, body)
}

// renderBody renders the neutral request as a generateContent body. The model is
// passed to geminiwire only for its model-dependent generationConfig (thinking);
// the request body itself carries no model field, the URL does.
func (c *GeminiClient) renderBody(target routing.Target, req inference.Request) ([]byte, error) {
	model := strings.TrimPrefix(providerModel(target, req), "models/")
	body, err := json.Marshal(geminiwire.RenderRequest(req, model))
	if err != nil {
		return nil, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	return body, nil
}

// post sends the rendered body to path and returns the 2xx response (the caller
// owns closing its Body). Every failure is already mapped to a provider error:
// ErrTimeout when ctx's deadline elapsed, ErrUnavailable for a transport error,
// and upstreamStatusError (unavailableStatus plus a bounded body snippet, so a 400
// INVALID_ARGUMENT explains itself) for a non-2xx status. It records the outbound
// request and, on success, the response headers with the context's capture sink.
// It sends x-goog-api-key via applyUpstreamAuth and never the ?key= query
// parameter, which would put the credential in URLs and logs.
func (c *GeminiClient) post(ctx context.Context, target routing.Target, path string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	httpReq.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, httpReq)
	sink := CaptureSinkFrom(ctx)
	sink.RecordRequest(httpReq.Header, body)
	httpResp, err := c.noRedirect.Do(httpReq)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		snippet := readUpstreamErrorSnippet(httpResp.Body, geminiErrorBodyLimit)
		httpResp.Body.Close()
		sink.WriteResponse([]byte(snippet))
		return nil, upstreamStatusError(httpResp.StatusCode, snippet)
	}
	sink.RecordResponseHeaders(httpResp.Header)
	return httpResp, nil
}

func geminiResponse(r geminiwire.Result) Response {
	return Response{
		Text:         r.Text,
		Reasoning:    r.Reasoning,
		ToolCalls:    r.ToolCalls,
		Usage:        r.Usage,
		FinishReason: r.FinishReason,
	}
}

// geminiStreamIsError reports whether an SSE data frame is an upstream error
// envelope ({"error":{...}}) rather than a response chunk.
func geminiStreamIsError(data []byte) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return len(probe.Error) > 0
}
