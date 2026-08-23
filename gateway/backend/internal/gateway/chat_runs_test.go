// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoopbackBaseFromAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080",
		"0.0.0.0:9000":   "http://127.0.0.1:9000",
		":8091":          "http://127.0.0.1:8091",
		"":               "http://127.0.0.1:8080",
	}
	for addr, want := range cases {
		if got := loopbackBaseFromAddr(addr); got != want {
			t.Fatalf("loopbackBaseFromAddr(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestParseChatSSELine(t *testing.T) {
	if ev, kind := parseChatSSELine(`data: [DONE]`); kind != sseDone {
		t.Fatalf("want done, got %v %+v", kind, ev)
	}
	if _, kind := parseChatSSELine(`: heartbeat`); kind != sseIgnore {
		t.Fatalf("want ignore for comment")
	}
	ev, kind := parseChatSSELine(`data: {"choices":[{"delta":{"content":"hi","reasoning":"th"}}]}`)
	if kind != sseDelta || ev.Content != "hi" || ev.Reasoning != "th" {
		t.Fatalf("bad delta: %v %+v", kind, ev)
	}
	if _, kind := parseChatSSELine(`data: {"error":{"code":"x","message":"boom"}}`); kind != sseError {
		t.Fatalf("want error frame")
	}
}

func TestRegistryStartCapAndSingleActive(t *testing.T) {
	reg := NewChatRunRegistry(2)
	r1, err := reg.start("u1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.start("u1", "c1"); err != ErrRunAlreadyActive {
		t.Fatalf("want ErrRunAlreadyActive, got %v", err)
	}
	if _, err := reg.start("u1", "c2"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.start("u1", "c3"); err != ErrTooManyRuns {
		t.Fatalf("want ErrTooManyRuns, got %v", err)
	}
	if got := reg.Get("u1", "c1"); got != r1 {
		t.Fatal("Get should return the active run")
	}
	if len(reg.ActiveForUser("u1")) != 2 {
		t.Fatalf("want 2 active")
	}
}

// TestActiveForUserRunningOnly: ActiveForUser is the reopen-resubscribe source,
// so it must return ONLY runs that are currently running. A run that has
// finished (but still lingers in the registry until its eviction grace period
// elapses) is terminal and must be excluded, or a reopened browser would
// resubscribe to it and fabricate a chat buffer from an empty base.
func TestActiveForUserRunningOnly(t *testing.T) {
	reg := NewChatRunRegistry(5)
	running, err := reg.start("u1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	finished, err := reg.start("u1", "c2")
	if err != nil {
		t.Fatal(err)
	}
	// Finish c2's run: it stays in the registry (not yet evicted) but is terminal.
	finished.finish("completed", "")
	if got := reg.Get("u1", "c2"); got != finished {
		t.Fatal("finished run should still be registered until eviction")
	}

	active := reg.ActiveForUser("u1")
	if len(active) != 1 {
		t.Fatalf("ActiveForUser returned %d runs, want 1 (running only)", len(active))
	}
	if active[0] != running {
		t.Fatalf("ActiveForUser returned the wrong run: got %+v", active[0])
	}
}

// TestRetireFreesSlotButKeepsByID unit-tests retire(): it unlinks the run from
// byUser (freeing the per-chat slot AND the per-user cap count) so the chat's
// next turn is accepted, yet KEEPS it in byID so a late subscriber still gets
// its terminal snapshot. It also proves remove()'s guard: after a NEWER run
// reuses the same chat slot, the eviction-delay remove() of the OLD run must not
// unlink the newer run's byUser entry.
func TestRetireFreesSlotButKeepsByID(t *testing.T) {
	reg := NewChatRunRegistry(2)
	r1, err := reg.start("u1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	reg.retire(r1)
	// The per-chat slot is free again: a new run on the same chat is accepted.
	r2, err := reg.start("u1", "c1")
	if err != nil {
		t.Fatalf("same-chat start after retire: %v", err)
	}
	if r2 == r1 {
		t.Fatal("expected a fresh run, got the retired one")
	}
	// The retired run is still reachable by id (late terminal snapshots).
	if reg.GetByID("u1", r1.ID) != r1 {
		t.Fatal("retired run should still be reachable by id")
	}
	// The eviction-delay remove() of the OLD run drops it from byID but must NOT
	// disturb the newer run that reused the same chat slot.
	reg.remove(r1)
	if reg.GetByID("u1", r1.ID) != nil {
		t.Fatal("remove should drop the retired run from byID")
	}
	if reg.Get("u1", "c1") != r2 {
		t.Fatal("remove wrongly unlinked the newer run's byUser entry")
	}
}

// TestCapFreesOnTerminal is a regression test for the per-user cap counting
// terminal runs. With a cap of 5, running-and-finishing runs across 6 different
// chats sequentially must ALL be accepted: each terminal run retires (frees its
// cap slot via finishRun) instead of lingering for the 30s eviction delay.
// Pre-fix the 6th start returns ErrTooManyRuns (429); post-fix it succeeds.
func TestCapFreesOnTerminal(t *testing.T) {
	srv, owner, firstChat := newRunTestServer(t) // instant mock, registry cap 5
	chatIDs := []string{firstChat}
	for i := 0; i < 5; i++ {
		created, err := srv.Portal.CreateChat(context.Background(), owner, portal.CreateChatRequest{
			Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u","role":"user","content":"hi"}]}`),
		})
		if err != nil {
			t.Fatalf("CreateChat %d: %v", i, err)
		}
		chatIDs = append(chatIDs, created.ID)
	}
	// 6 chats > cap 5: sequential run+finish must all succeed once slots free.
	for i, id := range chatIDs {
		run, err := srv.startChatRun(owner, id, PrepareRunResult{
			History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Settings: portal.ChatRunSettings{Model: "qwen-coder"},
		})
		if err != nil {
			t.Fatalf("start %d/%d: %v", i+1, len(chatIDs), err)
		}
		waitFor(t, func() bool { return run.statusValue() != "running" })
	}
}

func TestRunSubscribeSnapshotThenDelta(t *testing.T) {
	run := newChatRun("run_1", "c1", "u1", func() {})
	run.publish(sseDeltaEvent{Content: "he"})
	snap, ch, unsub := run.subscribe()
	defer unsub()
	if snap.Content != "he" || snap.Status != "running" {
		t.Fatalf("bad snapshot: %+v", snap)
	}
	run.publish(sseDeltaEvent{Content: "llo"})
	ev := <-ch
	if ev.Content != "llo" {
		t.Fatalf("bad delta: %+v", ev)
	}
	run.finish("completed", "")
	term := <-ch
	if term.Status != "completed" {
		t.Fatalf("bad terminal: %+v", term)
	}
}

// newRunTestServer builds a real gateway Server (behind httptest) with a fake
// streaming provider, an internal-auth secret + user directory so the loopback
// call authenticates, an encrypted chat store, and one seeded chat that already
// carries a user message. It returns the server, the session-style owner
// principal, and the chat id. selfBaseURL points at the httptest server so the
// executor calls the real /v1/chat/completions endpoint over loopback.
func newRunTestServer(t *testing.T) (*Server, auth.Token, string) {
	t.Helper()
	return newRunTestServerWithProvider(t, provider.Mock{})
}

// newRunTestServerWithProvider is newRunTestServer with an injectable streaming
// provider so tests can pace the deltas (e.g. to span checkpoint ticks).
func newRunTestServerWithProvider(t *testing.T, prov provider.Client) (*Server, auth.Token, string) {
	t.Helper()
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{
		ID:     "tok_dev",
		UserID: "usr_dev",
		Name:   "Dev Token",
		Active: true,
		Scopes: []string{"gateway:use"},
	}, "dev-secret")
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{
		ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User",
		Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: now, UpdatedAt: now,
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	svc := portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore,
		Clock: func() time.Time { return now }, ModelLister: provider.NewMock(),
		Chats: store.NewMemoryChatStore(0), Cipher: cipher,
	})
	srv := New(ServerDeps{
		Tokens:             tokens,
		Usage:              recorder,
		Provider:           prov,
		Routes:             routeStore,
		Portal:             svc,
		Captures:           &fakeCaptureStore{},
		Cipher:             cipher,
		InternalAuthSecret: "test-internal-secret",
		Users:              directory,
		ChatRuns:           NewChatRunRegistry(5),
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL

	owner := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}}
	created, err := svc.CreateChat(context.Background(), owner, portal.CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[{"id":"u","role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return srv, owner, created.ID
}

// waitFor polls cond every ~5ms up to ~3s, failing the test if it never holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("waitFor: condition not met within 3s")
}

func TestExecuteRunCommitsTranscript(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}

	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("run status = %q, want completed", got)
	}

	got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !strings.Contains(string(got.Content), `"status":"complete"`) {
		t.Fatalf("assistant not committed: %s", got.Content)
	}
}

// TestRunCommitWinsOverCheckpoint is a regression test for the checkpoint-vs-
// commit ordering race: the periodic CheckpointAssistant("pending") writes must
// never land after the final CommitAssistant("complete"). With the checkpoint
// interval shrunk far below the run duration, several checkpoint ticks fire per
// run; the executor must join the checkpoint goroutine before committing so the
// commit is deterministically the last writer. Pre-fix (no wg.Wait) this fails
// within a handful of the iterations below; post-fix it always passes.
func TestRunCommitWinsOverCheckpoint(t *testing.T) {
	old := runCheckpointInterval
	runCheckpointInterval = 2 * time.Millisecond
	defer func() { runCheckpointInterval = old }()

	// 6 deltas spaced 5ms apart (~30ms) span ~15 of the 2ms checkpoint ticks,
	// so a checkpoint reliably overlaps the final commit.
	prov := pacedStreamer{n: 6, gap: 5 * time.Millisecond}

	for i := 0; i < 20; i++ {
		srv, owner, chatID := newRunTestServerWithProvider(t, prov)
		run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
			History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Settings: portal.ChatRunSettings{Model: "qwen-coder"},
		})
		if err != nil {
			t.Fatalf("iter %d: startChatRun: %v", i, err)
		}
		waitFor(t, func() bool { return run.statusValue() != "running" })
		if got := run.statusValue(); got != "completed" {
			t.Fatalf("iter %d: run status = %q, want completed", i, got)
		}
		got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
		if err != nil {
			t.Fatalf("iter %d: GetChat: %v", i, err)
		}
		content := string(got.Content)
		if !strings.Contains(content, `"status":"complete"`) {
			t.Fatalf("iter %d: final transcript not committed as complete: %s", i, content)
		}
		if strings.Contains(content, `"status":"pending"`) {
			t.Fatalf("iter %d: a checkpoint landed after commit (transcript stuck pending): %s", i, content)
		}
	}
}

// headerCaptureServer stands in for selfBaseURL: it records the headers of the
// single request executeRun makes and answers with an immediately-terminal SSE
// stream, so the test observes exactly what executeRun sent without routing
// through the real /v1/chat/completions handler stack (out of scope here — see
// applyServerOverride's own tests for the re-authorization boundary this
// header feeds). Guarded by a mutex even though the request/response round
// trip already establishes a happens-before edge with the run's terminal
// status, for an unambiguous non-race under -race.
func newHeaderCaptureServer(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		captured = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
}

// TestExecuteRunSetsServerOverrideHeadersWhenConfigured proves executeRun
// carries a manageable per-chat server_override (see
// portal.Service.PrepareChatRun's self-heal) over the loopback call as the two
// headers applyServerOverride re-authorizes — mirroring exactly where the
// existing X-OP-Run-As-Token header is set.
func TestExecuteRunSetsServerOverrideHeadersWhenConfigured(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	fake, headers := newHeaderCaptureServer(t)
	srv.selfBaseURL = fake.URL

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History: []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{
			Model: "qwen-coder", ServerOverride: "srv-a", ServerOverrideForceUnreachable: true,
		},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	got := headers()
	if v := got.Get(serverOverrideHeaderName); v != "srv-a" {
		t.Fatalf("%s = %q, want srv-a", serverOverrideHeaderName, v)
	}
	if v := got.Get(serverOverrideForceHeaderName); v != "1" {
		t.Fatalf("%s = %q, want 1", serverOverrideForceHeaderName, v)
	}
}

// TestExecuteRunSetsServerOverrideForceHeaderFalseWhenNotForced proves the
// force header is explicitly "0" (not merely omitted) when the setting is
// false — applyServerOverride's consumer only checks `== "1"`, so either form
// reads as false, but the brief calls for an explicit value either way.
func TestExecuteRunSetsServerOverrideForceHeaderFalseWhenNotForced(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	fake, headers := newHeaderCaptureServer(t)
	srv.selfBaseURL = fake.URL

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder", ServerOverride: "srv-a"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	got := headers()
	if v := got.Get(serverOverrideHeaderName); v != "srv-a" {
		t.Fatalf("%s = %q, want srv-a", serverOverrideHeaderName, v)
	}
	if v := got.Get(serverOverrideForceHeaderName); v != "0" {
		t.Fatalf("%s = %q, want 0", serverOverrideForceHeaderName, v)
	}
}

// TestExecuteRunOmitsServerOverrideHeadersWhenUnset is the no-op-invariant
// counterpart: a chat with no configured server_override sends neither header
// (mirroring the RunAsTokenID-unset branch immediately above it in
// executeRun) — an ordinary chat run pays zero cost for this feature.
func TestExecuteRunOmitsServerOverrideHeadersWhenUnset(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	fake, headers := newHeaderCaptureServer(t)
	srv.selfBaseURL = fake.URL

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	got := headers()
	if v := got.Get(serverOverrideHeaderName); v != "" {
		t.Fatalf("%s = %q, want unset", serverOverrideHeaderName, v)
	}
	if v := got.Get(serverOverrideForceHeaderName); v != "" {
		t.Fatalf("%s = %q, want unset", serverOverrideForceHeaderName, v)
	}
}
