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

// LoadedModelInfoLister is LoadedModelLister plus what the SAME status document,
// read in the SAME request, says about each loaded model: its context window
// and a vision verdict. Only the "openai" format carries either today (see
// parseLoadedModelInfos); every other format yields the names alone. The
// health loop prefers it over LoadedModelLister so an upstream whose only
// readable route is its /v1/models listing (a Strata LAN share, which answers
// /props with 404) still reports its context size and vision support without a
// second probe.
type LoadedModelInfoLister interface {
	// LoadedModelInfos is LoadedModels with details: one entry per loaded
	// name, in LoadedModels' order. Only Name, ContextSize and Caps.Vision
	// are ever set.
	LoadedModelInfos(ctx context.Context, target routing.Target, statusPath, format string) ([]ModelInfo, error)
}

// fetchLoadedModels is the shared GET+parse used by every client's LoadedModels.
func fetchLoadedModels(ctx context.Context, httpClient *http.Client, target routing.Target, statusPath, format string) ([]string, error) {
	body, err := fetchLoadedModelsBody(ctx, httpClient, target, statusPath)
	if err != nil || body == nil {
		return nil, err
	}
	return parseLoadedModels(body, format), nil
}

// fetchLoadedModelInfos is the shared GET+parse used by LoadedModelInfos.
func fetchLoadedModelInfos(ctx context.Context, httpClient *http.Client, target routing.Target, statusPath, format string) ([]ModelInfo, error) {
	body, err := fetchLoadedModelsBody(ctx, httpClient, target, statusPath)
	if err != nil || body == nil {
		return nil, err
	}
	return parseLoadedModelInfos(body, format), nil
}

// fetchLoadedModelsBody GETs the status path (with the upstream credential)
// and returns the raw body. A blank statusPath returns (nil, nil).
func fetchLoadedModelsBody(ctx context.Context, httpClient *http.Client, target routing.Target, statusPath string) ([]byte, error) {
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
	return body, nil
}

// parseLoadedModels extracts loaded model names from a status response per format.
// It is deliberately TOLERANT: an unparseable/empty body yields nil (no loaded
// models) rather than an error, so a probe never breaks over an unexpected shape.
//   - "openai"     : {"data":[{"id":...}]}                     (vLLM / plain llama.cpp /v1/models — all served = loaded)
//   - "llama_swap" : {"running":[{"model":...}]} | {"models":[...]} | ["name", ...]
//   - "llama_cpp"  : {"model":...} | {"model_path":"/…/x.gguf"} (single loaded model)
//   - "litellm"    : {"healthy_endpoints":[{"model":...}]}     (LiteLLM /health — reachable deployments)
//   - "sdcpp_models": [{"model_name":...,"filename":...}]      (stable-diffusion.cpp /sdapi/v1/sd-models)
//   - "" / "auto"  : try the four shapes above and union the results; sdcpp_models is not in
//     the union because a bare array already means llama-swap
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
	case "sdcpp_models":
		return dedupNonEmpty(sdcppLoadedModels(v))
	default: // "" / "auto" / anything unknown -> tolerant union
		all := append(openaiLoadedModels(v), llamaSwapLoadedModels(v)...)
		all = append(all, llamaCppLoadedModels(v)...)
		all = append(all, litellmLoadedModels(v)...)
		return dedupNonEmpty(all)
	}
}

// parseLoadedModelInfos is parseLoadedModels plus what the same listing says
// about each name. The names, and their order, are exactly parseLoadedModels'.
// Only the "openai" format is read beyond the names, from the data[] entry the
// name came from:
//
//   - ContextSize: context_length (Strata's /v1/models, the OpenRouter-style
//     field), else meta.n_ctx (the engine block Strata adds). vLLM's
//     max_model_len is deliberately not read. A non-positive value is 0
//     (unknown).
//   - Caps.Vision: "yes" when architecture.input_modalities lists "image";
//     otherwise "" (no verdict), because nothing pins that list as
//     exhaustive.
//
// Every other format, and an entry without those keys, yields the name alone.
// The parse is generic; which application types RECORD these values is the
// health loop's decision (recordListedModelInfos in cmd/gateway).
func parseLoadedModelInfos(body []byte, format string) []ModelInfo {
	names := parseLoadedModels(body, format)
	if len(names) == 0 {
		return nil
	}
	entries := map[string]ModelInfo{}
	if strings.ToLower(strings.TrimSpace(format)) == "openai" {
		var v any
		if err := json.Unmarshal(body, &v); err == nil {
			entries = openaiEntryInfos(v)
		}
	}
	out := make([]ModelInfo, 0, len(names))
	for _, name := range names {
		info := entries[name]
		info.Name = name
		out = append(out, info)
	}
	return out
}

// openaiEntryInfos maps each {"data":[...]} entry's name (the same key
// modelNamesFromArray reads) to its context size and vision verdict, as
// parseLoadedModelInfos documents. The first entry for a repeated name wins.
func openaiEntryInfos(v any) map[string]ModelInfo {
	out := map[string]ModelInfo{}
	obj, ok := v.(map[string]any)
	if !ok {
		return out
	}
	arr, ok := obj["data"].([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		it, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := modelNameOf(it)
		if name == "" {
			continue
		}
		if _, seen := out[name]; seen {
			continue
		}
		out[name] = openaiEntryInfo(it)
	}
	return out
}

// openaiEntryInfo reads one entry's context size and vision verdict.
func openaiEntryInfo(it map[string]any) ModelInfo {
	info := ModelInfo{ContextSize: intFromAny(it["context_length"])}
	if info.ContextSize == 0 {
		if meta, ok := it["meta"].(map[string]any); ok {
			info.ContextSize = intFromAny(meta["n_ctx"])
		}
	}
	if arch, ok := it["architecture"].(map[string]any); ok {
		if mods, ok := arch["input_modalities"].([]any); ok && listsImageModality(mods) {
			info.Caps.Vision = routing.CapabilityYes
		}
	}
	return info
}

// listsImageModality reports whether a modality list names "image".
func listsImageModality(mods []any) bool {
	for _, m := range mods {
		if s, ok := m.(string); ok && strings.EqualFold(strings.TrimSpace(s), "image") {
			return true
		}
	}
	return false
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

// sdcppLoadedModels extracts the model names from stable-diffusion.cpp's
// /sdapi/v1/sd-models: [{"model_name":"flux1-dev","filename":"flux1-dev.safetensors"}].
//
// model_name, not filename: it is the name the operator recognises, and it is
// what becomes the mapping's app_model_name. Carrying it is safe even though
// the server ignores the request's `model` field entirely (measured: a real
// name, a bogus name and an empty string all return byte-identical
// responses) -- which is precisely what frees the gateway to carry a
// meaningful name instead of the static `sd-cpp-local` that /v1/models
// reports.
//
// It is the ONE definition of an sd model's name: the loaded probe
// (parseLoadedModels) and model discovery (decodeSdcppModelList) both call
// it, because a mapping reads as loaded only when the probe reports the very
// name discovery created it under.
func sdcppLoadedModels(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, entry := range arr {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := obj["model_name"].(string); ok && strings.TrimSpace(name) != "" {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
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
			if name := modelNameOf(it); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// modelNameOf is an array item's model name: its first non-blank string among
// "model", "id" and "name", trimmed; "" when it has none.
func modelNameOf(it map[string]any) string {
	for _, key := range []string{"model", "id", "name"} {
		if s, ok := it[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
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
