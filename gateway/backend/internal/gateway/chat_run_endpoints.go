// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"time"
)

// startRunRequest is the POST body for /api/portal/chats/{id}/runs. Exactly one
// of UserMessage / EditedHistory is meaningful: a new user turn, or a replaced
// history (edit/regenerate). Content is passed through verbatim so image parts
// survive.
type startRunRequest struct {
	UserMessage   json.RawMessage        `json:"user_message"`
	EditedHistory []json.RawMessage      `json:"edited_history"`
	Settings      portal.ChatRunSettings `json:"settings"`
}

// startRunResponse is the 201 body naming the launched run.
//
// It carries the kind as well as the snapshot does, because between this 201
// and the first SSE snapshot the sending tab knows nothing about the run: a
// kind that arrived only on the snapshot would leave that window rendering the
// TEXT pending state -- the character counter reading zero -- on the most
// common path of the very feature that exists to remove it.
type startRunResponse struct {
	RunID  string `json:"run_id"`
	ChatID string `json:"chat_id"`
	Status string `json:"status"`
	// Omitted for a text run, so an existing client's 201 is unchanged.
	Kind      string `json:"kind,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms,omitempty"`
}

// activeRunDTO is one entry of the active-runs list. Kind and ElapsedMs let a
// REOPENED browser render the same pending state, with the same true clock, as
// the tab that started the run -- which is the whole reason the age is measured
// by the server rather than from the moment of Send.
type activeRunDTO struct {
	ChatID    string `json:"chat_id"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	Kind      string `json:"kind,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms,omitempty"`
}

// portalRunErrRows are writePortalRunError's mapper-specific rows (checked
// before sharedErrorMap); portal.ErrChatNotFound and portal.ErrChatTooLarge
// map identically in writePortalChatError and live in sharedErrorMap
// instead. ErrRunAlreadyActive lives there too now (task 9: PUT /chats/{id}
// -- via writePortalChatError -- refuses identically while a run is active,
// so the single definition belongs where both mappers read it, not in this
// mapper-specific table). store.ErrNotFound maps to a different code in
// other mappers, so it must stay here (mirroring the original combined
// portal.ErrChatNotFound/store.ErrNotFound case: both still resolve to the
// same portal.chat_not_found response, one via the shared row, one via this
// one).
var portalRunErrRows = []errRow{
	{err: ErrTooManyRuns, status: http.StatusTooManyRequests, code: "portal.chat_run_limit", msg: "too many concurrent runs"},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: "portal.chat_not_found", msg: "chat not found"},
}

// writePortalRunError maps run/chat error sentinels to HTTP responses:
// already-active -> 409, per-user cap -> 429, missing/foreign chat -> 404 (no
// existence leak), oversized content -> 400, everything else -> 500.
func writePortalRunError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalRunErrRows, http.StatusInternalServerError, "portal.chat_run_failed", "run failed")
}

// handleStartChatRun commits the user turn (or replaces history) into the chat
// and launches a detached run executor, returning 201 with the run id.
func (s *Server) handleStartChatRun(w http.ResponseWriter, r *http.Request, chatID string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	var req startRunRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	// Reserve the single-active slot FIRST so a rejected start (already-active /
	// over cap) mutates nothing: PrepareChatRun below appends the user turn, and
	// must only run once the slot is ours.
	run, runCtx, err := s.reserveRun(token, chatID)
	if err != nil {
		writePortalRunError(w, err)
		return
	}
	// Prepare (commit user turn / settings, incl. the server_override
	// self-heal) + get the API history. On failure the reserved slot is
	// released (no executor goroutine has started yet). The executor is fed
	// the SETTINGS PrepareChatRun RETURNS, not req.Settings verbatim — those
	// two can differ exactly when the self-heal cleared a server_override the
	// owner no longer manages, and the executor must honor the cleared value.
	history, settings, err := s.Portal.PrepareChatRun(r.Context(), token, chatID, portal.PrepareRunRequest{
		UserMessage:   req.UserMessage,
		EditedHistory: req.EditedHistory,
		Settings:      req.Settings,
	})
	if err != nil {
		s.releaseRun(run)
		writePortalRunError(w, err)
		return
	}
	// launchRun stamps run.kind from the PREPARED settings, so the 201 below
	// reports what the executor will actually run rather than what the client
	// asked for (PrepareChatRun forces the thread's pinned kind on every send
	// after the first).
	s.launchRun(runCtx, token, run, PrepareRunResult{History: history, Settings: settings})
	writeJSON(w, http.StatusCreated, startRunResponse{
		RunID: run.ID, ChatID: chatID, Status: "running",
		Kind: run.kindValue(), ElapsedMs: run.elapsedMs(),
	})
}

// handleChatRunEvents streams a run over SSE: an initial snapshot event replays
// everything generated so far, then subsequent delta events tail the run until a
// terminal done event. A run that is already terminal short-circuits after the
// snapshot. Reconnect-safe: EventSource auto-reconnects and the fresh snapshot
// re-syncs. Reuses the handlePortalUsageEvents SSE scaffold.
func (s *Server) handleChatRunEvents(w http.ResponseWriter, r *http.Request, chatID, runID string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	run := s.ChatRuns.GetByID(token.UserID, runID)
	if run == nil || run.ChatID != chatID {
		writeJSON(w, http.StatusNotFound, apierror.Response("portal.chat_run_not_found", "run not found", ""))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.stream_unsupported", "streaming unsupported", ""))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Clear the server WriteTimeout for this long-lived response. An unsupported
	// writer (httptest recorder) returns an error we intentionally ignore.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	snapshot, ch, unsub := run.subscribe()
	defer unsub()
	if !writeRunEvent(w, flusher, "snapshot", snapshot) {
		return
	}
	if snapshot.Status != "running" {
		return // already terminal; the snapshot carried everything
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			name := ev.Event
			if name == "" {
				name = "delta"
			}
			if !writeRunEvent(w, flusher, name, ev) {
				return
			}
			if name == "done" {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeRunEvent serializes a runEvent as an SSE frame (`event: <name>` +
// `data: <json>`) and flushes. Returns false on a write/marshal failure so the
// caller can stop the stream.
func writeRunEvent(w http.ResponseWriter, flusher http.Flusher, name string, ev runEvent) bool {
	payload, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := io.WriteString(w, "event: "+name+"\ndata: "+string(payload)+"\n\n"); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// handleCancelChatRun cancels an owned, active run (the Stop button). Cancelling
// a missing/foreign/already-terminal run is 404.
func (s *Server) handleCancelChatRun(w http.ResponseWriter, r *http.Request, chatID, runID string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	run := s.ChatRuns.GetByID(token.UserID, runID)
	if run == nil || run.ChatID != chatID {
		writeJSON(w, http.StatusNotFound, apierror.Response("portal.chat_run_not_found", "run not found", ""))
		return
	}
	run.cancelContext()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleActiveChatRuns lists the caller's currently active runs so a reopened
// browser can resubscribe to background chats.
func (s *Server) handleActiveChatRuns(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	runs := s.ChatRuns.ActiveForUser(token.UserID)
	out := make([]activeRunDTO, 0, len(runs))
	for _, run := range runs {
		// One acquisition of the run's own mutex per row, taken here rather
		// than inside ActiveForUser: reg.mu is already released by the time
		// this loop runs, and the documented lock order (never both at once)
		// depends on it staying that way.
		status, kind, elapsedMs := run.listView()
		out = append(out, activeRunDTO{
			ChatID: run.ChatID, RunID: run.ID, Status: status,
			Kind: kind, ElapsedMs: elapsedMs,
		})
	}
	writeJSON(w, http.StatusOK, map[string][]activeRunDTO{"data": out})
}
