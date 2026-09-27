// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SdcppCapabilitiesPath is stable-diffusion.cpp's capability document. Its
// supported_modes list is EXHAUSTIVE (the server lists every mode it
// serves), which is what lets it answer "image: no" as well as "yes".
const SdcppCapabilitiesPath = "/sdcpp/v1/capabilities"

// sdcppModeImageGeneration is the supported_modes entry for image
// generation.
const sdcppModeImageGeneration = "img_gen"

// sdcppVerdicts is what the agent reads from the capability document.
type sdcppVerdicts struct {
	image string // "yes" | "no" | "" (no verdict)
}

// sdcppCapabilitiesDoc is decoded with the SAME shape as the gateway's
// provider.sdcppCapabilitiesDoc. That includes model.stem, which the agent
// does not use: the gateway reads it, so a wrongly typed model.stem is an
// error there, and decoding it here makes a wrongly typed model or
// model.stem an error in both copies (an empty struct would reject only a
// wrongly typed model). SupportedModes is a pointer so that an absent or
// null list stays distinguishable from an empty one.
type sdcppCapabilitiesDoc struct {
	SupportedModes *[]string `json:"supported_modes"`
	Model          struct {
		Stem string `json:"stem"`
	} `json:"model"`
}

// parseSdcppCapabilities is a DUPLICATE, on purpose, of the gateway's
// provider.parseSdcppCapabilities (gateway/backend/internal/provider/
// sdcpp_capabilities.go): the two Go modules share no package. The two
// copies must decide identically on identical input, and sdcppImageCases
// pins both. Change them together.
func parseSdcppCapabilities(body []byte) (sdcppVerdicts, error) {
	var doc sdcppCapabilitiesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return sdcppVerdicts{}, fmt.Errorf("decode sdcpp capabilities: %w", err)
	}
	if doc.SupportedModes == nil {
		return sdcppVerdicts{}, nil // an absent list is not an exhaustive one
	}
	out := sdcppVerdicts{image: "no"}
	for _, mode := range *doc.SupportedModes {
		if strings.TrimSpace(mode) == sdcppModeImageGeneration {
			out.image = "yes"
			break
		}
	}
	return out, nil
}

// ProbeSdcppVerdicts GETs baseURL+SdcppCapabilitiesPath and returns the
// image verdict the document yields, in the same PropsVerdicts shape and
// under the same stable/transient contract as ProbePropsVerdicts and
// ProbeOllamaVerdicts:
//   - 404/401/403/405 are fixed properties of the running binary, so they
//     are conclusive (stable, no verdict);
//   - no response and every other non-2xx (5xx above all) are transient;
//   - a body that is not syntactically JSON reads as a child still
//     mid-response, so it is transient;
//   - a JSON body whose shape this rule cannot read is conclusive with no
//     verdict, because the same build will answer the same way.
//
// LiveProgress is always "": sd-server has no timings surface.
func ProbeSdcppVerdicts(ctx context.Context, client *http.Client, baseURL string) (PropsVerdicts, bool) {
	body, status, err := fetchProbeBody(ctx, client, baseURL, SdcppCapabilitiesPath)
	if err != nil {
		stable := status == http.StatusNotFound ||
			status == http.StatusUnauthorized ||
			status == http.StatusForbidden ||
			status == http.StatusMethodNotAllowed
		return PropsVerdicts{}, stable
	}
	if !json.Valid(body) {
		return PropsVerdicts{}, false
	}
	v, err := parseSdcppCapabilities(body)
	if err != nil {
		return PropsVerdicts{}, true
	}
	return PropsVerdicts{Caps: Capabilities{Image: v.image}}, true
}
