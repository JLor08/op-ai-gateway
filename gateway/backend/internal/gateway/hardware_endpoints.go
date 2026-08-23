// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/auth"
	"time"
)

// hardwareDTO is the GET /hardware body: whether a report exists, its timestamps,
// and the structured report (the stored canonical JSON, passed through as raw JSON
// so the client gets the parsed inventory). When no agent has reported yet, the
// handler returns 200 with available:false so the view can show an empty state.
type hardwareDTO struct {
	Available   bool            `json:"available"`
	CollectedAt string          `json:"collected_at,omitempty"`
	UpdatedAt   string          `json:"updated_at,omitempty"`
	Report      json.RawMessage `json:"report,omitempty"`
}

// handleServerHardware serves GET /api/portal/servers/{id}/hardware. Owner/admin-
// gated through Portal.ServerHardware -> authorizeServer (404 no existence leak).
func (s *Server) handleServerHardware(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	hw, found, err := s.Portal.ServerHardware(r.Context(), token, serverID)
	if err != nil {
		writePortalServerError(w, err, "server.hardware_failed")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, hardwareDTO{Available: false})
		return
	}
	report := json.RawMessage(hw.ReportJSON)
	if len(hw.ReportJSON) == 0 {
		report = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, hardwareDTO{
		Available:   true,
		CollectedAt: hw.CollectedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   hw.UpdatedAt.UTC().Format(time.RFC3339),
		Report:      report,
	})
}
