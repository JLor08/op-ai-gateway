// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/auth"
)

// handlePortalServerGPUVendorNoteDismiss backs
// POST /api/portal/servers/{id}/gpu-vendor-note/dismiss: it lowers the review
// note a mixed-vendor legacy-vendor backfill raised on the server (the legacy
// vendor itself stays). Owner/admin-scoped through the service
// (authorizeServer -> 404 no-leak); the session CSRF rule is applied before
// the dispatcher like for every state-changing portal call. 204 on success.
func (s *Server) handlePortalServerGPUVendorNoteDismiss(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.Portal.DismissServerGPUVendorNote(r.Context(), token, id); err != nil {
		writePortalServerError(w, err, codeServerUpdateFailed)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
