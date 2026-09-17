// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// apiFlavorImages is the images endpoint's flavor string. It folds to the coarse
// "openai" through NormalizeAPIFlavor like every other openai* value (ADR-042),
// which is exactly why it cannot carry the capability gate on its own -- the
// gate keys on inference.Request.RequiredCapabilities instead. It exists for
// labelling (usage rows, upstreamPath) and for its own sessionEndpoint case,
// never for filtering.
const apiFlavorImages = "openai_images"

// handleOpenAIImages serves POST /v1/images/generations by relaying to a
// natively OpenAI-shaped image backend. There is no translate path: the gateway
// proxies image requests and does not synthesize them, so an application that
// does not serve this shape is simply not a candidate.
//
// The capability gate is what makes that true, and it runs inside the ONE
// existing admission gate: the request declares
// RequiredCapabilities = [routing.CapabilityImage] and inferencePreflight ->
// Resolve refuses a model without a yes verdict. No second admitPrincipal call
// site is added here; there is exactly one in this package and its comment
// records what broke when there were four.
func (s *Server) handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// sd-server IGNORES the request's model field -- one process serves one
	// model -- so the model here is purely OUR routing input, read with the same
	// tolerant probe every native-passthrough endpoint uses.
	model, _ := sniffRoutingModel(raw)
	if err := validateImagesRequest(raw, model); err != nil {
		writeRequestError(w, err)
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, inferenceShape{
		apiFlavor:            apiFlavorImages,
		endpoint:             endpointImages,
		model:                model,
		requiredCapabilities: []string{routing.CapabilityImage},
	})
	if handled {
		return
	}
	s.relayImages(w, r, token, pf.Req, raw)
}

// validateImagesRequest validates the shape this handler owns, instead of
// calling inference.Request.Validate: that validator requires len(Messages) > 0,
// and an images request has none, so calling it would reject every one of them.
// Only "prompt" (non-empty) and "model" (non-empty, resolved by the caller's own
// tolerant probe -- see sniffRoutingModel) are required; every other field is
// the client's own business and reaches the upstream unexamined.
func validateImagesRequest(raw []byte, model string) error {
	if strings.TrimSpace(model) == "" {
		return &inference.Error{Code: "images.model_required", Message: "model is required"}
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(raw, &body)
	if strings.TrimSpace(body.Prompt) == "" {
		return &inference.Error{Code: "images.prompt_required", Message: "prompt is required"}
	}
	return nil
}

// relayImages resolves the routing target for an already-gated images request
// and relays it via proxyNative. It follows tryProxyNative's resolve-then-relay
// shape, minus the translate fallback: images has none, so unlike
// tryProxyNative -- which returns false on an ordinary routing failure and lets
// the caller's translate path re-resolve and record the error -- EVERY resolve
// failure here is terminal and recorded directly, not just an admission-queue
// rejection. req is pf.Req from the caller's inferencePreflight call, already
// carrying RequiredCapabilities = [routing.CapabilityImage].
func (s *Server) relayImages(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	start := time.Now()
	target, err := s.resolveTarget(r.Context(), &token, req)
	if err != nil {
		id := nextRequestID()
		capturing := s.capturingEnabled(token)
		status := completionHTTPStatus(err)
		code := completionErrorCode(err)
		// Warn for an admission-queue rejection, mirroring tryProxyNative's own
		// severity split for the same error pair; Debug for every other routing
		// failure (no route, no healthy host, not capable), which is the
		// ordinary, expected shape of this gate doing its job.
		if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
			slog.Warn("images request admission rejected", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status)
		} else {
			slog.Debug("images request rejected: routing failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status, "err", err)
		}
		body := writeCompletionErrorCaptured(w, err)
		s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, status, req.APIFlavor))
		return
	}
	path := upstreamPath(target, req.APIFlavor)
	s.proxyNative(w, r, token, target, path, raw, req, endpointImages)
}
