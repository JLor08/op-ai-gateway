// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// newChatTestServer builds a server whose portal service has a MemoryChatStore
// + capture cipher wired, so chat create/save seal (KeyVersion 1) and get opens.
func newChatTestServer(t *testing.T) (*Server, *portal.MemoryDirectory) {
	t.Helper()
	ts := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(ts)
	acct := account.NewService(account.Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir}, account.Config{
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de",
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	cipher, err := capture.New(captureDetailKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore,
		SystemSettings: portal.NewMemorySystemSettings(), UIPrefs: portal.NewMemoryUIPreferences(),
		Chats: store.NewMemoryChatStore(0), Cipher: cipher,
	})
	srv := New(ServerDeps{
		Tokens: ts, Usage: recorder, Portal: svc, Account: acct, Routes: routeStore,
		CookieSecure: false, SessionMaxAge: 24 * time.Hour, PublicURL: "http://localhost:8080",
	})
	return srv, dir
}

// chatRequest issues a session-cookied request (CSRF header set for
// state-changing methods) and returns the recorder.
func chatRequest(t *testing.T, srv *Server, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.AddCookie(cookie)
	if method != http.MethodGet {
		r.Header.Set(csrfHeaderName, "1")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func TestPortalChatsCRUDHappyPath(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_c", "c@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "c@example.test", "password-1")

	// CREATE
	createBody := `{"title":"Hello","content":{"settings":{"model":"m"},"messages":[{"role":"user","content":"hi"}]}}`
	rec := chatRequest(t, srv, cookie, http.MethodPost, "/api/portal/chats", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID      string          `json:"id"`
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v (%s)", err, rec.Body.String())
	}
	if created.ID == "" || !strings.HasPrefix(created.ID, "chat_") {
		t.Fatalf("created id = %q, want chat_ prefix", created.ID)
	}
	if created.Title != "Hello" {
		t.Fatalf("created title = %q, want Hello", created.Title)
	}

	// LIST
	rec = chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != created.ID {
		t.Fatalf("list = %#v, want single chat %s", list.Data, created.ID)
	}

	// GET (opens/decrypts) — content must round-trip verbatim.
	rec = chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}
	wantContent := `{"settings":{"model":"m"},"messages":[{"role":"user","content":"hi"}]}`
	if string(got.Content) != wantContent {
		t.Fatalf("get content = %s, want %s", got.Content, wantContent)
	}

	// SAVE (PUT both title + content)
	saveBody := `{"title":"Renamed","content":{"messages":[]}}`
	rec = chatRequest(t, srv, cookie, http.MethodPut, "/api/portal/chats/"+created.ID, saveBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats/"+created.ID, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get-after-save: %v", err)
	}
	if string(got.Content) != `{"messages":[]}` {
		t.Fatalf("content after save = %s, want {\"messages\":[]}", got.Content)
	}

	// DELETE
	rec = chatRequest(t, srv, cookie, http.MethodDelete, "/api/portal/chats/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var okBody map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &okBody); err != nil || !okBody["ok"] {
		t.Fatalf("delete body = %s (err %v), want ok:true", rec.Body.String(), err)
	}

	// GET after delete -> 404
	rec = chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", rec.Code)
	}
}

func TestPortalChatsOwnershipIsolationEndpoint(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_a", "a@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_b", "b@example.test", "password-1", "user")
	cookieA := loginCookie(t, srv, "a@example.test", "password-1")
	cookieB := loginCookie(t, srv, "b@example.test", "password-1")

	rec := chatRequest(t, srv, cookieA, http.MethodPost, "/api/portal/chats", `{"title":"A","content":{"x":1}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("A create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)

	// B cannot GET or DELETE A's chat -> 404 (no existence leak).
	if rec := chatRequest(t, srv, cookieB, http.MethodGet, "/api/portal/chats/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("B get A's chat = %d, want 404", rec.Code)
	}
	if rec := chatRequest(t, srv, cookieB, http.MethodDelete, "/api/portal/chats/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("B delete A's chat = %d, want 404", rec.Code)
	}
	// B's list is empty.
	rec = chatRequest(t, srv, cookieB, http.MethodGet, "/api/portal/chats", "")
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Fatalf("B list = %#v, want empty", list.Data)
	}
}

func TestPortalChatsTitleTooLongRejected(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_c", "c@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "c@example.test", "password-1")

	longTitle := strings.Repeat("x", 201)
	body := `{"title":"` + longTitle + `","content":{}}`
	rec := chatRequest(t, srv, cookie, http.MethodPost, "/api/portal/chats", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create long-title status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}
