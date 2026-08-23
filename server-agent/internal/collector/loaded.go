// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

// LoadedModelLister reports which model names are currently LOADED on an
// inference server, scraped from a configured status endpoint. Available()
// gates it: an empty status URL yields an unavailable lister.
type LoadedModelLister interface {
	Available() bool
	Collect(ctx context.Context) ([]string, error)
}

// loadedModelScraper GETs a status URL and parses loaded model names per format.
// The parsing mirrors the gateway's provider.parseLoadedModels (the agent module
// imports nothing from the gateway, so the tolerant parser is duplicated here).
type loadedModelScraper struct {
	url    string
	format string
	client *http.Client
}

// NewLoadedModelLister returns a lister that polls url. An empty url yields an
// unavailable lister (Available() == false). A nil client defaults to
// http.DefaultClient.
func NewLoadedModelLister(url, format string, client *http.Client) LoadedModelLister {
	if client == nil {
		client = http.DefaultClient
	}
	return &loadedModelScraper{url: strings.TrimSpace(url), format: strings.TrimSpace(format), client: client}
}

func (s *loadedModelScraper) Available() bool { return s.url != "" }

// Collect GETs the status endpoint and returns the parsed loaded-model names. A
// non-2xx status or a transport error is returned so the loop can log + skip; an
// unparseable body yields an empty slice (no models), not an error.
func (s *loadedModelScraper) Collect(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("model status: upstream status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseLoadedModels(body, s.format), nil
}

// parseLoadedModels extracts loaded model names from a status response per
// format. TOLERANT: an unparseable/empty body yields nil.
//   - "openai"     : {"data":[{"id":...}]}
//   - "llama_swap" : {"running":[...]} | {"models":[...]} | ["name", ...]
//   - "llama_cpp"  : {"model":...} | {"model_path":"/…/x.gguf"}
//   - "litellm"    : {"healthy_endpoints":[{"model":...}]}
//   - "" / "auto"  : union of all of the above
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
	default:
		all := append(openaiLoadedModels(v), llamaSwapLoadedModels(v)...)
		all = append(all, llamaCppLoadedModels(v)...)
		all = append(all, litellmLoadedModels(v)...)
		return dedupNonEmpty(all)
	}
}

func openaiLoadedModels(v any) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return modelNamesFromArray(obj["data"])
}

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

// litellmLoadedModels reads LiteLLM's GET /health shape —
// {"healthy_endpoints":[{"model":…}]} — returning the reachable deployment
// names. unhealthy_endpoints are treated as not available. Tolerant: a
// non-matching shape yields nil. Mirrors the gateway's provider.litellmLoadedModels.
func litellmLoadedModels(v any) []string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := obj["healthy_endpoints"].([]any)
	if !ok {
		return nil
	}
	return modelNamesFromArray(arr)
}

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
