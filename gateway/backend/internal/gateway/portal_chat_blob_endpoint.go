// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"log"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"strconv"
	"strings"
)

// chatBlobRoutePrefix is the route the serving endpoint is registered under;
// the rest of the path is the blob id (one segment, like the other
// /api/portal/chats/{id} routes, parsed by hand off the prefix).
const chatBlobRoutePrefix = "/api/portal/chat-blobs/"

// handlePortalChatBlob serves GET /api/portal/chat-blobs/{id}: the raw bytes of
// a chat image that the transcript references by this url (the run commit moved
// them out of the sealed document into chat_blobs). It feeds the browser's
// <img>, so it is session-authed like every other /api/portal/chats* read, not
// an inference endpoint (no bearer-or-internal ladder).
//
// Authorization is the owner-scoped part and is decided in the service
// (portal.Service.ChatBlobBytes), on the BLOB's own chat -- never on a chat
// that merely mentions the id, because a transcript can carry a url to anyone's
// blob (create and a bare autosave do not validate the urls they store). A blob
// that is missing, evicted, someone else's or of a type that is not servable is
// the same 404 with the blob-specific code (no existence leak).
//
// The body is user-influenced bytes on a same-origin, cookie-authenticated,
// navigable url, so it is served locked down: the stored (allow-listed) type
// and nothing else as Content-Type, nosniff, a CSP that forbids everything,
// inline disposition (an <img> renders it; a navigation to it cannot execute
// anything) and no caching of the decrypted bytes.
//
// A chat of N images opens N of these in parallel, so the session check is the
// passive one (requireWebScopeNoTouch): the same validation, without the
// per-request TouchSession write that would stampede the connection pool.
//
// Doc parity (docs task): reference/api-surface.md and openapi.yaml gain this
// route; cross-cutting/security-auth-rbac.md gains the ownership + hardening.
func (s *Server) handlePortalChatBlob(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScopeNoTouch(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, chatBlobRoutePrefix)
	if id == "" || strings.Contains(id, "/") {
		writePortalChatBlobError(w, portal.ErrChatBlobNotFound)
		return
	}
	data, contentType, err := s.Portal.ChatBlobBytes(r.Context(), token, id)
	if err != nil {
		writePortalChatBlobError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Content-Disposition", "inline")
	h.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// writePortalChatBlobError maps the serving endpoint's errors: the blob
// not-found sentinel (every way of not being the owner of a servable blob) ->
// 404 portal.chat_blob_not_found via sharedErrorMap; anything else (store,
// cipher or open failures) -> 500, logged but never echoed.
func writePortalChatBlobError(w http.ResponseWriter, err error) {
	if writeMappedError(w, err, nil, 0, "", "") {
		return
	}
	log.Printf("portal: chat blob read failed: %v", err)
	writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.chat_blob_failed", "chat image could not be read", ""))
}
