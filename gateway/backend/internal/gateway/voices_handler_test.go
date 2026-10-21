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
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// voicesDevSecret is usr_dev's bearer (an admin: provisioning has no role
	// bypass, so admin does not widen what it can route to).
	voicesDevSecret = "voices-dev-secret"
	// voicesOtherSecret is usr_other's bearer, the one user provisioned into
	// the restricted resource group.
	voicesOtherSecret = "voices-other-secret"
	// voicesNoUseSecret is a token whose only scope is neither gateway:use nor
	// llm:invoke, the two scopes the endpoint admits.
	voicesNoUseSecret = "voices-nouse-secret"
	voicesLoopback    = "voices-loopback"
)

// voicesMapping describes one mapping of the voices world: its model, the
// verdicts the speech gate reads, and what the voices cache holds for it.
type voicesMapping struct {
	id      string
	app     string // "a", "b", "c", "r" or "text"
	gateway string
	// speech and clone are the (speech, X) and (speech_clone, X) verdicts; ""
	// writes no row (unknown).
	speech string
	clone  string
	// disabled makes the mapping inactive.
	disabled bool
	// voices is the cached list; nil writes no cache row at all (never fetched),
	// an empty non-nil slice writes "[]" (fetched, and the model has none).
	voices []string
	// payload, when set, is stored verbatim instead of the JSON of voices.
	payload string
}

// voicesWorld is a server over several audio.cpp applications, each reachable
// through a counting upstream that answers 500 to anything: the endpoint reads the
// cache, so a single request arriving there is a failure of its own.
type voicesWorld struct {
	srv *Server
	// deps is what srv was built from, so a test can build a sibling server over
	// a different routing.Store (a failing one) with everything else the same.
	deps          ServerDeps
	routes        *routing.MemoryStore
	now           time.Time
	upstreamCalls *atomic.Int64
}

// newVoicesWorld builds the world every test below queries by model name:
//
//   - servers/apps a, b and c are unrestricted audio.cpp applications; r sits in
//     a resource group provisioned ONLY for usr_other; "text" declares only the
//     text flavor;
//   - the mappings seed one model name per behaviour under test, so a test names
//     its case instead of building it.
func newVoicesWorld(t *testing.T) *voicesWorld {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	up, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", upstream.URL, err)
	}
	port, err := strconv.Atoi(up.Port())
	if err != nil {
		t.Fatalf("upstream port %q: %v", up.Port(), err)
	}

	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	directory.AddUser(store.User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	for _, tok := range []struct{ id, user, scopes, secret string }{
		{"tok_dev", "usr_dev", `["gateway:use","admin"]`, voicesDevSecret},
		{"tok_other", "usr_other", `["gateway:use"]`, voicesOtherSecret},
		{"tok_nouse", "usr_dev", `["usage:read"]`, voicesNoUseSecret},
	} {
		if err := directory.CreatePlainToken(ctx, store.TokenRecord{ID: tok.id, UserID: tok.user, Name: tok.id, Status: store.TokenStatusActive, Scopes: tok.scopes, CreatedAt: now, UpdatedAt: now}, tok.secret); err != nil {
			t.Fatalf("CreatePlainToken %s: %v", tok.id, err)
		}
	}

	routes := routing.NewMemoryStore()
	w := &voicesWorld{routes: routes, now: now, upstreamCalls: &calls}
	for _, app := range []struct {
		name    string
		flavors []string
	}{
		{"a", []string{routing.APIFlavorOpenAIAudio}},
		{"b", []string{routing.APIFlavorOpenAIAudio}},
		{"c", []string{routing.APIFlavorOpenAIAudio}},
		{"r", []string{routing.APIFlavorOpenAIAudio}},
		{"text", []string{routing.APIFlavorOpenAI}},
	} {
		if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-" + app.name, Name: "Server " + app.name, Domain: up.Hostname(), Provider: routing.ProviderAudioCpp, Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", app.name, err)
		}
		if err := routes.CreateApplication(ctx, routing.Application{ID: "app-" + app.name, ServerID: "srv-" + app.name, Type: routing.ProviderAudioCpp, Port: port, Scheme: up.Scheme, APIFlavors: app.flavors, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", app.name, err)
		}
	}
	if err := routes.CreateResourceGroup(ctx, routing.ResourceGroup{ID: "rgrp_voices", Name: "RG Voices", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}
	if err := routes.SetResourceGroupServer(ctx, "rgrp_voices", "srv-r"); err != nil {
		t.Fatalf("SetResourceGroupServer: %v", err)
	}
	if err := routes.SetResourceGroupProvision(ctx, "rgrp_voices", routing.ProvisionKindUser, "usr_other"); err != nil {
		t.Fatalf("SetResourceGroupProvision: %v", err)
	}

	const (
		yes = routing.CapabilityYes
		no  = routing.CapabilityNo
	)
	for _, m := range []voicesMapping{
		// Two ACTIVE same-name mappings on different servers: their voices union;
		// "bella" is on both and must appear once. The third is inactive, so its
		// voice must not.
		{id: "map-u1", app: "a", gateway: "tts-union", speech: yes, voices: []string{"bella", "alba"}},
		{id: "map-u2", app: "b", gateway: "tts-union", speech: yes, voices: []string{"bella", "cora"}},
		{id: "map-u3", app: "c", gateway: "tts-union", speech: yes, voices: []string{"zed"}, disabled: true},

		// Group members. asr-member has a stale voices row but is not a tts model,
		// so the group's union must leave it out.
		{id: "map-g1", app: "a", gateway: "tts-one", speech: yes, voices: []string{"one-a", "shared"}},
		{id: "map-g2", app: "b", gateway: "tts-two", speech: yes, voices: []string{"two-b", "shared"}},
		{id: "map-g3", app: "a", gateway: "asr-member", speech: no, voices: []string{"asr-leak"}},

		// "Nothing to show" cases. Each carries a voices row where a row could
		// exist, so only the gate keeps it out of the answer.
		{id: "map-asr", app: "a", gateway: "asr-model", speech: no, voices: []string{"asr-leak"}},
		{id: "map-clone", app: "a", gateway: "tts-clone", speech: yes, clone: yes, voices: []string{"clone-leak"}},
		{id: "map-unprobed", app: "a", gateway: "tts-unprobed", voices: []string{"unprobed-leak"}},
		{id: "map-uncached", app: "a", gateway: "tts-uncached", speech: yes},
		{id: "map-emptycache", app: "a", gateway: "tts-emptycache", speech: yes, voices: []string{}},
		{id: "map-textapp", app: "text", gateway: "tts-textapp", speech: yes, voices: []string{"textapp-leak"}},

		// A clone verdict that is merely unknown does not exclude: only an
		// affirmative one does (the writers' own rule).
		{id: "map-cloneno", app: "a", gateway: "tts-cloneno", speech: yes, clone: no, voices: []string{"plain"}},

		// Flipped after caching, by the flip tests.
		{id: "map-flip", app: "a", gateway: "tts-flip", speech: yes, voices: []string{"flip-voice"}},

		// Visibility. The restricted model is on the provisioned-only server.
		{id: "map-restricted", app: "r", gateway: "tts-restricted", speech: yes, voices: []string{"secret-voice"}},
		{id: "map-locked", app: "a", gateway: "tts-locked", speech: yes, voices: []string{"locked-voice"}},
		{id: "map-hidden", app: "a", gateway: "tts-hidden", speech: yes, voices: []string{"hidden-voice"}},

		// A corrupt payload on one of two mappings must not blank the other.
		{id: "map-mix-ok", app: "a", gateway: "tts-mix", speech: yes, voices: []string{"ok-voice"}},
		{id: "map-mix-bad", app: "b", gateway: "tts-mix", speech: yes, payload: `not json`},
	} {
		w.addMapping(t, m)
	}

	for _, setting := range []routing.ModelSetting{
		{GatewayModelName: "tts-locked", Visibility: "locked", CreatedAt: now, UpdatedAt: now},
		{GatewayModelName: "tts-hidden", Visibility: "hidden", CreatedAt: now, UpdatedAt: now},
		{GatewayModelName: "grp-locked", Visibility: "locked", CreatedAt: now, UpdatedAt: now},
	} {
		if err := routes.UpsertModelSetting(ctx, setting); err != nil {
			t.Fatalf("UpsertModelSetting %s: %v", setting.GatewayModelName, err)
		}
	}
	for _, g := range []struct {
		id, name string
		members  []string
	}{
		{"grp_voices", "tts-group", []string{"tts-one", "tts-two", "asr-member"}},
		{"grp_locked_member", "grp-of-locked", []string{"tts-locked"}},
		{"grp_locked", "grp-locked", []string{"tts-one"}},
		{"grp_restricted", "grp-restricted", []string{"tts-one", "tts-restricted"}},
	} {
		if err := routes.CreateModelGroup(ctx, routing.ModelGroup{ID: g.id, GatewayModelName: g.name, DisplayName: g.name, Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateModelGroup %s: %v", g.name, err)
		}
		members := make([]routing.GroupMember, len(g.members))
		for i, name := range g.members {
			members[i] = routing.GroupMember{MemberGatewayName: name, Priority: i}
		}
		if err := routes.SetGroupMembers(ctx, g.id, members); err != nil {
			t.Fatalf("SetGroupMembers %s: %v", g.name, err)
		}
	}
	groups := NewGroupRegistry(routes)
	if err := groups.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}

	recorder := usage.NewRecorder()
	w.deps = ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           provider.NewMock(),
		Routes:             routes,
		Groups:             groups,
		Portal:             portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routes, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
		InternalAuthSecret: voicesLoopback,
		Users:              directory,
	}
	w.srv = New(w.deps)
	return w
}

// addMapping seeds one mapping of the voices world: its verdict rows and its
// voices cache row.
func (w *voicesWorld) addMapping(t *testing.T, m voicesMapping) {
	t.Helper()
	ctx := context.Background()
	status := routing.ServerStatusActive
	if m.disabled {
		status = routing.ServerStatusDisabled
	}
	if err := w.routes.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-" + m.app, GatewayModelName: m.gateway, AppModelName: "up_" + m.id, Status: status, CreatedAt: w.now, UpdatedAt: w.now}); err != nil {
		t.Fatalf("CreateMapping %s: %v", m.id, err)
	}
	var rows []routing.CapabilityRow
	if m.speech != "" {
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilitySpeech, Verdict: m.speech, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: w.now})
	}
	if m.clone != "" {
		rows = append(rows, routing.CapabilityRow{Capability: routing.CapabilitySpeechClone, Verdict: m.clone, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: w.now})
	}
	if err := w.routes.UpsertMappingCapabilities(ctx, m.id, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", m.id, err)
	}
	payload := m.payload
	if payload == "" {
		if m.voices == nil {
			return
		}
		raw, err := json.Marshal(m.voices)
		if err != nil {
			t.Fatalf("marshal voices %s: %v", m.id, err)
		}
		payload = string(raw)
	}
	if err := w.routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: m.id, CacheType: routing.ModelMappingCacheVoices, ScopeKey: "", Payload: payload, UpdatedAt: w.now}); err != nil {
		t.Fatalf("UpsertModelMappingCache %s: %v", m.id, err)
	}
}

// getVoices GETs path through the real mux with the given headers.
func (w *voicesWorld) getVoices(t *testing.T, path, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	target := path
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	return rec
}

// voicesFor asks for model's voices as the bearer secret and returns the decoded
// list after asserting the whole response contract: 200 and exactly the two keys
// "model" (the requested name echoed) and "voices" (a JSON array, never null).
func (w *voicesWorld) voicesFor(t *testing.T, secret, model string) []string {
	t.Helper()
	rec := w.getVoices(t, "/v1/audio/voices", "model="+url.QueryEscape(model), map[string]string{"Authorization": "Bearer " + secret})
	return decodeVoicesResponse(t, rec, model)
}

func decodeVoicesResponse(t *testing.T, rec *httptest.ResponseRecorder, model string) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, rec.Body.String())
	}
	if len(fields) != 2 || fields["model"] == nil || fields["voices"] == nil {
		t.Fatalf("body = %s, want exactly the keys model and voices", rec.Body.String())
	}
	var echoed string
	if err := json.Unmarshal(fields["model"], &echoed); err != nil || echoed != model {
		t.Fatalf("model = %s, want %q echoed", fields["model"], model)
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

func requireVoices(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("voices = %v, want %v", got, want)
	}
}

// The endpoint answers the deduped, sorted UNION of the voices cached for the
// model name's active mappings, in the documented shape.
func TestAudioVoicesUnionsTheActiveMappingsOfOneName(t *testing.T) {
	w := newVoicesWorld(t)

	got := w.voicesFor(t, voicesDevSecret, "tts-union")

	// map-u3 is inactive and caches "zed"; map-u1 and map-u2 both cache "bella".
	requireVoices(t, got, []string{"alba", "bella", "cora"})
}

// The /openai alias answers exactly as the bare path.
func TestAudioVoicesAnswersOnTheOpenAIAlias(t *testing.T) {
	w := newVoicesWorld(t)

	rec := w.getVoices(t, "/openai/v1/audio/voices", "model=tts-union", map[string]string{"Authorization": "Bearer " + voicesDevSecret})

	requireVoices(t, decodeVoicesResponse(t, rec, "tts-union"), []string{"alba", "bella", "cora"})
}

// A GROUP name is accepted and expanded: the union spans its members' mappings,
// and a member that is not a tts model contributes nothing.
func TestAudioVoicesExpandsAGroupToItsMembers(t *testing.T) {
	w := newVoicesWorld(t)

	got := w.voicesFor(t, voicesDevSecret, "tts-group")

	requireVoices(t, got, []string{"one-a", "shared", "two-b"})
}

// A missing, empty or blank model is the one 400: a client bug must not read as a
// normal-looking empty list.
func TestAudioVoicesRequiresAModel(t *testing.T) {
	w := newVoicesWorld(t)
	bearer := map[string]string{"Authorization": "Bearer " + voicesDevSecret}

	for name, query := range map[string]string{
		"absent":    "",
		"empty":     "model=",
		"blank":     "model=%20%20",
		"wrong key": "name=tts-union",
	} {
		t.Run(name, func(t *testing.T) {
			rec := w.getVoices(t, "/v1/audio/voices", query, bearer)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != "model.name_required" {
				t.Fatalf("error code = %q, want model.name_required; body = %s", body.Error.Code, rec.Body.String())
			}
		})
	}
}

// Every "nothing to show" case is the SAME 200 with an empty list, so the answer
// cannot be used to tell an unknown name from a hidden, non-speech, clone or
// not-yet-cached one. Where a row could exist the world gave the mapping one, so
// only the gate keeps it out.
func TestAudioVoicesEveryNothingToShowCaseIsAnEmpty200(t *testing.T) {
	w := newVoicesWorld(t)

	for name, model := range map[string]string{
		"unknown name":                   "no-such-model",
		"not a speech model":             "asr-model",
		"a clone model":                  "tts-clone",
		"no verdict yet":                 "tts-unprobed",
		"nothing cached":                 "tts-uncached",
		"cached, and the model has none": "tts-emptycache",
		"an app that does not speak":     "tts-textapp",
		"restricted to another user":     "tts-restricted",
		"a locked group":                 "grp-locked",
	} {
		t.Run(name, func(t *testing.T) {
			requireVoices(t, w.voicesFor(t, voicesDevSecret, model), nil)
		})
	}
}

// An unknown clone verdict does not exclude a model: only an affirmative
// (speech_clone, yes) does.
func TestAudioVoicesAnExplicitNoCloneVerdictStillServes(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-cloneno"), []string{"plain"})
}

// The verdict gate runs at READ time on the CURRENT verdict, not on what was true
// when the row was written: a mapping that flips to clone, or to not-speech, after
// its voices were cached stops offering them, and a flip back restores them.
func TestAudioVoicesReadSideVerdictGate(t *testing.T) {
	w := newVoicesWorld(t)
	ctx := context.Background()
	set := func(capability, verdict string) {
		t.Helper()
		if err := w.routes.UpsertMappingCapabilities(ctx, "map-flip", []routing.CapabilityRow{{Capability: capability, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: w.now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities: %v", err)
		}
	}

	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-flip"), []string{"flip-voice"})

	set(routing.CapabilitySpeechClone, routing.CapabilityYes)
	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-flip"), nil)

	set(routing.CapabilitySpeechClone, routing.CapabilityNo)
	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-flip"), []string{"flip-voice"})

	set(routing.CapabilitySpeech, routing.CapabilityNo)
	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-flip"), nil)
}

// Visibility is ROUTING-consistent: exactly the mappings a dispatch for this token
// could reach. A server restricted by resource-group provisioning is invisible to
// a token whose user is not provisioned (the same empty 200 as an unknown name)
// and visible to one that is.
func TestAudioVoicesFollowsProvisioning(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-restricted"), nil)
	requireVoices(t, w.voicesFor(t, voicesOtherSecret, "tts-restricted"), []string{"secret-voice"})

	// A group is cut the same way, member by member: the unprovisioned user keeps
	// the open member's voices and loses the restricted one's.
	requireVoices(t, w.voicesFor(t, voicesDevSecret, "grp-restricted"), []string{"one-a", "shared"})
	requireVoices(t, w.voicesFor(t, voicesOtherSecret, "grp-restricted"), []string{"one-a", "secret-voice", "shared"})
}

// "locked" means group-only: a direct request for the model is refused, so the
// voices endpoint offers none for the bare name, yet the same model's voices are
// in its group's union (a group dispatch may route to it). "hidden" only delists a
// model, which stays directly requestable, so its voices ARE offered (the portal
// route, which hides it, is a different gate over the same union).
func TestAudioVoicesFollowsTheLockedAndHiddenRules(t *testing.T) {
	w := newVoicesWorld(t)

	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-locked"), nil)
	requireVoices(t, w.voicesFor(t, voicesDevSecret, "grp-of-locked"), []string{"locked-voice"})
	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-hidden"), []string{"hidden-voice"})
}

// A row that does not decode costs only its own mapping: the other mappings'
// voices are still offered, and the damage is visible in the log.
func TestAudioVoicesSkipsAnUndecodableRow(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	w := newVoicesWorld(t)

	requireVoices(t, w.voicesFor(t, voicesDevSecret, "tts-mix"), []string{"ok-voice"})

	if !findLogRecord(buf.Snapshot(), "WARN", "voices cache row") {
		t.Fatalf("no warning about the undecodable voices row; records: %+v", buf.Snapshot())
	}
}

// The endpoint reads the cache and never touches an upstream: asking for a
// model's voices must not load it, whatever the cache holds.
func TestAudioVoicesNeverCallsAnUpstream(t *testing.T) {
	w := newVoicesWorld(t)

	for _, model := range []string{"tts-union", "tts-group", "tts-uncached", "tts-restricted", "no-such-model"} {
		w.voicesFor(t, voicesDevSecret, model)
	}

	if got := w.upstreamCalls.Load(); got != 0 {
		t.Fatalf("the upstream received %d requests, want 0", got)
	}
}

// Auth is the speech endpoints' own: a bearer with gateway:use (or llm:invoke) or
// the internal loopback pair, and nothing else.
func TestAudioVoicesAuth(t *testing.T) {
	w := newVoicesWorld(t)

	t.Run("no credentials", func(t *testing.T) {
		rec := w.getVoices(t, "/v1/audio/voices", "model=tts-union", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a bearer without an admitted scope", func(t *testing.T) {
		rec := w.getVoices(t, "/v1/audio/voices", "model=tts-union", map[string]string{"Authorization": "Bearer " + voicesNoUseSecret})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("the internal loopback pair", func(t *testing.T) {
		rec := w.getVoices(t, "/v1/audio/voices", "model=tts-union", map[string]string{
			internalAuthHeaderName: voicesLoopback,
			internalUserHeaderName: "usr_dev",
		})
		requireVoices(t, decodeVoicesResponse(t, rec, "tts-union"), []string{"alba", "bella", "cora"})
	})
	t.Run("a wrong loopback secret is not a credential", func(t *testing.T) {
		rec := w.getVoices(t, "/v1/audio/voices", "model=tts-union", map[string]string{
			internalAuthHeaderName: "wrong",
			internalUserHeaderName: "usr_dev",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("only GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/voices?model=tts-union", nil)
		req.Header.Set("Authorization", "Bearer "+voicesDevSecret)
		rec := httptest.NewRecorder()
		w.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// voicesForModel is the shared core Task 8's portal route reuses: it takes the
// token as a value, so the same union serves any principal.
func TestVoicesForModelIsTheRoutingConsistentUnion(t *testing.T) {
	w := newVoicesWorld(t)
	ctx := context.Background()
	dev := auth.Token{ID: "tok_dev", UserID: "usr_dev", Scopes: []string{scopeGatewayUse}}
	other := auth.Token{ID: "tok_other", UserID: "usr_other", Scopes: []string{scopeGatewayUse}}

	got, err := w.srv.voicesForModel(ctx, dev, "tts-union")
	if err != nil {
		t.Fatalf("voicesForModel: %v", err)
	}
	requireVoices(t, got, []string{"alba", "bella", "cora"})

	got, err = w.srv.voicesForModel(ctx, dev, "tts-restricted")
	if err != nil {
		t.Fatalf("voicesForModel: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("voicesForModel = %#v, want an empty non-nil slice", got)
	}
	got, err = w.srv.voicesForModel(ctx, other, "tts-restricted")
	if err != nil {
		t.Fatalf("voicesForModel: %v", err)
	}
	requireVoices(t, got, []string{"secret-voice"})
}

// cachedVoicesForMappings is the visibility-free half of the core, for a caller
// that has already decided which mappings its principal may see: it still applies
// the CURRENT tts verdict (a mapping the caller was given that is not plain tts
// contributes nothing), dedupes an id given twice, and answers an empty non-nil
// slice for no mappings at all.
func TestCachedVoicesForMappingsAppliesTheVerdictGateOverGivenIDs(t *testing.T) {
	w := newVoicesWorld(t)
	ctx := context.Background()

	got, err := w.srv.cachedVoicesForMappings(ctx, []string{"map-u1", "map-u2", "map-u1", "map-asr", "map-clone", "map-uncached", "map-nope"})
	if err != nil {
		t.Fatalf("cachedVoicesForMappings: %v", err)
	}
	requireVoices(t, got, []string{"alba", "bella", "cora"})

	got, err = w.srv.cachedVoicesForMappings(ctx, nil)
	if err != nil {
		t.Fatalf("cachedVoicesForMappings(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("cachedVoicesForMappings(nil) = %#v, want an empty non-nil slice", got)
	}
}

// failingVoicesStore fails the two reads the voices union makes, selectively, so
// a test can tell that either failure is a 500 and not an empty answer.
type failingVoicesStore struct {
	routing.Store
	failCache, failVerdicts bool
}

func (f failingVoicesStore) ModelMappingCacheForMappings(ctx context.Context, ids []string, cacheType string) (map[string][]routing.ModelMappingCacheRow, error) {
	if f.failCache {
		return nil, errors.New("cache store down")
	}
	return f.Store.ModelMappingCacheForMappings(ctx, ids, cacheType)
}

func (f failingVoicesStore) MappingCapabilitiesForMappings(ctx context.Context, ids []string) (map[string][]routing.CapabilityRow, error) {
	if f.failVerdicts {
		return nil, errors.New("verdict store down")
	}
	return f.Store.MappingCapabilitiesForMappings(ctx, ids)
}

// A store failure is a 500 with its own code, never the empty 200 of "nothing to
// show": a client must be able to tell "no voices" from "could not look".
func TestAudioVoicesAStoreFailureIsA500NotAnEmptyAnswer(t *testing.T) {
	for name, failing := range map[string]func(routing.Store) routing.Store{
		"the voices cache": func(s routing.Store) routing.Store { return failingVoicesStore{Store: s, failCache: true} },
		"the verdicts":     func(s routing.Store) routing.Store { return failingVoicesStore{Store: s, failVerdicts: true} },
	} {
		t.Run(name, func(t *testing.T) {
			buf := withCapturedSlogAtTheDefaultLevel(t)
			w := newVoicesWorld(t)
			deps := w.deps
			deps.Routes = failing(w.routes)
			srv := New(deps)

			req := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?model=tts-union", nil)
			req.Header.Set("Authorization", "Bearer "+voicesDevSecret)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "model.voices_failed" {
				t.Fatalf("body = %s, want error code model.voices_failed (%v)", rec.Body.String(), err)
			}
			if !findLogRecord(buf.Snapshot(), "ERROR", "audio voices lookup failed") {
				t.Fatalf("the failure was not logged; records: %+v", buf.Snapshot())
			}
		})
	}
}

// A server with no routing store has no resolver and can reach nothing: the union
// is empty, not a panic and not an error.
func TestVoicesForModelWithoutARoutingStoreIsEmpty(t *testing.T) {
	got, err := (&Server{}).voicesForModel(context.Background(), auth.Token{UserID: "usr_dev"}, "tts-union")
	if err != nil {
		t.Fatalf("voicesForModel: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("voicesForModel = %#v, want an empty non-nil slice", got)
	}
	got, err = (&Server{}).cachedVoicesForMappings(context.Background(), []string{"map-u1"})
	if err != nil {
		t.Fatalf("cachedVoicesForMappings: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("cachedVoicesForMappings = %#v, want an empty non-nil slice", got)
	}
}
