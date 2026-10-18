// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	chatBlobPath = "/api/portal/chat-blobs/"
	// The blob endpoint's own not-found code, and the chat endpoints' one it
	// must not be confused with.
	codeChatBlobNotFound = "portal.chat_blob_not_found"
	codeChatGetNotFound  = "portal.chat_not_found"
)

// touchCountingSessions counts TouchSession writes on top of the real session
// store, so a test can tell an endpoint that refreshes the session on every hit
// from one that does not.
type touchCountingSessions struct {
	account.SessionStore
	touches atomic.Int64
}

func (s *touchCountingSessions) TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error {
	s.touches.Add(1)
	return s.SessionStore.TouchSession(ctx, id, lastSeenAt)
}

// chatBlobEnv is a gateway over a held MemoryChatStore + cipher (so a test can
// seed sealed chat_blobs rows the way the run commit stores them) and a
// session store that counts touches, with two logged-in users, usr_a and usr_b.
type chatBlobEnv struct {
	srv      *Server
	chats    *store.MemoryChatStore
	cipher   *capture.Cipher
	sessions *touchCountingSessions
	cookieA  *http.Cookie
	cookieB  *http.Cookie
}

func newChatBlobEnv(t *testing.T) *chatBlobEnv {
	t.Helper()
	ts := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(ts)
	sessions := &touchCountingSessions{SessionStore: dir}
	acct := account.NewService(account.Deps{Users: dir, Sessions: sessions, SetPasswordTokens: dir}, account.Config{
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de",
	})
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	chats := store.NewMemoryChatStore(0)
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore,
		SystemSettings: portal.NewMemorySystemSettings(), UIPrefs: portal.NewMemoryUIPreferences(),
		Chats: chats, Cipher: cipher,
	})
	srv := New(ServerDeps{
		Tokens: ts, Usage: recorder, Portal: svc, Account: acct, Routes: routeStore,
		CookieSecure: false, SessionMaxAge: 24 * time.Hour, PublicURL: "http://localhost:8080",
	})
	seedLoginUser(t, dir, "usr_a", "a@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_b", "b@example.test", "password-1", "user")
	return &chatBlobEnv{
		srv: srv, chats: chats, cipher: cipher, sessions: sessions,
		cookieA: loginCookie(t, srv, "a@example.test", "password-1"),
		cookieB: loginCookie(t, srv, "b@example.test", "password-1"),
	}
}

// newChat creates a chat for userID through the portal service, with content
// as its (opaque) document, and returns its id.
func (e *chatBlobEnv) newChat(t *testing.T, userID, content string) string {
	t.Helper()
	dto, err := e.srv.Portal.CreateChat(context.Background(), auth.Token{UserID: userID, Scopes: []string{scopeGatewayUse}},
		portal.CreateChatRequest{Content: json.RawMessage(content)})
	if err != nil {
		t.Fatalf("CreateChat(%s): %v", userID, err)
	}
	return dto.ID
}

// putBlob stores raw as chat_blobs row id of chatID, sealed with the cipher
// exactly as the run commit stores it.
func (e *chatBlobEnv) putBlob(t *testing.T, chatID, id, contentType string, raw []byte) {
	t.Helper()
	if err := e.chats.PutChatBlob(context.Background(), store.ChatBlob{
		ID: id, ChatID: chatID, KeyVersion: capture.KeyVersion, Blob: e.cipher.Seal(raw),
		ContentType: contentType, ByteLen: int64(len(raw)), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutChatBlob(%s): %v", id, err)
	}
}

// getBlob issues GET /api/portal/chat-blobs/{id} as cookie (none when nil).
func (e *chatBlobEnv) getBlob(cookie *http.Cookie, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, chatBlobPath+id, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

// pngLike is bytes that are not valid UTF-8 text and carry a PNG signature, so
// a body that round-tripped through a string or JSON encoder would not compare
// equal.
func pngLike(n int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	for i := len(b); i < n; i++ {
		b = append(b, byte(i*37+11))
	}
	return b
}

// The owner's GET returns the raw bytes with every hardening header: a
// same-origin, cookie-authenticated, navigable url whose body came from a user
// must not be able to run as a page.
func TestChatBlobEndpointServesTheOwnersImageHardened(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	raw := pngLike(3000)
	e.putBlob(t, chatID, "blob_owner", "image/png", raw)

	rec := e.getBlob(e.cookieA, "blob_owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %.200s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("served %d bytes, want the %d stored (unsealed) bytes", rec.Body.Len(), len(raw))
	}
	want := map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'",
		"Content-Disposition":     "inline",
		"Cache-Control":           "private, no-store",
		"Content-Length":          strconv.Itoa(len(raw)),
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	// One value per header: a duplicated Content-Type is a sniffing lever.
	for _, name := range []string{"Content-Type", "X-Content-Type-Options", "Content-Security-Policy", "Content-Disposition", "Cache-Control"} {
		if n := len(rec.Header().Values(name)); n != 1 {
			t.Errorf("%s has %d values, want exactly 1", name, n)
		}
	}
}

// Each allow-listed type is served as itself.
func TestChatBlobEndpointServesEachAllowListedType(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	for i, contentType := range []string{"image/png", "image/jpeg", "image/webp", "image/gif", "audio/wav"} {
		id := fmt.Sprintf("blob_type_%d", i)
		e.putBlob(t, chatID, id, contentType, pngLike(64+i))
		rec := e.getBlob(e.cookieA, id)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != contentType {
			t.Errorf("%s: status %d, Content-Type %q", contentType, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// A speech reply's WAV is served as audio/wav with the same hardening as an
// image, byte for byte: the stored (allow-listed) type and nothing else as
// Content-Type, nosniff, a CSP that forbids everything, inline disposition and no
// caching of the decrypted bytes.
func TestChatBlobEndpointServesASpeechReplyHardened(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	raw := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), pngLike(3000)...)
	e.putBlob(t, chatID, "blob_speech", "audio/wav", raw)

	rec := e.getBlob(e.cookieA, "blob_speech")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %.200s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("served %d bytes, want the %d stored (unsealed) bytes", rec.Body.Len(), len(raw))
	}
	want := map[string]string{
		"Content-Type":            "audio/wav",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'",
		"Content-Disposition":     "inline",
		"Cache-Control":           "private, no-store",
		"Content-Length":          strconv.Itoa(len(raw)),
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}

	// Another user's speech reply is the same 404 as any other blob.
	if other := e.getBlob(e.cookieB, "blob_speech"); other.Code != http.StatusNotFound || errorCodeOf(t, other.Body.Bytes()) != codeChatBlobNotFound {
		t.Errorf("non-owner: status %d, body %.200s; want 404 %s", other.Code, other.Body.String(), codeChatBlobNotFound)
	}
}

// A row whose type is not an allow-listed type is never served, even to its
// owner: the store-time allow-list is not the only line of defense. Only the
// listed audio types are served (wav, and the transcription input formats), so an
// unlisted subtype and the other spellings of a listed one are refused too.
func TestChatBlobEndpointNeverServesAnUnlistedType(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	for i, contentType := range []string{"text/html", "image/svg+xml", "application/javascript", "image/png; charset=utf-8", "audio/flac", "audio/x-wav", "audio/wav; codecs=1", "audio/webm; codecs=opus"} {
		id := fmt.Sprintf("blob_bad_%d", i)
		e.putBlob(t, chatID, id, contentType, []byte(`<html><script>alert(document.cookie)</script></html>`))
		rec := e.getBlob(e.cookieA, id)
		if rec.Code != http.StatusNotFound || errorCodeOf(t, rec.Body.Bytes()) != codeChatBlobNotFound {
			t.Errorf("%s: status %d, body %.200s; want 404 %s", contentType, rec.Code, rec.Body.String(), codeChatBlobNotFound)
		}
		if strings.Contains(rec.Body.String(), "<script>") || rec.Header().Get("Content-Type") == contentType {
			t.Errorf("%s: the unlisted payload or type was echoed: %s %.200s", contentType, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
}

// Another user's blob is a 404 with the blob-specific code, and the answer is
// byte-for-byte the one a never-stored id gets, so the endpoint cannot be used
// to probe which ids exist.
func TestChatBlobEndpointNonOwnerIs404AndIndistinguishableFromMissing(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	raw := pngLike(512)
	e.putBlob(t, chatID, "blob_of_a", "image/png", raw)

	foreign := e.getBlob(e.cookieB, "blob_of_a")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("non-owner status = %d, want 404", foreign.Code)
	}
	if code := errorCodeOf(t, foreign.Body.Bytes()); code != codeChatBlobNotFound || code == codeChatGetNotFound {
		t.Fatalf("non-owner code = %q, want the blob-specific %q (not %q)", code, codeChatBlobNotFound, codeChatGetNotFound)
	}
	if bytes.Contains(foreign.Body.Bytes(), raw[:16]) {
		t.Fatal("the non-owner's response carries the other user's bytes")
	}

	missing := e.getBlob(e.cookieB, "blob_00000000000000000000000000000000")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("never-stored id status = %d, want 404", missing.Code)
	}
	if foreign.Body.String() != missing.Body.String() {
		t.Fatalf("a foreign blob and a missing blob answer differently (existence leak):\n foreign: %s\n missing: %s", foreign.Body.String(), missing.Body.String())
	}
	if foreign.Header().Get("Content-Type") != missing.Header().Get("Content-Type") {
		t.Fatalf("content types differ: %q vs %q", foreign.Header().Get("Content-Type"), missing.Header().Get("Content-Type"))
	}

	// The owner is unaffected.
	if rec := e.getBlob(e.cookieA, "blob_of_a"); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("owner: status %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

// The grafted-url attack. Create (and a bare autosave) store the urls a
// transcript carries without validating them, so user B can put A's
// /api/portal/chat-blobs/{id} into B's OWN chat. Following that url from B's
// session must still 404: ownership is decided on the blob's own chat, not on
// the chat that references it.
func TestChatBlobEndpointGraftedForeignIdInOwnChatIs404(t *testing.T) {
	e := newChatBlobEnv(t)
	chatA := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	raw := pngLike(777)
	e.putBlob(t, chatA, "blob_secret_of_a", "image/png", raw)

	graft := `{"settings":{},"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + chatBlobPath + `blob_secret_of_a"}}]}]}`
	chatB := e.newChat(t, "usr_b", graft)

	// The graft really landed: B reads their own chat back and it names A's blob.
	req := httptest.NewRequest(http.MethodGet, "/api/portal/chats/"+chatB, nil)
	req.AddCookie(e.cookieB)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), chatBlobPath+"blob_secret_of_a") {
		t.Fatalf("B's chat = %d %.300s; the test needs it to carry the grafted url", rec.Code, rec.Body.String())
	}

	// Following the url the way the browser would, as B.
	got := e.getBlob(e.cookieB, "blob_secret_of_a")
	if got.Code != http.StatusNotFound || errorCodeOf(t, got.Body.Bytes()) != codeChatBlobNotFound {
		t.Fatalf("grafted foreign id: status %d, body %.200s; want 404 %s", got.Code, got.Body.String(), codeChatBlobNotFound)
	}
	if bytes.Contains(got.Body.Bytes(), raw[:16]) {
		t.Fatal("the grafted url served the other user's bytes")
	}
	// The same graft inside the owner's session is, of course, the owner's.
	if rec := e.getBlob(e.cookieA, "blob_secret_of_a"); rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d", rec.Code)
	}
}

// A blob whose chat was deleted is gone with it.
func TestChatBlobEndpointDeletedChatIs404(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	e.putBlob(t, chatID, "blob_doomed", "image/png", pngLike(64))
	if rec := e.getBlob(e.cookieA, "blob_doomed"); rec.Code != http.StatusOK {
		t.Fatalf("before delete: status %d", rec.Code)
	}
	if err := e.srv.Portal.DeleteChat(context.Background(), auth.Token{UserID: "usr_a", Scopes: []string{scopeGatewayUse}}, chatID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	rec := e.getBlob(e.cookieA, "blob_doomed")
	if rec.Code != http.StatusNotFound || errorCodeOf(t, rec.Body.Bytes()) != codeChatBlobNotFound {
		t.Fatalf("after delete: status %d, body %.200s; want 404 %s", rec.Code, rec.Body.String(), codeChatBlobNotFound)
	}
}

// Only GET: <img> never needs another method, so every other one is 405 with
// the Allow header, whoever asks.
func TestChatBlobEndpointRejectsNonGet(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	e.putBlob(t, chatID, "blob_m", "image/png", pngLike(64))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
		req := httptest.NewRequest(method, chatBlobPath+"blob_m", nil)
		req.AddCookie(e.cookieA)
		req.Header.Set(csrfHeaderName, "1")
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Errorf("%s: Allow = %q, want GET", method, allow)
		}
	}
	// And the blob survived all of that.
	if rec := e.getBlob(e.cookieA, "blob_m"); rec.Code != http.StatusOK {
		t.Fatalf("blob after the rejected methods: status %d", rec.Code)
	}
}

// An id that is not a single path segment is not a blob id: the empty id and a
// nested path are the same blob-specific 404, never another handler's answer.
func TestChatBlobEndpointMalformedPathIs404(t *testing.T) {
	e := newChatBlobEnv(t)
	for _, path := range []string{chatBlobPath, chatBlobPath + "blob_x/extra", chatBlobPath + "blob_x/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(e.cookieA)
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || errorCodeOf(t, rec.Body.Bytes()) != codeChatBlobNotFound {
			t.Errorf("%s: status %d, body %.200s; want 404 %s", path, rec.Code, rec.Body.String(), codeChatBlobNotFound)
		}
	}
}

// Session auth, not inference auth: no credentials, or an invalid session, is a
// 401 whether or not the id exists.
func TestChatBlobEndpointRequiresASession(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	e.putBlob(t, chatID, "blob_auth", "image/png", pngLike(64))

	for _, id := range []string{"blob_auth", "blob_nope"} {
		if rec := e.getBlob(nil, id); rec.Code != http.StatusUnauthorized {
			t.Errorf("no credentials, %s: status = %d, want 401", id, rec.Code)
		}
		bad := &http.Cookie{Name: sessionCookieName, Value: "not-a-session"}
		if rec := e.getBlob(bad, id); rec.Code != http.StatusUnauthorized {
			t.Errorf("invalid session, %s: status = %d, want 401", id, rec.Code)
		}
	}
	// An unknown bearer token is refused like any other portal read.
	req := httptest.NewRequest(http.MethodGet, chatBlobPath+"blob_auth", nil)
	req.Header.Set("Authorization", "Bearer not-a-token")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown bearer token: status = %d, want 401", rec.Code)
	}
}

// The session-ladder pin. A chat of N images makes the browser open N <img>
// GETs at once; each used to cost SessionBySecret + UserByID + a TouchSession
// WRITE, against a small connection pool. The serving endpoint's session check
// is passive: N parallel GETs serve N images and issue no session-touch write.
// The interactive chat read is the control, so a counter that never moves
// cannot make this pass.
func TestChatBlobEndpointParallelGetsDoNotStampedeSessionTouches(t *testing.T) {
	const images = 50
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	for i := range images {
		e.putBlob(t, chatID, fmt.Sprintf("blob_many_%d", i), "image/png", pngLike(256+i))
	}

	// Control: an interactive read of the same session does touch it.
	before := e.sessions.touches.Load()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/chats/"+chatID, nil)
	req.AddCookie(e.cookieA)
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("control chat read: status %d", rec.Code)
	}
	if n := e.sessions.touches.Load() - before; n != 1 {
		t.Fatalf("control: the interactive chat read issued %d session touches, want 1 (the counter must be live)", n)
	}

	before = e.sessions.touches.Load()
	results := make([]*httptest.ResponseRecorder, images)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range images {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = e.getBlob(e.cookieA, fmt.Sprintf("blob_many_%d", i))
		}()
	}
	close(start)
	wg.Wait()

	for i, rec := range results {
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), pngLike(256+i)) {
			t.Fatalf("image %d: status %d, %d bytes", i, rec.Code, rec.Body.Len())
		}
	}
	if n := e.sessions.touches.Load() - before; n != 0 {
		t.Fatalf("%d parallel image GETs issued %d session-touch writes, want 0", images, n)
	}
}

// Skipping the touch must not weaken the session check: a revoked session is
// refused by this endpoint like by every other one. (Idle expiry of the passive
// resolve is pinned where it lives, in the account package's tests.)
func TestChatBlobEndpointRefusesARevokedSession(t *testing.T) {
	e := newChatBlobEnv(t)
	chatID := e.newChat(t, "usr_a", `{"settings":{},"messages":[]}`)
	e.putBlob(t, chatID, "blob_idle", "image/png", pngLike(64))

	// Before the logout the same cookie works; the logout revokes the session.
	if got := e.getBlob(e.cookieA, "blob_idle"); got.Code != http.StatusOK {
		t.Fatalf("live session: status = %d, want 200", got.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(e.cookieA)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status %d", rec.Code)
	}
	if got := e.getBlob(e.cookieA, "blob_idle"); got.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: status = %d, want 401", got.Code)
	}
}
