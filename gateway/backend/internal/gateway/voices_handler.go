// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
)

// audioVoicesModelRequired is the code of the one request error
// handleAudioVoices answers with. It is API surface, and it is the portal
// voices route's code too (model.name_required, as handlePortalModelImageOptions
// answers a missing ?name=).
const audioVoicesModelRequired = "model.name_required"

// audioVoicesResponse is the body of GET /v1/audio/voices: the model name as the
// client asked for it and the voice names a speech request to that model may name
// in its "voice" field. Voices is never nil, so an empty answer encodes as [].
type audioVoicesResponse struct {
	Model  string   `json:"model"`
	Voices []string `json:"voices"`
}

// handleAudioVoices serves GET /v1/audio/voices?model=<gateway model name> (and
// its /openai/v1/audio/voices alias, server.go): the voices the gateway has
// cached for the model, as the union across everything the model name resolves to
// (voicesForModel). It READS THE CACHE and nothing else: no upstream is asked and
// no model is loaded, so a model that is not loaded is answered from what the
// health loop recorded while it was (the names stay on offer after an unload).
//
// Auth is requireInternalOrBearerAnyScope, the speech endpoints' own: a bearer
// token with gateway:use or llm:invoke, or the internal trusted-loopback pair,
// DELIBERATELY not the browser session cookie (authenticateInternalOrBearer).
// A browser reaches voices through the session-cookie portal route instead.
// run-as is not applied: it only swaps the loopback principal for another token
// OF THE SAME USER, and what a user may route to (provisioning) never depends on
// which of their tokens asks. The token's OWN model rules do, and are applied
// below, as dispatch applies them: the model override first, then a service
// token's model allowlist against the effective name.
//
// Status codes are the no-leak ones: a missing or blank model is 400
// model.name_required, because it is a client bug that an empty list would hide,
// and EVERY other "nothing to show" case -- an unknown name, a model the token
// cannot route to or is not allowed to use, a non-speech or clone model, one with
// nothing cached yet -- is the same 200 with "voices":[], so the answer cannot be
// used to probe which models exist or which servers a token is kept away from. A
// store failure is the only 500, and it is never read as an empty answer.
//
// Voices are deliberately not part of /v1/models: a model list stays one cheap
// read and a voice list is per-model detail a client asks for on demand.
func (s *Server) handleAudioVoices(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	// The name is looked up exactly as sent, as the speech endpoint routes the
	// model it is sent; only an empty or blank one is refused.
	model := r.URL.Query().Get("model")
	if strings.TrimSpace(model) == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response(audioVoicesModelRequired, "model is required", ""))
		return
	}
	// The two TOKEN-level gates dispatch applies before it resolves anything
	// (inferencePreflight): the token's own model override rewrites the requested
	// name, and a service token's allowlist is then checked against the EFFECTIVE
	// name. Skipping them would make this endpoint an existence oracle for a
	// restricted token, which gets a 403 from speech for the same model. A refusal
	// here is deliberately NOT a 403: it is the same empty 200 every other
	// "nothing to show" case gives, so the answer says nothing about the model.
	// The request's own name is what the response echoes either way.
	effective := resolveModelOverride(r.Context(), token, model, s.Portal)
	voices := []string{}
	if modelAllowed(token, effective) {
		var err error
		voices, err = s.voicesForModel(r.Context(), token, effective)
		if err != nil {
			slog.Error("audio voices lookup failed", "model", model, "err", err)
			writeJSON(w, http.StatusInternalServerError, apierror.Response("model.voices_failed", "model voices failed", ""))
			return
		}
	}
	writeJSON(w, http.StatusOK, audioVoicesResponse{Model: model, Voices: voices})
}

// voicesForModel returns the voices the gateway has cached for modelName that a
// speech dispatch by token could reach: the deduped, sorted union of the cached
// voice lists of every mapping the name resolves to, a group name being expanded
// to its members. It never loads a model and never contacts an upstream.
//
// The visibility rule is ROUTING-consistent, so the answer neither offers a
// model the token cannot dispatch to nor hides one it can:
// routing.Resolver.ReachableCandidates (the group expansion, the locked/direct
// rule and resource-group provisioning, dispatch's own code) over the speech
// flavor, then the CURRENT tts verdict (cachedVoicesForMappings). It does NOT
// apply the token's own model rules (override, service allowlist): those rewrite
// or refuse the NAME a request carries, so they belong to the caller that holds
// the request (handleAudioVoices), which hands this the effective name.
//
// This is the endpoint's answer for that name, split from handleAudioVoices so another
// route can reuse the union under a DIFFERENT visibility rule: the portal voices
// route (a session-cookie caller that must not see what the portal hides or
// locks) decides which mappings its principal may see itself and hands exactly
// those ids to cachedVoicesForMappings, which is the half that does not depend on
// who is asking.
//
// An unresolvable name is an empty, non-nil slice, not an error. So is a server
// without a resolver (no routing store), which can reach nothing.
func (s *Server) voicesForModel(ctx context.Context, token auth.Token, modelName string) ([]string, error) {
	if s.Resolver == nil {
		return []string{}, nil
	}
	cands, err := s.Resolver.ReachableCandidates(ctx, token, modelName, apiFlavorSpeech)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Mapping.ID)
	}
	return s.cachedVoicesForMappings(ctx, ids)
}

// cachedVoicesForMappings returns the deduped, sorted union of the voice lists
// cached for the given mappings, restricted to the mappings that are plain tts
// models NOW. It is the visibility-free half of voicesForModel: the caller has
// already decided which mappings its principal may see, and this applies only
// what does not depend on the principal.
//
// The verdict gate runs here, at READ time, on the mappings' current stored
// rows (routing.CapabilityRowsPlainTTS): the cache is written while a model is
// loaded and a row can outlive the verdict that justified it, so a mapping that
// has since become a clone model, or stopped being a speech model, offers no
// voices even though a list is still on file. The same read keeps a manual
// verdict override authoritative.
//
// A voices row whose payload does not decode costs only its own mapping (logged
// at Warn, since the writer cannot produce one), so a single bad row never blanks
// the union. An empty payload and a JSON null both read as "no voices". Only the
// scope-"" row is read: it is the one the voices writers keep.
//
// The result is never nil. A store failure is returned, never read as empty.
func (s *Server) cachedVoicesForMappings(ctx context.Context, mappingIDs []string) ([]string, error) {
	voices := []string{}
	if s.Routes == nil {
		return voices, nil
	}
	ids := make([]string, 0, len(mappingIDs))
	seenID := make(map[string]struct{}, len(mappingIDs))
	for _, id := range mappingIDs {
		if _, dup := seenID[id]; dup || id == "" {
			continue
		}
		seenID[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return voices, nil
	}
	verdicts, err := s.Routes.MappingCapabilitiesForMappings(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read capability verdicts: %w", err)
	}
	tts := ids[:0:0]
	for _, id := range ids {
		if routing.CapabilityRowsPlainTTS(verdicts[id]) {
			tts = append(tts, id)
		}
	}
	if len(tts) == 0 {
		return voices, nil
	}
	cached, err := s.Routes.ModelMappingCacheForMappings(ctx, tts, routing.ModelMappingCacheVoices)
	if err != nil {
		return nil, fmt.Errorf("read the voices cache: %w", err)
	}
	seenName := map[string]struct{}{}
	for _, id := range tts {
		for _, row := range cached[id] {
			if row.ScopeKey != "" || strings.TrimSpace(row.Payload) == "" {
				continue
			}
			var names []string
			if err := json.Unmarshal([]byte(row.Payload), &names); err != nil {
				slog.Warn("voices cache row is undecodable and was skipped", "mapping_id", id, "err", err)
				continue
			}
			for _, name := range names {
				if _, dup := seenName[name]; dup || strings.TrimSpace(name) == "" {
					continue
				}
				seenName[name] = struct{}{}
				voices = append(voices, name)
			}
		}
	}
	slices.Sort(voices)
	return voices, nil
}
