// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// The live-transcription ticket endpoint (POST /api/portal/transcription-live-ticket) is the
// one CSRF-protected, authenticated step before the browser opens the WebSocket: it resolves
// the model, gates it for streaming and (for an agent target) the fullduplex_proxy flag,
// validates the audio params and mints the ticket the WebSocket upgrade then presents. These
// tests drive it through the real mux, on the fixtures the buffered transcription endpoint's
// own tests use, so the gate is held to the same routing the relay would apply.

const (
	liveTicketPath = "/api/portal/transcription-live-ticket"

	// The wire codes of the endpoint's own refusals. Literals on purpose: they are the
	// client contract, so a typo in the gateway's constants must fail here rather than agree
	// with itself.
	liveCodeParamsInvalid    = "portal.live_transcription_params_invalid"
	liveCodeNotStreaming     = "portal.live_transcription_not_streaming"
	liveCodeAgentUnsupported = "portal.live_transcription_agent_unsupported"
	liveCodeTicketFailed     = "portal.live_transcription_ticket_failed"

	// liveAgentModel is the one mapping of the server_agent fixture (gateway and upstream
	// name equal).
	liveAgentModel = "agent-asr"
)

// liveTicketJSON is a valid request body for model: 16 kHz mono signed 16-bit PCM.
func liveTicketJSON(model string) string {
	return `{"model":"` + model + `","sample_rate":16000,"channels":1,"sample_format":"s16le"}`
}

// postLiveTicketWith posts body to the ticket endpoint through the real mux with hdr as its
// headers (a bearer, or a session cookie and the CSRF header).
func postLiveTicketWith(t *testing.T, srv *Server, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, liveTicketPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// postLiveTicket is the common case: the dev bearer, which the transcription fixtures seed
// as usr_dev / tok_dev with gateway:use.
func postLiveTicket(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postLiveTicketWith(t, srv, body, map[string]string{"Authorization": "Bearer dev-secret"})
}

// redeemMintedTicket reads the ticket out of a 200 response and redeems it on the server's
// own store, which is what the WebSocket upgrade does with it.
func redeemMintedTicket(t *testing.T, srv *Server, rec *httptest.ResponseRecorder) liveTicketClaims {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body = %s)", err, rec.Body.String())
	}
	if body.Ticket == "" {
		t.Fatalf("response carries no ticket: %s", rec.Body.String())
	}
	claims, ok := srv.liveTickets.Redeem(body.Ticket)
	if !ok {
		t.Fatalf("the minted ticket does not redeem on the server's own store")
	}
	return claims
}

// requireNoTicket fails when the response carries a ticket: a refusal must mint nothing.
func requireNoTicket(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), `"ticket"`) {
		t.Fatalf("a refused request carried a ticket: %s", rec.Body.String())
	}
}

// newLiveTicketAgentServer is the server_agent transcription fixture (one mapping,
// transcription yes and transcription_streaming yes, its spec serving openai_audio) with the
// feature set its agent declares. A nil features means the agent never reported at all.
func newLiveTicketAgentServer(t *testing.T, features []string) *Server {
	t.Helper()
	prov := &recordingProxyProvider{respBody: transcriptionOKBody}
	srv := newServerAgentTranscriptionTestServer(t, prov, liveAgentModel, liveAgentModel, []string{routing.APIFlavorOpenAIAudio})
	if features != nil {
		srv.AgentFeatures.Set(agentTranscriptionServerID, features)
	}
	return srv
}

// withLiveTicketSessions gives srv a real account service on directory, so a test can log a
// browser session in (the transcription fixtures are bearer-only) and exercise the cookie
// path -- the one CSRF applies to.
func withLiveTicketSessions(srv *Server, directory *portal.MemoryDirectory) {
	srv.Account = account.NewService(account.Deps{Users: directory, Sessions: directory, SetPasswordTokens: directory}, account.Config{
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de",
	})
}

// A streaming-capable transcription model on a direct audio.cpp application is allowed with
// no agent flag (there is no agent in front of it), and the ticket captures the server's own
// routing decision, the one validated params triple and who asked.
func TestLiveTranscriptionTicketMintsForADirectStreamingModel(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postLiveTicket(t, srv, `{"model":"asr-model","sample_rate":16000,"channels":1,"sample_format":"s16le","language":"de"}`)

	claims := redeemMintedTicket(t, srv, rec)
	if claims.Target.Provider != routing.ProviderAudioCpp || claims.Target.RouteID != "route-asr" || claims.Target.ServerID != "srv-audio" {
		t.Fatalf("Target provider/route/server = %q/%q/%q, want audio_cpp/route-asr/srv-audio", claims.Target.Provider, claims.Target.RouteID, claims.Target.ServerID)
	}
	if claims.Target.ProviderModel != transcriptionUpstreamModel {
		t.Fatalf("Target.ProviderModel = %q, want the mapping's upstream name %q", claims.Target.ProviderModel, transcriptionUpstreamModel)
	}
	if claims.SampleRate != 16000 || claims.Channels != 1 || claims.Format != "s16le" || claims.Language != "de" {
		t.Fatalf("params = %d/%d/%q/%q, want 16000/1/s16le/de", claims.SampleRate, claims.Channels, claims.Format, claims.Language)
	}
	if claims.UserID != "usr_dev" || claims.TokenID != "tok_dev" {
		t.Fatalf("UserID/TokenID = %q/%q, want usr_dev/tok_dev", claims.UserID, claims.TokenID)
	}
	// The ticket endpoint is a gate: it contacts no upstream and records no usage row (the
	// live session itself bills, later).
	requireNoTranscriptionUpstreamCall(t, calls)
	if n := len(srv.Usage.All()); n != 0 {
		t.Fatalf("usage events = %d, want 0 for a gate", n)
	}
}

// The response is exactly {"ticket": ...}: the claims are encrypted inside it, and nothing of
// the routing decision (the upstream endpoint, the sealed credential) is echoed beside it.
func TestLiveTranscriptionTicketResponseCarriesOnlyTheTicket(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postLiveTicket(t, srv, liveTicketJSON("asr-model"))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body) != 1 || body["ticket"] == nil {
		t.Fatalf("response = %v, want exactly one key, ticket", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// Every mint is its own ticket: a second request for the same model is a different token
// (the nonce), so one cannot stand in for the other.
func TestLiveTranscriptionTicketEveryMintIsDistinct(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	first := redeemMintedTicket(t, srv, postLiveTicket(t, srv, liveTicketJSON("asr-model")))
	second := redeemMintedTicket(t, srv, postLiveTicket(t, srv, liveTicketJSON("asr-model")))

	if first.Nonce == second.Nonce {
		t.Fatalf("two mints share the nonce %q", first.Nonce)
	}
}

// The model name is trimmed the way the transcription endpoint trims it, and an absent
// language is the empty one (the backend's own auto-detect).
func TestLiveTranscriptionTicketTrimsTheModelAndLanguageIsOptional(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	claims := redeemMintedTicket(t, srv, postLiveTicket(t, srv, `{"model":"  asr-model  ","sample_rate":48000,"channels":2,"sample_format":"f32le"}`))

	if claims.Target.RouteID != "route-asr" {
		t.Fatalf("RouteID = %q, want route-asr", claims.Target.RouteID)
	}
	if claims.Language != "" || claims.SampleRate != 48000 || claims.Channels != 2 || claims.Format != "f32le" {
		t.Fatalf("params = %d/%d/%q/%q, want 48000/2/f32le/<empty language>", claims.SampleRate, claims.Channels, claims.Format, claims.Language)
	}
}

// A model whose transcription_streaming verdict is not yes cannot stream live: an explicit
// no (an offline model) and no row at all (a model no probe has stated the mode of) are both
// refused 409, and nothing is minted.
func TestLiveTranscriptionTicketRefusesAModelThatDoesNotStream(t *testing.T) {
	for _, model := range []string{"asr-offline", "asr-unprobed"} {
		t.Run(model, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postLiveTicket(t, srv, liveTicketJSON(model))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), liveCodeNotStreaming)
			requireNoTicket(t, rec)
			requireNoTranscriptionUpstreamCall(t, calls)
		})
	}
}

// A model the transcription endpoint itself would refuse is refused here the same way: a
// speech-only model, an explicit transcription no, an unprobed audio model, a chat model and
// an unknown name. 404 with the routing code, exactly as POST /v1/audio/transcriptions
// answers -- no new way to probe which models exist.
func TestLiveTranscriptionTicketRefusesWhatTheTranscriptionEndpointRefuses(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		wantCode string
	}{
		{"a speech-only model", "tts-voice", "routing.model_not_capable"},
		{"an explicit transcription no", "asr-denied", "routing.model_not_capable"},
		{"an audio model that was never probed", "unprobed-voice", "routing.model_not_capable"},
		{"a chat model on a text application", "qwen-coder", "routing.no_model_route"},
		{"an unknown model", "no-such-asr", "routing.no_model_route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postLiveTicket(t, srv, liveTicketJSON(tc.model))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoTicket(t, rec)
			requireNoTranscriptionUpstreamCall(t, calls)
			// A gate, not a relay: the refusal is not a usage row either.
			if n := len(srv.Usage.All()); n != 0 {
				t.Fatalf("usage events = %d, want 0 for a refused gate", n)
			}
		})
	}
}

// An agent-managed child whose runtime spec excludes audio is refused the same way the
// transcription relay refuses it (the post-resolve effective-flavor rule), though the
// application-level flavors admitted it as a candidate.
func TestLiveTranscriptionTicketRefusesAnAgentChildWhoseSpecExcludesAudio(t *testing.T) {
	prov := &recordingProxyProvider{respBody: transcriptionOKBody}
	srv := newServerAgentTranscriptionTestServer(t, prov, liveAgentModel, liveAgentModel, []string{routing.APIFlavorOpenAI})
	srv.AgentFeatures.Set(agentTranscriptionServerID, []string{"runtime_manager", "fullduplex_proxy"})

	rec := postLiveTicket(t, srv, liveTicketJSON(liveAgentModel))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
	requireNoTicket(t, rec)
}

// An agent target is served only on positive evidence its agent declared fullduplex_proxy:
// its router must carry the /live relay route, and an older one answers 404 for it, a failure
// no later signal tells from an unknown model. Each case is an agent the gateway has no
// evidence about, so each is refused (fail closed); multipart_proxy, the buffered endpoint's
// flag, is NOT a substitute.
func TestLiveTranscriptionTicketRefusesAnAgentTargetWithoutFullduplexProxy(t *testing.T) {
	cases := []struct {
		name     string
		features func() *Server
	}{
		{"the agent has never reported", func() *Server { return newLiveTicketAgentServer(t, nil) }},
		{"the agent reports other features", func() *Server {
			return newLiveTicketAgentServer(t, []string{"runtime_manager", "runtime_ensure", "runtime_model_probe"})
		}},
		{"the agent declares multipart_proxy but not fullduplex_proxy", func() *Server {
			return newLiveTicketAgentServer(t, []string{"runtime_manager", "multipart_proxy"})
		}},
		{"only another server declares fullduplex_proxy", func() *Server {
			srv := newLiveTicketAgentServer(t, nil)
			srv.AgentFeatures.Set("srv-elsewhere", []string{"fullduplex_proxy"})
			return srv
		}},
		{"a name that merely resembles it", func() *Server {
			return newLiveTicketAgentServer(t, []string{"fullduplex", "fullduplex-proxy", "fullduplex_proxy_v2"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.features()

			rec := postLiveTicket(t, srv, liveTicketJSON(liveAgentModel))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), liveCodeAgentUnsupported)
			if !strings.Contains(rec.Body.String(), "fullduplex_proxy") {
				t.Fatalf("body = %s, want a message naming the missing fullduplex_proxy feature", rec.Body.String())
			}
			requireNoTicket(t, rec)
		})
	}
}

// The positive control of the gate, and its edge: an agent that declares fullduplex_proxy is
// minted a ticket (its Target is the server_agent one), and the registry is a full snapshot
// per sample, so an agent that stops declaring it is refused again on its next request.
func TestLiveTranscriptionTicketServesAnAgentTargetThatDeclaresFullduplexProxyAndStopsWhenItDoesNot(t *testing.T) {
	srv := newLiveTicketAgentServer(t, []string{"runtime_manager", "fullduplex_proxy"})

	claims := redeemMintedTicket(t, srv, postLiveTicket(t, srv, liveTicketJSON(liveAgentModel)))

	if claims.Target.Provider != routing.ProviderServerAgent || claims.Target.ServerID != agentTranscriptionServerID || claims.Target.RouteID != "route-agent-audio" {
		t.Fatalf("Target provider/server/route = %q/%q/%q, want server_agent/%s/route-agent-audio", claims.Target.Provider, claims.Target.ServerID, claims.Target.RouteID, agentTranscriptionServerID)
	}

	srv.AgentFeatures.Set(agentTranscriptionServerID, []string{"runtime_manager"})

	rec := postLiveTicket(t, srv, liveTicketJSON(liveAgentModel))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status after the agent stopped declaring the feature = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), liveCodeAgentUnsupported)
}

// The gate is on the TARGET being an agent, not on the server having one: a direct audio.cpp
// target has no router in front of it, whatever features its server's registry entry carries.
func TestLiveTranscriptionTicketDirectTargetIsNotGatedOnFullduplexProxy(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	srv.AgentFeatures.Set("srv-audio", []string{"runtime_manager"})

	redeemMintedTicket(t, srv, postLiveTicket(t, srv, liveTicketJSON("asr-model")))
}

// Out-of-range or unknown audio params are refused 400 with the structured code, before
// anything is resolved -- so a refusal also names no model (the unknown one here).
func TestLiveTranscriptionTicketRefusesInvalidParams(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no sample rate", `{"model":"no-such-asr","channels":1,"sample_format":"s16le"}`},
		{"sample rate below the range", `{"model":"no-such-asr","sample_rate":999,"channels":1,"sample_format":"s16le"}`},
		{"sample rate above the range", `{"model":"no-such-asr","sample_rate":384001,"channels":1,"sample_format":"s16le"}`},
		{"negative sample rate", `{"model":"no-such-asr","sample_rate":-16000,"channels":1,"sample_format":"s16le"}`},
		{"no channels", `{"model":"no-such-asr","sample_rate":16000,"sample_format":"s16le"}`},
		{"zero channels", `{"model":"no-such-asr","sample_rate":16000,"channels":0,"sample_format":"s16le"}`},
		{"too many channels", `{"model":"no-such-asr","sample_rate":16000,"channels":17,"sample_format":"s16le"}`},
		{"no sample format", `{"model":"no-such-asr","sample_rate":16000,"channels":1}`},
		{"a format audio.cpp does not accept", `{"model":"no-such-asr","sample_rate":16000,"channels":1,"sample_format":"s24le"}`},
		{"a container name for a format", `{"model":"no-such-asr","sample_rate":16000,"channels":1,"sample_format":"wav"}`},
		{"a format in the wrong case", `{"model":"no-such-asr","sample_rate":16000,"channels":1,"sample_format":"S16LE"}`},
		{"a language that would reach the upstream query as more than a language", `{"model":"no-such-asr","sample_rate":16000,"channels":1,"sample_format":"s16le","language":"de&model=other"}`},
		{"a language with a space", `{"model":"no-such-asr","sample_rate":16000,"channels":1,"sample_format":"s16le","language":"d e"}`},
		{"an overlong language", `{"model":"no-such-asr","sample_rate":16000,"channels":1,"sample_format":"s16le","language":"` + strings.Repeat("a", 65) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postLiveTicket(t, srv, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), liveCodeParamsInvalid)
			requireNoTicket(t, rec)
			requireNoTranscriptionUpstreamCall(t, calls)
		})
	}
}

// The edges of each range are valid: audio.cpp's own bounds, both formats.
func TestLiveTranscriptionTicketAcceptsTheEdgesOfTheRanges(t *testing.T) {
	for _, tc := range []struct {
		rate, channels int
		format         string
	}{
		{1000, 1, "s16le"},
		{384000, 16, "f32le"},
		{44100, 2, "s16le"},
	} {
		up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)
		body, _ := json.Marshal(map[string]any{"model": "asr-model", "sample_rate": tc.rate, "channels": tc.channels, "sample_format": tc.format})

		claims := redeemMintedTicket(t, srv, postLiveTicket(t, srv, string(body)))

		if claims.SampleRate != tc.rate || claims.Channels != tc.channels || claims.Format != tc.format {
			t.Fatalf("params = %d/%d/%q, want %d/%d/%q", claims.SampleRate, claims.Channels, claims.Format, tc.rate, tc.channels, tc.format)
		}
	}
}

// A blank model is the endpoint family's own 400 (model.name_required): without it an empty
// name would read as a model that cannot stream.
func TestLiveTranscriptionTicketRefusesABlankModel(t *testing.T) {
	for _, body := range []string{
		`{"sample_rate":16000,"channels":1,"sample_format":"s16le"}`,
		`{"model":"","sample_rate":16000,"channels":1,"sample_format":"s16le"}`,
		`{"model":"   ","sample_rate":16000,"channels":1,"sample_format":"s16le"}`,
	} {
		up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)

		rec := postLiveTicket(t, srv, body)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body = %s", body, rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "model.name_required")
	}
}

// A body that is not one JSON object of the right shape is the shared request.invalid_json
// 400: not JSON, two values, a number that is not a whole one, a wrong-typed field.
func TestLiveTranscriptionTicketRefusesAMalformedBody(t *testing.T) {
	for _, body := range []string{
		``,
		`not json`,
		`[]`,
		liveTicketJSON("asr-model") + liveTicketJSON("asr-model"),
		`{"model":"asr-model","sample_rate":16000.5,"channels":1,"sample_format":"s16le"}`,
		`{"model":"asr-model","sample_rate":"16000","channels":1,"sample_format":"s16le"}`,
		`{"model":7,"sample_rate":16000,"channels":1,"sample_format":"s16le"}`,
	} {
		up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)

		rec := postLiveTicket(t, srv, body)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d, want 400; body = %s", body, rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), codeRequestInvalidJSON)
		requireNoTicket(t, rec)
	}
}

// The ticket is minted on POST only: it creates something, and a GET is CSRF-exempt, which
// would hand the ticket to a cross-site request.
func TestLiveTranscriptionTicketRefusesOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)

		req := httptest.NewRequest(method, liveTicketPath, strings.NewReader(liveTicketJSON("asr-model")))
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
	}
}

// No credentials at all is 401, before the body is read or anything resolved.
func TestLiveTranscriptionTicketRequiresAuthentication(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postLiveTicketWith(t, srv, liveTicketJSON("asr-model"), nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoTicket(t, rec)
}

// A bearer that is not a known token is 401 as well.
func TestLiveTranscriptionTicketRefusesAnUnknownBearer(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postLiveTicketWith(t, srv, liveTicketJSON("asr-model"), map[string]string{"Authorization": "Bearer not-a-token"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

// THE CSRF DEFENSE: a browser session cookie on this POST must carry X-OP-CSRF. The WebSocket
// upgrade cannot carry that header, so this request is the only place the portal's CSRF
// protection is applied to the live path; a ticket minted without it would let a cross-site
// page open a live session as the logged-in user. The same cookie with the header is the
// positive control (so the 403 is the missing header, not a broken fixture), and a wrong
// header value does not count.
func TestLiveTranscriptionTicketEnforcesCSRFOnASessionCookie(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, directory := newTranscriptionCapableTestServer(t, up.URL)
	withLiveTicketSessions(srv, directory)
	seedLoginUser(t, directory, "usr_live", "live@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "live@example.test", "password-1")
	cookieHeader := cookie.Name + "=" + cookie.Value

	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"no CSRF header", map[string]string{"Cookie": cookieHeader}},
		{"a CSRF header that is not the portal's value", map[string]string{"Cookie": cookieHeader, csrfHeaderName: "0"}},
		{"an empty CSRF header", map[string]string{"Cookie": cookieHeader, csrfHeaderName: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postLiveTicketWith(t, srv, liveTicketJSON("asr-model"), tc.hdr)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "auth.csrf_required")
			requireNoTicket(t, rec)
		})
	}

	rec := postLiveTicketWith(t, srv, liveTicketJSON("asr-model"), map[string]string{"Cookie": cookieHeader, csrfHeaderName: "1"})
	claims := redeemMintedTicket(t, srv, rec)
	// The session principal carries the user and no token row: TokenID is empty, and the
	// ticket is the logged-in user's.
	if claims.UserID != "usr_live" || claims.TokenID != "" {
		t.Fatalf("UserID/TokenID = %q/%q, want usr_live and no token", claims.UserID, claims.TokenID)
	}
	if claims.Target.RouteID != "route-asr" {
		t.Fatalf("RouteID = %q, want route-asr", claims.Target.RouteID)
	}
	requireNoTranscriptionUpstreamCall(t, calls)
}

// A session cookie the server does not know is 401 (and the same header changes nothing).
func TestLiveTranscriptionTicketRefusesAnInvalidSession(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, directory := newTranscriptionCapableTestServer(t, up.URL)
	withLiveTicketSessions(srv, directory)

	rec := postLiveTicketWith(t, srv, liveTicketJSON("asr-model"), map[string]string{"Cookie": sessionCookieName + "=forged-session", csrfHeaderName: "1"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoTicket(t, rec)
}

// A token without gateway:use is 403, the scope the portal's other audio gates ask for: an
// API token minted for inference only (llm:invoke) opens no live session.
func TestLiveTranscriptionTicketRequiresTheGatewayUseScope(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, directory := newTranscriptionCapableTestServer(t, up.URL)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_invoke", UserID: "usr_dev", Name: "Invoke only", Status: store.TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now}, "invoke-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}

	rec := postLiveTicketWith(t, srv, liveTicketJSON("asr-model"), map[string]string{"Authorization": "Bearer invoke-secret"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "auth.insufficient_scope")
	requireNoTicket(t, rec)
}

// The ticket is the admission of the live session: the one shared pre-dispatch gate
// (inferencePreflight) runs here, once per mint, so a principal over its rate limit is
// refused with a clean 429 on this ordinary HTTP response (a WebSocket refusal is opaque to
// the browser). Request validation runs BEFORE it -- the refusals of a malformed request spend
// no admission -- and the limiter is live the whole time (the second valid mint is denied).
func TestLiveTranscriptionTicketAdmitsOncePerMintAfterValidation(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	fakeStore := newFakePrincipalStore()
	fakeStore.setConfig(Principal{Type: routing.PrincipalTypeUser, ID: "usr_dev"}, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	if rec := postLiveTicket(t, srv, `{"model":"asr-model","sample_rate":1,"channels":1,"sample_format":"s16le"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid params: status = %d, want 400", rec.Code)
	}
	if rec := postLiveTicket(t, srv, `{"sample_rate":16000,"channels":1,"sample_format":"s16le"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank model: status = %d, want 400", rec.Code)
	}
	redeemMintedTicket(t, srv, postLiveTicket(t, srv, liveTicketJSON("asr-model")))

	rec := postLiveTicket(t, srv, liveTicketJSON("asr-model"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the second valid mint: status = %d, want 429 (the limiter must have been live); body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "limit.rate_limited")
	requireNoTicket(t, rec)
}

// A server with no ticket store (a bare Server that never went through New) fails the mint
// closed with a 500 rather than panicking.
func TestLiveTranscriptionTicketWithoutAStoreFailsClosed(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	srv.liveTickets = nil

	rec := postLiveTicket(t, srv, liveTicketJSON("asr-model"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), liveCodeTicketFailed)
	requireNoTicket(t, rec)
}

// New builds the store the endpoint mints on, so a real server has one.
func TestNewServerWiresALiveTicketStore(t *testing.T) {
	srv := NewTestServer()
	if srv.liveTickets == nil {
		t.Fatal("New left liveTickets nil, want a store")
	}
}

// fullduplexProxyFeature is the literal an agent that relays /live declares in its own
// registry (server-agent/internal/agent/features.go; the agent module cannot import this
// one), and it stays off the gateway's own list: it states a fact about the agent's router
// alone, which the gateway only reads before it mints a ticket.
func TestFullduplexProxyFeatureName(t *testing.T) {
	if fullduplexProxyFeature != "fullduplex_proxy" {
		t.Fatalf("fullduplexProxyFeature = %q, want the agent's literal %q", fullduplexProxyFeature, "fullduplex_proxy")
	}
	for _, f := range gatewayAgentFeatures {
		if f == fullduplexProxyFeature {
			t.Fatalf("gatewayAgentFeatures = %v, want it without the agent-declared %q", gatewayAgentFeatures, fullduplexProxyFeature)
		}
	}
}
