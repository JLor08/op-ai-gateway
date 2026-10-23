// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
)

// portalSpeechLanguage is the gate of the portal's speech language control: whether the
// composer may offer one for gatewayModel, and the default to pre-fill it with. accepts is
// true when a mapping a speech request for the model could be routed to (the portal-
// visible, plain-tts ones: portalPlainTTSMappings, which is what the voice picker and the
// streaming gate resolve too) has a cached speech_language fact (routing.ParseSpeechLanguage
// of the scope-"" row the health loop writes) that says it accepts a `language` request
// field; def is the default that fact declares ("" for none), and is empty whenever accepts
// is false.
//
// A route is picked per request, after this is asked, and the streaming gate therefore
// requires EVERY candidate to agree. This one is the other way round, and deliberately:
// it opens as soon as ANY candidate accepts. A `language` sent to a model that has no such
// parameter is silently dropped by audio.cpp, so offering the control costs nothing where
// the request lands on a model without it, whereas withholding it would take the control
// away from a model that has it. The default is the first accepting candidate's, in the
// portal's (sorted) mapping order: a pre-fill and not a promise, since the model behind the
// route that serves the request applies its own default to an empty field either way.
//
// "No fact" is not "no": a mapping without a row, or with one nobody can read, was never
// observed loaded, which the composer cannot tell from a model that declares no parameter --
// both offer no control. An empty candidate set is a closed gate too. A store failure is
// an error, never read as a closed gate.
//
// This is the composer's question and not the run's authority: audio.cpp decides what a
// `language` field means, per model, when the request arrives.
func (s *Server) portalSpeechLanguage(ctx context.Context, principal auth.Token, gatewayModel string) (accepts bool, def string, err error) {
	candidates, _, err := s.portalPlainTTSMappings(ctx, principal, gatewayModel)
	if err != nil || len(candidates) == 0 {
		return false, "", err
	}
	cached, err := s.Routes.ModelMappingCacheForMappings(ctx, candidates, routing.ModelMappingCacheSpeechLanguage)
	if err != nil {
		return false, "", err
	}
	accepts, def = acceptedSpeechLanguage(candidates, cached)
	return accepts, def, nil
}

// acceptedSpeechLanguage folds the candidate mappings' cached speech_language facts into the
// gate's answer: true and the default of the first candidate (in the order given) whose
// fact says it accepts a language, and (false, "") when none does.
func acceptedSpeechLanguage(candidates []string, cached map[string][]routing.ModelMappingCacheRow) (bool, string) {
	for _, id := range candidates {
		if accepts, def, ok := scopeLessSpeechLanguage(cached[id]); ok && accepts {
			return true, def
		}
	}
	return false, ""
}

// scopeLessSpeechLanguage reads the usable speech_language fact out of one mapping's cache
// rows: the scope-"" row, the one the health loop writes (scopeLessByteRate's rule), through
// routing.ParseSpeechLanguage, the one definition of a usable fact.
func scopeLessSpeechLanguage(rows []routing.ModelMappingCacheRow) (accepts bool, def string, ok bool) {
	for _, row := range rows {
		if row.ScopeKey == "" {
			return routing.ParseSpeechLanguage(row.Payload)
		}
	}
	return false, "", false
}

// handlePortalModelSpeechLanguage answers whether the portal's speech composer may offer a
// language control for a gateway model (?name=<model>, the model the run will route for):
// {"accepts":true,"default":"<code>"} (default only when the model declares one) when
// portalSpeechLanguage's gate is open, {"accepts":false} otherwise. It is
// handlePortalModelSpeechStreaming's twin in everything but its answer: the same session-
// cookie auth (requireWebScope, gateway:use), the same name-as-a-query-parameter, the same
// blank-name 400 (model.name_required, for the reason an empty name would otherwise read as
// "no language control"), and the same no-leak rule -- every "no" (an unknown, hidden,
// locked or restricted model, a non-speech or clone one, one whose language no probe has
// stated yet) is the same 200, so the answer cannot probe which models exist. A store
// failure is the only 500, never read as a "no". Nothing here loads a model or contacts an
// upstream.
func (s *Server) handlePortalModelSpeechLanguage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(audioVoicesModelRequired, "model name is required", ""))
		return
	}
	accepts, def, err := s.portalSpeechLanguage(r.Context(), token, name)
	if err != nil {
		slog.Error("portal speech language lookup failed", "model", name, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response(portalSpeechLanguageFailed, "speech language lookup failed", ""))
		return
	}
	if !accepts {
		writeJSON(w, http.StatusOK, portalSpeechLanguageResponse{})
		return
	}
	writeJSON(w, http.StatusOK, portalSpeechLanguageResponse{Accepts: true, Default: def})
}

// portalSpeechLanguageFailed is the code of a failed read of the gate (API surface).
const portalSpeechLanguageFailed = "model.speech_language_failed"

// portalSpeechLanguageResponse is the body of GET /api/portal/model-speech-language.
// default is present only with accepts and a declared default: a "no" carries nothing to
// learn from.
type portalSpeechLanguageResponse struct {
	Accepts bool   `json:"accepts"`
	Default string `json:"default,omitempty"`
}
