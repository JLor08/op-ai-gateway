// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/apierror"
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

// TestPortalChatPutRefusedWhileRunActive closes the data-loss window a save
// racing an active run opens: while the run registry holds a run for a chat,
// PUT on that chat must be refused rather than overwrite whatever the run has
// already committed (or is about to commit) to the transcript. The guard is
// per (user, chat), not a blanket refusal: a run active on one chat must not
// block a save on a different, idle chat, and a chat with no active run must
// save exactly as before.
func TestPortalChatPutRefusedWhileRunActive(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_c", "c@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "c@example.test", "password-1")
	srv.ChatRuns = NewChatRunRegistry(5)

	// Chat A gets the active run.
	createA := chatRequest(t, srv, cookie, http.MethodPost, "/api/portal/chats",
		`{"title":"A","content":{"messages":[{"role":"user","content":"hi"}]}}`)
	if createA.Code != http.StatusCreated {
		t.Fatalf("create A status = %d, body = %s", createA.Code, createA.Body.String())
	}
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createA.Body.Bytes(), &a); err != nil {
		t.Fatalf("unmarshal create A: %v", err)
	}

	// Chat B is an unrelated, idle chat for the same user.
	createB := chatRequest(t, srv, cookie, http.MethodPost, "/api/portal/chats",
		`{"title":"B","content":{"messages":[]}}`)
	if createB.Code != http.StatusCreated {
		t.Fatalf("create B status = %d, body = %s", createB.Code, createB.Body.String())
	}
	var b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createB.Body.Bytes(), &b); err != nil {
		t.Fatalf("unmarshal create B: %v", err)
	}

	if _, err := srv.ChatRuns.start("usr_c", a.ID); err != nil {
		t.Fatalf("start run on A: %v", err)
	}

	// PUT on A (the chat with the active run) is refused: 409, and the body's
	// error code is EXACTLY portal.chat_run_active -- decoded and compared for
	// equality, not with strings.Contains, which would also pass for a code
	// that grew an unwanted suffix.
	saveBody := `{"title":"Overwritten","content":{"messages":[]}}`
	putA := chatRequest(t, srv, cookie, http.MethodPut, "/api/portal/chats/"+a.ID, saveBody)
	if putA.Code != http.StatusConflict {
		t.Fatalf("PUT on chat with active run = %d, want 409, body = %s", putA.Code, putA.Body.String())
	}
	var errBody apierror.Body
	if err := json.Unmarshal(putA.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, putA.Body.String())
	}
	if errBody.Error.Code != "portal.chat_run_active" {
		t.Fatalf("error code = %q, want exactly %q", errBody.Error.Code, "portal.chat_run_active")
	}

	// The refused PUT must not have touched chat A's transcript.
	getA := chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats/"+a.ID, "")
	var gotA struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(getA.Body.Bytes(), &gotA); err != nil {
		t.Fatalf("unmarshal get A: %v", err)
	}
	if string(gotA.Content) != `{"messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("chat A content after refused PUT = %s, want unchanged", gotA.Content)
	}

	// PUT on B succeeds: the run is active for A, not B, so the guard must not
	// reject a save on an unrelated chat for the same user.
	putB := chatRequest(t, srv, cookie, http.MethodPut, "/api/portal/chats/"+b.ID, saveBody)
	if putB.Code != http.StatusOK {
		t.Fatalf("PUT on idle chat B = %d, want 200, body = %s", putB.Code, putB.Body.String())
	}
	getB := chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats/"+b.ID, "")
	var gotB struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(getB.Body.Bytes(), &gotB); err != nil {
		t.Fatalf("unmarshal get B: %v", err)
	}
	if string(gotB.Content) != `{"messages":[]}` {
		t.Fatalf("chat B content after PUT = %s, want saved", gotB.Content)
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

// The chat list carries the pre-seal content cap so the portal composer can
// state the remaining capacity instead of hardcoding its own copy of
// MaxChatContentBytes (which would drift from the Go constant silently). A
// zero here would make the portal treat the capacity as unknown, and nothing
// else in the suite would notice.
func TestPortalChatsListCarriesMaxContentBytes(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_cap", "cap@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "cap@example.test", "password-1")

	rec := chatRequest(t, srv, cookie, http.MethodGet, "/api/portal/chats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		MaxContentBytes int `json:"max_content_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v (%s)", err, rec.Body.String())
	}
	if list.MaxContentBytes != portal.MaxChatContentBytes {
		t.Fatalf("max_content_bytes = %d, want %d", list.MaxContentBytes, portal.MaxChatContentBytes)
	}
}
