// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
)

// The two manual mutations of the audio answer lifecycle (#281), both under
// /api/portal/chats/{id}/ and both routed by handlePortalChatItem:
//
//	DELETE /api/portal/chats/{id}/messages/{messageID}/audio   one answer's audio
//	POST   /api/portal/chats/{id}/audio/prune   {"keep_last": N}   down to N answers
//
// Each rewrites the chat's WHOLE stored document into the audio_removed marker
// state and hard-deletes the chat_blobs rows that leaves unreferenced, in one
// atomic save (portal.Service.DeleteMessageAudio / PruneChatAudio). That is what PUT
// /chats/{id} does to the document too, so both take PUT's guard, chatRunActive: while
// a run is live for the chat the answer is 409 portal.chat_run_active and nothing is
// written -- the run owns the transcript tail, and a rewrite racing it would overwrite
// its checkpoint or its terminal commit.
//
// Everything else is the chat endpoints' own: the standard portal auth (a session
// cookie also needs the CSRF header, a bearer token does not; scope gateway:use), the
// service's owner scoping (another owner's chat is the same 404 as a missing one, no
// existence leak; the run guard is keyed by the caller's own user, so it never answers
// for somebody else's run and a foreign chat still reads as 404, not 409), and
// writePortalChatError as the mapper.

// pruneChatAudioRequest is the prune body. KeepLast is a pointer so a body that omits
// it (or sends null) is told apart from keep-none: 0 is a real count, "remove all
// stored audio", and an absent one must not silently become it.
type pruneChatAudioRequest struct {
	KeepLast *int `json:"keep_last"`
}

// handleDeleteMessageAudio serves DELETE /api/portal/chats/{id}/messages/{messageID}/audio:
// the audio of that one answer -- every part of a long one -- is removed and the answer
// becomes the not-stored marker. A message with no stored audio is a 200 no-op, so the
// call is safe to repeat. 200 {"ok":true}.
func (s *Server) handleDeleteMessageAudio(w http.ResponseWriter, r *http.Request, chatID, messageID string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if chatID == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("portal.chat_not_found", "chat not found", ""))
		return
	}
	if s.chatRunActive(token.UserID, chatID) {
		writePortalChatError(w, ErrRunAlreadyActive)
		return
	}
	if err := s.Portal.DeleteMessageAudio(r.Context(), token, chatID, messageID); err != nil {
		writePortalChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePruneChatAudio serves POST /api/portal/chats/{id}/audio/prune: the chat's stored
// audio is pruned down to the newest keep_last answers that hold it (0 removes all), the
// retroactive half of the retention setting. The client states only the count; which
// answers and blobs go is computed server-side from the stored document. A chat already
// within the count is a 200 no-op. A missing or negative keep_last is 400
// portal.chat_audio_keep_last_invalid, a value that is not a whole number the decoder's
// request.invalid_json. 200 {"ok":true}.
func (s *Server) handlePruneChatAudio(w http.ResponseWriter, r *http.Request, chatID string) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if chatID == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response("portal.chat_not_found", "chat not found", ""))
		return
	}
	if s.chatRunActive(token.UserID, chatID) {
		writePortalChatError(w, ErrRunAlreadyActive)
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req pruneChatAudioRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if req.KeepLast == nil {
		writePortalChatError(w, portal.ErrChatAudioKeepLastInvalid)
		return
	}
	if err := s.Portal.PruneChatAudio(r.Context(), token, chatID, *req.KeepLast); err != nil {
		writePortalChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
