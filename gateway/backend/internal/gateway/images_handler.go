// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
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

// relayImagesUpstreamError handles a non-2xx response from the images
// upstream (sd-server): it buffers the (bounded) body, normalises it via
// normalizeImagesUpstreamError, and relays either the normalised OpenAI object
// or -- when the body matches neither recognised shape -- the original bytes
// unchanged, never guessed at. Called from proxyNative (native_passthrough.go),
// gated to apiFlavorImages so no other native-passthrough flavor's error
// handling changes.
//
// Reads the whole body via io.ReadAll rather than proxyNative's streaming
// nativeCopier: images never streams (Stream is pinned false in
// handleOpenAIImages), so sd-server's response is always a single buffered
// payload, and normalising it requires seeing all of it before writing
// anything to the client -- the opposite of the copier's byte-as-it-arrives
// design. The read is bounded by s.captureMaxBytes (the same cap the copier's
// own capture buffer already uses, not widened here): an error body has no
// reason to exceed it.
func (s *Server) relayImagesUpstreamError(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, target routing.Target, resp *provider.ProxyResponse, serverName string, start time.Time, id string, capturing bool, raw []byte) {
	upstreamBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
	errorCode := fmt.Sprintf("upstream.%d", resp.StatusCode)
	if readErr != nil {
		// A read failure this late (upstream status line already arrived, body
		// broke mid-read) is the same class nativeTerminalStatus calls
		// provider.stream_copy_error on the streaming path; named the same way
		// here for one consistent code across both.
		errorCode = "provider.stream_copy_error"
		slog.Error("inference native passthrough copy error", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", serverName, "err", readErr)
	} else {
		slog.Warn("inference native passthrough upstream error", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "server", serverName, "status", resp.StatusCode)
	}

	var written []byte
	var sentContentType string
	if body, ok := normalizeImagesUpstreamError(resp.StatusCode, upstreamBody); ok {
		written = writeJSONCaptured(w, resp.StatusCode, body)
		sentContentType = jsonContentType
	} else {
		// Neither recognised shape: relay upstreamBody unchanged rather than
		// guess at one, per normalizeImagesUpstreamError's own contract.
		sentContentType = resp.Header.Get("Content-Type")
		if sentContentType == "" {
			sentContentType = jsonContentType
		}
		w.Header().Set("Content-Type", sentContentType)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(upstreamBody)
		written = upstreamBody
	}

	s.recordUsage(start, token, req, target, provider.Response{}, errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: sentContentType}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), written, resp.StatusCode, req.APIFlavor))
}

// imagesUpstreamErrorType and imagesUpstreamErrorCode are the GATEWAY's own
// classification for a normalised sd-server error (see
// normalizeImagesUpstreamError). They are fixed regardless of the upstream's
// HTTP status: sd-server's plain-string body carries no more information than
// the string itself, so inventing a status-dependent value would claim a
// distinction sd-server never drew.
const (
	imagesUpstreamErrorType = "upstream_error"
	imagesUpstreamErrorCode = "images.upstream_error"
)

// normalizeImagesUpstreamError converts sd-server's error shape into OpenAI's.
// sd-server returns {"error": "<plain string>"}; OpenAI clients expect
// {"error": {message, type, code}}.
//
// The type and code in the result are OURS, not the backend's: sd-server
// states neither, and nothing downstream may read them as an upstream
// statement. Recorded here because the alternative -- inventing a plausible
// upstream code -- would make a gateway-authored value indistinguishable from
// an attested one, which is the same class of error as reporting an unmeasured
// zero as a measurement.
//
// ok is false when the body is neither shape, and the caller then relays the
// upstream bytes unchanged rather than guessing.
func normalizeImagesUpstreamError(status int, body []byte) (apierror.Body, bool) {
	// Already OpenAI-shaped: pass through untouched. Re-normalising it would
	// overwrite the upstream's OWN type/code with ours, restating an attested
	// value as a gateway-authored one -- the inverse of the mistake this
	// function exists to avoid. Message is the discriminator: a plain-string
	// body's "error" key does not unmarshal into ErrorBody at all (its JSON
	// value is a string, not an object), so err != nil there and this branch
	// is skipped.
	var already apierror.Body
	if err := json.Unmarshal(body, &already); err == nil && already.Error.Message != "" {
		return already, true
	}

	// sd-server's own shape: {"error": "<plain string>"}.
	var plain struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &plain); err == nil && plain.Error != "" {
		return apierror.Body{Error: apierror.ErrorBody{
			Message: plain.Error,
			Type:    imagesUpstreamErrorType,
			Code:    imagesUpstreamErrorCode,
		}}, true
	}

	return apierror.Body{}, false
}
