// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The voice-chat synth endpoint (POST /api/portal/voice-chat-synth) is driven through the
// real mux over the speech tests' own world: a real routing store, the OpenAI-compatible
// provider and an audio.cpp-shaped upstream that answers a small WAV with the metering
// header, so the status, the bytes, the upstream request and the usage row are the ones
// the real speech core produced. What the endpoint adds in front of that core -- the
// portal principal, the CSRF header, the voice-chat model gate -- is added to that world
// here, plus a chat store that counts writes: the endpoint's whole point is that it
// leaves the thread alone.

const (
	voiceSynthPath = "/api/portal/voice-chat-synth"

	// voiceSynthServiceBearer carries gateway:use ON PURPOSE: the portal gate refuses a
	// service token by its KIND, so a refusal proves the kind check and not a missing scope.
	voiceSynthServiceBearer = "synth-service-secret"
	voiceSynthInvokeBearer  = "synth-invoke-secret"
	voiceSynthUser          = "usr_synth"
)

// voiceSynthChatSpy is the chat store the endpoint must never write to: it records every
// mutating call (the document, and chat_blobs, which only SaveChatWithBlobs reaches) and
// serves everything else from the in-memory store.
type voiceSynthChatSpy struct {
	*store.MemoryChatStore
	mu     sync.Mutex
	writes []string
}

func (s *voiceSynthChatSpy) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, op)
}

func (s *voiceSynthChatSpy) writeLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

func (s *voiceSynthChatSpy) CreateChat(ctx context.Context, chat store.Chat) error {
	s.record("CreateChat")
	return s.MemoryChatStore.CreateChat(ctx, chat)
}

func (s *voiceSynthChatSpy) UpdateChat(ctx context.Context, chat store.Chat) error {
	s.record("UpdateChat")
	return s.MemoryChatStore.UpdateChat(ctx, chat)
}

func (s *voiceSynthChatSpy) DeleteChat(ctx context.Context, id string) error {
	s.record("DeleteChat")
	return s.MemoryChatStore.DeleteChat(ctx, id)
}

func (s *voiceSynthChatSpy) SaveChatWithBlobs(ctx context.Context, chat store.Chat, blobs []store.ChatBlob, deleteBlobIDs []string) error {
	s.record("SaveChatWithBlobs")
	return s.MemoryChatStore.SaveChatWithBlobs(ctx, chat, blobs, deleteBlobIDs)
}

// voiceSynthWorld is the speech streaming fixture (tts-voice: speech yes, speech_streaming
// yes; tts-offline: speech_streaming no; tts-unprobed: no streaming row; asr-model: speech
// no) with a logged-in session user, the chat store spy and the two refusal bearers.
type voiceSynthWorld struct {
	srv          *Server
	chats        *voiceSynthChatSpy
	chatSvc      *portal.Service
	cookieHeader string
}

func newVoiceSynthWorld(t *testing.T, upstreamURL string) *voiceSynthWorld {
	t.Helper()
	srv := newSpeechStreamingTestServer(t, upstreamURL)
	directory, ok := srv.users.(*portal.MemoryDirectory)
	if !ok {
		t.Fatalf("the speech fixture's user lookup is %T, want the memory directory", srv.users)
	}
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	chats := &voiceSynthChatSpy{MemoryChatStore: store.NewMemoryChatStore(0)}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	svc := portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: srv.Usage, Routes: srv.Routes,
		Clock: func() time.Time { return now }, ModelLister: provider.NewMock(),
		Chats: chats, Cipher: cipher,
	})
	srv.Portal = svc

	tokens, ok := srv.Tokens.(*auth.TokenStore)
	if !ok {
		t.Fatalf("the speech fixture's token store is %T, want *auth.TokenStore", srv.Tokens)
	}
	tokens.AddPlainToken(auth.Token{ID: "tok_synth_svc", Kind: "service", ServiceID: "svc_synth", ServiceName: "Synth", Active: true, Scopes: []string{"gateway:use"}}, voiceSynthServiceBearer)
	tokens.AddPlainToken(auth.Token{ID: "tok_synth_invoke", UserID: voiceSynthUser, Active: true, Scopes: []string{"llm:invoke"}}, voiceSynthInvokeBearer)

	withLiveTicketSessions(srv, directory)
	seedLoginUser(t, directory, voiceSynthUser, "synth@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "synth@example.test", "password-1")
	return &voiceSynthWorld{srv: srv, chats: chats, chatSvc: svc, cookieHeader: cookie.Name + "=" + cookie.Value}
}

// voiceSynthBody is the request body for the given fields; an empty voice and language are
// left out, as the portal leaves them out.
func voiceSynthBody(text, ttsModel, voice, language string) string {
	body := map[string]string{"text": text, "tts_model": ttsModel}
	if voice != "" {
		body["voice"] = voice
	}
	if language != "" {
		body["language"] = language
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (w *voiceSynthWorld) post(body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, voiceSynthPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	return rec
}

// postSession posts body as the logged-in session user, with the CSRF header a browser sends.
func (w *voiceSynthWorld) postSession(body string) *httptest.ResponseRecorder {
	return w.post(body, map[string]string{"Cookie": w.cookieHeader, csrfHeaderName: "1"})
}

// A section is synthesized through the speech core: the WAV the upstream produced reaches
// the browser byte for byte under the upstream's own Content-Type, and the upstream is
// asked for exactly what the portal named -- the mapped model, the text, the voice and the
// language -- as a one-shot request (no stream, no response format).
func TestVoiceChatSynthRelaysTheUpstreamAudio(t *testing.T) {
	wav := speechTestWAV()
	up, calls := newSpeechUpstream(t, wav)
	w := newVoiceSynthWorld(t, up.URL)

	rec := w.postSession(voiceSynthBody("Guten Tag. Wie geht es dir?", speechGatewayModel, "alba", "de"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav relayed from the upstream", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("body = %x, want the upstream WAV %x byte for byte", rec.Body.Bytes(), wav)
	}

	call := <-calls
	if call.method != http.MethodPost || call.path != "/v1/audio/speech" {
		t.Fatalf("upstream call = %s %s, want POST /v1/audio/speech", call.method, call.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, call.body)
	}
	want := map[string]any{"model": speechUpstreamModel, "input": "Guten Tag. Wie geht es dir?", "voice": "alba", "language": "de"}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("upstream body = %v, want %v: the mapped model, the text, the voice and the language, nothing else", sent, want)
	}
}

// A voice or language the portal does not name is not sent at all (not as an empty or
// blank string), so the upstream keeps its own default rather than being handed an inert
// value.
func TestVoiceChatSynthOmitsAVoiceAndLanguageItIsNotGiven(t *testing.T) {
	for name, body := range map[string]string{
		"absent": voiceSynthBody("Hello.", speechGatewayModel, "", ""),
		"blank":  `{"text":"Hello.","tts_model":"` + speechGatewayModel + `","voice":"  ","language":""}`,
		"null":   `{"text":"Hello.","tts_model":"` + speechGatewayModel + `","voice":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			w := newVoiceSynthWorld(t, up.URL)

			rec := w.postSession(body)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
			}
			var sent map[string]any
			if err := json.Unmarshal((<-calls).body, &sent); err != nil {
				t.Fatalf("upstream body is not JSON: %v", err)
			}
			if want := map[string]any{"model": speechUpstreamModel, "input": "Hello."}; !reflect.DeepEqual(sent, want) {
				t.Fatalf("upstream body = %v, want only %v", sent, want)
			}
		})
	}
}

// The synthesis is billed by the speech core it reuses, and by nothing else: exactly one
// usage row, a successful speech row in audio seconds (the upstream's 1.5 s metering
// header), attributed to the session user.
func TestVoiceChatSynthBillsExactlyOneSpeechRow(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	w := newVoiceSynthWorld(t, up.URL)

	rec := w.postSession(voiceSynthBody("Hello there.", speechGatewayModel, "", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
	rows := w.srv.Usage.All()
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want exactly one (the speech core's): %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Status != "success" || row.HTTPStatus != http.StatusOK || row.APIFlavor != apiFlavorSpeech {
		t.Fatalf("row = %q/%d/%q, want success/200/%s", row.Status, row.HTTPStatus, row.APIFlavor, apiFlavorSpeech)
	}
	if row.BillingUnit != usage.BillingUnitAudioSecond || row.BillingQuantity != 1.5 {
		t.Fatalf("billing = %v %q, want 1.5 %q from the upstream's metering header", row.BillingQuantity, row.BillingUnit, usage.BillingUnitAudioSecond)
	}
	if row.UserID != voiceSynthUser || row.TokenID != "" {
		t.Fatalf("row UserID/TokenID = %q/%q, want the session user and no token row", row.UserID, row.TokenID)
	}
	if row.Model != speechGatewayModel || row.ProviderModel != speechUpstreamModel {
		t.Fatalf("row Model/ProviderModel = %q/%q, want %q/%q", row.Model, row.ProviderModel, speechGatewayModel, speechUpstreamModel)
	}
	if row.ReqPath != voiceSynthPath || row.ProviderPath != "/v1/audio/speech" {
		t.Fatalf("row ReqPath/ProviderPath = %q/%q, want the portal route the browser called and the upstream's speech path", row.ReqPath, row.ProviderPath)
	}
	if err := usage.ValidateBillingXOR(row); err != nil {
		t.Fatalf("recorded row violates the billing XOR: %v", err)
	}
}

// THE POINT OF THE ENDPOINT: it speaks a section and leaves the thread alone. The request
// has no chat to name, so even a body that does name a real chat (a client that sends one
// anyway) writes nothing: no document write, no chat_blobs row, and the chat reads back
// exactly as it was.
func TestVoiceChatSynthIsNonCommitting(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	w := newVoiceSynthWorld(t, up.URL)
	owner := auth.Token{UserID: voiceSynthUser}
	created, err := w.chatSvc.CreateChat(context.Background(), owner, portal.CreateChatRequest{
		Title:   "A spoken thread",
		Content: json.RawMessage(`{"settings":{"audio":{"voice":"alba"}},"messages":[{"role":"assistant","content":"Hello."}]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	// The spy is wired: the seed's own write went through it.
	writesBefore := w.chats.writeLog()
	if !slices.Contains(writesBefore, "CreateChat") {
		t.Fatalf("chat store writes after the seed = %v, want the seed's CreateChat: the spy is not in the portal's path", writesBefore)
	}

	body := `{"text":"Hello.","tts_model":"` + speechGatewayModel + `","chat_id":"` + created.ID + `"}`
	rec := w.postSession(body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
	if writesAfter := w.chats.writeLog(); !reflect.DeepEqual(writesAfter, writesBefore) {
		t.Fatalf("chat store writes = %v, want none after the seed's %v", writesAfter, writesBefore)
	}
	after, err := w.chatSvc.GetChat(context.Background(), owner, created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if after.Title != created.Title || !after.UpdatedAt.Equal(created.UpdatedAt) || !bytes.Equal(after.Content, created.Content) {
		t.Fatalf("the chat changed: title %q, updated %v, content %s; want the seed's %q, %v, %s", after.Title, after.UpdatedAt, after.Content, created.Title, created.UpdatedAt, created.Content)
	}
	chats, err := w.chatSvc.ListChats(context.Background(), owner)
	if err != nil || len(chats) != 1 {
		t.Fatalf("ListChats = %v (%v), want only the seeded chat: a synth creates no thread", chats, err)
	}
}

// A model the voice-chat gate refuses is refused with the gate's own code before anything
// is spent: the upstream is never asked and no usage row is written. "Refuses" is the very
// gate the live voice chat applies to its speech leg (resolveVoiceTTS), so a model a
// session could not have spoken with is no model the replay can re-speak with.
func TestVoiceChatSynthRefusesAModelTheVoiceChatGateRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		setup func(t *testing.T, w *voiceSynthWorld)
	}{
		{name: "an unknown model", model: "no-such-model"},
		{name: "a model that is not speech-capable", model: "asr-model"},
		{name: "a speech model that does not stream", model: speechOfflineModel},
		{name: "a speech model with no streaming verdict", model: speechUnprobedModel},
		{name: "a clone model with no named preset", model: speechGatewayModel, setup: func(t *testing.T, w *voiceSynthWorld) {
			t.Helper()
			err := w.srv.Routes.UpsertMappingCapabilities(context.Background(), "route-tts", []routing.CapabilityRow{{
				Capability: routing.CapabilitySpeechClone, Verdict: routing.CapabilityYes,
				Source: routing.CapabilitySourceAudioCppModels, CheckedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
			}})
			if err != nil {
				t.Fatalf("UpsertMappingCapabilities: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			w := newVoiceSynthWorld(t, up.URL)
			if tc.setup != nil {
				tc.setup(t, w)
			}

			rec := w.postSession(voiceSynthBody("Hello.", tc.model, "", ""))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), voiceChatTTSUnavailable)
			requireNoSpeechUpstreamCall(t, calls)
			if rows := w.srv.Usage.All(); len(rows) != 0 {
				t.Fatalf("usage rows = %+v, want none for a request refused before the relay", rows)
			}
		})
	}
}

// Blank text is the speech core's own 400 (speech.input_required), a blank model the
// endpoint's own (the portal names the model; a request without one is a client bug), and a
// body that is not one JSON object of this shape (text a non-string included) is
// request.invalid_json -- each before the gate, so nothing is resolved or spent.
func TestVoiceChatSynthValidatesTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantCode string
	}{
		{"missing text", `{"tts_model":"` + speechGatewayModel + `"}`, "speech.input_required"},
		{"empty text", voiceSynthBody("", speechGatewayModel, "", ""), "speech.input_required"},
		{"whitespace-only text", voiceSynthBody(" \n\t ", speechGatewayModel, "", ""), "speech.input_required"},
		{"text that is no string", `{"text":["a"],"tts_model":"` + speechGatewayModel + `"}`, codeRequestInvalidJSON},
		{"missing model", `{"text":"Hello."}`, "voice_chat.tts_model_required"},
		{"empty model", voiceSynthBody("Hello.", "", "", ""), "voice_chat.tts_model_required"},
		{"whitespace-only model", voiceSynthBody("Hello.", "  ", "", ""), "voice_chat.tts_model_required"},
		{"a model that is blank and a text that is too", voiceSynthBody("", "", "", ""), "voice_chat.tts_model_required"},
		{"not JSON", `not json`, codeRequestInvalidJSON},
		{"a JSON array", `["Hello."]`, codeRequestInvalidJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			w := newVoiceSynthWorld(t, up.URL)

			rec := w.postSession(tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoSpeechUpstreamCall(t, calls)
			if rows := w.srv.Usage.All(); len(rows) != 0 {
				t.Fatalf("usage rows = %+v, want none for a request refused before the relay", rows)
			}
		})
	}
}

// The model name is trimmed, like the ticket's: a padded name reaches the gate as the name
// it is.
func TestVoiceChatSynthTrimsTheModelName(t *testing.T) {
	up, _ := newSpeechUpstream(t, speechTestWAV())
	w := newVoiceSynthWorld(t, up.URL)

	rec := w.postSession(voiceSynthBody("Hello.", "  "+speechGatewayModel+"\t", "", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
}

// A body past the cap is refused 413 like every capped body, before the gate: a section is a
// few paragraphs, so the cap is far above any real one.
func TestVoiceChatSynthCapsTheBody(t *testing.T) {
	up, calls := newSpeechUpstream(t, speechTestWAV())
	w := newVoiceSynthWorld(t, up.URL)

	large := w.postSession(voiceSynthBody(strings.Repeat("a", 100<<10), speechGatewayModel, "", ""))
	if large.Code != http.StatusOK {
		t.Fatalf("a 100 KiB section: status = %d, want 200; body = %.200q", large.Code, large.Body.Bytes())
	}
	<-calls

	rec := w.postSession(voiceSynthBody(strings.Repeat("a", 2<<20), speechGatewayModel, "", ""))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %.200s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), codeRequestBodyTooLarge)
	requireNoSpeechUpstreamCall(t, calls)
}

// An upstream that fails is relayed as the speech core relays it: the upstream's own status
// and error object reach the browser (which keeps the section's text on screen), and the
// row is an error row billed zero -- the same failure accounting as /v1/audio/speech.
func TestVoiceChatSynthRelaysAnUpstreamFailure(t *testing.T) {
	up := newSpeechErrorUpstream(t, http.StatusInternalServerError, "application/json", `{"error":{"message":"model offline","type":"server_error"}}`)
	w := newVoiceSynthWorld(t, up.URL)

	rec := w.postSession(voiceSynthBody("Hello.", speechGatewayModel, "", ""))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream's 500; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model offline") {
		t.Fatalf("body = %s, want the upstream's error object", rec.Body.String())
	}
	rows := w.srv.Usage.All()
	if len(rows) != 1 || rows[0].Status != "error" || rows[0].HTTPStatus != http.StatusInternalServerError || rows[0].BillingQuantity != 0 {
		t.Fatalf("usage rows = %+v, want one error row, 500, billed zero", rows)
	}
}

// THE CSRF DEFENSE: this POST spends the user's speech budget, so a session cookie without
// the X-OP-CSRF header (or with a wrong one) is 403 and starts nothing; the same cookie
// with it is the positive control.
func TestVoiceChatSynthRequiresCSRF(t *testing.T) {
	up, calls := newSpeechUpstream(t, speechTestWAV())
	w := newVoiceSynthWorld(t, up.URL)
	body := voiceSynthBody("Hello.", speechGatewayModel, "", "")

	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"no CSRF header", map[string]string{"Cookie": w.cookieHeader}},
		{"a CSRF header that is not the portal's value", map[string]string{"Cookie": w.cookieHeader, csrfHeaderName: "0"}},
		{"an empty CSRF header", map[string]string{"Cookie": w.cookieHeader, csrfHeaderName: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := w.post(body, tc.hdr)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "auth.csrf_required")
			requireNoSpeechUpstreamCall(t, calls)
		})
	}

	if rec := w.postSession(body); rec.Code != http.StatusOK {
		t.Fatalf("with the CSRF header: status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
	}
}

// No credentials, an unknown bearer, a service token (the gateway's "LLM-invoke only"
// principal, which never reaches a portal route whatever it carries) and a token without
// gateway:use are each refused before the body is read or anything is resolved.
func TestVoiceChatSynthRequiresAuthenticationAndScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hdr        map[string]string
		wantStatus int
		wantCode   string
	}{
		{"no credentials", nil, http.StatusUnauthorized, "auth.invalid_token"},
		{"an unknown bearer", map[string]string{"Authorization": "Bearer not-a-token"}, http.StatusUnauthorized, "auth.invalid_token"},
		{"a service token", map[string]string{"Authorization": "Bearer " + voiceSynthServiceBearer}, http.StatusUnauthorized, "auth.invalid_token"},
		{"a token without gateway:use", map[string]string{"Authorization": "Bearer " + voiceSynthInvokeBearer}, http.StatusForbidden, "auth.insufficient_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newSpeechUpstream(t, speechTestWAV())
			w := newVoiceSynthWorld(t, up.URL)

			rec := w.post(voiceSynthBody("Hello.", speechGatewayModel, "", ""), tc.hdr)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoSpeechUpstreamCall(t, calls)
		})
	}
}

// The synthesis is requested on POST only: it spends something, and a GET is CSRF-exempt.
func TestVoiceChatSynthRefusesOtherMethods(t *testing.T) {
	up, calls := newSpeechUpstream(t, speechTestWAV())
	w := newVoiceSynthWorld(t, up.URL)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, voiceSynthPath, nil)
		req.Header.Set("Cookie", w.cookieHeader)
		rec := httptest.NewRecorder()
		w.srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
	}
	requireNoSpeechUpstreamCall(t, calls)
}
