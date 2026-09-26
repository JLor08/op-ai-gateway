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

// sdcppCapabilitiesPath is stable-diffusion.cpp's own capability document.
// It is fixed by the server, not configured per application: there is no
// sd-server that serves it anywhere else.
const sdcppCapabilitiesPath = "/sdcpp/v1/capabilities"

// sdcppModeImageGeneration is the supported_modes entry that means the loaded
// model generates images.
const sdcppModeImageGeneration = "img_gen"

// SdcppVerdicts is what one stable-diffusion.cpp capability document says.
//
// Image is routing.CapabilityYes, routing.CapabilityNo, or "" for "no
// answer". It is BIDIRECTIONAL because supported_modes is exhaustive -- the
// server lists every mode it serves -- so a list without "img_gen" is a real
// "no", unlike Ollama's capability array, which is not exhaustive and so can
// only ever yield a "yes" or nothing. "" is reserved for a document that
// carries no supported_modes list at all: an absent list is not an
// exhaustive one. See routing.CapabilitySourceSdcppCapabilities.
//
// ModelStem is the document's model.stem, the loaded model's identity
// ("flux1-dev"), or "" when the document names none. It is what attributes
// the verdict to a mapping: on the measured server it equals the model_name
// /sdapi/v1/sd-models reports, which is what model discovery made the
// mapping's AppModelName (sdcppLoadedModels).
type SdcppVerdicts struct {
	Image     string
	ModelStem string
}

// SdcppCapabilitiesProber is an optional provider capability: it GETs
// target.Endpoint + /sdcpp/v1/capabilities and derives the verdicts that
// document states. A transport failure, a non-2xx status or a body that is not
// the document's JSON is an error and carries no verdict, so the caller writes
// nothing. A JSON document without supported_modes is NOT an error, only an
// empty Image.
//
// It is a client method, like Probe, LoadedModels and ProbeModelInfo, and not
// a free function, because the request must ride the client's own
// *http.Client: cmd/gateway builds that one on the outbound app transport,
// which carries the mesh routing and TLS settings a separate client would
// bypass.
type SdcppCapabilitiesProber interface {
	ProbeSdcppCapabilities(ctx context.Context, target routing.Target) (SdcppVerdicts, error)
}

var _ SdcppCapabilitiesProber = (*OpenAICompatibleClient)(nil)

// ProbeSdcppCapabilities reads stable-diffusion.cpp's capability document
// through this client's transport. See SdcppCapabilitiesProber.
func (c *OpenAICompatibleClient) ProbeSdcppCapabilities(ctx context.Context, target routing.Target) (SdcppVerdicts, error) {
	return fetchSdcppCapabilities(ctx, c.http, target)
}

// fetchSdcppCapabilities is the GET behind ProbeSdcppCapabilities, modelled on
// fetchLoadedModels: it honours target.Timeout, attaches the per-application
// upstream credential, and bounds the body at 1 MiB.
func fetchSdcppCapabilities(ctx context.Context, httpClient *http.Client, target routing.Target) (SdcppVerdicts, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, sdcppCapabilitiesPath), nil)
	if err != nil {
		return SdcppVerdicts{}, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return SdcppVerdicts{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SdcppVerdicts{}, unavailableStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return SdcppVerdicts{}, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	return parseSdcppCapabilities(body)
}

// sdcppCapabilitiesDoc is the part of the capability document this code reads.
// Only supported_modes and model.stem answer a question the gateway has; the
// document's limits, samplers, features, output_formats and the rest are
// deliberately not decoded.
//
// SupportedModes is a POINTER so that an absent (or null) list stays
// distinguishable from an empty one: absent is no answer, while an empty list
// is still exhaustive and so says the server serves no mode at all.
type sdcppCapabilitiesDoc struct {
	SupportedModes *[]string `json:"supported_modes"`
	Model          struct {
		Stem string `json:"stem"`
	} `json:"model"`
}

// parseSdcppCapabilities derives SdcppVerdicts from a capability document.
// Unlike the tolerant /props parser it is STRICT about the shape it reads: a
// body that is not JSON, or whose supported_modes or model is of the wrong
// type, is an error and yields no verdict -- a document this code cannot read
// must not be mistaken for one that lists no image mode, which would be a
// "no".
func parseSdcppCapabilities(body []byte) (SdcppVerdicts, error) {
	var doc sdcppCapabilitiesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return SdcppVerdicts{}, fmt.Errorf("%w: decode sdcpp capabilities: %v", ErrInvalidResponse, err)
	}
	out := SdcppVerdicts{ModelStem: strings.TrimSpace(doc.Model.Stem)}
	if doc.SupportedModes == nil {
		return out, nil // an absent list is not an exhaustive one: no verdict
	}
	out.Image = routing.CapabilityNo
	for _, mode := range *doc.SupportedModes {
		if strings.TrimSpace(mode) == sdcppModeImageGeneration {
			out.Image = routing.CapabilityYes
			break
		}
	}
	return out, nil
}
