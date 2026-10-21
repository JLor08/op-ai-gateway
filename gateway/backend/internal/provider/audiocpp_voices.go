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
	"strings"
)

// audiocppVoicesPath is audio.cpp's voice listing. It is fixed by the server, not
// configured per application.
const audiocppVoicesPath = "/v1/audio/voices"

// Voice is one voice an audio.cpp TTS model offers. audio.cpp lists a voice by
// NAME only -- a flat array of strings, with no id, language or gender beside it
// (live-verified against v0.9.1) -- and the name is also the exact value the
// speech request's "voice" field takes: it names a cached voice-reference
// directory on the server, so the match is exact, not fuzzy.
type Voice struct {
	Name string
}

// AudioCppVoicesLister is an optional provider capability: it GETs
// target.Endpoint + /v1/audio/voices and returns the voice names that endpoint
// lists. A transport failure or a non-2xx status is an error and carries no
// voices, so the caller stores nothing. A 2xx body that lists no voices, or
// cannot be read, is NOT an error, only an empty (non-nil) slice.
//
// audio.cpp answers for the model it currently has LOADED and ignores a ?model=
// query parameter (live-verified against v0.9.1: the same result for no
// parameter, a valid id and a bogus one). So the request never sends one, and
// the model argument states which model the caller means, it does not select
// one: the caller must make sure that model is the one loaded on target before
// it attributes the result to it.
//
// It is a client method, like ProbeAudioCppModels, and not a free function,
// because the request must ride the client's own *http.Client: cmd/gateway
// builds that one on the outbound app transport, which carries the mesh routing
// and TLS settings a separate client would bypass.
type AudioCppVoicesLister interface {
	ListVoices(ctx context.Context, target routing.Target, model string) ([]Voice, error)
}

var _ AudioCppVoicesLister = (*OpenAICompatibleClient)(nil)

// ListVoices reads audio.cpp's voice listing through this client's transport.
// See AudioCppVoicesLister.
func (c *OpenAICompatibleClient) ListVoices(ctx context.Context, target routing.Target, model string) ([]Voice, error) {
	return fetchAudioCppVoices(ctx, c.http, target, model)
}

// fetchAudioCppVoices is the GET behind ListVoices. Modelled on
// fetchAudioCppModels: it honours target.Timeout, attaches the per-application
// upstream credential, and bounds the body at 1 MiB. A transport failure or a
// non-2xx status is an ErrUnavailable; decoding is the tolerant
// parseAudioCppVoices. model is intentionally not put on the wire: see
// AudioCppVoicesLister.
func fetchAudioCppVoices(ctx context.Context, httpClient *http.Client, target routing.Target, _ string) ([]Voice, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, audiocppVoicesPath), nil)
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
	return parseAudioCppVoices(body), nil
}

// audioCppVoicesDoc is the whole of the listing: one array of voice names.
type audioCppVoicesDoc struct {
	Voices []string `json:"voices"`
}

// parseAudioCppVoices decodes a /v1/audio/voices body, {"voices":["a","b"]}, into
// one Voice per name, in the server's order. It is TOLERANT, like
// parseAudioCppModels: it never fails and never returns nil. An empty or absent
// or null voices array, a body that is not JSON, and a voices field (or an entry
// in it) of the wrong type all yield an empty slice, so a probe never breaks over
// an unexpected shape. A blank name is dropped: it names no voice directory, and
// an empty voice on a speech request means "no voice chosen". Other names are
// kept verbatim -- the match against the server is exact.
func parseAudioCppVoices(body []byte) []Voice {
	voices := []Voice{}
	var doc audioCppVoicesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return voices
	}
	for _, name := range doc.Voices {
		if strings.TrimSpace(name) == "" {
			continue
		}
		voices = append(voices, Voice{Name: name})
	}
	return voices
}
