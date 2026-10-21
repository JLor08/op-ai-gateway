// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The voice -> server filter, end to end through the real speech handler (#251). The
// routing package pins the filter's rules; these tests pin that the handler feeds it the
// voice from the raw body and answers its one dedicated failure with a stable code.

func TestSniffSpeechVoice(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"a voice", `{"model":"m","input":"hi","voice":"alba"}`, "alba"},
		{"absent", `{"model":"m","input":"hi"}`, ""},
		{"null", `{"model":"m","input":"hi","voice":null}`, ""},
		{"empty", `{"model":"m","input":"hi","voice":""}`, ""},
		{"blank is none", `{"model":"m","input":"hi","voice":"   "}`, ""},
		// The name is relayed to the upstream byte for byte, so it is matched exactly:
		// surrounding whitespace is part of a (different) name, not noise to trim.
		{"kept exactly", `{"model":"m","input":"hi","voice":" alba "}`, " alba "},
		{"a number is not a voice name", `{"model":"m","input":"hi","voice":7}`, ""},
		{"an object (OpenAI custom voice) is not a name this filter knows", `{"model":"m","input":"hi","voice":{"id":"voice_123"}}`, ""},
		{"not JSON at all", `not json`, ""},
		{"an empty body", ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffSpeechVoice([]byte(tc.body)); got != tc.want {
				t.Fatalf("sniffSpeechVoice(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// The shape the handler hands the one preflight gate carries the voice, so the Request
// the resolver sees has RequiredVoice set from the body and nothing else.
func TestSpeechInferenceShapeCarriesTheVoice(t *testing.T) {
	shape := speechInferenceShape("tts-voice", "alba")
	if shape.requiredVoice != "alba" || shape.model != "tts-voice" || shape.apiFlavor != apiFlavorSpeech {
		t.Fatalf("shape = %+v, want voice alba on model tts-voice, flavor %q", shape, apiFlavorSpeech)
	}
	if len(shape.requiredCapabilities) != 1 || shape.requiredCapabilities[0] != routing.CapabilitySpeech {
		t.Fatalf("requiredCapabilities = %v, want [speech]: the voice is an addition to the capability gate, not a replacement", shape.requiredCapabilities)
	}
}

// The preflight carries the shape's voice onto the Request the resolver is given, and
// only the voice-bearing shape sets it.
func TestPreflightCarriesTheSpeechVoiceOntoTheRequest(t *testing.T) {
	s := &Server{Portal: &fakeOfferingPortal{}}
	for _, voice := range []string{"alba", ""} {
		pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), auth.Token{}, nil, speechInferenceShape("tts-voice", voice))
		if handled {
			t.Fatal("preflight refused the request")
		}
		if pf.Req.RequiredVoice != voice {
			t.Fatalf("Request.RequiredVoice = %q, want %q", pf.Req.RequiredVoice, voice)
		}
		if len(pf.Req.RequiredCapabilities) != 1 || pf.Req.RequiredCapabilities[0] != routing.CapabilitySpeech {
			t.Fatalf("Request.RequiredCapabilities = %v, want [speech]", pf.Req.RequiredCapabilities)
		}
	}
	// No other endpoint's shape names a voice.
	pf, handled := s.inferencePreflight(httptest.NewRecorder(), newInferenceRequest(t), auth.Token{}, nil, inferenceShape{apiFlavor: "openai_chat_completions", endpoint: endpointChat, model: "m"})
	if handled || pf.Req.RequiredVoice != "" {
		t.Fatalf("a chat preflight produced RequiredVoice %q (handled %v), want none", pf.Req.RequiredVoice, handled)
	}
}

// speechVoiceWorld is one speech gateway model, "tts-voice", on two audio.cpp servers
// (a and b, each a real HTTP upstream that records what it receives), plus a group
// "tts-group" over two members, one on each server.
type speechVoiceWorld struct {
	srv    *Server
	routes *routing.MemoryStore
	callsA <-chan speechUpstreamCall
	callsB <-chan speechUpstreamCall
}

// newSpeechVoiceWorld builds the world. Server a's mappings list voice "alba" and
// server b's list "bella", unless cacheVoices is false, in which case nothing is cached
// anywhere (a gateway that has not seen a model load yet).
func newSpeechVoiceWorld(t *testing.T, cacheVoices bool) *speechVoiceWorld {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	upA, callsA := newSpeechUpstream(t, speechTestWAV())
	upB, callsB := newSpeechUpstream(t, speechTestWAV())

	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	routes := routing.NewMemoryStore()
	for _, s := range []struct{ name, url string }{{"a", upA.URL}, {"b", upB.URL}} {
		u, err := url.Parse(s.url)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", s.url, err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatalf("upstream port %q: %v", u.Port(), err)
		}
		if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-" + s.name, Name: "Audio " + s.name, Domain: u.Hostname(), Provider: routing.ProviderAudioCpp, Endpoint: s.url, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", s.name, err)
		}
		if err := routes.CreateApplication(ctx, routing.Application{ID: "app-" + s.name, ServerID: "srv-" + s.name, Type: routing.ProviderAudioCpp, Port: port, Scheme: u.Scheme, APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", s.name, err)
		}
		if err := routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-" + s.name, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", s.name, err)
		}
	}
	for _, m := range []struct{ id, app, gateway, upstream, voice string }{
		{"map-a", "a", "tts-voice", "model_a", "alba"},
		{"map-b", "b", "tts-voice", "model_b", "bella"},
		{"map-ga", "a", "member-a", "member_a_up", "alba"},
		{"map-gb", "b", "member-b", "member_b_up", "bella"},
	} {
		if err := routes.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-" + m.app, GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
		if err := routes.UpsertMappingCapabilities(ctx, m.id, []routing.CapabilityRow{{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", m.id, err)
		}
		if !cacheVoices {
			continue
		}
		payload, err := json.Marshal([]string{m.voice})
		if err != nil {
			t.Fatalf("marshal voices: %v", err)
		}
		if err := routes.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{MappingID: m.id, CacheType: routing.ModelMappingCacheVoices, Payload: string(payload), UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertModelMappingCache %s: %v", m.id, err)
		}
	}
	if err := routes.CreateModelGroup(ctx, routing.ModelGroup{ID: "grp_voice", GatewayModelName: "tts-group", DisplayName: "tts-group", Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateModelGroup: %v", err)
	}
	if err := routes.SetGroupMembers(ctx, "grp_voice", []routing.GroupMember{{MemberGatewayName: "member-a", Priority: 0}, {MemberGatewayName: "member-b", Priority: 1}}); err != nil {
		t.Fatalf("SetGroupMembers: %v", err)
	}
	groups := NewGroupRegistry(routes)
	if err := groups.RefreshGroups(ctx); err != nil {
		t.Fatalf("RefreshGroups: %v", err)
	}
	recorder := usage.NewRecorder()
	return &speechVoiceWorld{
		srv: New(ServerDeps{
			Tokens:             tokens,
			Usage:              recorder,
			Provider:           provider.NewOpenAICompatibleClient(http.DefaultClient),
			Routes:             routes,
			Groups:             groups,
			Portal:             portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routes, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
			InternalAuthSecret: "s3cret",
			Users:              directory,
		}),
		routes: routes,
		callsA: callsA,
		callsB: callsB,
	}
}

// speak posts one speech request for model, naming voice ("" = no voice field).
func (w *speechVoiceWorld) speak(t *testing.T, model, voice string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","input":"Hello there."`
	if voice != "" {
		body += `,"voice":"` + voice + `"`
	}
	return postSpeech(t, w.srv, body+`}`)
}

// servedBy reports which server answered the request that just completed with 200:
// "a" or "b", failing if both or neither saw it (each upstream's call is consumed).
func (w *speechVoiceWorld) servedBy(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var served []string
	for name, calls := range map[string]<-chan speechUpstreamCall{"a": w.callsA, "b": w.callsB} {
		select {
		case <-calls:
			served = append(served, name)
		default:
		}
	}
	if len(served) != 1 {
		t.Fatalf("servers that saw the request = %v, want exactly one", served)
	}
	return served[0]
}

// A chosen voice reaches the server that has it, for a single model and for a group, and
// the voice itself is relayed to that server untouched.
func TestSpeechRoutesAChosenVoiceToTheServerThatHasIt(t *testing.T) {
	for _, tc := range []struct {
		name, model, voice, want string
	}{
		{"model, voice on the second server", "tts-voice", "bella", "b"},
		{"model, voice on the first server", "tts-voice", "alba", "a"},
		{"group, voice on the second member's server", "tts-group", "bella", "b"},
		{"group, voice on the first member's server", "tts-group", "alba", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newSpeechVoiceWorld(t, true)
			rec := w.speak(t, tc.model, tc.voice)
			if got := w.servedBy(t, rec); got != tc.want {
				t.Fatalf("served by %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("the voice is relayed to the upstream untouched", func(t *testing.T) {
		w := newSpeechVoiceWorld(t, true)
		rec := w.speak(t, "tts-voice", "bella")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		var call speechUpstreamCall
		select {
		case call = <-w.callsB:
		default:
			t.Fatal("server b never saw the request")
		}
		var sent map[string]any
		if err := json.Unmarshal(call.body, &sent); err != nil {
			t.Fatalf("upstream body is not JSON: %v (%s)", err, call.body)
		}
		if sent["voice"] != "bella" || sent["model"] != "model_b" {
			t.Fatalf("upstream body = %v, want voice bella on server b's own model id", sent)
		}
	})
}

// FAIL-OPEN, the existing-client guarantee: a voice the gateway does not know (free text,
// a name no cached list holds) and a gateway with nothing cached at all route exactly as a
// request with no voice does, so a bearer client that always sent its own voice name is
// not refused and not re-routed.
func TestSpeechFreeTextVoiceIsUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cacheVoices bool
		model       string
		voice       string
	}{
		{"an unknown voice on a model", true, "tts-voice", "my-own-voice"},
		{"an unknown voice on a group", true, "tts-group", "my-own-voice"},
		{"a known voice name, nothing cached yet (model)", false, "tts-voice", "bella"},
		{"a known voice name, nothing cached yet (group)", false, "tts-group", "bella"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Two fresh worlds, one request each: consecutive requests in ONE world are not
			// comparable, because the gateway's swap protection steers the second away from the
			// server the first just used.
			want := newSpeechVoiceWorld(t, tc.cacheVoices)
			wantServer := want.servedBy(t, want.speak(t, tc.model, "")) // today's choice with no voice at all
			w := newSpeechVoiceWorld(t, tc.cacheVoices)
			if got := w.servedBy(t, w.speak(t, tc.model, tc.voice)); got != wantServer {
				t.Fatalf("voice %q served by %q, want %q (the same server as with no voice)", tc.voice, got, wantServer)
			}
		})
	}
}

// The voice's servers are all unusable: a dedicated 503 routing.voice_unavailable, in the
// body AND on the usage row, where the generic routing.no_healthy_host would otherwise be
// -- and no upstream is called. The controls show the refusal is about this voice only:
// the healthy server's voice, and a voice nobody is known to offer (not narrowed, so not
// refused), are still served by the same world.
func TestSpeechVoiceOnADownServerIsRefusedAsVoiceUnavailable(t *testing.T) {
	// One request per world (see TestSpeechFreeTextVoiceIsUnaffected for why), with
	// server b, the only one that offers bella, unhealthy.
	downB := func(t *testing.T) *speechVoiceWorld {
		t.Helper()
		w := newSpeechVoiceWorld(t, true)
		if err := w.routes.SetServerHealth(context.Background(), "srv-b", routing.HealthUnhealthy); err != nil {
			t.Fatalf("SetServerHealth: %v", err)
		}
		return w
	}
	for _, model := range []string{"tts-voice", "tts-group"} {
		t.Run(model, func(t *testing.T) {
			w := downB(t)
			rec := w.speak(t, model, "bella")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "routing.voice_unavailable")
			if msg := rec.Body.String(); !strings.Contains(msg, "voice") {
				t.Fatalf("body = %s, want a message that says it is about the voice", msg)
			}
			requireNoSpeechUpstreamCall(t, w.callsA)
			requireNoSpeechUpstreamCall(t, w.callsB)

			events := w.srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage events = %+v, want exactly one refusal row", events)
			}
			if got := events[0]; got.HTTPStatus != http.StatusServiceUnavailable || got.Status != "error" || got.ErrorCode != "routing.voice_unavailable" || got.APIFlavor != apiFlavorSpeech {
				t.Fatalf("row = status %d/%q/%q flavor %q, want 503/error/routing.voice_unavailable/%s", got.HTTPStatus, got.Status, got.ErrorCode, got.APIFlavor, apiFlavorSpeech)
			}

			for _, voice := range []string{"alba", "my-own-voice", ""} {
				w := downB(t)
				if got := w.servedBy(t, w.speak(t, model, voice)); got != "a" {
					t.Fatalf("voice %q served by %q, want the healthy server a", voice, got)
				}
			}
		})
	}
}

// The new code is part of the same stable mapping every routing failure goes through: the
// status, the code and the message come from one place, and an unrelated routing failure
// is untouched.
func TestCompletionErrorMappingForVoiceUnavailable(t *testing.T) {
	if got := completionHTTPStatus(routing.ErrVoiceUnavailable); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got)
	}
	if got := completionErrorCode(routing.ErrVoiceUnavailable); got != "routing.voice_unavailable" {
		t.Fatalf("code = %q, want routing.voice_unavailable", got)
	}
	if got := completionErrorCode(routing.ErrNoHealthyHost); got != "routing.no_healthy_host" {
		t.Fatalf("no_healthy_host code = %q, want it unchanged", got)
	}
	if body := completionErrorResponse(routing.ErrVoiceUnavailable); !strings.Contains(body.Error.Message, "voice") {
		t.Fatalf("message = %q, want it to name the voice", body.Error.Message)
	}
}
