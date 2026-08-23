// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"strings"
	"sync"
	"time"
)

// loopbackBaseFromAddr turns an OP_AI_GATEWAY_ADDR value (host:port) into the
// base URL the process uses to call ITSELF over loopback. TLS is terminated
// externally (nginx), so this is always plain HTTP on 127.0.0.1. Mirrors the
// port handling of cmd/gateway/main.go:portFromAddr and the runHealthcheck
// loopback precedent.
func loopbackBaseFromAddr(addr string) string {
	port := "8080"
	if i := strings.LastIndex(addr, ":"); i >= 0 && i+1 < len(addr) {
		port = addr[i+1:]
	}
	return "http://127.0.0.1:" + port
}

type sseKind int

const (
	sseIgnore sseKind = iota
	sseDelta
	sseDone
	sseError
)

type sseDeltaEvent struct {
	Content   string
	Reasoning string
	ErrCode   string
	ErrMsg    string
}

// parseChatSSELine mirrors portal-ui/src/components/shared/chatStream.ts:parseLine.
func parseChatSSELine(line string) (sseDeltaEvent, sseKind) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return sseDeltaEvent{}, sseIgnore
	}
	data := strings.TrimSpace(trimmed[len("data:"):])
	if data == "[DONE]" {
		return sseDeltaEvent{}, sseDone
	}
	var chunk struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return sseDeltaEvent{}, sseIgnore
	}
	if chunk.Error != nil {
		return sseDeltaEvent{ErrCode: chunk.Error.Code, ErrMsg: chunk.Error.Message}, sseError
	}
	var ev sseDeltaEvent
	if len(chunk.Choices) > 0 {
		d := chunk.Choices[0].Delta
		ev.Content = d.Content
		ev.Reasoning = d.ReasoningContent
		if ev.Reasoning == "" {
			ev.Reasoning = d.Reasoning
		}
	}
	if ev.Content == "" && ev.Reasoning == "" {
		return sseDeltaEvent{}, sseIgnore
	}
	return ev, sseDelta
}

var (
	ErrRunAlreadyActive = errors.New("gateway.chat_run_active")
	ErrTooManyRuns      = errors.New("gateway.chat_run_limit")
)

type runEvent struct {
	Event     string      `json:"-"` // "snapshot" | "delta" | "done"
	Reasoning string      `json:"reasoning,omitempty"`
	Content   string      `json:"content,omitempty"`
	Metrics   *runMetrics `json:"metrics,omitempty"`
	Status    string      `json:"status,omitempty"`
	Err       string      `json:"error,omitempty"`
}

type runMetrics struct {
	TTFTMs      int64   `json:"ttft_ms,omitempty"`
	ReasoningMs int64   `json:"reasoning_ms,omitempty"`
	TPS         float64 `json:"tps,omitempty"`
}

type ChatRun struct {
	ID     string
	ChatID string
	UserID string

	mu          sync.Mutex
	status      string // running | completed | error | canceled
	reasoning   strings.Builder
	content     strings.Builder
	metrics     runMetrics
	errMsg      string
	cancel      func()
	subscribers map[chan runEvent]struct{}
	endedAt     time.Time
}

func newChatRun(id, chatID, userID string, cancel func()) *ChatRun {
	return &ChatRun{
		ID: id, ChatID: chatID, UserID: userID,
		status: "running", cancel: cancel,
		subscribers: map[chan runEvent]struct{}{},
	}
}

func (r *ChatRun) snapshotLocked() runEvent {
	m := r.metrics
	return runEvent{
		Event: "snapshot", Reasoning: r.reasoning.String(), Content: r.content.String(),
		Metrics: &m, Status: r.status, Err: r.errMsg,
	}
}

// subscribe atomically returns the current snapshot and a channel of subsequent
// events, so no delta is lost between snapshot and registration.
func (r *ChatRun) subscribe() (runEvent, chan runEvent, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := r.snapshotLocked()
	ch := make(chan runEvent, 64)
	if r.status == "running" {
		r.subscribers[ch] = struct{}{}
	} else {
		close(ch) // already terminal: snapshot carries everything
	}
	unsub := func() {
		r.mu.Lock()
		delete(r.subscribers, ch)
		r.mu.Unlock()
	}
	return snap, ch, unsub
}

func (r *ChatRun) publish(d sseDeltaEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "running" {
		return
	}
	r.reasoning.WriteString(d.Reasoning)
	r.content.WriteString(d.Content)
	ev := runEvent{Event: "delta", Reasoning: d.Reasoning, Content: d.Content}
	r.fanoutLocked(ev)
}

func (r *ChatRun) setMetrics(m runMetrics) {
	r.mu.Lock()
	r.metrics = m
	r.mu.Unlock()
}

func (r *ChatRun) finish(status, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "running" {
		return
	}
	r.status = status
	r.errMsg = errMsg
	r.endedAt = time.Now()
	term := r.snapshotLocked()
	term.Event = "done"
	r.fanoutLocked(term)
	for ch := range r.subscribers {
		close(ch)
		delete(r.subscribers, ch)
	}
}

// fanoutLocked non-blockingly sends to each subscriber; a full buffer drops the
// event for that slow subscriber, which recovers on its next reconnect snapshot.
func (r *ChatRun) fanoutLocked(ev runEvent) {
	for ch := range r.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (r *ChatRun) buffered() (reasoning, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasoning.String(), r.content.String()
}

type chatRunRegistry struct {
	mu         sync.Mutex
	byUser     map[string]map[string]*ChatRun
	byID       map[string]*ChatRun
	maxPerUser int
}

func NewChatRunRegistry(maxPerUser int) *chatRunRegistry {
	if maxPerUser <= 0 {
		maxPerUser = 5
	}
	return &chatRunRegistry{byUser: map[string]map[string]*ChatRun{}, byID: map[string]*ChatRun{}, maxPerUser: maxPerUser}
}

// start registers a running placeholder (used by tests); production uses
// startRun in the executor which supplies a cancel func.
func (reg *chatRunRegistry) start(userID, chatID string) (*ChatRun, error) {
	return reg.add(userID, chatID, func() { /* no-op: test-only placeholder, no real cancel to run */ })
}

func (reg *chatRunRegistry) add(userID, chatID string, cancel func()) (*ChatRun, error) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	chats := reg.byUser[userID]
	if chats == nil {
		chats = map[string]*ChatRun{}
		reg.byUser[userID] = chats
	}
	if _, ok := chats[chatID]; ok {
		return nil, ErrRunAlreadyActive
	}
	if len(chats) >= reg.maxPerUser {
		return nil, ErrTooManyRuns
	}
	run := newChatRun("run_"+compactHex(16), chatID, userID, cancel)
	chats[chatID] = run
	reg.byID[run.ID] = run
	return run, nil
}

func (reg *chatRunRegistry) Get(userID, chatID string) *ChatRun {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.byUser[userID][chatID]
}

func (reg *chatRunRegistry) GetByID(userID, runID string) *ChatRun {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	run := reg.byID[runID]
	if run == nil || run.UserID != userID {
		return nil
	}
	return run
}

// ActiveForUser returns the user's runs that are CURRENTLY running. A terminal
// run lingering in the registry for its eviction grace period (runEvictionDelay)
// is NOT active and must be excluded — otherwise a reopened browser would
// resubscribe to a finished run and fabricate a chat buffer from an empty base
// (see ChatStore bootstrap). The candidates are snapshotted under the registry
// lock, which is then released BEFORE reading each run's status via its own
// mutex, so reg.mu and a run's mu are never held at the same time (no lock-order
// inversion / deadlock).
func (reg *chatRunRegistry) ActiveForUser(userID string) []*ChatRun {
	reg.mu.Lock()
	candidates := make([]*ChatRun, 0, len(reg.byUser[userID]))
	for _, r := range reg.byUser[userID] {
		candidates = append(candidates, r)
	}
	reg.mu.Unlock()

	out := make([]*ChatRun, 0, len(candidates))
	for _, r := range candidates {
		if r.statusValue() == "running" {
			out = append(out, r)
		}
	}
	return out
}

// retire unlinks a run from byUser — freeing its per-chat slot AND the per-user
// cap count — the instant it goes terminal, while KEEPING it in byID so
// GetByID still serves a late subscriber the terminal snapshot until the
// eviction-delay remove() drops it entirely. Without this, a finished run would
// linger in byUser for runEvictionDelay (30s), wrongly rejecting the chat's next
// turn as already-active (409) and counting against the per-user cap (429).
// Called from Server.finishRun on every terminal path.
//
// Only reads the run's IMMUTABLE identity fields (UserID/ChatID) under reg.mu —
// never a run.mu-guarded field — so reg.mu and a run's mu are never held at once
// (no lock-order inversion). Idempotent, and guarded like remove so a NEWER run
// that reused the same chat slot is not unlinked: unlink byUser only if it still
// points to THIS run.
func (reg *chatRunRegistry) retire(run *ChatRun) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if chats := reg.byUser[run.UserID]; chats != nil {
		if chats[run.ChatID] == run {
			delete(chats, run.ChatID)
		}
		if len(chats) == 0 {
			delete(reg.byUser, run.UserID)
		}
	}
}

// remove drops a run from the active maps (called after eviction delay). It is
// idempotent w.r.t. a run already retired from byUser (the guards below only act
// when the maps still point to THIS run), so a newer run that reused the same
// chat slot or run id is never disturbed.
func (reg *chatRunRegistry) remove(run *ChatRun) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if chats := reg.byUser[run.UserID]; chats != nil {
		if chats[run.ChatID] == run {
			delete(chats, run.ChatID)
		}
		if len(chats) == 0 {
			delete(reg.byUser, run.UserID)
		}
	}
	if reg.byID[run.ID] == run {
		delete(reg.byID, run.ID)
	}
}

// cancelChat cancels the active run (if any) for a chat and returns whether one
// was canceled.
func (reg *chatRunRegistry) cancelChat(userID, chatID string) bool {
	run := reg.Get(userID, chatID)
	if run == nil {
		return false
	}
	run.mu.Lock()
	cancel := run.cancel
	run.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// compactHex mirrors compactRandomHex used elsewhere; kept package-local for the
// gateway. (crypto/rand.)
func compactHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runCheckpointInterval is the cadence of the periodic pending-checkpoint write
// during a run. It is a package-level var (not const) so tests can shrink it to
// force multiple checkpoint ticks within a run.
var runCheckpointInterval = 3 * time.Second

const runEvictionDelay = 30 * time.Second

// PrepareRunResult bundles the prepared history + settings from
// portal.PrepareChatRun for the executor.
type PrepareRunResult struct {
	History  []portal.ChatAPIMessage
	Settings portal.ChatRunSettings
}

func (r *ChatRun) statusValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// reserveRun atomically claims the single-active slot for a chat (and enforces
// the per-user cap) BEFORE any transcript mutation, so a rejected start commits
// nothing. It returns the reserved run and the context its executor will use.
// On rejection the freshly-created context is cancelled and nil is returned.
// The caller MUST either launchRun the reservation or releaseRun it.
func (s *Server) reserveRun(owner auth.Token, chatID string) (*ChatRun, context.Context, error) {
	ctx, cancel := context.WithCancel(context.Background())
	run, err := s.ChatRuns.add(owner.UserID, chatID, cancel)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return run, ctx, nil
}

// launchRun starts the executor goroutine for a previously reserved run.
func (s *Server) launchRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	go s.executeRun(ctx, owner, run, prep)
}

// releaseRun cancels a reserved-but-not-launched run's context and drops it from
// the registry, fully freeing the slot so a retry is not wrongly rejected. Only
// valid before launchRun starts the executor goroutine.
func (s *Server) releaseRun(run *ChatRun) {
	run.mu.Lock()
	cancel := run.cancel
	run.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.ChatRuns.remove(run)
}

// startChatRun registers a run and launches its executor goroutine. It is the
// reserve-then-launch shorthand used by direct callers/tests that have no
// separate prepare step to interleave.
func (s *Server) startChatRun(owner auth.Token, chatID string, prep PrepareRunResult) (*ChatRun, error) {
	run, ctx, err := s.reserveRun(owner, chatID)
	if err != nil {
		return nil, err
	}
	s.launchRun(ctx, owner, run, prep)
	return run, nil
}

func (s *Server) executeRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	defer func() {
		// Evict after a grace period so late subscribers still see the terminal.
		time.AfterFunc(runEvictionDelay, func() { s.ChatRuns.remove(run) })
	}()

	body, err := buildChatCompletionsBody(prep)
	if err != nil {
		s.finishRun(ctx, owner, run, "error", err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.finishRun(ctx, owner, run, "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, "1")
	req.Header.Set(internalAuthHeaderName, s.internalAuthSecret)
	req.Header.Set(internalUserHeaderName, owner.UserID)
	req.Header.Set(sessionHeaderName, run.ChatID)
	if prep.Settings.RunAsTokenID != "" {
		req.Header.Set(runAsHeaderName, prep.Settings.RunAsTokenID)
	}
	// Carry the chat's (self-healed, see portal.Service.PrepareChatRun) per-run
	// server override over the loopback call as the same two headers the
	// gateway's own applyServerOverride re-authorizes on every routed request
	// (never trusting this value's provenance — see auth.go's doc on the two
	// consts and applyServerOverride's doc in server.go). Precedence is
	// TOKEN-FIRST: when the run-as token carries its own ServerOverride, that
	// governs and this chat header is ignored (the chat UI locks its
	// server-override controls to match); the chat header applies only when the
	// run-as token has none. So sending it unconditionally here is safe.
	if prep.Settings.ServerOverride != "" {
		req.Header.Set(serverOverrideHeaderName, prep.Settings.ServerOverride)
		if prep.Settings.ServerOverrideForceUnreachable {
			req.Header.Set(serverOverrideForceHeaderName, "1")
		} else {
			req.Header.Set(serverOverrideForceHeaderName, "0")
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			s.finishRun(context.Background(), owner, run, "canceled", "")
			return
		}
		s.finishRun(ctx, owner, run, "error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.finishRun(ctx, owner, run, "error", "upstream status "+resp.Status)
		return
	}

	s.consumeRunStream(ctx, owner, run, resp)
}

// consumeRunStream reads the loopback SSE, publishing deltas, computing metrics,
// checkpointing every runCheckpointInterval, and committing on terminal.
func (s *Server) consumeRunStream(ctx context.Context, owner auth.Token, run *ChatRun, resp *http.Response) {
	ticker := time.NewTicker(runCheckpointInterval)
	defer ticker.Stop()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // periodic checkpoint
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				reasoning, content := run.buffered()
				m := run.currentMetrics()
				_ = s.Portal.CheckpointAssistant(context.Background(), owner, run.ChatID, portal.AssistantTurn{
					Reasoning: reasoning, Content: content, TTFTMs: m.TTFTMs, ReasoningMs: m.ReasoningMs, TPS: m.TPS,
				})
			}
		}
	}()

	start := time.Now()
	var firstContentAt time.Time
	var reasoningStart time.Time
	status, errMsg := "completed", ""

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		ev, kind := parseChatSSELine(scanner.Text())
		switch kind {
		case sseDone:
			goto finish
		case sseError:
			status, errMsg = "error", ev.ErrMsg
			goto finish
		case sseDelta:
			if ev.Reasoning != "" && reasoningStart.IsZero() {
				reasoningStart = time.Now()
			}
			if ev.Content != "" && firstContentAt.IsZero() {
				firstContentAt = time.Now()
				m := run.currentMetrics()
				m.TTFTMs = time.Since(start).Milliseconds()
				if !reasoningStart.IsZero() {
					m.ReasoningMs = firstContentAt.Sub(reasoningStart).Milliseconds()
				}
				run.setMetrics(m)
			}
			run.publish(ev)
			if !firstContentAt.IsZero() {
				_, content := run.buffered()
				secs := time.Since(firstContentAt).Seconds()
				if secs > 0 {
					m := run.currentMetrics()
					m.TPS = float64(len(content)) / secs
					run.setMetrics(m)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			status, errMsg = "canceled", ""
		} else {
			status, errMsg = "error", err.Error()
		}
	}
finish:
	// Signal the checkpoint goroutine to stop AND wait for any in-flight
	// CheckpointAssistant("pending") write to complete before committing, so
	// CommitAssistant is deterministically the last writer to the transcript.
	// Without this join, a checkpoint's read-modify-write could land after the
	// commit and leave the trailing assistant message stuck at "pending".
	close(done)
	wg.Wait()
	s.finishRun(context.Background(), owner, run, status, errMsg)
}

func (r *ChatRun) currentMetrics() runMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metrics
}

// finishRun commits the final assistant turn (mapped status) and marks the run
// terminal. The persisted status uses the transcript vocabulary
// (complete/error/canceled).
func (s *Server) finishRun(ctx context.Context, owner auth.Token, run *ChatRun, status, errMsg string) {
	reasoning, content := run.buffered()
	m := run.currentMetrics()
	persistStatus := map[string]string{"completed": "complete", "error": "error", "canceled": "canceled"}[status]
	if persistStatus == "" {
		persistStatus = "complete"
	}
	// A failed terminal commit leaves the trailing assistant message stuck at
	// "pending", so a later restart would infer a false "interrupted". Log it
	// (control flow is unchanged — the run still finishes) so the failure is not
	// silently swallowed.
	if err := s.Portal.CommitAssistant(ctx, owner, run.ChatID, portal.AssistantTurn{
		Reasoning: reasoning, Content: content, TTFTMs: m.TTFTMs, ReasoningMs: m.ReasoningMs, TPS: m.TPS,
	}, persistStatus); err != nil {
		log.Printf("chat run %s: commit assistant turn failed: %v", run.ID, err)
	}
	run.finish(status, errMsg)
	// Free the per-chat/cap slot immediately on terminal so the chat's next turn
	// (and the user's next chat) is not blocked during the 30s eviction grace;
	// the run stays reachable by id for late terminal snapshots (see retire).
	s.ChatRuns.retire(run)
}

func buildChatCompletionsBody(prep PrepareRunResult) ([]byte, error) {
	payload := map[string]any{
		"model":    prep.Settings.Model,
		"messages": prep.History,
		"stream":   true,
	}
	if prep.Settings.Temperature != 0 {
		payload["temperature"] = prep.Settings.Temperature
	}
	if prep.Settings.MaxTokens > 0 {
		payload["max_tokens"] = prep.Settings.MaxTokens
	}
	return json.Marshal(payload)
}
