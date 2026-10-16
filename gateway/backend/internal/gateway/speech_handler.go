// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// apiFlavorSpeech is the speech endpoint's (/v1/audio/speech) flavor string, the
// same local alias for the coarse routing.APIFlavorOpenAIAudio that
// apiFlavorImages is for routing.APIFlavorOpenAIImages: NormalizeAPIFlavor folds
// it to itself, so it filters candidacy (only an application declaring
// openai_audio is a speech candidate), labels usage rows, and answers
// upstreamPath. Like images, speech is a single-purpose flavor with no
// EndpointMode and no translate path; what the flavor cannot express -- whether
// a MODEL can synthesize speech -- is the capability gate's job, keyed on
// routing.CapabilitySpeech.
const apiFlavorSpeech = "openai_audio"

// The request-error codes validateSpeechRequest answers with. They are API
// surface: clients branch on them.
const (
	speechModelRequired = "speech.model_required"
	speechInputRequired = "speech.input_required"
	// speechResponseFormatUnsupported is the code for a response_format this relay
	// cannot honor -- every value but "wav"; see validateSpeechRequest.
	speechResponseFormatUnsupported = "speech.response_format_unsupported"
	// speechStreamUnsupported is the code for a request asking for a streamed
	// response, which v1 does not produce; see validateSpeechRequest.
	speechStreamUnsupported = "speech.stream_unsupported"
)

// speechInferenceShape is the shape handleOpenAISpeech hands the ONE preflight
// gate (inferencePreflight). It is a function, not an inline literal, so the
// exact values the handler uses are the ones its test pins:
//
//   - apiFlavor is exactly apiFlavorSpeech, never a fine variant: upstreamPath
//     answers by string equality, and the coarse flavor is what candidacy,
//     targetServesFlavor and the usage row key on.
//   - requiredCapabilities is [routing.CapabilitySpeech], which does two jobs.
//     It feeds the capability gate (a model without a (speech, yes) verdict is
//     refused, an unprobed one included). And it keeps a speech request off the
//     vendor-account path: vendorAccountRoutingEligible excludes the images
//     flavor by name but has no openai_audio twin, and declines a request only
//     because it carries a capability list, while a vendor account's model match
//     reads the model name and never the flavor. A handler that dropped the list
//     would skip the gate AND become routable to a vendor model of the same name.
func speechInferenceShape(model string) inferenceShape {
	return inferenceShape{
		apiFlavor:            apiFlavorSpeech,
		endpoint:             endpointSpeech,
		model:                model,
		requiredCapabilities: []string{routing.CapabilitySpeech},
	}
}

// handleOpenAISpeech serves POST /v1/audio/speech (and its /openai/v1/audio/speech
// alias, server.go) by relaying to a natively OpenAI-shaped speech backend
// (audio.cpp). It mirrors handleOpenAIImages step for step: there is no
// translate path, the gateway proxies speech requests and does not synthesize
// them, so an application that does not serve openai_audio is simply not a
// candidate. The response is binary audio, relayed by proxyNative without
// buffering it.
//
// Auth: requireInternalOrBearerAnyScope admits a bearer token (every ordinary API
// client) OR the internal trusted-loopback header pair, DELIBERATELY not the
// browser session cookie -- see authenticateInternalOrBearer's own doc comment
// (auth_internal_or_bearer.go).
//
// The capability gate is what makes the "not a candidate" claim true, and it
// runs inside the ONE existing admission gate: the request declares
// RequiredCapabilities = [routing.CapabilitySpeech] (speechInferenceShape) and
// inferencePreflight -> Resolve refuses a model without a yes verdict. No second
// admitPrincipal call site is added here.
//
// Run-as: exactly handleOpenAIChat's block, copied (as handleOpenAIImages copies
// it) rather than shared. token.ID == "" only for the loopback principal, so
// this fires only for the run executor's own loopback calls; without it a
// run-as speech call would bill and capture under the bare session instead of
// the token.
func (s *Server) handleOpenAISpeech(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// The model read here is the client's gateway model name: purely OUR routing
	// input (proxyNative rewrites it to the application's own model id before the
	// relay), read with the same tolerant probe every native-passthrough endpoint
	// uses.
	model, _ := sniffRoutingModel(raw)
	if err := validateSpeechRequest(raw, model); err != nil {
		writeRequestError(w, err)
		return
	}
	if token.ID == "" { // loopback principal (not a bearer token): honor optional run-as, exactly as handleOpenAIChat
		if runAsID := strings.TrimSpace(r.Header.Get(runAsHeaderName)); runAsID != "" {
			if s.Portal == nil {
				// Fail closed rather than silently fall back to the bare session
				// principal: the caller explicitly asked to run as a specific
				// token, and with no Portal there is no way to authorize it.
				writePortalTokenError(w, portal.ErrTokenForbidden)
				return
			}
			runAs, rErr := s.Portal.AuthorizeRunAsToken(r.Context(), token, runAsID)
			if rErr != nil {
				writePortalTokenError(w, rErr)
				return
			}
			token = runAs
		}
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, speechInferenceShape(model))
	if handled {
		return
	}
	s.relaySpeech(w, r, token, pf.Req, raw)
}

// validateSpeechRequest validates the shape this handler owns, instead of
// calling inference.Request.Validate (which requires messages, and a speech
// request has none). "model" (non-empty, resolved by the caller's tolerant probe
// -- see sniffRoutingModel) and "input" (a non-empty string) are required, and
// refused here with a gateway 400 rather than relayed: audio.cpp answers a client
// error such as a missing input or an unknown model with HTTP 500, which the
// gateway would otherwise hand on as a server failure. Every other field (voice,
// instructions, speed, ...) is the client's own business and reaches the
// upstream unexamined, with three exceptions.
//
// response_format decision: only "wav" (or an absent value) is accepted, and
// EVERY other value -- mp3, opus, flac, aac and pcm, but also json and b64_json
// -- is refused with speechResponseFormatUnsupported. v1 relays a purely binary
// audio/wav body: audio.cpp answers an unsupported value such as mp3 with raw
// WAV labelled audio/wav (a client asking for mp3 would be handed WAV without
// being told), and json/b64_json are not OpenAI speech formats at all and,
// unlike the wav response, carry none of the metering headers the usage row's
// billing quantity is read from -- relayed, they would be billed as zero
// seconds. A 400 the client can act on beats both silent outcomes.
//
// stream decision: `stream: true` and ANY stream_format (sse or audio) are
// refused with speechStreamUnsupported. The relay's buffering and billing
// contract is that of a single complete audio response, and relaying a streaming
// request would hand the client a shape the gateway has not been built, metered
// or tested for. `stream: false`, an absent stream and a null stream_format
// describe what the endpoint already does and are accepted.
//
// response_format, stream and stream_format are decoded as json.RawMessage and
// then type-checked, for the reason validateImagesRequest gives: a typed decode
// would discard the type error and leave the field at its zero value, walking a
// non-string response_format such as ["mp3"] or 123 past the check. A JSON null
// is an absent value throughout.
//
// The result is the concrete *inference.Error, nil for a valid request. Callers
// compare it to nil as returned and only then hand it on as an error, which
// avoids the typed-nil-in-an-interface trap.
func validateSpeechRequest(raw []byte, model string) *inference.Error {
	if strings.TrimSpace(model) == "" {
		return &inference.Error{Code: speechModelRequired, Message: "model is required"}
	}
	var body struct {
		Input          string          `json:"input"`
		ResponseFormat json.RawMessage `json:"response_format"`
		Stream         json.RawMessage `json:"stream"`
		StreamFormat   json.RawMessage `json:"stream_format"`
	}
	_ = json.Unmarshal(raw, &body)
	if strings.TrimSpace(body.Input) == "" {
		return &inference.Error{Code: speechInputRequired, Message: "input is required and must be a non-empty string"}
	}
	if len(body.ResponseFormat) > 0 {
		var format string
		if err := json.Unmarshal(body.ResponseFormat, &format); err != nil {
			return &inference.Error{Code: speechResponseFormatUnsupported, Message: fmt.Sprintf("response_format must be the string \"wav\"; got %s", body.ResponseFormat)}
		}
		if format != "" && format != "wav" {
			return &inference.Error{Code: speechResponseFormatUnsupported, Message: fmt.Sprintf("response_format %q is not supported; only \"wav\" is", format)}
		}
	}
	if len(body.Stream) > 0 {
		var stream bool
		if err := json.Unmarshal(body.Stream, &stream); err != nil {
			return &inference.Error{Code: speechStreamUnsupported, Message: fmt.Sprintf("stream must be the boolean false or absent; got %s", body.Stream)}
		}
		if stream {
			return &inference.Error{Code: speechStreamUnsupported, Message: "stream is not supported on this endpoint; it always returns a single complete audio/wav response"}
		}
	}
	if len(body.StreamFormat) > 0 && string(body.StreamFormat) != "null" {
		return &inference.Error{Code: speechStreamUnsupported, Message: "stream_format is not supported on this endpoint; it always returns a single complete audio/wav response"}
	}
	return nil
}

// recordSpeechRoutingFailure writes the client-facing error response for a
// terminal speech routing failure and records its usage row, so
// resolveSpeechTarget's two refusal sites -- an outright resolveTarget failure
// and the post-resolve effective-flavor conjunct -- cannot drift in what they
// record or capture. Mirrors recordImagesRoutingFailure: the row is recorded
// against routing.Target{} for BOTH callers because neither reached proxyNative,
// so no upstream call was made, and a populated Host/RouteID/ProviderPath would
// claim one that never happened (and make the energy reconciler price the row
// against a server that never saw the request).
//
// No billing unit is set here: a refused request produced no audio, and the
// speech billing measure is recorded where the relay finishes, not where a
// request is refused.
func (s *Server) recordSpeechRoutingFailure(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte, start time.Time, err error) {
	id := nextRequestID()
	capturing := s.capturingEnabled(token)
	status := completionHTTPStatus(err)
	code := completionErrorCode(err)
	// Warn for an admission-queue rejection, mirroring tryProxyNative's own
	// severity split for the same error pair; Debug for every other routing
	// failure (no route, no healthy host, not capable), which is the ordinary,
	// expected shape of the gate doing its job.
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
		slog.Warn("speech request admission rejected", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status)
	} else {
		slog.Debug("speech request rejected: routing failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status, "err", err)
	}
	body := writeCompletionErrorCaptured(w, err)
	s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, Endpoint: endpointSpeech}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, status, req.APIFlavor), nil)
}

// resolveSpeechTarget is resolveImagesTarget's speech twin: it resolves the
// routing target for an already-gated speech request and applies the
// effective-served conjunct to it. On a refusal it has already written the
// client's response and recorded the usage row, and answers ok == false.
//
// token is a pointer for the reason tryProxyNative's is: resolveTarget refreshes
// the caller's last-used-model snapshot through it, and the relay that follows
// must carry that refreshed token.
func (s *Server) resolveSpeechTarget(w http.ResponseWriter, r *http.Request, token *auth.Token, req inference.Request, raw []byte, start time.Time) (routing.Target, bool) {
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		s.recordSpeechRoutingFailure(w, r, *token, req, raw, start, err)
		return routing.Target{}, false
	}
	// The effective-served rule is a CONJUNCT (§6 / ADR-033): flavor ∈ APIFlavors
	// AND the endpoint is not disabled. For a server_agent mapping the resolved
	// runtime spec -- not the application -- is the authority for its model's
	// flavors (targetFrom), and candidacy gated that app type on its coarser
	// app-level flavors only. The speech relay reaches proxyNative directly, so
	// without this an agent-managed child whose spec lists only text would still
	// serve a speech request.
	if !targetServesFlavor(target, req.APIFlavor) {
		// The refusal looks like candidacy's own no-route answer WHEN THIS WAS THE
		// ONLY ROUTE for the model, and is not retried against a sibling
		// application that might still serve the request -- the same limitation
		// resolveImagesTarget and tryProxyNative's own equivalent carry.
		slog.Debug("speech request rejected: resolved target's effective flavors exclude speech",
			"path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model,
			"server", s.serverName(target.ServerID), "route_id", target.RouteID)
		s.recordSpeechRoutingFailure(w, r, *token, req, raw, start, routing.ErrNoModelRoute)
		return routing.Target{}, false
	}
	return target, true
}

// relaySpeech relays an already-gated speech request via proxyNative, following
// relayImages' resolve-then-relay shape with no translate fallback: EVERY resolve
// failure is terminal and recorded directly (resolveSpeechTarget). req is pf.Req
// from the caller's inferencePreflight call, already carrying
// RequiredCapabilities = [routing.CapabilitySpeech].
//
// proxyNative streams the upstream body to the client chunk by chunk and relays
// the upstream Content-Type verbatim, which is what carries an audio/wav body.
func (s *Server) relaySpeech(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	start := time.Now()
	target, ok := s.resolveSpeechTarget(w, r, &token, req, raw, start)
	if !ok {
		return
	}
	s.proxyNative(w, r, nativeRelay{
		token:    token,
		target:   target,
		path:     upstreamPath(target, apiFlavorSpeech, endpointSpeech),
		raw:      raw,
		pfReq:    req,
		endpoint: endpointSpeech,
	})
}
