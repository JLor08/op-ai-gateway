// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/usage"
	"sort"
	"strings"
	"sync"
	"time"
)

// ActiveRequest is one in-flight inference request tracked by the gateway while
// its provider call is running. It carries only lightweight metadata (no
// payloads) so the "running connections" view can show what is currently in
// flight without touching request/response bodies.
type ActiveRequest struct {
	ID        string
	UserID    string
	TokenID   string
	TokenName string
	// ServiceID / ServiceName attribute the in-flight request to a Service
	// Account (Phase 1 service accounts) when it is being served by a service
	// token: ServiceID is the service's id, ServiceName its display name at
	// request start (denormalized, mirrors TokenName). Both "" for a
	// user-token/session request. Mirrors usage.Event.ServiceID/ServiceName.
	ServiceID   string
	ServiceName string
	ServerName  string
	ServerID    string // resolved routing target server id (swap-protection key)
	Model       string
	APIFlavor   string
	ReqPath     string
	// ProviderPath is the upstream endpoint path this request is calling (the
	// built-in translation's chat-completions path, or the native passthrough path);
	// it differs from ReqPath exactly when translation is happening.
	ProviderPath string
	// ProviderModel is the upstream model name the provider receives (the per-model
	// provider override target.ProviderModel; empty when the requested model is used
	// as-is). Mirrors the usage event's provider_model.
	ProviderModel string
	SessionID     string
	SessionSource string
	AgentID       string
	Stream        bool
	StartedAt     time.Time
}

// activeRegistry is a thread-safe, in-memory set of in-flight requests. It is
// held on the Server like usage.Broker: purely volatile (never persisted). Every
// mutation pokes the usage broker (when present) so the existing SSE stream
// (/api/portal/usage/events) fires at both request start and end, and the portal
// re-fetches the active list. All methods are nil-safe so a bare *Server built
// in a test keeps working.
type activeRegistry struct {
	mu            sync.RWMutex
	items         map[string]ActiveRequest
	lastCompleted map[string]time.Time
	broker        *usage.Broker
	now           func() time.Time
	admission     *admissionQueue
}

// setAdmission attaches the CP4 admission queue so Remove can wake a parked waiter
// when a slot frees on the completing request's server. Nil-safe.
func (a *activeRegistry) setAdmission(q *admissionQueue) {
	if a != nil {
		a.admission = q
	}
}

func newActiveRegistry(b *usage.Broker) *activeRegistry {
	return &activeRegistry{
		items:         make(map[string]ActiveRequest),
		lastCompleted: make(map[string]time.Time),
		broker:        b,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// Add stores the in-flight request and pokes the broker so subscribers refetch.
func (a *activeRegistry) Add(req ActiveRequest) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.items[req.ID] = req
	a.mu.Unlock()
	if a.broker != nil {
		a.broker.Publish()
	}
}

// Remove drops the in-flight request by id and pokes the broker.
func (a *activeRegistry) Remove(id string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	freedServer := ""
	if req, ok := a.items[id]; ok && req.ServerID != "" {
		freedServer = req.ServerID
		a.lastCompleted[req.ServerID] = a.now()
	}
	delete(a.items, id)
	a.mu.Unlock()
	a.admission.release(freedServer) // wake a queued waiter for that server (nil-safe; no-op if freedServer=="")
	if a.broker != nil {
		a.broker.Publish()
	}
}

// CountByServerName reports how many in-flight requests are currently routed to
// the server with the given name. Used by the benchmark idle-gate to refuse a
// run while the server still has traffic. A nil registry returns 0.
func (a *activeRegistry) CountByServerName(name string) int {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	n := 0
	for _, req := range a.items {
		if req.ServerName == name {
			n++
		}
	}
	return n
}

// ServerActivity reports, for routing swap-protection, how many requests are currently
// in flight on the given server id and when the last one completed (zero time if never).
// Nil-safe. Fed only by real user traffic (benchmark traffic never registers here).
func (a *activeRegistry) ServerActivity(serverID string) (int, time.Time) {
	if a == nil {
		return 0, time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	inFlight := 0
	for _, req := range a.items {
		if req.ServerID == serverID {
			inFlight++
		}
	}
	return inFlight, a.lastCompleted[serverID]
}

// Snapshot returns a copy of the current in-flight requests. A nil registry
// returns nil. The returned slice is safe for the caller to mutate/sort.
func (a *activeRegistry) Snapshot() []ActiveRequest {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ActiveRequest, 0, len(a.items))
	for _, req := range a.items {
		out = append(out, req)
	}
	return out
}

// activeRequestDTO is the JSON shape returned by GET /api/portal/usage/active.
type activeRequestDTO struct {
	ID            string `json:"id"`
	UserID        string `json:"user_id"`
	UserName      string `json:"user_name"`
	TokenID       string `json:"token_id"`
	TokenName     string `json:"token_name"`
	ServiceID     string `json:"service_id"`
	ServiceName   string `json:"service_name"`
	ServerName    string `json:"server_name"`
	Model         string `json:"model"`
	APIFlavor     string `json:"api_flavor"`
	ReqPath       string `json:"req_path"`
	ProviderPath  string `json:"provider_path"`
	ProviderModel string `json:"provider_model"`
	SessionID     string `json:"session_id"`
	SessionSource string `json:"session_source"`
	AgentID       string `json:"agent_id"`
	Stream        bool   `json:"stream"`
	StartedAt     string `json:"started_at"`
}

// handlePortalUsageActive returns the in-flight requests visible to the caller.
// Scope filtering mirrors the usage list: an owner sees only their own requests
// unless scope=all AND the caller has the admin scope (then all are returned).
// Rows are sorted by StartedAt ascending (oldest/longest-running first).
func (s *Server) handlePortalUsageActive(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	principal, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	all := r.URL.Query().Get("scope") == "all" && principal.HasScope("admin")

	// Optional filters mirroring the usage list: an admin may pin to a specific
	// user; a token filter (incl. the NoTokenWire chat sentinel -> empty token_id)
	// applies to everyone. A non-admin's user_id is ignored (they only ever see
	// their own rows via the scope gate below).
	filterUserID := ""
	if principal.HasScope("admin") {
		filterUserID = strings.TrimSpace(r.URL.Query().Get("user_id"))
	}
	hasTokenFilter := r.URL.Query().Has("token_id")
	tokenID := r.URL.Query().Get("token_id")
	if tokenID == NoTokenWire {
		tokenID = ""
	}

	rows := s.Active.Snapshot()
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].StartedAt.Before(rows[j].StartedAt)
	})

	filtered := make([]ActiveRequest, 0, len(rows))
	for _, row := range rows {
		if !all && row.UserID != principal.UserID {
			continue
		}
		if filterUserID != "" && row.UserID != filterUserID {
			continue
		}
		if hasTokenFilter && row.TokenID != tokenID {
			continue
		}
		filtered = append(filtered, row)
	}

	// Best-effort display-name resolution: label each row with the user's name
	// (like the usage table's denormalised user_name) so the frontend can show
	// the owner column. A nil Portal (some tests build a bare Server) or an
	// unresolved id simply leaves UserName empty; the frontend falls back to the id.
	var names map[string]string
	if s.Portal != nil {
		ids := make([]string, 0, len(filtered))
		for _, row := range filtered {
			ids = append(ids, row.UserID)
		}
		names = s.Portal.DisplayNames(r.Context(), ids)
	}

	dtos := make([]activeRequestDTO, 0, len(filtered))
	for _, row := range filtered {
		dtos = append(dtos, activeRequestDTO{
			ID:            row.ID,
			UserID:        row.UserID,
			UserName:      names[row.UserID],
			TokenID:       row.TokenID,
			TokenName:     row.TokenName,
			ServiceID:     row.ServiceID,
			ServiceName:   row.ServiceName,
			ServerName:    row.ServerName,
			Model:         row.Model,
			APIFlavor:     row.APIFlavor,
			ReqPath:       row.ReqPath,
			ProviderPath:  row.ProviderPath,
			ProviderModel: row.ProviderModel,
			SessionID:     row.SessionID,
			SessionSource: row.SessionSource,
			AgentID:       row.AgentID,
			Stream:        row.Stream,
			StartedAt:     row.StartedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": dtos})
}
