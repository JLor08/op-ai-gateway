// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// GET /api/portal/model-speech-language is the composer's gate for the speech language
// control: whether the model accepts a `language` request field, and the default it
// declares. The fixture is the voices world (voices_handler_test.go): the same mappings,
// verdicts, visibility rules and provisioning the voices and speech-streaming gates are
// tested over, plus a speech_language cache row seeded where a case needs one, so each
// "no" below differs from a "yes" in the gate alone. usr_other (voicesOtherSecret) is the
// plain user the hidden/locked rules apply to; usr_dev (voicesDevSecret) is the admin that
// bypasses them but not provisioning.

// setSpeechLanguage writes the scope-"" speech_language row of a mapping, as the health
// loop does (routing.SpeechLanguagePayload).
func (w *voicesWorld) setSpeechLanguage(t *testing.T, mappingID string, accepts bool, def string) {
	t.Helper()
	w.setSpeechLanguageRow(t, mappingID, "", routing.SpeechLanguagePayload(accepts, def))
}

// setSpeechLanguageRow writes a speech_language row of a mapping verbatim, for the
// payloads and scopes the health loop never writes.
func (w *voicesWorld) setSpeechLanguageRow(t *testing.T, mappingID, scope, payload string) {
	t.Helper()
	row := routing.ModelMappingCacheRow{MappingID: mappingID, CacheType: routing.ModelMappingCacheSpeechLanguage, ScopeKey: scope, Payload: payload, UpdatedAt: w.now}
	if err := w.routes.UpsertModelMappingCache(context.Background(), row); err != nil {
		t.Fatalf("UpsertModelMappingCache %s: %v", mappingID, err)
	}
}

// askSpeechLanguage asks the gate for model as the bearer secret.
func (w *voicesWorld) askSpeechLanguage(t *testing.T, secret, model string) *httptest.ResponseRecorder {
	t.Helper()
	return w.getVoices(t, "/api/portal/model-speech-language", "name="+url.QueryEscape(model), map[string]string{"Authorization": "Bearer " + secret})
}

// requireSpeechLanguage fails unless the gate answers exactly want (a body ending in the
// newline writeJSON adds): the byte-for-byte check also pins that a "no" is one key and a
// "yes" without a declared default carries no `default` key.
func (w *voicesWorld) requireSpeechLanguage(t *testing.T, secret, model, want string) {
	t.Helper()
	rec := w.askSpeechLanguage(t, secret, model)
	if got := rec.Body.String(); rec.Code != http.StatusOK || got != want+"\n" {
		t.Fatalf("%q as %s: status = %d, body = %q, want 200 %s", model, secret, rec.Code, got, want)
	}
}

const (
	speechLanguageNo       = `{"accepts":false}`
	speechLanguageZh       = `{"accepts":true,"default":"zh"}`
	speechLanguageNoDef    = `{"accepts":true}`
	speechLanguageFailCode = "model.speech_language_failed"
)

// A model whose mapping has a cached {accepts:true} fact answers it, with the default the
// model declares; a model that declares none answers accepts alone, and a "no" never
// carries a default, even a cached one: it has nothing to say.
func TestHandlePortalModelSpeechLanguage(t *testing.T) {
	w := newVoicesWorld(t)
	for _, m := range []voicesMapping{
		{id: "map-lang-zh", app: "a", gateway: "tts-lang-zh", speech: routing.CapabilityYes},
		{id: "map-lang-nodef", app: "a", gateway: "tts-lang-nodef", speech: routing.CapabilityYes},
		{id: "map-lang-no", app: "a", gateway: "tts-lang-no", speech: routing.CapabilityYes},
		{id: "map-lang-nodefault", app: "a", gateway: "tts-lang-no-default", speech: routing.CapabilityYes},
		{id: "map-lang-none", app: "a", gateway: "tts-lang-unprobed", speech: routing.CapabilityYes},
	} {
		w.addMapping(t, m)
	}
	w.setSpeechLanguage(t, "map-lang-zh", true, "zh")
	w.setSpeechLanguage(t, "map-lang-nodef", true, "")
	w.setSpeechLanguage(t, "map-lang-no", false, "")
	w.setSpeechLanguage(t, "map-lang-nodefault", false, "zh")

	for name, c := range map[string]struct{ model, want string }{
		"accepts, with a default":          {"tts-lang-zh", speechLanguageZh},
		"accepts, declaring no default":    {"tts-lang-nodef", speechLanguageNoDef},
		"declares no language param":       {"tts-lang-no", speechLanguageNo},
		"a cached default of a no is mute": {"tts-lang-no-default", speechLanguageNo},
		"never probed (no row)":            {"tts-lang-unprobed", speechLanguageNo},
	} {
		t.Run(name, func(t *testing.T) {
			w.requireSpeechLanguage(t, voicesOtherSecret, c.model, c.want)
		})
	}
}

// Every "no" -- an unknown, hidden, locked, restricted, non-speech, clone, unprobed, text-
// only-app or inactive model, or a payload nobody can read -- is the SAME 200 {"accepts":
// false}, so the answer cannot be used to tell which models exist. Wherever a row could
// exist the case carries an accepting one, so only the gate keeps it out; the positive
// controls prove the row is otherwise readable.
func TestHandlePortalModelSpeechLanguageEveryNoIsTheSame200(t *testing.T) {
	w := newVoicesWorld(t)
	w.addMapping(t, voicesMapping{id: "map-lang-inactive", app: "a", gateway: "tts-lang-inactive", speech: routing.CapabilityYes, disabled: true})
	w.addMapping(t, voicesMapping{id: "map-lang-corrupt", app: "a", gateway: "tts-lang-corrupt", speech: routing.CapabilityYes})
	w.addMapping(t, voicesMapping{id: "map-lang-scoped", app: "a", gateway: "tts-lang-scoped", speech: routing.CapabilityYes})
	for _, id := range []string{"map-asr", "map-clone", "map-unprobed", "map-textapp", "map-hidden", "map-locked", "map-restricted", "map-g1", "map-lang-inactive"} {
		w.setSpeechLanguage(t, id, true, "zh")
	}
	// A payload that is no fact is exactly an unprobed mapping: no accepts, no answer.
	w.setSpeechLanguageRow(t, "map-lang-corrupt", "", `not json`)
	// Only the scope-"" row is the fact; a scoped one is none the health loop writes.
	w.setSpeechLanguageRow(t, "map-lang-scoped", "other", routing.SpeechLanguagePayload(true, "zh"))

	for name, c := range map[string]struct{ secret, model string }{
		"unknown name":                  {voicesOtherSecret, "no-such-model"},
		"not a speech model":            {voicesOtherSecret, "asr-model"},
		"a clone model":                 {voicesOtherSecret, "tts-clone"},
		"no speech verdict yet":         {voicesOtherSecret, "tts-unprobed"},
		"nothing cached":                {voicesOtherSecret, "tts-uncached"},
		"an app that does not speak":    {voicesOtherSecret, "tts-textapp"},
		"a hidden model":                {voicesOtherSecret, "tts-hidden"},
		"a locked model":                {voicesOtherSecret, "tts-locked"},
		"a locked group":                {voicesOtherSecret, "grp-locked"},
		"restricted to another user":    {voicesDevSecret, "tts-restricted"},
		"an inactive mapping":           {voicesOtherSecret, "tts-lang-inactive"},
		"an unreadable payload":         {voicesOtherSecret, "tts-lang-corrupt"},
		"a row that is not scope-empty": {voicesOtherSecret, "tts-lang-scoped"},
	} {
		t.Run(name, func(t *testing.T) {
			w.requireSpeechLanguage(t, c.secret, c.model, speechLanguageNo)
		})
	}

	// Positive controls: the same rows ARE served where the portal shows them.
	w.requireSpeechLanguage(t, voicesDevSecret, "tts-hidden", speechLanguageZh)
	w.requireSpeechLanguage(t, voicesDevSecret, "tts-locked", speechLanguageZh)
	w.requireSpeechLanguage(t, voicesDevSecret, "grp-locked", speechLanguageZh)
	w.requireSpeechLanguage(t, voicesOtherSecret, "tts-restricted", speechLanguageZh)
}

// A group is the union of its visible members: a member that accepts makes the group
// accept, and the visibility rules are the voices gate's (a locked group is suppressed
// whole under its own name, a locked member is reachable through a visible group).
func TestHandlePortalModelSpeechLanguageExpandsAGroupToItsMembers(t *testing.T) {
	w := newVoicesWorld(t)
	w.setSpeechLanguage(t, "map-g1", true, "zh")     // tts-one, a member of tts-group and grp-open
	w.setSpeechLanguage(t, "map-locked", true, "de") // tts-locked, a member of grp-of-locked

	w.requireSpeechLanguage(t, voicesOtherSecret, "tts-group", speechLanguageZh)
	w.requireSpeechLanguage(t, voicesOtherSecret, "grp-of-locked", `{"accepts":true,"default":"de"}`)
	w.requireSpeechLanguage(t, voicesOtherSecret, "tts-locked", speechLanguageNo)
}

// A request is routed to ONE of a model's mappings, picked after this is asked, so the
// gate is fail-safe the other way round from the streaming one: it accepts as soon as ANY
// plain-tts candidate says so (a language sent to a model that has no such parameter is
// silently dropped by audio.cpp, whereas hiding the control would take it away from a
// model that has it). The default is the first accepting candidate's, in mapping-id order.
func TestHandlePortalModelSpeechLanguageReconcilesMappings(t *testing.T) {
	w := newVoicesWorld(t)
	const yes = routing.CapabilityYes
	for _, m := range []voicesMapping{
		// One accepts, one was never probed.
		{id: "map-lm-a1", app: "a", gateway: "tts-lm-one", speech: yes},
		{id: "map-lm-a2", app: "b", gateway: "tts-lm-one", speech: yes},
		// One declares no param, the other accepts.
		{id: "map-lm-b1", app: "a", gateway: "tts-lm-no-then-yes", speech: yes},
		{id: "map-lm-b2", app: "b", gateway: "tts-lm-no-then-yes", speech: yes},
		// Both accept, with different defaults: the first by id.
		{id: "map-lm-c1", app: "a", gateway: "tts-lm-both", speech: yes},
		{id: "map-lm-c2", app: "b", gateway: "tts-lm-both", speech: yes},
		// None accepts: one declares no param, the other was never probed.
		{id: "map-lm-d1", app: "a", gateway: "tts-lm-none", speech: yes},
		{id: "map-lm-d2", app: "b", gateway: "tts-lm-none", speech: yes},
		// The accepting mapping is a clone one: a speech request is never routed to it.
		{id: "map-lm-e1", app: "a", gateway: "tts-lm-clone", speech: yes},
		{id: "map-lm-e2", app: "b", gateway: "tts-lm-clone", speech: yes, clone: yes},
		// A corrupt row costs only its own mapping.
		{id: "map-lm-f1", app: "a", gateway: "tts-lm-corrupt", speech: yes},
		{id: "map-lm-f2", app: "b", gateway: "tts-lm-corrupt", speech: yes},
		// The accepting mapping is inactive.
		{id: "map-lm-g1", app: "a", gateway: "tts-lm-inactive", speech: yes},
		{id: "map-lm-g2", app: "b", gateway: "tts-lm-inactive", speech: yes, disabled: true},
	} {
		w.addMapping(t, m)
	}
	w.setSpeechLanguage(t, "map-lm-a2", true, "zh")
	w.setSpeechLanguage(t, "map-lm-b1", false, "")
	w.setSpeechLanguage(t, "map-lm-b2", true, "en")
	w.setSpeechLanguage(t, "map-lm-c1", true, "de")
	w.setSpeechLanguage(t, "map-lm-c2", true, "fr")
	w.setSpeechLanguage(t, "map-lm-d1", false, "")
	w.setSpeechLanguage(t, "map-lm-e2", true, "zh")
	w.setSpeechLanguageRow(t, "map-lm-f1", "", `{"accepts":`)
	w.setSpeechLanguage(t, "map-lm-f2", true, "zh")
	w.setSpeechLanguage(t, "map-lm-g2", true, "zh")

	for name, c := range map[string]struct{ model, want string }{
		"accepts if one of two does":               {"tts-lm-one", speechLanguageZh},
		"a no beside a yes is a yes":               {"tts-lm-no-then-yes", `{"accepts":true,"default":"en"}`},
		"two yeses take the first by mapping id":   {"tts-lm-both", `{"accepts":true,"default":"de"}`},
		"no candidate accepting is a no":           {"tts-lm-none", speechLanguageNo},
		"a clone mapping is no candidate":          {"tts-lm-clone", speechLanguageNo},
		"an unreadable row costs only its mapping": {"tts-lm-corrupt", speechLanguageZh},
		"an inactive mapping is no candidate":      {"tts-lm-inactive", speechLanguageNo},
	} {
		t.Run(name, func(t *testing.T) {
			w.requireSpeechLanguage(t, voicesOtherSecret, c.model, c.want)
		})
	}
}

// A missing, empty or blank ?name= is a 400 model.name_required, the code the voices and
// the streaming gates use: an empty name would otherwise read as "no language control".
func TestHandlePortalModelSpeechLanguageRequiresAName(t *testing.T) {
	w := newVoicesWorld(t)
	for _, target := range []string{"", "name=", "name=%20%20", "model=tts-union"} {
		rec := w.getVoices(t, "/api/portal/model-speech-language", target, map[string]string{"Authorization": "Bearer " + voicesOtherSecret})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d, want 400; body = %s", target, rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "model.name_required" {
			t.Fatalf("?%s: error code = %q, want model.name_required", target, code)
		}
	}
}

// The gate reads the cache and never loads a model or touches an upstream.
func TestHandlePortalModelSpeechLanguageNeverCallsAnUpstream(t *testing.T) {
	w := newVoicesWorld(t)
	w.setSpeechLanguage(t, "map-u1", true, "zh")

	for _, model := range []string{"tts-union", "tts-group", "tts-uncached", "tts-restricted", "no-such-model"} {
		w.askSpeechLanguage(t, voicesOtherSecret, model)
	}
	if got := w.upstreamCalls.Load(); got != 0 {
		t.Fatalf("the upstream received %d requests, want 0", got)
	}
}

// A store failure is the ONLY 500, with its own code and a log line, never the 200 {"accepts":
// false} of "no": the portal's hidden/locked suppression fails closed, and so do the two
// reads the gate makes.
func TestHandlePortalModelSpeechLanguageAStoreFailureIsA500NotANo(t *testing.T) {
	for name, failing := range map[string]func(routing.Store) routing.Store{
		"the model settings": func(s routing.Store) routing.Store {
			return settingsFailStore{Store: s, err: errors.New("settings table unavailable")}
		},
		"the verdicts":       func(s routing.Store) routing.Store { return failingVoicesStore{Store: s, failVerdicts: true} },
		"the language cache": func(s routing.Store) routing.Store { return failingVoicesStore{Store: s, failCache: true} },
	} {
		t.Run(name, func(t *testing.T) {
			buf := withCapturedSlogAtTheDefaultLevel(t)
			w := newVoicesWorld(t)
			w.setSpeechLanguage(t, "map-u1", true, "zh")
			deps := w.deps
			deps.Routes = failing(w.routes)
			// The portal reads the settings through ITS OWN store handle, so it is
			// rebuilt over the failing store too.
			deps.Portal = portal.NewService(portal.ServiceDeps{Users: w.directory, Tokens: w.directory, Usage: deps.Usage, Routes: deps.Routes, Clock: func() time.Time { return w.now }, ModelLister: provider.NewMock()})
			srv := New(deps)

			req := httptest.NewRequest(http.MethodGet, "/api/portal/model-speech-language?name=tts-union", nil)
			req.Header.Set("Authorization", "Bearer "+voicesOtherSecret)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
			}
			if code := errorBodyOf(t, rec); code != speechLanguageFailCode {
				t.Fatalf("error code = %q, want %s; body = %s", code, speechLanguageFailCode, rec.Body.String())
			}
			if !findLogRecord(buf.Snapshot(), "ERROR", "portal speech language lookup failed") {
				t.Fatalf("the failure was not logged; records: %+v", buf.Snapshot())
			}
		})
	}
}

// Auth is the portal's own: gateway:use, like the other model gates, and GET only.
func TestHandlePortalModelSpeechLanguageAuth(t *testing.T) {
	w := newVoicesWorld(t)

	t.Run("no credentials", func(t *testing.T) {
		rec := w.getVoices(t, "/api/portal/model-speech-language", "name=tts-union", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a token without gateway:use", func(t *testing.T) {
		rec := w.askSpeechLanguage(t, voicesNoUseSecret, "tts-union")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("only GET", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req := httptest.NewRequest(method, "/api/portal/model-speech-language?name=tts-union", nil)
			req.Header.Set("Authorization", "Bearer "+voicesOtherSecret)
			rec := httptest.NewRecorder()
			w.srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s: status = %d, want 405; body = %s", method, rec.Code, rec.Body.String())
			}
			if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
				t.Fatalf("%s: Allow = %q, want GET", method, allow)
			}
		}
	})
}

// The browser path: a real login and its session cookie reach the route, and the answer is
// the user's own.
func TestHandlePortalModelSpeechLanguageAcceptsARealSessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_lang_cookie", "lang-cookie@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "lang-cookie@example.test", "password-1")

	ctx := context.Background()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	rs := srv.Routes
	for _, step := range []struct {
		what string
		err  error
	}{
		{"server", rs.CreateAIServer(ctx, routing.AIServer{ID: "srv-l", Name: "Server l", Domain: "l.test", Provider: routing.ProviderAudioCpp, Endpoint: "http://l.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now})},
		{"application", rs.CreateApplication(ctx, routing.Application{ID: "app-l", ServerID: "srv-l", Type: routing.ProviderAudioCpp, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now})},
		{"mapping", rs.CreateMapping(ctx, routing.ModelMapping{ID: "map-l", ApplicationID: "app-l", GatewayModelName: "tts-lang-cookie", AppModelName: "tts-lang-cookie", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now})},
		{"verdict", rs.UpsertMappingCapabilities(ctx, "map-l", []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}})},
		{"language", rs.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "map-l", CacheType: routing.ModelMappingCacheSpeechLanguage, ScopeKey: "", Payload: routing.SpeechLanguagePayload(true, "zh"), UpdatedAt: now})},
	} {
		if step.err != nil {
			t.Fatalf("seed %s: %v", step.what, step.err)
		}
	}

	rec := chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/model-speech-language?name=tts-lang-cookie", "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (body = %s)", err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || len(body) != 2 || body["accepts"] != true || body["default"] != "zh" {
		t.Fatalf("%d %s, want 200 %s", rec.Code, rec.Body.String(), speechLanguageZh)
	}
}

// A server with no routing store or no portal can reach nothing: a closed gate, not a
// panic and not an error. A blank name is one too (the handler answers it with a 400
// first).
func TestPortalSpeechLanguageWithoutStoresIsClosed(t *testing.T) {
	accepts, def, err := (&Server{}).portalSpeechLanguage(context.Background(), auth.Token{UserID: "usr_dev"}, "tts-union")
	if err != nil || accepts || def != "" {
		t.Fatalf("portalSpeechLanguage = (%v, %q, %v), want (false, \"\", nil)", accepts, def, err)
	}
	w := newVoicesWorld(t)
	accepts, def, err = w.srv.portalSpeechLanguage(context.Background(), auth.Token{UserID: "usr_other"}, "  ")
	if err != nil || accepts || def != "" {
		t.Fatalf("a blank name: portalSpeechLanguage = (%v, %q, %v), want (false, \"\", nil)", accepts, def, err)
	}
}
