// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
)

// handlePortalChats serves the chat collection: GET lists the principal's chat
// summaries, POST creates a new chat (title + opaque content). Content bodies
// can exceed the default 1 MiB JSON limit (the service caps the pre-seal
// content at a few MiB, surfacing ErrChatTooLarge as 400), so the body is read
// uncapped and the service is the size authority.
func (s *Server) handlePortalChats(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		chats, err := s.Portal.ListChats(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.chat_list_failed", "chat list failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, portal.ChatListResponse{Data: chats})
	case http.MethodPost:
		raw, ok := readRawJSONUnlimited(w, r)
		if !ok {
			return
		}
		var req portal.CreateChatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateChat(r.Context(), token, req)
		if err != nil {
			writePortalChatError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalChatItem dispatches everything under /api/portal/chats/: the
// single-chat CRUD (GET/PUT/DELETE on /{id}) plus the run sub-paths (start,
// SSE events, cancel) and the active-runs list. Chat ids are "chat_"+hex so
// they never collide with the "runs" path segment.
func (s *Server) handlePortalChatItem(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/portal/chats/")
	parts := strings.Split(suffix, "/")
	// /api/portal/chats/runs/active
	if len(parts) == 2 && parts[0] == "runs" && parts[1] == "active" {
		s.handleActiveChatRuns(w, r)
		return
	}
	chatID := parts[0]
	switch {
	case len(parts) == 1: // /{id}
		s.handlePortalChatSingle(w, r, chatID)
	case len(parts) == 2 && parts[1] == "runs": // /{id}/runs
		s.handleStartChatRun(w, r, chatID)
	case len(parts) == 4 && parts[1] == "runs" && parts[3] == "events": // /{id}/runs/{runId}/events
		s.handleChatRunEvents(w, r, chatID, parts[2])
	case len(parts) == 4 && parts[1] == "runs" && parts[3] == "cancel": // /{id}/runs/{runId}/cancel
		s.handleCancelChatRun(w, r, chatID, parts[2])
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response("portal.not_found", "not found", ""))
	}
}

// handlePortalChatSingle serves a single chat: GET opens (decrypts) it, PUT
// saves title+content, DELETE cancels any active run then removes it. All are
// owner-scoped in the service; a foreign or missing chat is 404 with no
// existence leak.
func (s *Server) handlePortalChatSingle(w http.ResponseWriter, r *http.Request, id string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("portal.chat_not_found", "chat not found", ""))
		return
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetChat(r.Context(), token, id)
		if err != nil {
			writePortalChatError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSONUnlimited(w, r)
		if !ok {
			return
		}
		var req portal.UpdateChatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.SaveChat(r.Context(), token, id, req)
		if err != nil {
			writePortalChatError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if s.ChatRuns != nil {
			s.ChatRuns.cancelChat(token.UserID, id) // tear down an active run before deleting
		}
		if err := s.Portal.DeleteChat(r.Context(), token, id); err != nil {
			writePortalChatError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// portalChatErrRows are writePortalChatError's mapper-specific rows (checked
// before sharedErrorMap); portal.ErrChatNotFound and portal.ErrChatTooLarge
// map identically in writePortalRunError and live in sharedErrorMap instead.
// store.ErrNotFound maps to a different code in other mappers, so it stays
// here (mirroring the original combined portal.ErrChatNotFound/
// store.ErrNotFound case: both still resolve to portal.chat_not_found, one
// via the shared row, one via this one).
var portalChatErrRows = []errRow{
	{err: store.ErrNotFound, status: http.StatusNotFound, code: "portal.chat_not_found", msg: "chat not found"},
	{err: portal.ErrChatTitleInvalid, status: http.StatusBadRequest, code: "portal.chat_title_invalid", msg: "chat title is invalid"},
}

// writePortalChatError maps the chat service's error sentinels to HTTP
// responses: not-found (missing or foreign, no leak) -> 404, title/size
// validation -> 400, everything else (seal/open/cipher/store failures) -> 500.
// The 500 arm never echoes chat content; only the error is logged.
func writePortalChatError(w http.ResponseWriter, err error) {
	if writeMappedError(w, err, portalChatErrRows, 0, "", "") {
		return
	}
	log.Printf("portal: chat operation failed: %v", err)
	writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.chat_failed", "chat operation failed", ""))
}
