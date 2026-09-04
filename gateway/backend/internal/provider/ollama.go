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

type OllamaClient struct {
	http *http.Client
}

func NewOllamaClient(httpClient *http.Client) *OllamaClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &OllamaClient{http: httpClient}
}

func (c *OllamaClient) Complete(ctx context.Context, target routing.Target, req inference.Request) (Response, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	reqBody := map[string]any{
		"model":    providerModel(target, req),
		"stream":   false,
		"messages": ollamaMessages(req.Messages),
	}
	ollamaOptions(reqBody, req)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, "/api/chat"), bytes.NewReader(body))
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
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount    int   `json:"prompt_eval_count"`
		PromptEvalDuration int64 `json:"prompt_eval_duration"`
		EvalCount          int   `json:"eval_count"`
		EvalDuration       int64 `json:"eval_duration"`
	}
	if err := json.Unmarshal(respBytes, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	if decoded.Message.Content == "" {
		return Response{}, fmt.Errorf("%w: missing message content", ErrInvalidResponse)
	}
	return Response{
		Text: decoded.Message.Content,
		Usage: inference.Usage{
			InputTokens:     decoded.PromptEvalCount,
			OutputTokens:    decoded.EvalCount,
			TotalTokens:     decoded.PromptEvalCount + decoded.EvalCount,
			PromptPerSecond: tokensPerSecond(decoded.PromptEvalCount, decoded.PromptEvalDuration),
			TokensPerSecond: tokensPerSecond(decoded.EvalCount, decoded.EvalDuration),
		},
	}, nil
}

// tokensPerSecond derives a token rate from a count and a duration in
// nanoseconds. Returns 0 if the duration is not positive.
func tokensPerSecond(count int, durationNs int64) float64 {
	if durationNs <= 0 {
		return 0
	}
	return float64(count) / (float64(durationNs) / 1e9)
}

// ollamaMessages builds Ollama chat messages: content is always the text, and
// image parts are added as a base64 images array (data-URL prefix stripped).
func ollamaMessages(messages []inference.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		m := map[string]any{"role": string(msg.Role), "content": msg.Text()}
		var images []string
		for _, p := range msg.Content {
			if p.Type == inference.ContentImage {
				images = append(images, stripDataURLPrefix(p.ImageURL))
			}
		}
		if len(images) > 0 {
			m["images"] = images
		}
		out = append(out, m)
	}
	return out
}

// stripDataURLPrefix returns the base64 payload of a data:<mime>;base64,<payload>
// URL. A non-data or non-base64 URL is returned verbatim; since Ollama's images
// array expects raw base64, such a URL will be rejected by the upstream.
func stripDataURLPrefix(url string) string {
	if i := strings.Index(url, ";base64,"); i >= 0 {
		return url[i+len(";base64,"):]
	}
	return url
}

// ollamaOptions adds temperature/num_predict (max tokens) to the body when set.
func ollamaOptions(body map[string]any, req inference.Request) {
	options := map[string]any{}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		options["num_predict"] = req.MaxTokens
	}
	if len(options) > 0 {
		body["options"] = options
	}
}

var _ StreamingClient = (*OllamaClient)(nil)

func (c *OllamaClient) CompleteStream(ctx context.Context, target routing.Target, req inference.Request, emit StreamEmit) error {
	body := map[string]any{
		"model":    providerModel(target, req),
		"stream":   true,
		"messages": ollamaMessages(req.Messages),
	}
	ollamaOptions(body, req)
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL(target.Endpoint, "/api/chat"), bytes.NewReader(raw))
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
		return unavailableStatus(httpResp.StatusCode)
	}
	sink.RecordResponseHeaders(httpResp.Header)

	var usage *inference.Usage
	var streamReader io.Reader = httpResp.Body
	if rw := sink.ResponseWriter(); rw != nil {
		streamReader = io.TeeReader(httpResp.Body, rw)
	}
	scanner := bufio.NewScanner(streamReader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done               bool   `json:"done"`
			PromptEvalCount    int    `json:"prompt_eval_count"`
			PromptEvalDuration int64  `json:"prompt_eval_duration"`
			EvalCount          int    `json:"eval_count"`
			EvalDuration       int64  `json:"eval_duration"`
			Error              string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return fmt.Errorf("%w: upstream stream error: %s", ErrUnavailable, chunk.Error)
		}
		if chunk.Message.Content != "" {
			if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: chunk.Message.Content}); err != nil {
				return err
			}
		}
		if chunk.Done {
			usage = &inference.Usage{
				InputTokens:     chunk.PromptEvalCount,
				OutputTokens:    chunk.EvalCount,
				TotalTokens:     chunk.PromptEvalCount + chunk.EvalCount,
				PromptPerSecond: tokensPerSecond(chunk.PromptEvalCount, chunk.PromptEvalDuration),
				TokensPerSecond: tokensPerSecond(chunk.EvalCount, chunk.EvalDuration),
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: read stream: %v", ErrUnavailable, err)
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: usage})
}

var _ Prober = (*OllamaClient)(nil)

// Probe issues a GET against the application's health path; a 2xx is reachable.
func (c *OllamaClient) Probe(ctx context.Context, target routing.Target, path string) error {
	return httpProbe(ctx, c.http, target, path)
}

var _ LoadedModelLister = (*OllamaClient)(nil)

// LoadedModels GETs the configured status endpoint (e.g. Ollama /api/ps) and parses
// the running model names (the tolerant parser handles the {"models":[...]} shape).
func (c *OllamaClient) LoadedModels(ctx context.Context, target routing.Target, statusPath, format string) ([]string, error) {
	return fetchLoadedModels(ctx, c.http, target, statusPath, format)
}

var _ ModelUnloader = (*OllamaClient)(nil)

// UnloadModel implements ModelUnloader via ollama's keep_alive:0 (unloads the model).
func (c *OllamaClient) UnloadModel(ctx context.Context, target routing.Target, model string) (bool, error) {
	if strings.TrimSpace(model) == "" {
		return false, nil
	}
	body, err := json.Marshal(map[string]any{"model": model, "keep_alive": 0})
	if err != nil {
		return false, err
	}
	// Bound the call by target.Timeout (mirroring fetchLoadedModels) so a wedged upstream
	// cannot hang the caller indefinitely — the default http client has no timeout.
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	base := strings.TrimRight(target.Endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set(contentTypeHeader, jsonContentType)
	applyUpstreamAuth(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

var _ ModelInfoProber = (*OllamaClient)(nil)

// ProbeModelInfo GETs the configured probe path (llama.cpp /props) and parses the
// loaded model's name + context size.
func (c *OllamaClient) ProbeModelInfo(ctx context.Context, target routing.Target, probePath string) ([]ModelInfo, error) {
	return fetchModelInfo(ctx, c.http, target, probePath)
}

var _ MemoryProber = (*OllamaClient)(nil)

// ProbeServerMemory GETs the configured capacity-probe path and parses the upstream
// saturation signal (llama.cpp /metrics Prometheus or /props|/slots JSON).
func (c *OllamaClient) ProbeServerMemory(ctx context.Context, target routing.Target, probePath, format string) (ServerMemory, error) {
	return fetchServerMemory(ctx, c.http, target, probePath, format)
}

func (c *OllamaClient) ListModels(ctx context.Context, target routing.Target) ([]string, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, "/api/tags"), nil)
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
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	models := make([]string, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}
	return models, nil
}
