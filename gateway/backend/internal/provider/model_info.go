// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/routing"
	"path"
	"strings"
)

// ModelInfo is a single upstream model's discoverable info. ContextSize is the max
// context in tokens (0 = unknown / not reported).
type ModelInfo struct {
	Name        string
	ContextSize int
}

// ModelInfoProber GETs target.Endpoint+probePath and parses model info (currently
// the llama.cpp /props shape: the loaded model's name + n_ctx). A blank probePath
// returns (nil, nil): the feature is off for the app. Tolerant — an unparseable
// body yields nil, never an error.
type ModelInfoProber interface {
	ProbeModelInfo(ctx context.Context, target routing.Target, probePath string) ([]ModelInfo, error)
}

func fetchModelInfo(ctx context.Context, httpClient *http.Client, target routing.Target, probePath string) ([]ModelInfo, error) {
	probePath = strings.TrimSpace(probePath)
	if probePath == "" {
		return nil, nil
	}
	if !strings.HasPrefix(probePath, "/") {
		probePath = "/" + probePath
	}
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, probePath), nil)
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
	return parseModelInfo(body), nil
}

// parseModelInfo reads the llama.cpp /props shape: the loaded model's name (from
// "model", or basename of "model_path") + n_ctx (default_generation_settings.n_ctx,
// else top-level n_ctx). Returns one entry when a model name is present, else nil.
func parseModelInfo(body []byte) []ModelInfo {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil
	}
	name := ""
	if s, ok := obj["model"].(string); ok && strings.TrimSpace(s) != "" {
		name = strings.TrimSpace(s)
	} else if s, ok := obj["model_path"].(string); ok && strings.TrimSpace(s) != "" {
		name = path.Base(strings.TrimSpace(s))
	}
	if name == "" {
		return nil
	}
	nctx := 0
	if dgs, ok := obj["default_generation_settings"].(map[string]any); ok {
		nctx = intFromAny(dgs["n_ctx"])
	}
	if nctx == 0 {
		nctx = intFromAny(obj["n_ctx"])
	}
	return []ModelInfo{{Name: name, ContextSize: nctx}}
}

// ExpandModelPath substitutes the {model} placeholder in a probe path with the upstream model
// name, so a per-model endpoint (e.g. "/upstream/{model}/props") can be queried. Each "/"-split
// segment of the model name is URL-path-escaped (spaces/specials made safe) while "/" itself is
// kept literal, so a slash-bearing name like "openai/gpt-4o" stays a multi-segment path. Only the
// exact token "{model}" is replaced; any other "{...}" is left as-is. A path without "{model}" is
// returned unchanged.
func ExpandModelPath(path, model string) string {
	if !strings.Contains(path, "{model}") {
		return path
	}
	segs := strings.Split(model, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.ReplaceAll(path, "{model}", strings.Join(segs, "/"))
}

// PickModelContextSize returns the context size for a per-model probe of the given model: an info
// whose Name matches (case-sensitive) wins; otherwise the first info with a positive ContextSize
// (a per-model /props returns one model, so this is that model's value). Returns 0 when nothing
// usable is present.
func PickModelContextSize(infos []ModelInfo, model string) int {
	first := 0
	for _, info := range infos {
		if info.ContextSize <= 0 {
			continue
		}
		if info.Name == model {
			return info.ContextSize
		}
		if first == 0 {
			first = info.ContextSize
		}
	}
	return first
}

// intFromAny coerces a JSON number (float64) or int to a non-negative int; anything
// else (or <= 0) -> 0.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return 0
}
