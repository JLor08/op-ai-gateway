// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"time"
)

// availabilityPointDTO renders one availability sample for the timeline: the
// derived health state, the reachable/active counts, and whether the ServerAgent
// was reporting. `t` is the RFC3339 sample timestamp.
type availabilityPointDTO struct {
	T                string `json:"t"` // RFC3339
	Health           string `json:"health"`
	ReachableCount   int    `json:"reachable_count"`
	ActiveCount      int    `json:"active_count"`
	AgentReporting   bool   `json:"agent_reporting"`
	NetbirdConnected bool   `json:"netbird_connected"`
	// GapBefore marks that the RAW predecessor of this point was more than the gap
	// floor away (an observer gap). The frontend paints the interval leading into a
	// gap_before=true point as "unknown" instead of holding the prior state.
	GapBefore bool `json:"gap_before"`
}

// availabilityHistoryDTO is the GET /availability body: the reduced window of
// points plus its [from,to] RFC3339 bounds.
type availabilityHistoryDTO struct {
	Points []availabilityPointDTO `json:"points"`
	From   string                 `json:"from"`
	To     string                 `json:"to"`
}

// availabilityWindowSeconds mirrors the frontend TS_WINDOW_SECONDS whitelist so
// the availability view offers the same window tokens as the Activity view.
var availabilityWindowSeconds = map[string]int{
	"5m": 300, "15m": 900, "30m": 1800, "1h": 3600, "6h": 21600, "12h": 43200,
	"1d": 86400, "1w": 604800, "2w": 1209600, "1mo": 2592000, "3mo": 7776000,
	"6mo": 15552000, "1y": 31536000,
}

// defaultAvailabilityWindow is used when `window=` is missing or unknown.
const defaultAvailabilityWindow = time.Hour

// resolveAvailabilityWindow maps a raw `window=` token to its duration, defaulting
// to defaultAvailabilityWindow for a missing/unknown value.
func resolveAvailabilityWindow(token string) time.Duration {
	if secs, ok := availabilityWindowSeconds[token]; ok {
		return time.Duration(secs) * time.Second
	}
	return defaultAvailabilityWindow
}

// availabilityPointsFromSamples maps a slice of samples to their wire points,
// preserving order and always returning a non-nil slice.
func availabilityPointsFromSamples(samples []routing.ServerAvailabilitySample) []availabilityPointDTO {
	points := make([]availabilityPointDTO, 0, len(samples))
	for _, s := range samples {
		points = append(points, availabilityPointDTO{
			T:                s.ReportedAt.UTC().Format(time.RFC3339),
			Health:           s.Health,
			ReachableCount:   s.ReachableCount,
			ActiveCount:      s.ActiveCount,
			AgentReporting:   s.AgentReporting,
			NetbirdConnected: s.NetbirdConnected,
			GapBefore:        s.GapBefore,
		})
	}
	return points
}

// handleServerAvailability serves GET /api/portal/servers/{id}/availability: the
// reduced persisted availability window for the resolved ?window=. Owner/admin-gated
// through Portal.ServerAvailability -> authorizeServer (404 no existence leak).
func (s *Server) handleServerAvailability(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	window := resolveAvailabilityWindow(r.URL.Query().Get("window"))
	samples, err := s.Portal.ServerAvailability(r.Context(), token, serverID, window)
	if err != nil {
		writePortalServerError(w, err, "server.availability_failed")
		return
	}
	to := time.Now().UTC()
	from := to.Add(-window)
	writeJSON(w, http.StatusOK, availabilityHistoryDTO{
		Points: availabilityPointsFromSamples(samples),
		From:   from.Format(time.RFC3339),
		To:     to.Format(time.RFC3339),
	})
}
