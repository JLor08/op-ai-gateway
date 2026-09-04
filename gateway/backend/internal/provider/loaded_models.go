// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
	"path"
	"strings"
)

// LoadedModelLister is an optional provider capability: it reports which model(s)
// are currently LOADED/RUNNING on the upstream (as opposed to merely available),
// by GETting a configurable status endpoint and parsing the response. It is used
// to show, per application, which models can be requested without waiting for a
// swap/load. It is a read-only probe and MUST NOT trigger a load.
type LoadedModelLister interface {
	// LoadedModels GETs target.Endpoint+statusPath and parses it per format
	// (""/"auto" = tolerant multi-shape). Returned names are UPSTREAM (app) model
	// names. A blank statusPath returns (nil, nil): the feature is off for the app.
	LoadedModels(ctx context.Context, target routing.Target, statusPath, format string) ([]string, error)
}

// fetchLoadedModels is the shared GET+parse used by every client's LoadedModels.
func fetchLoadedModels(ctx context.Context, httpClient *http.Client, target routing.Target, statusPath, format string) ([]string, error) {
	statusPath = strings.TrimSpace(statusPath)
	if statusPath == "" {
		return nil, nil
	}
	if !strings.HasPrefix(statusPath, "/") {
		statusPath = "/" + statusPath
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, statusPath), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, unavailableStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	return parseLoadedModels(body, format), nil
}

// parseLoadedModels extracts loaded model names from a status response per format.
// It is deliberately TOLERANT: an unparseable/empty body yields nil (no loaded
// models) rather than an error, so a probe never breaks over an unexpected shape.
//   - "openai"     : {"data":[{"id":...}]}                     (vLLM / plain llama.cpp /v1/models — all served = loaded)
//   - "llama_swap" : {"running":[{"model":...}]} | {"models":[...]} | ["name", ...]
//   - "llama_cpp"  : {"model":...} | {"model_path":"/…/x.gguf"} (single loaded model)
//   - "litellm"    : {"healthy_endpoints":[{"model":...}]}     (LiteLLM /health — reachable deployments)
//   - "" / "auto"  : try all of the above and union the results
func parseLoadedModels(body []byte, format string) []string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai":
		return dedupNonEmpty(openaiLoadedModels(v))
	case "llama_swap":
		return dedupNonEmpty(llamaSwapLoadedModels(v))
	case "llama_cpp":
		return dedupNonEmpty(llamaCppLoadedModels(v))
	case "litellm":
		return dedupNonEmpty(litellmLoadedModels(v))
	default: // "" / "auto" / anything unknown -> tolerant union
		all := append(openaiLoadedModels(v), llamaSwapLoadedModels(v)...)
		all = append(all, llamaCppLoadedModels(v)...)
		all = append(all, litellmLoadedModels(v)...)
		return dedupNonEmpty(all)
	}
}

// openaiLoadedModels parses {"data":[{"id":...}]}.
func openaiLoadedModels(v any) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return modelNamesFromArray(obj["data"])
}

// llamaSwapLoadedModels parses llama-swap /running ({"running":[...]}), an
// {"models":[...]} object (e.g. ollama /api/ps), or a bare top-level array.
func llamaSwapLoadedModels(v any) []string {
	switch typed := v.(type) {
	case []any:
		return modelNamesFromArray(typed)
	case map[string]any:
		out := modelNamesFromArray(typed["running"])
		out = append(out, modelNamesFromArray(typed["models"])...)
		return out
	default:
		return nil
	}
}

// llamaCppLoadedModels parses llama.cpp /props: {"model":...} or {"model_path":...}.
func llamaCppLoadedModels(v any) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if s, ok := obj["model"].(string); ok && strings.TrimSpace(s) != "" {
		return []string{strings.TrimSpace(s)}
	}
	if s, ok := obj["model_path"].(string); ok && strings.TrimSpace(s) != "" {
		return []string{path.Base(strings.TrimSpace(s))}
	}
	return nil
}

// litellmLoadedModels reads LiteLLM's GET /health shape — {"healthy_endpoints":[{"model":…}]} —
// returning the reachable deployment names. unhealthy_endpoints are treated as not available.
// Tolerant: a non-matching shape yields nil.
func litellmLoadedModels(v any) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := obj["healthy_endpoints"].([]any)
	if !ok {
		return nil
	}
	return modelNamesFromArray(arr) // reuses the existing model/id/name extractor
}

// modelNamesFromArray pulls model names from an array whose items are either
// strings or objects carrying a model/id/name string field.
func modelNamesFromArray(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		switch it := item.(type) {
		case string:
			if strings.TrimSpace(it) != "" {
				out = append(out, strings.TrimSpace(it))
			}
		case map[string]any:
			for _, key := range []string{"model", "id", "name"} {
				if s, ok := it[key].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
					break
				}
			}
		}
	}
	return out
}

func dedupNonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
