// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"strings"
)

// handlePortalPreferences returns the current user's UI preferences as a JSON
// object mapping each stored key to its opaque JSON value (an empty object when
// none are stored). GET only, guarded by requireWebScope.
func (s *Server) handlePortalPreferences(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	prefs, err := s.Portal.UserPreferences(r.Context(), token.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.preferences_read_failed", "failed to read preferences", ""))
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// handlePortalPreferenceItem upserts a single UI preference for the current user.
// The key is the URL path segment after /api/portal/preferences/ (net/http has
// already percent-decoded r.URL.Path; may contain dots). The request body is the
// raw JSON value stored opaquely. PUT only, guarded by requireWebScope (which
// enforces CSRF on this unsafe method).
func (s *Server) handlePortalPreferenceItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	// r.URL.Path is already decoded by net/http; do NOT unescape again (that would
	// double-decode keys containing a literal percent-escape sequence).
	key := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/portal/preferences/"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response("portal.preference_key_required", "preference key required", ""))
		return
	}
	// readRawJSON validates the body is exactly one valid JSON value (400 otherwise).
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	if err := s.Portal.SetUserPreference(r.Context(), token.UserID, key, json.RawMessage(raw)); err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("portal.preference_write_failed", "failed to store preference", ""))
		return
	}
	// The frontend request() helper treats an empty body as an error, so always
	// respond with a non-empty JSON body on success.
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
