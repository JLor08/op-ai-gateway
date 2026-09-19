// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	"unicode/utf8"
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
	// The terminal usage chunk (stream_options.include_usage) has an empty
	// choices/delta but the exact completion-token count: surface it as sseUsage
	// carrying that count, not sseIgnore as an empty delta (issue #56).
	if ev, kind := parseChatSSELine(`data: {"choices":[],"usage":{"completion_tokens":42}}`); kind != sseUsage || ev.OutputTokens != 42 {
		t.Fatalf("want usage kind with 42 tokens, got %v %+v", kind, ev)
	}
	// A usage chunk reporting zero completion tokens carries no rate signal: ignore.
	if _, kind := parseChatSSELine(`data: {"choices":[],"usage":{"completion_tokens":0}}`); kind != sseIgnore {
		t.Fatalf("want ignore for zero-token usage chunk")
	}
}

func TestFlooredRate(t *testing.T) {
	// Below the floor the divisor is a microsecond artefact -> 0, no matter the
	// count. This is the whole point of the floor (issue #56).
	if got := flooredRate(1000, minGatewayRateWindow-time.Nanosecond); got != 0 {
		t.Fatalf("flooredRate below floor = %v, want 0", got)
	}
	// The floor is inclusive (>=): a window of exactly minGatewayRateWindow
	// engages rather than returning 0.
	if got, want := flooredRate(50, minGatewayRateWindow), 50/minGatewayRateWindow.Seconds(); got != want {
		t.Fatalf("flooredRate at the exact floor = %v, want %v (inclusive boundary)", got, want)
	}
	// Above the floor: 100 over 100ms = 1000/s.
	if got := flooredRate(100, 100*time.Millisecond); got != 1000 {
		t.Fatalf("flooredRate(100, 100ms) = %v, want 1000", got)
	}
	// A non-positive count is 0 even above the floor.
	if got := flooredRate(0, time.Second); got != 0 {
		t.Fatalf("flooredRate(0, 1s) = %v, want 0", got)
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
	finished.finish("completed", "", nil)
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
	run.finish("completed", "", nil)
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

// TestExecuteRunMetricsRunesNotBytesAndTokens pins the two halves of issue #56
// on the committed transcript:
//
//   - chars/s (`tps`) counts RUNES, not bytes. The stream emits multibyte text
//     whose byte length exceeds its rune length, so a byte-counting bug would
//     inflate the rate. Because the final chars/s and tokens/sec are computed
//     over the SAME window, their ratio is exactly runes:tokens regardless of
//     wall-clock timing -- a deterministic assertion despite real-time rates.
//   - tokens/sec (`tokens_per_second`) is present and derived from the upstream
//     usage chunk (requested via stream_options.include_usage), whose token
//     count is decoupled here from the delta count.
func TestExecuteRunMetricsRunesNotBytesAndTokens(t *testing.T) {
	const delta = "aé" // 2 runes, 3 bytes -- runes != bytes
	const deltas = 15
	const outTokens = 10
	runesPerDelta := utf8.RuneCountInString(delta)
	bytesPerDelta := len(delta)
	if runesPerDelta == bytesPerDelta {
		t.Fatal("test delta must be multibyte so runes != bytes")
	}
	// 15 deltas * 6ms = ~90ms window, comfortably above minGatewayRateWindow so
	// both rates engage (a shorter window would floor them both to 0).
	prov := pacedTextStreamer{text: delta, n: deltas, gap: 6 * time.Millisecond, outTokens: outTokens}
	srv, owner, chatID := newRunTestServerWithProvider(t, prov)

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
	var doc struct {
		Messages []struct {
			Role            string  `json:"role"`
			Content         string  `json:"content"`
			TPS             float64 `json:"tps"`
			TokensPerSecond float64 `json:"tokensPerSecond"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.Content, &doc); err != nil {
		t.Fatalf("unmarshal chat doc: %v (%s)", err, got.Content)
	}
	var asst *struct {
		Role            string  `json:"role"`
		Content         string  `json:"content"`
		TPS             float64 `json:"tps"`
		TokensPerSecond float64 `json:"tokensPerSecond"`
	}
	for i := range doc.Messages {
		if doc.Messages[i].Role == "assistant" {
			asst = &doc.Messages[i]
		}
	}
	if asst == nil {
		t.Fatalf("no assistant message: %s", got.Content)
	}
	if asst.TPS <= 0 {
		t.Fatalf("chars/s (tps) = %v, want > 0", asst.TPS)
	}
	if asst.TokensPerSecond <= 0 {
		t.Fatalf("tokens_per_second = %v, want > 0 (from usage chunk)", asst.TokensPerSecond)
	}
	// The window cancels in the ratio, leaving content-runes : output-tokens.
	wantRunes := utf8.RuneCountInString(asst.Content)
	if wantRunes != deltas*runesPerDelta {
		t.Fatalf("buffered content = %d runes, want %d", wantRunes, deltas*runesPerDelta)
	}
	ratio := asst.TPS / asst.TokensPerSecond
	runeRatio := float64(wantRunes) / float64(outTokens)
	byteRatio := float64(deltas*bytesPerDelta) / float64(outTokens)
	if diff := ratio - runeRatio; diff > 0.01 || diff < -0.01 {
		t.Fatalf("tps/tokens_per_second = %.4f, want %.4f (runes:tokens); a byte count would give %.4f",
			ratio, runeRatio, byteRatio)
	}
}

// TestExecuteRunTokensPerSecondAbsentWithoutUsage: when the upstream reports no
// output tokens, tokens/sec is absent (not a spurious 0) while chars/s is still
// reported. This exercises the completion_tokens==0 -> sseIgnore guard, so the
// run never receives a usable token count (issue #56).
func TestExecuteRunTokensPerSecondAbsentWithoutUsage(t *testing.T) {
	// n content deltas, 8ms apart (~64ms >= floor), but a usage chunk of 0 tokens.
	prov := pacedTextStreamer{text: "hi", n: 8, gap: 8 * time.Millisecond, outTokens: 0}
	srv, owner, chatID := newRunTestServerWithProvider(t, prov)

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
	content := string(got.Content)
	// chars/s present (a rate was measurable), tokens/sec key omitted entirely.
	if !strings.Contains(content, `"tps":`) {
		t.Fatalf("chars/s missing despite measurable content: %s", content)
	}
	if strings.Contains(content, `"tokensPerSecond":`) {
		t.Fatalf("tokens/sec must be absent when the upstream reported no usage: %s", content)
	}
}

// TestExecuteRunTokensPerSecondSpansReasoning pins the reasoning half of issue
// #56 part (B): the upstream's completion_tokens count includes reasoning
// tokens, so tokens/sec must be divided by the FULL generation window
// (reasoning + content), not just the content window. chars/s stays on the
// content window. The check is timing-robust: it reconstructs each rate's window
// from the committed rate and asserts their difference is the recorded reasoning
// duration. A regression that anchors tokens/sec on the first CONTENT delta
// would make that difference ~0 and fail here.
func TestExecuteRunTokensPerSecondSpansReasoning(t *testing.T) {
	const gap = 8 * time.Millisecond
	prov := reasoningThenTextStreamer{
		reasoning:  "rz",
		reasoningN: 12, // ~96ms of reasoning before any content
		text:       "ab",
		textN:      10, // ~80ms content window; content = 20 runes
		gap:        gap,
		outTokens:  30, // includes the reasoning tokens, like a real upstream
	}
	srv, owner, chatID := newRunTestServerWithProvider(t, prov)

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
	var doc struct {
		Messages []struct {
			Role            string  `json:"role"`
			Content         string  `json:"content"`
			ReasoningMs     float64 `json:"reasoningMs"`
			TPS             float64 `json:"tps"`
			TokensPerSecond float64 `json:"tokensPerSecond"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got.Content, &doc); err != nil {
		t.Fatalf("unmarshal chat doc: %v (%s)", err, got.Content)
	}
	var m *struct {
		Role            string  `json:"role"`
		Content         string  `json:"content"`
		ReasoningMs     float64 `json:"reasoningMs"`
		TPS             float64 `json:"tps"`
		TokensPerSecond float64 `json:"tokensPerSecond"`
	}
	for i := range doc.Messages {
		if doc.Messages[i].Role == "assistant" {
			m = &doc.Messages[i]
		}
	}
	if m == nil {
		t.Fatalf("no assistant message: %s", got.Content)
	}
	if m.ReasoningMs <= 0 {
		t.Fatalf("reasoningMs = %v, want > 0 (reasoning happened)", m.ReasoningMs)
	}
	if m.TPS <= 0 || m.TokensPerSecond <= 0 {
		t.Fatalf("rates not both set: tps=%v tokens/s=%v", m.TPS, m.TokensPerSecond)
	}
	// Reconstruct each rate's window: chars/s covers the content window, tokens/s
	// the generation window. Their difference is the reasoning phase, which the
	// message also records as reasoningMs.
	contentWindow := float64(utf8.RuneCountInString(m.Content)) / m.TPS
	genWindow := 30.0 / m.TokensPerSecond
	gotReasoningSecs := genWindow - contentWindow
	wantReasoningSecs := m.ReasoningMs / 1000.0
	if math.Abs(gotReasoningSecs-wantReasoningSecs) > 0.005 {
		t.Fatalf("tokens/s window excludes the reasoning phase: gen-content window = %.4fs, want ~%.4fs (reasoningMs). "+
			"A content-anchored tokens/s (the bug) would give ~0.", gotReasoningSecs, wantReasoningSecs)
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

// TestRunSnapshotCarriesAServerMeasuredAge: the snapshot must carry a
// server-measured age, because a reopened tab never saw the moment of Send and
// must still show the same true number as the tab that did.
//
// The assertions run through snapshotLocked (via subscribe), which is the path
// Task 14's clock anchors on -- and they are bounded on BOTH sides: a
// hardwired zero, an age measured from the wrong instant, and an age that is
// not milliseconds all fail. A non-negativity check would pass for all three.
func TestRunSnapshotCarriesAServerMeasuredAge(t *testing.T) {
	const age = 30 * time.Millisecond
	srv, owner, chatID := newRunTestServer(t)
	before := time.Now()
	run, err := srv.ChatRuns.add(owner.UserID, chatID, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if run.startedAt.IsZero() {
		t.Fatal("startedAt must be set at construction")
	}
	time.Sleep(age)

	snap, _, unsub := run.subscribe()
	defer unsub()
	// Lower bound: time.Sleep sleeps at LEAST its duration, and the run was
	// constructed before it, so a truthful age cannot be under it. Upper bound:
	// the wall time this test itself spent, which the run's age cannot exceed
	// because startedAt was stamped after `before` (+1ms for the truncation
	// Milliseconds() does).
	ceiling := time.Since(before).Milliseconds() + 1
	if snap.ElapsedMs < age.Milliseconds() || snap.ElapsedMs > ceiling {
		t.Fatalf("snapshot ElapsedMs = %d, want within [%d, %d] -- the snapshot's "+
			"age is the value the client anchors its clock on",
			snap.ElapsedMs, age.Milliseconds(), ceiling)
	}
}

// TestRunAgeIsFrozenOnceTerminal: a terminal run's age is its DURATION, not its
// time since start. A late subscriber (the eviction grace period is 30s) would
// otherwise be told a two-second run took thirty.
//
// The run is given a real, measurable duration first, so the frozen value has
// to land in a window around that duration. Two reads of a ~0ms run would let
// a hardwired zero satisfy the equality check on its own.
func TestRunAgeIsFrozenOnceTerminal(t *testing.T) {
	const duration = 30 * time.Millisecond
	before := time.Now()
	run := newChatRun("run_age", "c1", "u1", func() {})
	time.Sleep(duration)
	run.finish("completed", "", nil)
	ceiling := time.Since(before).Milliseconds() + 1

	first, _, unsub := run.subscribe()
	unsub()
	if first.ElapsedMs < duration.Milliseconds() || first.ElapsedMs > ceiling {
		t.Fatalf("terminal ElapsedMs = %d, want within [%d, %d] (the run's real duration)",
			first.ElapsedMs, duration.Milliseconds(), ceiling)
	}
	// The done event -- the other snapshotLocked caller -- carries the same
	// duration, because finish() stamps endedAt before it builds the frame.
	time.Sleep(20 * time.Millisecond)
	second, _, unsub2 := run.subscribe()
	defer unsub2()
	if first.ElapsedMs != second.ElapsedMs {
		t.Fatalf("terminal age moved: %d -> %d; a finished run's age is its duration",
			first.ElapsedMs, second.ElapsedMs)
	}
}

// TestRunKindIsOnSnapshotAndDoneButNeverOnDelta pins the wire contract this
// feature inherits from `metrics`: the per-run facts ride on `snapshot` and
// `done`, never on `delta`. The client ticks its own clock between snapshots
// from the anchor a snapshot gave it, so a per-delta age would be both
// redundant and (on a text run) a per-token cost.
func TestRunKindIsOnSnapshotAndDoneButNeverOnDelta(t *testing.T) {
	run := newChatRun("run_kind", "c1", "u1", func() {})
	run.setKind("image")
	snap, ch, unsub := run.subscribe()
	defer unsub()
	if snap.Kind != "image" {
		t.Fatalf("snapshot kind = %q, want image", snap.Kind)
	}
	run.publish(sseDeltaEvent{Content: "x"})
	ev := <-ch
	if ev.Kind != "" || ev.ElapsedMs != 0 {
		t.Fatalf("delta carries per-run facts: %+v", ev)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	if strings.Contains(string(raw), "elapsed_ms") || strings.Contains(string(raw), `"kind"`) {
		t.Fatalf("delta wire frame must carry neither kind nor elapsed_ms: %s", raw)
	}
	run.finish("completed", "", nil)
	term := <-ch
	if term.Kind != "image" {
		t.Fatalf("done kind = %q, want image", term.Kind)
	}
}

// TestRunDeadlineIsDistinguishableFromACancel: a deadline must NOT masquerade
// as a user cancel -- the user pressed nothing.
func TestRunDeadlineIsDistinguishableFromACancel(t *testing.T) {
	restore := imageRunDeadline
	imageRunDeadline = 50 * time.Millisecond
	t.Cleanup(func() { imageRunDeadline = restore })

	// An upstream that never answers, so the deadline is what ends the run.
	//
	// The harness is the IMAGE one (chat_runs_images_test.go), not a text
	// route with an image kind: only an image run is bounded, and the
	// executor now dispatches that kind to /v1/images/generations, so a text
	// model under an image kind no longer describes a reachable run at all --
	// it is refused as not image-capable long before any deadline.
	upstream, release := stalledImagesUpstream(t, "")
	defer release()
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error -- a deadline is not a cancel", got)
	}
	snap, _, unsub := run.subscribe()
	defer unsub()
	if snap.Err != runTimedOutMessage {
		t.Fatalf("error = %q, want the timeout code %q -- an empty message is "+
			"what a USER cancel looks like, and conflating the two is the "+
			"silent-conflation this feature refuses", snap.Err, runTimedOutMessage)
	}
}

// TestRunDeadlineMidResponseIsNotACancelEither covers the SECOND context
// branch: once the upstream has answered 200 the deadline lands on the run's
// READ of the response rather than on its round trip, and that branch reported
// "canceled" with an empty message too.
//
// This test was originally aimed at consumeRunStream's scanner error, where
// the same branch exists, but it cannot pin THAT branch: it is currently
// UNREACHABLE -- only the image kind is bounded and an image run never enters
// consumeRunStream -- so pointing this test at it would be pinning nothing.
// The branch is kept and annotated where it lives (chat_runs.go, the
// DeadlineExceeded case), for the day the text kind is bounded; its lack of a
// test is a consequence of that unreachability rather than a gap, and one
// cannot be written without first making it reachable.
//
// The upstream below sends the first body chunk (which the gateway's copier
// flushes straight through, so the run's Do returns) and then stalls, putting
// the run mid-response when the deadline fires.
func TestRunDeadlineMidResponseIsNotACancelEither(t *testing.T) {
	restore := imageRunDeadline
	imageRunDeadline = 150 * time.Millisecond
	t.Cleanup(func() { imageRunDeadline = restore })

	upstream, release := stalledImagesUpstream(t, `{"created":1,`)
	defer release()
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error -- a deadline is not a cancel", got)
	}
	snap, _, unsub := run.subscribe()
	defer unsub()
	if snap.Err != runTimedOutMessage {
		t.Fatalf("error = %q, want the timeout code %q", snap.Err, runTimedOutMessage)
	}
}

// TestUserCancelStaysACancelUnderADeadline is the counterpart invariant: with a
// deadline in place, an explicit Stop must still end the run as "canceled" with
// an empty message. A branch that treated every context error as a timeout
// would be the same conflation in the other direction.
func TestUserCancelStaysACancelUnderADeadline(t *testing.T) {
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 30, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	srv.ChatRuns.cancelChat(owner.UserID, chatID)
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled", got)
	}
	snap, _, unsub := run.subscribe()
	defer unsub()
	if snap.Err != "" {
		t.Fatalf("cancel error = %q, want empty", snap.Err)
	}
}

// TestTimedOutRunStillCommitsItsTurn is an END-TO-END SMOKE TEST: a run its own
// deadline ended still has a terminal turn in the transcript, recorded as
// "error". It is deliberately NOT the pin for trap 2 (the detached commit
// context) and cannot fail for that reason -- the memory chat store ignores
// the context it is handed, so this stays green with commitCtx reverted to
// ctx. TestFinishRunCommitDoesNotInheritAnExpiredContext is that pin. What
// this one catches is the coarser failure: a timeout path that finishes the
// run without committing anything at all.
func TestTimedOutRunStillCommitsItsTurn(t *testing.T) {
	restore := imageRunDeadline
	imageRunDeadline = 10 * time.Millisecond
	t.Cleanup(func() { imageRunDeadline = restore })

	// The image harness, for the reason spelled out in
	// TestRunDeadlineIsDistinguishableFromACancel: with the executor now
	// branching on the kind, a text model under an image kind is refused as
	// not image-capable and this test would assert its own `"status":"error"`
	// against a routing failure instead of a timeout.
	upstream, release := stalledImagesUpstream(t, "")
	defer release()
	srv, _, owner, chatID := newImageRunTestServer(t, upstream.URL)
	run, err := srv.startChatRun(owner, chatID, imageRunPrep())
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := runError(t, run); got != runTimedOutMessage {
		t.Fatalf("run ended %q, not by its deadline -- the transcript check below would prove nothing", got)
	}

	got, err := srv.Portal.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !strings.Contains(string(got.Content), `"status":"error"`) {
		t.Fatalf("a timed-out run must still record its terminal turn: %s", got.Content)
	}
}

// TestCompletedRunReleasesItsDeadlineContext: reserveRun's context is now a
// WithTimeout, whose timer stays armed until the deadline unless its cancel
// runs. That cancel was only ever invoked on the release/cancel paths, never on
// the normal terminal one, so every completed run would otherwise pin a
// runDeadline-long timer.
func TestCompletedRunReleasesItsDeadlineContext(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	run, ctx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	srv.launchRun(ctx, owner, run, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	waitFor(t, func() bool { return run.statusValue() == "completed" })
	waitFor(t, func() bool { return ctx.Err() != nil })
}

// commitCtxRecorder wraps a real portal.API and records the context state
// CommitAssistant was handed, so a test can prove the terminal commit does not
// inherit the run's deadline. It embeds the interface rather than implementing
// it: portal.API is a wide facade and only this one method is under test.
type commitCtxRecorder struct {
	portal.API
	mu  sync.Mutex
	err error
	hit bool
}

func (c *commitCtxRecorder) CommitAssistant(ctx context.Context, owner auth.Token, chatID string, turn portal.AssistantTurn, status string) error {
	c.mu.Lock()
	c.err, c.hit = ctx.Err(), true
	c.mu.Unlock()
	return c.API.CommitAssistant(ctx, owner, chatID, turn, status)
}

func (c *commitCtxRecorder) seen() (hit bool, ctxErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hit, c.err
}

// TestFinishRunCommitDoesNotInheritAnExpiredContext is trap 2, pinned where it
// actually bites. finishRun is called with the RUN's own context on four paths
// (the two request-construction failures, a non-context Do failure and the
// non-200), and that context now carries the deadline -- so a run whose
// deadline fires anywhere around those paths would have its terminal commit
// cancelled by the very timeout that ended it, losing the turn instead of
// recording why it ended. The memory chat store ignores the context, so the
// assertion is on the context the commit was HANDED, which is what a real
// database/sql store checks before it writes.
func TestFinishRunCommitDoesNotInheritAnExpiredContext(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	rec := &commitCtxRecorder{API: srv.Portal}
	srv.Portal = rec

	run, err := srv.ChatRuns.add(owner.UserID, chatID, func() {})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if expired.Err() == nil {
		t.Fatal("test setup: the context must already be expired")
	}
	srv.finishRun(expired, owner, run, "error", runTimedOutMessage)

	hit, gotErr := rec.seen()
	if !hit {
		t.Fatal("CommitAssistant was never called")
	}
	if gotErr != nil {
		t.Fatalf("the terminal commit inherited the run's expired context (%v); "+
			"the deadline that ended the run must not cancel the record of why", gotErr)
	}
	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
}

// TestTextRunIsNotBoundedByTheImageDeadline is the no-op invariant for the
// existing, overwhelmingly common path: only an image run is bounded. The
// image deadline below is shrunk to a tenth of the time this text run takes,
// so a run that inherited it would die mid-stream as a timeout; a text run
// must still complete, exactly as it does today.
//
// The bound exists because an image run emits nothing between dispatch and its
// finished image. A text run streams deltas, so its liveness is visible, and a
// ceiling would newly kill long generations from a slow local model that
// succeed today -- a behaviour change to someone else's feature, needing its
// own decision.
func TestTextRunIsNotBoundedByTheImageDeadline(t *testing.T) {
	restore := imageRunDeadline
	imageRunDeadline = 10 * time.Millisecond
	t.Cleanup(func() { imageRunDeadline = restore })

	// ~100ms of streaming, ten times the image deadline.
	srv, owner, chatID := newRunTestServerWithProvider(t, pacedStreamer{n: 5, gap: 20 * time.Millisecond})
	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"}, // kind "" == text
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q, want completed -- a text run must not inherit the image bound", got)
	}
	snap, _, unsub := run.subscribe()
	defer unsub()
	if snap.Err != "" {
		t.Fatalf("error = %q, want empty", snap.Err)
	}
}

// TestRunDeadlineForOnlyBoundsImages states the rule directly, so a future
// change that folds the two kinds back into one global deadline fails here
// with the reason attached rather than only in a timing-dependent test.
func TestRunDeadlineForOnlyBoundsImages(t *testing.T) {
	if d, bounded := runDeadlineFor("image"); !bounded || d != imageRunDeadline {
		t.Fatalf("image run: got (%v, %v), want (%v, true)", d, bounded, imageRunDeadline)
	}
	for _, kind := range []string{"", "text", "unknown"} {
		if d, bounded := runDeadlineFor(kind); bounded || d != 0 {
			t.Fatalf("kind %q: got (%v, %v), want (0, false) -- only image runs are bounded", kind, d, bounded)
		}
	}
}

// TestRunKindWriteIsSynchronizedWithItsReaders pins the mutex guard on
// ChatRun.kind -- and it exists because `-race` alone does NOT catch that
// guard's removal. reserveRun registers the run, so it is reachable by GET
// runs/active and by a subscriber, but launchRun writes the kind afterwards
// and every other test calls both from the same goroutine: the detector never
// observes a concurrent pair, and an unguarded kind passes the whole suite.
//
// So the pair is constructed here. Both goroutines wait on one barrier, and
// the reader hammers listView() across the write. Under -race an unsynchronized
// setKind reports a data race even though listView itself takes the lock --
// synchronization requires BOTH sides to take the same one.
func TestRunKindWriteIsSynchronizedWithItsReaders(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	run, ctx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}

	var wg sync.WaitGroup
	barrier := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-barrier
		srv.launchRun(ctx, owner, run, PrepareRunResult{
			History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Settings: portal.ChatRunSettings{Model: "qwen-coder", Kind: "image"},
		})
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < 5000; i++ {
			_, _, _ = run.listView()
		}
	}()
	close(barrier)
	wg.Wait()

	waitFor(t, func() bool { return run.statusValue() != "running" })
	if got := run.kindValue(); got != "image" {
		t.Fatalf("kind = %q after the concurrent window, want image", got)
	}
}

// commitFailingPortal wraps a real portal.API and makes every CommitAssistant
// call fail with a configurable error, WITHOUT touching the underlying store
// -- so a test can prove the run's TERMINAL status/message follow the
// commit's own outcome rather than the status the executor was about to
// report. Embeds the interface, like commitCtxRecorder above: only this one
// method is under test.
type commitFailingPortal struct {
	portal.API
	err error
}

func (c *commitFailingPortal) CommitAssistant(context.Context, auth.Token, string, portal.AssistantTurn, string) error {
	return c.err
}

// TestFinishRunCommitFailureEndsTheRunAsError is the PRIMARY fix in this
// task. Before it, a failed terminal commit was only logged (finishRun's own
// comment said control flow was deliberately unchanged) and the run still
// reported "completed" -- an over-cap image turn is a silent success end to
// end, confirmed by task 7. Proven NON-VACUOUSLY: commitFailingPortal
// actually fails the commit rather than the test asserting the mapping
// function's own return value, so reverting the fix in finishRunWithParts
// makes this fail (status would read "completed").
func TestFinishRunCommitFailureEndsTheRunAsError(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	srv.Portal = &commitFailingPortal{API: srv.Portal, err: errors.New("boom: store unavailable")}

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error -- a failed commit must not report completed", got)
	}
	if got := runError(t, run); got != chatRunCommitFailedMessage {
		t.Fatalf("error = %q, want the generic commit-failure code %q", got, chatRunCommitFailedMessage)
	}
}

// TestFinishRunCommitFailureSurfacesChatTooLarge is the over-cap case task 7
// confirmed as a silent success: portal.ErrChatTooLarge is a named,
// actionable sentinel (the frontend's label tells the user that nothing was
// stored for this turn and to start a new chat), so it must be surfaced
// VERBATIM rather than degraded to the generic commitFailureCode.
func TestFinishRunCommitFailureSurfacesChatTooLarge(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	srv.Portal = &commitFailingPortal{API: srv.Portal, err: portal.ErrChatTooLarge}

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := runError(t, run); got != "portal.chat_too_large" {
		t.Fatalf("error = %q, want portal.chat_too_large -- named and actionable, unlike the generic code", got)
	}
}

// TestFinishRunCommitFailureDoesNotOverrideACanceledRunWhenTheChatIsGone is
// the carve-out the primary fix needs: DELETE /chats/{id} cancels the active
// run AND removes the chat row in the SAME request, so this run's own
// terminal commit can lose that race and see store.ErrNotFound. That is not
// a loss to report -- the chat is gone, so there is nothing left to persist
// for and no one will ever read its transcript again -- so the run's own
// status (here "canceled", from the delete's own cancellation) must stand,
// not flip to "error". TestDeleteChatCancelsActiveRun
// (chat_run_endpoints_test.go) is the end-to-end pin for the same race; this
// isolates just the commit-failure branch.
func TestFinishRunCommitFailureDoesNotOverrideACanceledRunWhenTheChatIsGone(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	srv.Portal = &commitFailingPortal{API: srv.Portal, err: store.ErrNotFound}

	run, err := srv.ChatRuns.add(owner.UserID, chatID, func() {})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	srv.finishRun(context.Background(), owner, run, "canceled", "")

	if got := run.statusValue(); got != "canceled" {
		t.Fatalf("status = %q, want canceled -- a chat deleted out from under the run is not a loss to report", got)
	}
	if got := runError(t, run); got != "" {
		t.Fatalf("error = %q, want empty (the run's own cancel message, unchanged)", got)
	}
}

// TestFinishRunCommitFailureOverridesACompletedRunEvenWhenTheChatIsGone is
// fix-round finding 4: the carve-out above is conditioned on status ==
// "canceled", not merely on commitFailureIsChatGone, and this is what proves
// why. A DELETE can land AFTER the stream ended (status about to be
// "completed") but BEFORE this commit runs, so the SAME store.ErrNotFound
// race the canceled test above exercises can hit a run that was never
// canceled at all. There the chat being gone really is nothing to report --
// but here there IS real content that was never saved, and reporting
// "completed" over it is exactly the silent-loss invariant this task exists
// to close: a completed run must never survive a failed store write, whatever
// the reason for that failure.
func TestFinishRunCommitFailureOverridesACompletedRunEvenWhenTheChatIsGone(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	srv.Portal = &commitFailingPortal{API: srv.Portal, err: store.ErrNotFound}

	run, err := srv.ChatRuns.add(owner.UserID, chatID, func() {})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	srv.finishRun(context.Background(), owner, run, "completed", "")

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error -- completed must not survive a failed store write, even when the store failure is that the chat is gone", got)
	}
	if got := runError(t, run); got != chatRunCommitFailedMessage {
		t.Fatalf("error = %q, want the generic commit-failure code %q", got, chatRunCommitFailedMessage)
	}
}

// TestExecuteRunKeepsTheUpstreamErrorCode is executeRun's non-200 branch: the
// text path's counterpart to TestImageRunKeepsTheUpstreamErrorCode
// (chat_runs_images_test.go). Before this the branch read no body at all, so
// the run reported the flat "upstream status 429 Too Many Requests" and
// discarded every code the loopback's own error envelope carried.
func TestExecuteRunKeepsTheUpstreamErrorCode(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"routing.no_healthy_host","message":"no healthy host"}}`)
	}))
	t.Cleanup(fake.Close)
	srv.selfBaseURL = fake.URL

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := run.statusValue(); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := runError(t, run); got != "routing.no_healthy_host" {
		t.Fatalf("error = %q, want the upstream body's own code, not a flat status line", got)
	}
}

// TestExecuteRunFallsBackToTheStatusLineForANonEnvelopeBody is
// upstreamErrorCode's fallback branch, reached from the text path: a non-200
// whose body is not the apierror.Body envelope (an HTML error page, a proxy
// in front of the gateway) must not crash or produce an empty message -- it
// degrades to the same "upstream status ..." line the branch used
// unconditionally before this task.
func TestExecuteRunFallsBackToTheStatusLineForANonEnvelopeBody(t *testing.T) {
	srv, owner, chatID := newRunTestServer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	t.Cleanup(fake.Close)
	srv.selfBaseURL = fake.URL

	run, err := srv.startChatRun(owner, chatID, PrepareRunResult{
		History:  []portal.ChatAPIMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Settings: portal.ChatRunSettings{Model: "qwen-coder"},
	})
	if err != nil {
		t.Fatalf("startChatRun: %v", err)
	}
	waitFor(t, func() bool { return run.statusValue() != "running" })

	if got := runError(t, run); got != "upstream status 502 Bad Gateway" {
		t.Fatalf("error = %q, want %q (fallback to the status line)", got, "upstream status 502 Bad Gateway")
	}
}
