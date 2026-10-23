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
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The portal voices route reuses the voices world (voices_handler_test.go): the
// same mappings, verdicts and cached voices the public endpoint is tested over,
// so each case here differs from its public twin in the VISIBILITY gate alone.
// usr_other (voicesOtherSecret) is the plain user: no admin scope, and the one
// user provisioned into the restricted resource group. usr_dev (voicesDevSecret)
// carries the admin scope, which bypasses hidden/locked but not provisioning.

// portalVoices asks GET /api/portal/model-voices?name=<name> as the bearer secret
// and returns the decoded list after asserting the response contract: 200 and
// exactly one key, "voices", a JSON array that is never null.
func (w *voicesWorld) portalVoices(t *testing.T, secret, name string) []string {
	t.Helper()
	rec := w.getVoices(t, "/api/portal/model-voices", "name="+url.QueryEscape(name), map[string]string{"Authorization": "Bearer " + secret})
	return decodePortalVoices(t, rec)
}

func decodePortalVoices(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, rec.Body.String())
	}
	if len(fields) != 1 || fields["voices"] == nil {
		t.Fatalf("body = %s, want exactly the key voices", rec.Body.String())
	}
	if string(fields["voices"]) == "null" {
		t.Fatalf("voices is null, want an array (an empty answer is [])")
	}
	var voices []string
	if err := json.Unmarshal(fields["voices"], &voices); err != nil {
		t.Fatalf("voices is not an array of strings: %v (%s)", err, fields["voices"])
	}
	return voices
}

// A visible model answers the deduped, sorted UNION of the voices cached for its
// active mappings, in the documented shape: a voice two servers share appears
// once, and one only a single server has is kept (an intersection, as the image
// options use, would drop it).
func TestPortalModelVoicesUnionsTheMappingsOfOneName(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-union"), []string{"alba", "bella", "cora"})
}

// A group name is the union of its visible members' voices; a member that is not
// a tts model (asr-member, with a stale voices row) adds nothing.
func TestPortalModelVoicesExpandsAGroupToItsMembers(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-group"), []string{"one-a", "shared", "two-b"})
}

// A clone model that ships named presets offers them in the portal picker too
// (#301), through the same union core as the public endpoint; a group takes a
// preset clone member's presets beside its plain members' voices, and a
// reference-only or never-fetched clone member adds nothing.
func TestPortalModelVoicesOffersAPresetCloneModelsVoices(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-clone-presets"), []string{"Frank", "Thorsten"})
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "grp-clones"), []string{"Frank", "Thorsten", "one-a", "shared"})
}

// A missing, empty or blank ?name= is a 400 model.name_required, because it is a
// client bug that an empty list would hide.
func TestPortalModelVoicesRequiresAName(t *testing.T) {
	w := newVoicesWorld(t)
	for _, target := range []string{"", "name=", "name=%20%20", "model=tts-union"} {
		rec := w.getVoices(t, "/api/portal/model-voices", target, map[string]string{"Authorization": "Bearer " + voicesOtherSecret})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("?%s: status = %d, want 400; body = %s", target, rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "model.name_required" {
			t.Fatalf("?%s: error code = %q, want model.name_required", target, code)
		}
	}
}

// Every "nothing to show" case is the SAME 200 with an empty list, so the answer
// cannot be used to tell an unknown name from a hidden, locked, restricted,
// non-speech, preset-less clone or not-yet-cached one. Where a row could exist the
// world gave the mapping one, so only the gate keeps it out. Each case is asked as the user
// the gate is about: the plain user for the portal's hidden/locked rules (the
// admin bypasses them), the unprovisioned admin for the restricted server.
func TestPortalModelVoicesEveryNothingToShowCaseIsAnEmpty200(t *testing.T) {
	w := newVoicesWorld(t)

	for name, c := range map[string]struct{ secret, model string }{
		"unknown name":                   {voicesOtherSecret, "no-such-model"},
		"not a speech model":             {voicesOtherSecret, "asr-model"},
		"a reference-only clone model":   {voicesOtherSecret, "tts-clone"},
		"a clone model, nothing cached":  {voicesOtherSecret, "tts-clone-uncached"},
		"no verdict yet":                 {voicesOtherSecret, "tts-unprobed"},
		"nothing cached":                 {voicesOtherSecret, "tts-uncached"},
		"cached, and the model has none": {voicesOtherSecret, "tts-emptycache"},
		"an app that does not speak":     {voicesOtherSecret, "tts-textapp"},
		"a hidden model":                 {voicesOtherSecret, "tts-hidden"},
		"a locked model":                 {voicesOtherSecret, "tts-locked"},
		"a locked group":                 {voicesOtherSecret, "grp-locked"},
		"restricted to another user":     {voicesDevSecret, "tts-restricted"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := w.getVoices(t, "/api/portal/model-voices", "name="+url.QueryEscape(c.model), map[string]string{"Authorization": "Bearer " + c.secret})
			if got := rec.Body.String(); rec.Code != http.StatusOK || got != "{\"voices\":[]}\n" {
				t.Fatalf("status = %d, body = %q, want 200 {\"voices\":[]}", rec.Code, got)
			}
		})
	}
}

// THE portal gate, where it differs from the public endpoint's: a hidden or
// locked model asked for BY NAME is invisible to a non-admin here even though the
// public endpoint offers a hidden model (still directly requestable). An admin
// bypasses the portal suppression, as it bypasses it everywhere else in the
// portal.
func TestPortalModelVoicesHidesHiddenAndLockedModelsFromANonAdmin(t *testing.T) {
	w := newVoicesWorld(t)

	for _, model := range []string{"tts-hidden", "tts-locked"} {
		requireVoices(t, w.portalVoices(t, voicesOtherSecret, model), nil)
	}
	requireVoices(t, w.portalVoices(t, voicesDevSecret, "tts-hidden"), []string{"hidden-voice"})
	requireVoices(t, w.portalVoices(t, voicesDevSecret, "tts-locked"), []string{"locked-voice"})

	// The public endpoint answers the same non-admin differently for the same
	// hidden model: it is routing-consistent, this route is portal-consistent.
	requireVoices(t, w.voicesFor(t, voicesOtherSecret, "tts-hidden"), []string{"hidden-voice"})
}

// The hidden/locked gate applies to the name that was ASKED ABOUT, and a member's
// own visibility does not narrow a visible group: a hidden or locked model is
// still a full member of it ("locked" means reachable through a group, not
// directly), so a non-admin gets its voices THROUGH the group, which is the main
// locked-via-group setup, while the model's own name stays suppressed. A locked
// group is suppressed whole under its own name. An admin bypasses the gate.
func TestPortalModelVoicesGroupVisibility(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "grp-of-locked"), []string{"locked-voice"})
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "grp-open"), []string{"hidden-voice", "locked-voice", "one-a", "shared"})
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-locked"), nil)
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-hidden"), nil)
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "grp-locked"), nil)

	requireVoices(t, w.portalVoices(t, voicesDevSecret, "grp-of-locked"), []string{"locked-voice"})
	requireVoices(t, w.portalVoices(t, voicesDevSecret, "grp-open"), []string{"hidden-voice", "locked-voice", "one-a", "shared"})
	requireVoices(t, w.portalVoices(t, voicesDevSecret, "grp-locked"), []string{"one-a", "shared"})
}

// Resource-group provisioning applies: a server restricted to another user's
// group is invisible (the same empty 200 as an unknown name) and visible to the
// provisioned user, and a group is cut member by member (provisioning, not a
// member's own visibility, is what bounds a group's members).
func TestPortalModelVoicesFollowsProvisioning(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.portalVoices(t, voicesDevSecret, "tts-restricted"), nil)
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-restricted"), []string{"secret-voice"})

	requireVoices(t, w.portalVoices(t, voicesDevSecret, "grp-restricted"), []string{"one-a", "shared"})
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "grp-restricted"), []string{"one-a", "secret-voice", "shared"})
}

// The portal route shares the public endpoint's union core, so the READ-time
// verdict gate and the undecodable-row rule are the same: a mapping flipped to
// clone after its voices were cached keeps offering a non-empty list (a preset
// clone) and stops when the list is empty (a reference-only one), and a corrupt
// row costs only its own mapping.
func TestPortalModelVoicesSharesTheUnionCore(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	w := newVoicesWorld(t)
	ctx := context.Background()

	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-flip"), []string{"flip-voice"})
	if err := w.routes.UpsertMappingCapabilities(ctx, "map-flip", []routing.CapabilityRow{{Capability: routing.CapabilitySpeechClone, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: w.now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-flip"), []string{"flip-voice"})
	if err := w.routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "map-flip", CacheType: routing.ModelMappingCacheVoices, ScopeKey: "", Payload: `[]`, UpdatedAt: w.now}); err != nil {
		t.Fatalf("UpsertModelMappingCache: %v", err)
	}
	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-flip"), nil)

	requireVoices(t, w.portalVoices(t, voicesOtherSecret, "tts-mix"), []string{"ok-voice"})
	if !findLogRecord(buf.Snapshot(), "WARN", "voices cache row") {
		t.Fatalf("no warning about the undecodable voices row; records: %+v", buf.Snapshot())
	}
}

// The route reads the cache and never touches an upstream.
func TestPortalModelVoicesNeverCallsAnUpstream(t *testing.T) {
	w := newVoicesWorld(t)

	for _, model := range []string{"tts-union", "tts-group", "tts-uncached", "tts-restricted", "no-such-model"} {
		w.portalVoices(t, voicesOtherSecret, model)
	}
	if got := w.upstreamCalls.Load(); got != 0 {
		t.Fatalf("the upstream received %d requests, want 0", got)
	}
}

// Auth is the portal's own: gateway:use, like model-servers and model-image-options.
func TestPortalModelVoicesAuth(t *testing.T) {
	w := newVoicesWorld(t)

	t.Run("no credentials", func(t *testing.T) {
		rec := w.getVoices(t, "/api/portal/model-voices", "name=tts-union", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a token without gateway:use", func(t *testing.T) {
		rec := w.getVoices(t, "/api/portal/model-voices", "name=tts-union", map[string]string{"Authorization": "Bearer " + voicesNoUseSecret})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("only GET", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req := httptest.NewRequest(method, "/api/portal/model-voices?name=tts-union", nil)
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

// The browser path: a real login and its session cookie reach the route (the
// public /v1/audio/voices refuses the very same cookie, which is why this route
// exists), and the answer is the user's own: a mapping the cache holds voices for.
func TestPortalModelVoicesAcceptsARealSessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_voices_cookie", "voices-cookie@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "voices-cookie@example.test", "password-1")

	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	rs := srv.Routes
	for _, step := range []struct {
		what string
		err  error
	}{
		{"server", rs.CreateAIServer(ctx, routing.AIServer{ID: "srv-c", Name: "Server c", Domain: "c.test", Provider: routing.ProviderAudioCpp, Endpoint: "http://c.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now})},
		{"application", rs.CreateApplication(ctx, routing.Application{ID: "app-c", ServerID: "srv-c", Type: routing.ProviderAudioCpp, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now})},
		{"mapping", rs.CreateMapping(ctx, routing.ModelMapping{ID: "map-c", ApplicationID: "app-c", GatewayModelName: "tts-cookie", AppModelName: "tts-cookie", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now})},
		{"verdict", rs.UpsertMappingCapabilities(ctx, "map-c", []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}})},
		{"voices", rs.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: "map-c", CacheType: routing.ModelMappingCacheVoices, ScopeKey: "", Payload: `["zed","amy"]`, UpdatedAt: now})},
	} {
		if step.err != nil {
			t.Fatalf("seed %s: %v", step.what, step.err)
		}
	}

	rec := chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/model-voices?name=tts-cookie", "")
	requireVoices(t, decodePortalVoices(t, rec), []string{"amy", "zed"})
}

// A store failure is a 500 with its own code, never the empty 200 of "nothing to
// show": the portal's hidden/locked suppression fails closed, and so do the two
// reads the union makes.
func TestPortalModelVoicesAStoreFailureIsA500NotAnEmptyAnswer(t *testing.T) {
	for name, failing := range map[string]func(routing.Store) routing.Store{
		"the model settings": func(s routing.Store) routing.Store {
			return settingsFailStore{Store: s, err: errors.New("settings table unavailable")}
		},
		"the voices cache": func(s routing.Store) routing.Store { return failingVoicesStore{Store: s, failCache: true} },
		"the verdicts":     func(s routing.Store) routing.Store { return failingVoicesStore{Store: s, failVerdicts: true} },
	} {
		t.Run(name, func(t *testing.T) {
			buf := withCapturedSlogAtTheDefaultLevel(t)
			w := newVoicesWorld(t)
			deps := w.deps
			deps.Routes = failing(w.routes)
			// The portal reads the settings through ITS OWN store handle, so it is
			// rebuilt over the failing store too.
			deps.Portal = portal.NewService(portal.ServiceDeps{Users: w.directory, Tokens: w.directory, Usage: deps.Usage, Routes: deps.Routes, Clock: func() time.Time { return w.now }, ModelLister: provider.NewMock()})
			srv := New(deps)

			req := httptest.NewRequest(http.MethodGet, "/api/portal/model-voices?name=tts-union", nil)
			req.Header.Set("Authorization", "Bearer "+voicesOtherSecret)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
			}
			if code := errorBodyOf(t, rec); code != "model.voices_failed" {
				t.Fatalf("error code = %q, want model.voices_failed; body = %s", code, rec.Body.String())
			}
			if !findLogRecord(buf.Snapshot(), "ERROR", "portal model voices lookup failed") {
				t.Fatalf("the failure was not logged; records: %+v", buf.Snapshot())
			}
		})
	}
}
