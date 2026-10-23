// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
)

// The codes of the live-transcription ticket endpoint's own refusals (API surface). A
// refusal that is the routing layer's -- an unknown model, one without the transcription
// capability -- keeps the routing code the buffered transcription endpoint answers it with.
const (
	// portalLiveTranscriptionParamsInvalid: the audio params (sample_rate, channels,
	// sample_format) or the language are outside what the live relay accepts. 400.
	portalLiveTranscriptionParamsInvalid = "portal.live_transcription_params_invalid"
	// portalLiveTranscriptionNotStreaming: the model's mapping has no
	// transcription_streaming = yes verdict, so it cannot take a live session. 409.
	portalLiveTranscriptionNotStreaming = "portal.live_transcription_not_streaming"
	// portalLiveTranscriptionAgentUnsupported: the model runs behind an agent that has not
	// declared fullduplex_proxy. 409.
	portalLiveTranscriptionAgentUnsupported = "portal.live_transcription_agent_unsupported"
	// portalLiveTranscriptionTicketFailed: the ticket could not be minted (no store, or the
	// random source or the encoding failed). 500.
	portalLiveTranscriptionTicketFailed = "portal.live_transcription_ticket_failed"
)

// liveLanguageMaxLen bounds a ticket's language. The value is forwarded to the backend as a
// query parameter, so it is a language tag or name and nothing longer.
const liveLanguageMaxLen = 64

// bytesPerSample returns the size in bytes of one sample of a live PCM format.
// Only the two formats audio.cpp accepts for live transcription are known.
func bytesPerSample(format string) (int, bool) {
	switch format {
	case "s16le":
		return 2, true
	case "f32le":
		return 4, true
	}
	return 0, false
}

// liveByteRate returns the PCM bytes per second of a live session, the divisor that
// turns a byte count into audio seconds for billing. The sample rate and channel
// ranges are audio.cpp's own; ok is false when any param falls outside them.
func liveByteRate(sampleRate, channels int, format string) (int, bool) {
	size, ok := bytesPerSample(format)
	if !ok {
		return 0, false
	}
	if sampleRate < 1000 || sampleRate > 384000 {
		return 0, false
	}
	if channels < 1 || channels > 16 {
		return 0, false
	}
	rate := sampleRate * channels * size
	if rate <= 0 {
		return 0, false
	}
	return rate, true
}

// validLiveLanguage reports whether language may ride in a ticket: empty (the backend
// detects it), or a language tag or name of at most liveLanguageMaxLen characters from
// [A-Za-z0-9_-]. The language is forwarded as a query parameter of the upstream request,
// and this alphabet is what keeps a value from carrying a second parameter (an '&' or '='),
// an escape ('%') or anything else of the request line with it.
func validLiveLanguage(language string) bool {
	if len(language) > liveLanguageMaxLen {
		return false
	}
	for _, c := range language {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// liveTicketRequest is the body of POST /api/portal/transcription-live-ticket: the gateway
// model and the PCM the browser will stream, in the three params audio.cpp's /live takes.
type liveTicketRequest struct {
	Model        string `json:"model"`
	SampleRate   int    `json:"sample_rate"`
	Channels     int    `json:"channels"`
	SampleFormat string `json:"sample_format"`
	Language     string `json:"language"`
}

// liveTicketResponse is the 200 body: the opaque ticket and nothing of the decision it
// carries.
type liveTicketResponse struct {
	Ticket string `json:"ticket"`
}

// handlePortalLiveTranscriptionTicket serves POST /api/portal/transcription-live-ticket: the
// one authenticated, CSRF-protected step before a live transcription session. A browser
// WebSocket cannot send the portal's X-OP-CSRF header, so the upgrade cannot be protected the
// way every other portal mutation is; this POST is, and what it hands back -- a short-lived,
// single-use, encrypted ticket (liveTicketStore) -- is what the upgrade presents. A page of
// another origin cannot obtain one, which makes the ticket the upgrade's CSRF defense, and
// it carries the server's own decision, so the WebSocket handler never takes a model, a
// route or a billing rate from the client.
//
// Auth is requireWebScope(gateway:use), the portal's own: a session cookie on this POST also
// needs the X-OP-CSRF header (authenticateWebSession refuses an unsafe method without it:
// 403 auth.csrf_required), a bearer token does not (it is no ambient credential). The method
// is POST for the reason the header is required -- a GET is CSRF-exempt.
//
// The body is {model, sample_rate, channels, sample_format, language?}. In order:
//
//  1. The request is validated, before anything is spent on it: a blank model (400
//     model.name_required, as the model-voices and speech-streaming gates answer it), and the
//     audio params -- liveByteRate's own ranges, audio.cpp's, and a language that is a bare
//     tag -- as 400 portal.live_transcription_params_invalid. The params are validated here
//     ONCE and captured in the ticket, which is what keeps the rate the session bills at the
//     rate it tells the backend to decode at.
//  2. The model goes through the buffered transcription endpoint's own gate
//     (transcriptionInferenceShape, inferencePreflight, resolveTarget): the token's model
//     override and server override, the principal's rate/quota/budget admission -- the live
//     session's one admission, taken here because a refusal is a plain HTTP response on this
//     request and opaque on a WebSocket upgrade -- then the resolve, which enforces the
//     user's reachable servers and the transcription capability, and the effective-flavor
//     rule. A refusal is that endpoint's own (404 routing.no_model_route or
//     routing.model_not_capable, so no new way to probe which models exist), but this is a
//     gate and not a relay: no usage row is recorded, no capture is written and no upstream
//     is contacted.
//  3. The mapping must be known to stream a transcription (mappingStreamsTranscription, the
//     stored transcription_streaming verdict, yes only): 409
//     portal.live_transcription_not_streaming otherwise.
//  4. A server_agent target is served only on positive evidence its agent declared
//     fullduplex_proxy (AgentFeatures.Has, fail closed): 409
//     portal.live_transcription_agent_unsupported otherwise. A direct (audio_cpp) target has
//     no router in front of it and needs no flag.
//  5. The ticket is minted with the resolved routing.Target, the validated params and who
//     asked, and answered 200 {"ticket": "..."}. A mint that fails is 500
//     portal.live_transcription_ticket_failed.
func (s *Server) handlePortalLiveTranscriptionTicket(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req liveTicketRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(audioVoicesModelRequired, "model name is required", ""))
		return
	}
	language := strings.TrimSpace(req.Language)
	if _, valid := liveByteRate(req.SampleRate, req.Channels, req.SampleFormat); !valid || !validLiveLanguage(language) {
		writeJSON(w, http.StatusBadRequest, apierror.Response(portalLiveTranscriptionParamsInvalid,
			"sample_rate must be 1000-384000, channels 1-16, sample_format s16le or f32le, and language a bare language tag", ""))
		return
	}

	pf, handled := s.inferencePreflight(w, r, token, nil, transcriptionInferenceShape(model))
	if handled {
		return
	}
	target, err := s.resolveLiveTranscriptionTarget(r.Context(), &token, pf.Req)
	if err != nil {
		slog.Debug("live transcription ticket rejected: routing failed", "model", model, "err", err)
		writeJSON(w, completionHTTPStatus(err), completionErrorResponse(err))
		return
	}
	if !s.mappingStreamsTranscription(r.Context(), target.RouteID) {
		writeJSON(w, http.StatusConflict, apierror.Response(portalLiveTranscriptionNotStreaming,
			"this model does not support live transcription: its mapping has no transcription_streaming verdict of yes", ""))
		return
	}
	if !s.liveTranscriptionTargetServed(target) {
		writeJSON(w, http.StatusConflict, apierror.Response(portalLiveTranscriptionAgentUnsupported,
			"live transcription is not available on this model's server: its agent has not declared the "+fullduplexProxyFeature+" feature. The agent is older than 0.12.0, or it has not reported to the gateway since the gateway restarted or the agent was upgraded; update the agent, or retry once it has reported", ""))
		return
	}

	if s.liveTickets == nil {
		slog.Error("live transcription ticket not minted: no ticket store", "model", model)
		writeJSON(w, http.StatusInternalServerError, apierror.Response(portalLiveTranscriptionTicketFailed, "live transcription ticket could not be issued", ""))
		return
	}
	ticket, err := s.liveTickets.Mint(liveTicketClaims{
		Target:     target,
		SampleRate: req.SampleRate,
		Channels:   req.Channels,
		Format:     req.SampleFormat,
		Language:   language,
		UserID:     token.UserID,
		TokenID:    token.ID,
		// The WebSocket has no principal, so the usage row's service/project
		// attribution rides in the ticket (as the buffered path reads it off the token).
		ServiceID:   token.ServiceID,
		ServiceName: token.ServiceName,
		ProjectID:   token.ProjectID,
		ProjectName: token.ProjectName,
	})
	if err != nil {
		slog.Error("live transcription ticket not minted", "model", model, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response(portalLiveTranscriptionTicketFailed, "live transcription ticket could not be issued", ""))
		return
	}
	writeJSON(w, http.StatusOK, liveTicketResponse{Ticket: ticket})
}

// resolveLiveTranscriptionTarget resolves the routing target of an already-gated live
// transcription (pf.Req from inferencePreflight over transcriptionInferenceShape) and
// applies the effective-served conjunct to it, as resolveTranscriptionTarget does for the
// buffered relay: a server_agent child's own runtime spec, not its application, is the
// authority for the flavors it serves, so a child whose spec excludes audio is refused with
// routing.ErrNoModelRoute, candidacy's own no-route answer. Unlike that function it only
// returns the error: it writes no response and records no usage row, because this is a gate
// and not a relay -- the caller maps the error. token is a pointer for the reason
// resolveTarget's is.
func (s *Server) resolveLiveTranscriptionTarget(ctx context.Context, token *auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.resolveTarget(ctx, token, req)
	if err != nil {
		return routing.Target{}, err
	}
	if !targetServesFlavor(target, req.APIFlavor) {
		slog.Debug("live transcription ticket rejected: resolved target's effective flavors exclude audio",
			"api_flavor", req.APIFlavor, "model", req.Model, "server", s.serverName(target.ServerID), "route_id", target.RouteID)
		return routing.Target{}, routing.ErrNoModelRoute
	}
	return target, nil
}

// liveTranscriptionTargetServed reports whether target can be sent a live session. An
// agent-launched (server_agent) target is reached through the agent's runtime router, whose
// /live/{model} relay exists only in an agent that has DECLARED fullduplex_proxy; a router
// without it answers 404, which no later signal tells from an unknown model. So an agent
// target is served only on positive evidence: AgentFeatures.Has is false for a server that
// has never reported (a gateway restart empties the registry until the next telemetry
// sample), for one whose last sample lacked the name, and for a nil registry.
//
// Every other target is served: a direct audio_cpp application has no agent in front of it.
// The decision is on the TARGET's provider, not on whether its server happens to run an
// agent. It is transcriptionTargetServed's twin, on the live flag.
func (s *Server) liveTranscriptionTargetServed(target routing.Target) bool {
	if target.Provider != routing.ProviderServerAgent {
		return true
	}
	return s.AgentFeatures.Has(target.ServerID, fullduplexProxyFeature)
}
