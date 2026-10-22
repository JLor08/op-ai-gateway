// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two manual mutations of the audio answer lifecycle (#281) as a client meets
// them: DELETE /api/portal/chats/{id}/messages/{messageID}/audio removes one
// answer's audio, POST /api/portal/chats/{id}/audio/prune brings a chat's stored
// audio down to a count. The rewriting itself is pinned in internal/portal; what is
// pinned here is the wire: the routes, the owner scoping, the request validation,
// and above all the refusal while a run is active, which both mutations share with
// PUT /chats because each rewrites the whole document and hard-deletes blob rows.

// audioChatFixture is a stored speech thread: two prompts, each answered with audio.
// The first answer is a long ADR-070 one -- three ordered audio_url parts, three
// chat_blobs rows -- the second a short one.
type audioChatFixture struct {
	chatID string
	long   []string // blob ids of msg_a1
	short  string   // blob id of msg_a2
}

const (
	audioMsgLong  = "msg_a1"
	audioMsgShort = "msg_a2"
)

// seedAudioChat stores the fixture's chat for userID, with a sealed blob row behind
// every audio url, the way the run commit leaves them.
func (e *chatBlobEnv) seedAudioChat(t *testing.T, userID string, nonce int) audioChatFixture {
	t.Helper()
	blobID := func(n int) string { return fmt.Sprintf("blob_%032x", nonce*100+n) }
	part := func(id string) string {
		return fmt.Sprintf(`{"type":"audio_url","audio_url":{"url":"%s%s"}}`, chatBlobPath, id)
	}
	fx := audioChatFixture{long: []string{blobID(1), blobID(2), blobID(3)}, short: blobID(4)}
	fx.chatID = e.newChat(t, userID, fmt.Sprintf(`{"settings":{"kind":"speech"},"messages":[`+
		`{"id":"msg_u1","role":"user","content":"read the chapter"},`+
		`{"id":"%s","role":"assistant","status":"complete","content":[%s,%s,%s]},`+
		`{"id":"msg_u2","role":"user","content":"and a line more"},`+
		`{"id":"%s","role":"assistant","status":"complete","content":[%s]}]}`,
		audioMsgLong, part(fx.long[0]), part(fx.long[1]), part(fx.long[2]), audioMsgShort, part(fx.short)))
	for i, id := range append(append([]string{}, fx.long...), fx.short) {
		e.putBlob(t, fx.chatID, id, "audio/wav", []byte(fmt.Sprintf("RIFF....WAVEfmt blob %d", i)))
	}
	return fx
}

const removedMarker = `[{"type":"audio_removed"}]`

// messageContents is the stored content of each message of a chat, by message id,
// as the owner reads it back over GET /chats/{id}.
func (e *chatBlobEnv) messageContents(t *testing.T, chatID string) map[string]string {
	t.Helper()
	rec := chatRequest(t, e.srv, e.cookieA, http.MethodGet, "/api/portal/chats/"+chatID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET chat = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto struct {
		Content struct {
			Messages []struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode chat: %v (%s)", err, rec.Body.String())
	}
	out := map[string]string{}
	for _, m := range dto.Content.Messages {
		out[m.ID] = string(m.Content)
	}
	return out
}

// requireBlobs fails unless every id is served (want true) or is a clean 404 with the
// blob-specific code (want false) to the owner.
func (e *chatBlobEnv) requireBlobs(t *testing.T, want bool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		rec := e.getBlob(e.cookieA, id)
		if want {
			if rec.Code != http.StatusOK {
				t.Fatalf("GET blob %s = %d, want 200 (still stored)", id, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET blob %s = %d, want 404 after the audio was removed", id, rec.Code)
		}
		requireErrorCode(t, rec.Body.String(), codeChatBlobNotFound)
	}
}

// requireUntouched fails unless fx's chat is exactly as seedAudioChat left it: both
// answers still carry their audio urls, no marker anywhere, every blob still served.
func (e *chatBlobEnv) requireUntouched(t *testing.T, fx audioChatFixture) {
	t.Helper()
	contents := e.messageContents(t, fx.chatID)
	for _, id := range []string{audioMsgLong, audioMsgShort} {
		if strings.Contains(contents[id], "audio_removed") || !strings.Contains(contents[id], chatBlobPath) {
			t.Fatalf("%s = %s, want its audio kept", id, contents[id])
		}
	}
	e.requireBlobs(t, true, append(append([]string{}, fx.long...), fx.short)...)
}

func audioPath(chatID, messageID string) string {
	return "/api/portal/chats/" + chatID + "/messages/" + messageID + "/audio"
}

func prunePath(chatID string) string { return "/api/portal/chats/" + chatID + "/audio/prune" }

// DELETE removes ALL the blobs of the one answer it names, leaves a marker in its
// place, keeps the neighbouring answer, and is safe to repeat. A read of a removed
// blob afterwards is a clean 404 with the blob-specific code, which is what the
// portal's player turns into "no longer stored" -- never a 5xx.
func TestDeleteMessageAudioEndpointRemovesTheAnswersAudio(t *testing.T) {
	e := newChatBlobEnv(t)
	fx := e.seedAudioChat(t, "usr_a", 1)

	rec := chatRequest(t, e.srv, e.cookieA, http.MethodDelete, audioPath(fx.chatID, audioMsgLong), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil || !ok.OK {
		t.Fatalf("DELETE body = %s (err %v), want ok:true", rec.Body.String(), err)
	}

	contents := e.messageContents(t, fx.chatID)
	if contents[audioMsgLong] != removedMarker {
		t.Fatalf("deleted answer content = %s, want exactly the marker %s", contents[audioMsgLong], removedMarker)
	}
	if !strings.Contains(contents[audioMsgShort], fx.short) {
		t.Fatalf("the neighbouring answer = %s, want it to keep its audio", contents[audioMsgShort])
	}
	if contents["msg_u1"] != `"read the chapter"` || contents["msg_u2"] != `"and a line more"` {
		t.Fatalf("the user turns changed: %v", contents)
	}
	e.requireBlobs(t, false, fx.long...)
	e.requireBlobs(t, true, fx.short)

	if again := chatRequest(t, e.srv, e.cookieA, http.MethodDelete, audioPath(fx.chatID, audioMsgLong), ""); again.Code != http.StatusOK {
		t.Fatalf("repeated DELETE = %d, body = %s, want a 200 no-op", again.Code, again.Body.String())
	}
	if got := e.messageContents(t, fx.chatID)[audioMsgLong]; got != removedMarker {
		t.Fatalf("repeated DELETE changed the answer to %s", got)
	}
}

// POST prune keeps the newest keep_last answers that hold audio and removes the rest,
// all of their blobs with them; 0 keeps none.
func TestPruneChatAudioEndpoint(t *testing.T) {
	t.Run("keep_last 1 keeps the newest answer", func(t *testing.T) {
		e := newChatBlobEnv(t)
		fx := e.seedAudioChat(t, "usr_a", 1)

		rec := chatRequest(t, e.srv, e.cookieA, http.MethodPost, prunePath(fx.chatID), `{"keep_last":1}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("prune = %d, body = %s, want 200", rec.Code, rec.Body.String())
		}
		contents := e.messageContents(t, fx.chatID)
		if contents[audioMsgLong] != removedMarker {
			t.Fatalf("older answer = %s, want the marker", contents[audioMsgLong])
		}
		if !strings.Contains(contents[audioMsgShort], fx.short) {
			t.Fatalf("newest answer = %s, want it kept", contents[audioMsgShort])
		}
		e.requireBlobs(t, false, fx.long...)
		e.requireBlobs(t, true, fx.short)
	})
	t.Run("keep_last 0 removes all", func(t *testing.T) {
		e := newChatBlobEnv(t)
		fx := e.seedAudioChat(t, "usr_a", 1)

		rec := chatRequest(t, e.srv, e.cookieA, http.MethodPost, prunePath(fx.chatID), `{"keep_last":0}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("prune = %d, body = %s, want 200", rec.Code, rec.Body.String())
		}
		contents := e.messageContents(t, fx.chatID)
		if contents[audioMsgLong] != removedMarker || contents[audioMsgShort] != removedMarker {
			t.Fatalf("answers = %s / %s, want both the marker", contents[audioMsgLong], contents[audioMsgShort])
		}
		e.requireBlobs(t, false, append(append([]string{}, fx.long...), fx.short)...)
	})
}

// A body that is not exactly {"keep_last": <whole number >= 0>} is a 400 that stores
// nothing. Missing and negative are the validation's own code; a value that is not a
// whole number, or not JSON at all, is the decoder's.
func TestPruneChatAudioEndpointRejectsAnInvalidCount(t *testing.T) {
	e := newChatBlobEnv(t)
	fx := e.seedAudioChat(t, "usr_a", 1)

	cases := []struct {
		name, body, code string
	}{
		{"missing", `{}`, "portal.chat_audio_keep_last_invalid"},
		{"null", `{"keep_last":null}`, "portal.chat_audio_keep_last_invalid"},
		{"negative", `{"keep_last":-1}`, "portal.chat_audio_keep_last_invalid"},
		{"fraction", `{"keep_last":1.5}`, codeRequestInvalidJSON},
		{"string", `{"keep_last":"1"}`, codeRequestInvalidJSON},
		{"not json", `keep_last=1`, codeRequestInvalidJSON},
		{"empty", ``, codeRequestInvalidJSON},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := chatRequest(t, e.srv, e.cookieA, http.MethodPost, prunePath(fx.chatID), c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("prune(%q) = %d, body = %s, want 400", c.body, rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), c.code)
			e.requireUntouched(t, fx)
		})
	}
}

// Both mutations rewrite the WHOLE chat document and hard-delete chat_blobs rows, so
// -- exactly like PUT /chats -- they are refused while a run is active for the chat:
// the run owns the transcript tail, and a rewrite racing it would overwrite its
// checkpoint or its terminal commit and delete audio it is about to reference. The
// refusal is the same 409 portal.chat_run_active PUT answers with, it leaves the chat
// untouched, and it is per chat: another idle chat of the same user is not blocked, and
// once the run is over the same call goes through. [Review Focus]
func TestAudioMutationsRefusedWhileRunActive(t *testing.T) {
	e := newChatBlobEnv(t)
	e.srv.ChatRuns = NewChatRunRegistry(5)
	busy := e.seedAudioChat(t, "usr_a", 1)
	idle := e.seedAudioChat(t, "usr_a", 2)
	run, err := e.srv.ChatRuns.start("usr_a", busy.chatID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	for _, c := range []struct {
		name, method, path, body string
	}{
		{"delete one answer's audio", http.MethodDelete, audioPath(busy.chatID, audioMsgLong), ""},
		{"retroactive prune", http.MethodPost, prunePath(busy.chatID), `{"keep_last":0}`},
	} {
		t.Run("refused: "+c.name, func(t *testing.T) {
			rec := chatRequest(t, e.srv, e.cookieA, c.method, c.path, c.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s with a run active = %d, body = %s, want 409", c.name, rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "portal.chat_run_active")
			e.requireUntouched(t, busy)
		})
	}

	// The refusal is for the chat with the run, not a blanket one: the user's idle chat
	// is mutated as usual.
	if rec := chatRequest(t, e.srv, e.cookieA, http.MethodDelete, audioPath(idle.chatID, audioMsgLong), ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE on an idle chat = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	e.requireBlobs(t, false, idle.long...)

	// Once the run is retired (finishRun does this on every terminal path) the chat is
	// mutable again.
	e.srv.ChatRuns.retire(run)
	if rec := chatRequest(t, e.srv, e.cookieA, http.MethodPost, prunePath(busy.chatID), `{"keep_last":0}`); rec.Code != http.StatusOK {
		t.Fatalf("prune after the run ended = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	e.requireBlobs(t, false, append(append([]string{}, busy.long...), busy.short)...)
}

// Another owner's chat -- and a chat that does not exist -- is the same 404 for both
// mutations: byte for byte the same body, so the response does not say whether the
// chat exists, and not the run-active 409 either, so it does not say whether its
// owner has a run going. Nothing of the real owner's is touched.
func TestAudioMutationsAreOwnerScoped(t *testing.T) {
	e := newChatBlobEnv(t)
	e.srv.ChatRuns = NewChatRunRegistry(5)
	fx := e.seedAudioChat(t, "usr_a", 1)
	if _, err := e.srv.ChatRuns.start("usr_a", fx.chatID); err != nil {
		t.Fatalf("start run: %v", err)
	}

	for _, c := range []struct {
		name, method, path, body string
		missing                  string
	}{
		{"delete", http.MethodDelete, audioPath(fx.chatID, audioMsgLong), "", audioPath("chat_missing", audioMsgLong)},
		{"prune", http.MethodPost, prunePath(fx.chatID), `{"keep_last":0}`, prunePath("chat_missing")},
	} {
		t.Run(c.name, func(t *testing.T) {
			foreign := chatRequest(t, e.srv, e.cookieB, c.method, c.path, c.body)
			if foreign.Code != http.StatusNotFound {
				t.Fatalf("another owner's %s = %d, body = %s, want 404", c.name, foreign.Code, foreign.Body.String())
			}
			requireErrorCode(t, foreign.Body.String(), codeChatGetNotFound)
			missing := chatRequest(t, e.srv, e.cookieB, c.method, c.missing, c.body)
			if missing.Code != foreign.Code || missing.Body.String() != foreign.Body.String() {
				t.Fatalf("a foreign chat answers %d %s but a missing one %d %s: the difference leaks existence",
					foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
			}
		})
	}
	e.requireUntouched(t, fx)
}

// A message the chat does not hold is 404 too, with the chat's own not-found code.
func TestDeleteMessageAudioEndpointUnknownMessageIsNotFound(t *testing.T) {
	e := newChatBlobEnv(t)
	fx := e.seedAudioChat(t, "usr_a", 1)

	rec := chatRequest(t, e.srv, e.cookieA, http.MethodDelete, audioPath(fx.chatID, "msg_nope"), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE of an unknown message = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), codeChatGetNotFound)
	e.requireUntouched(t, fx)
}

// The routes are exactly the two documented shapes, each answering only its own
// method, and a state-changing call needs the same credentials as every other chat
// write: a session cookie alone is not enough without the CSRF header.
func TestAudioMutationRoutesMethodsAndAuth(t *testing.T) {
	e := newChatBlobEnv(t)
	fx := e.seedAudioChat(t, "usr_a", 1)

	for _, c := range []struct {
		method, path, allow string
	}{
		{http.MethodGet, audioPath(fx.chatID, audioMsgLong), http.MethodDelete},
		{http.MethodPost, audioPath(fx.chatID, audioMsgLong), http.MethodDelete},
		{http.MethodPut, audioPath(fx.chatID, audioMsgLong), http.MethodDelete},
		{http.MethodGet, prunePath(fx.chatID), http.MethodPost},
		{http.MethodDelete, prunePath(fx.chatID), http.MethodPost},
		{http.MethodPut, prunePath(fx.chatID), http.MethodPost},
	} {
		rec := chatRequest(t, e.srv, e.cookieA, c.method, c.path, `{"keep_last":0}`)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != c.allow {
			t.Fatalf("%s %s = %d (Allow %q), want 405 with Allow %q", c.method, c.path, rec.Code, rec.Header().Get("Allow"), c.allow)
		}
	}

	// Neighbouring paths are not routes.
	for _, path := range []string{
		"/api/portal/chats/" + fx.chatID + "/audio",
		"/api/portal/chats/" + fx.chatID + "/messages/" + audioMsgLong,
		"/api/portal/chats/" + fx.chatID + "/messages/" + audioMsgLong + "/audio/prune",
		"/api/portal/chats/" + fx.chatID + "/audio/prune/now",
	} {
		rec := chatRequest(t, e.srv, e.cookieA, http.MethodDelete, path, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("DELETE %s = %d, want 404", path, rec.Code)
		}
	}

	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodDelete, audioPath(fx.chatID, audioMsgLong), ""},
		{http.MethodPost, prunePath(fx.chatID), `{"keep_last":0}`},
	} {
		noCSRF := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		noCSRF.AddCookie(e.cookieA)
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, noCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s without the CSRF header = %d, want 403", c.method, c.path, rec.Code)
		}

		anonymous := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		anonymous.Header.Set(csrfHeaderName, "1")
		rec = httptest.NewRecorder()
		e.srv.ServeHTTP(rec, anonymous)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without credentials = %d, want 401", c.method, c.path, rec.Code)
		}
	}
	e.requireUntouched(t, fx)
}
