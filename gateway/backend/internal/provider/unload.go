// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"op-ai-gateway/internal/routing"
	"strings"
)

// ModelUnloader best-effort unloads a model from an upstream so the next request reloads it
// cold (the benchmark uses it to measure a genuine load time). Tolerant: an upstream with no
// unload mechanism returns (false, nil) — "not supported here", never an error. The call
// carries NO client bearer token (bare, like the loaded-probe).
type ModelUnloader interface {
	// UnloadModel returns (unloaded, err). unloaded=true means the upstream accepted the
	// unload; false+nil means unsupported/refused. model is the UPSTREAM (app) model name.
	UnloadModel(ctx context.Context, target routing.Target, model string) (bool, error)
}

// postUnloadLlamaSwap issues llama-swap's POST {endpoint}/api/models/unload/{model}. 2xx =>
// unloaded (true); any other status => not a llama-swap unload endpoint (false, nil). A
// transport error is returned so the caller can decide (the benchmark swallows it). No auth
// header attached; the response body is discarded (bounded).
func postUnloadLlamaSwap(ctx context.Context, httpClient *http.Client, target routing.Target, model string) (bool, error) {
	// Bound the call by target.Timeout (mirroring fetchLoadedModels) so a wedged upstream
	// cannot hang the caller indefinitely — the default http client has no timeout.
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	base := strings.TrimRight(target.Endpoint, "/")
	reqURL := base + "/api/models/unload/" + url.PathEscape(model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return false, err
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}
