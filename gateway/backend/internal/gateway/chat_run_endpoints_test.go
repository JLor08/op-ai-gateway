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
	"op-ai-gateway/internal/portal"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWritePortalRunError(t *testing.T) {
	cases := map[error]int{
		ErrRunAlreadyActive:    409,
		ErrTooManyRuns:         429,
		portal.ErrChatNotFound: 404,
		portal.ErrChatTooLarge: 400,
	}
	for err, want := range cases {
		w := httptest.NewRecorder()
		writePortalRunError(w, err)
		if w.Code != want {
			t.Fatalf("err %v -> %d, want %d", err, w.Code, want)
		}
	}
}

// authBearer authenticates a request as the harness dev token (usr_dev,
// gateway:use). Bearer auth needs no CSRF header (CSRF applies to cookie auth).
func authBearer(r *http.Request, secret string) {
	r.Header.Set("Authorization", "Bearer "+secret)
}

// syncRecorder is a minimal http.ResponseWriter + http.Flusher whose buffer is
// mutex-guarded, so a handler running in a goroutine (a live SSE stream) can be
// polled safely from the test goroutine. It does not implement the write-
// deadline setter, so the handler's SetWriteDeadline is a harmless no-op.
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	code   int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{header: http.Header{}, code: http.StatusOK}
}

func (r *syncRecorder) Header() http.Header { return r.header }

func (r *syncRecorder) WriteHeader(code int) {
	r.mu.Lock()
	r.code = code
	r.mu.Unlock()
}

func (r *syncRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}

func (r *syncRecorder) Flush() {}

func (r *syncRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// startRunViaHandler drives POST /api/portal/chats/{chatID}/runs through the
// dispatcher and returns the recorder.
func startRunViaHandler(srv *Server, chatID, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/portal/chats/"+chatID+"/runs", strings.NewReader(body))
	authBearer(r, "dev-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r)
	return w
}

// TestStartRunCreatesRunAndConflicts: the first start launches a run (201); a
// second start for the same chat while it is still active is rejected 409. A
// paced provider keeps the first run streaming across the second request.
func TestStartRunCreatesRunAndConflicts(t *testing.T) {
	srv, _, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 20, gap: 20 * time.Millisecond})

	w1 := startRunViaHandler(srv, chatID, `{"user_message":"hi","settings":{"model":"qwen-coder"}}`)
	if w1.Code != http.StatusCreated {
		t.Fatalf("start: got %d body %s", w1.Code, w1.Body.String())
	}
	var res startRunResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if res.RunID == "" || res.ChatID != chatID || res.Status != "running" {
		t.Fatalf("unexpected start response: %+v", res)
	}

	w2 := startRunViaHandler(srv, chatID, `{"user_message":"again","settings":{"model":"qwen-coder"}}`)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second start: expected 409, got %d body %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "portal.chat_run_active") {
		t.Fatalf("expected chat_run_active code, got %s", w2.Body.String())
	}
}

// TestSameChatSecondStartAfterTerminal is the endpoint-level regression for the
// registry counting terminal runs: a finished run lingers until its 30s
// eviction, so before retire() the chat's next turn within that window got a
// spurious 409 (chat_run_active). With finishRun retiring the run on terminal,
// a second start on the SAME chat after the first completes succeeds (201),
// while the first run stays reachable by id for a late terminal snapshot.
func TestSameChatSecondStartAfterTerminal(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t) // instant mock -> completes at once

	w1 := startRunViaHandler(srv, chatID, `{"user_message":"hi","settings":{"model":"qwen-coder"}}`)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first start: got %d body %s", w1.Code, w1.Body.String())
	}
	var res1 startRunResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &res1); err != nil {
		t.Fatalf("decode first start: %v", err)
	}
	waitFor(t, func() bool {
		run := srv.ChatRuns.GetByID(owner.UserID, res1.RunID)
		return run != nil && run.statusValue() != "running"
	})

	// The second turn in the SAME chat, while the first still lingers pre-eviction.
	w2 := startRunViaHandler(srv, chatID, `{"user_message":"again","settings":{"model":"qwen-coder"}}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second same-chat start after terminal: expected 201, got %d body %s", w2.Code, w2.Body.String())
	}
	// The first (terminal) run is still reachable by id for a late snapshot.
	if srv.ChatRuns.GetByID(owner.UserID, res1.RunID) == nil {
		t.Fatal("first terminal run should still serve late snapshots until eviction")
	}
}

// TestRejectedStartDoesNotMutateTranscript is a regression test for the
// prepare-then-reserve ordering bug: a start rejected as already-active (409)
// must not have appended a user turn. We count user-role messages before and
// after the rejected second start (the first run's own background assistant
// commit adds an assistant message, never a user one, so the user count is a
// stable signal). Pre-fix the rejected start committed a second user message
// and this fails; post-fix the count is unchanged.
func TestRejectedStartDoesNotMutateTranscript(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})

	w1 := startRunViaHandler(srv, chatID, `{"user_message":"hi","settings":{"model":"qwen-coder"}}`)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first start: got %d body %s", w1.Code, w1.Body.String())
	}

	before, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatalf("GetChat before: %v", err)
	}
	userBefore := strings.Count(string(before.Content), `"role":"user"`)

	w2 := startRunViaHandler(srv, chatID, `{"user_message":"again","settings":{"model":"qwen-coder"}}`)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second start: expected 409, got %d body %s", w2.Code, w2.Body.String())
	}

	after, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatalf("GetChat after: %v", err)
	}
	userAfter := strings.Count(string(after.Content), `"role":"user"`)
	if userAfter != userBefore {
		t.Fatalf("rejected start mutated transcript: user messages %d -> %d\n after=%s",
			userBefore, userAfter, string(after.Content))
	}
	if strings.Contains(string(after.Content), "again") {
		t.Fatalf("rejected start committed its user message: %s", string(after.Content))
	}
}

// TestSubscribeReplaysTerminalSnapshot: after a completed run, subscribing
// replays the whole answer in the snapshot event and the handler returns
// immediately (a terminal run's snapshot short-circuits the tail loop), so a
// plain single-threaded recorder never blocks.
func TestSubscribeReplaysTerminalSnapshot(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	r := httptest.NewRequest(http.MethodGet, "/api/portal/chats/"+chatID+"/runs/"+run.ID+"/events", nil)
	authBearer(r, "dev-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r) // must return without blocking

	body := w.Body.String()
	if !strings.Contains(body, "event: snapshot") {
		t.Fatalf("missing snapshot event: %s", body)
	}
	if !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("snapshot did not carry completed status: %s", body)
	}
	if !strings.Contains(body, "Mock response") {
		t.Fatalf("snapshot did not replay content: %s", body)
	}
}

// TestSubscribeMidRunShowsRunningSnapshot: subscribing while a paced run is still
// generating yields a snapshot event carrying status "running". The handler
// blocks in its tail loop, so it runs in a goroutine against a mutex-guarded
// recorder; cancelling the request context unblocks it.
func TestSubscribeMidRunShowsRunningSnapshot(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/portal/chats/"+chatID+"/runs/"+run.ID+"/events", nil).WithContext(ctx)
	authBearer(r, "dev-secret")
	w := newSyncRecorder()

	done := make(chan struct{})
	go func() {
		srv.handlePortalChatItem(w, r)
		close(done)
	}()

	waitFor(t, func() bool { return strings.Contains(w.body(), "event: snapshot") })
	if !strings.Contains(w.body(), `"status":"running"`) {
		t.Fatalf("mid-run snapshot missing running status: %s", w.body())
	}
	cancel() // unblock the tail loop
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
}

// TestCancelRunStopsIt: POST cancel returns 200 and the run transitions to
// canceled.
func TestCancelRunStopsIt(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/portal/chats/"+chatID+"/runs/"+run.ID+"/cancel", nil)
	authBearer(r, "dev-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: got %d body %s", w.Code, w.Body.String())
	}
	waitFor(t, func() bool { return run.statusValue() == "canceled" })
}

// TestActiveRunsListsUsersRuns: GET runs/active returns the caller's active run
// with its chat id.
func TestActiveRunsListsUsersRuns(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/portal/chats/runs/active", nil)
	authBearer(r, "dev-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("active: got %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, run.ID) || !strings.Contains(body, chatID) {
		t.Fatalf("active list missing run/chat: %s", body)
	}
}

// TestActiveRunsExcludesFinishedRun: a run that has finished but still lingers
// in the registry (within its eviction grace period) is NOT reported by GET
// runs/active. "Active" means currently running — the reopen path must not
// resubscribe to a terminal run. The instant dev mock completes the run
// immediately; we wait for it to go terminal (but it is not yet evicted), then
// assert the active list is empty.
func TestActiveRunsExcludesFinishedRun(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t) // instant mock -> run completes at once
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	// The finished run still lingers in the registry (eviction is 30s away).
	if srv.ChatRuns.GetByID(owner.UserID, run.ID) == nil {
		t.Fatal("finished run should still be registered until eviction")
	}

	r := httptest.NewRequest(http.MethodGet, "/api/portal/chats/runs/active", nil)
	authBearer(r, "dev-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("active: got %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []activeRunDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode active response: %v (body %s)", err, w.Body.String())
	}
	if len(resp.Data) != 0 {
		t.Fatalf("active list should exclude the finished run, got %+v", resp.Data)
	}
}

// TestDeleteChatCancelsActiveRun: deleting a chat with an active run cancels the
// run before removing the chat.
func TestDeleteChatCancelsActiveRun(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/portal/chats/"+chatID, nil)
	authBearer(r, "dev-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: got %d body %s", w.Code, w.Body.String())
	}
	waitFor(t, func() bool { return run.statusValue() == "canceled" })
}

// TestChatRunEventsForeignUserIs404: a run is scoped to its owner; another
// authenticated user cannot subscribe to it — the run appears not to exist
// (no existence leak).
func TestChatRunEventsForeignUserIs404(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	// Register a second user's bearer token in the same server.
	ts, ok := srv.Tokens.(*auth.TokenStore)
	if !ok {
		t.Fatalf("harness token store is %T, want *auth.TokenStore", srv.Tokens)
	}
	ts.AddPlainToken(auth.Token{
		ID: "tok_other", UserID: "usr_other", Name: "Other", Active: true,
		Scopes: []string{"gateway:use"},
	}, "other-secret")

	r := httptest.NewRequest(http.MethodGet, "/api/portal/chats/"+chatID+"/runs/"+run.ID+"/events", nil)
	authBearer(r, "other-secret")
	w := httptest.NewRecorder()
	srv.handlePortalChatItem(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign subscribe: expected 404, got %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "portal.chat_run_not_found") {
		t.Fatalf("expected chat_run_not_found, got %s", w.Body.String())
	}
	// The owner is still allowed (sanity: the run really exists).
	if srv.ChatRuns.GetByID(owner.UserID, run.ID) == nil {
		t.Fatal("owner lost access to their own run")
	}
}
