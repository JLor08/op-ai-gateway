// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
)

const (
	msgServerNotFound = "server not found"

	codeServerUpdateFailed = "server.update_failed"
)

func (s *Server) handlePortalServers(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		resp, err := s.Portal.ListServers(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apierror.Response("server.list_failed", "server list failed", ""))
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateServerRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateServer(r.Context(), token, req)
		if err != nil {
			writePortalServerError(w, err, "server.create_failed")
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

func (s *Server) handlePortalServerItem(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/portal/servers/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "applications" && parts[0] != "" {
		s.handlePortalServerApplications(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "agent-token" && parts[0] != "" {
		s.handlePortalServerAgentToken(w, r, token, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "perf" && parts[0] != "" {
		if len(parts) == 3 && parts[2] == "events" {
			s.handleServerPerfEvents(w, r, token, parts[0])
			return
		}
		if len(parts) == 2 {
			s.handleServerPerfHistory(w, r, token, parts[0])
			return
		}
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServerNotFound, msgServerNotFound, ""))
		return
	}
	if len(parts) == 2 && parts[1] == "availability" && parts[0] != "" {
		s.handleServerAvailability(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "hardware" && parts[0] != "" {
		s.handleServerHardware(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "models" && parts[0] != "" {
		s.handlePortalServerModels(w, r, token, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "benchmark" && parts[0] != "" {
		if len(parts) == 2 {
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			mode, ok := parseBenchmarkMode(w, r)
			if !ok {
				return
			}
			s.startBenchmark(w, r, token, "server", parts[0], mode)
			return
		}
		if len(parts) == 3 && parts[2] == "status" {
			if !requireMethod(w, r, http.MethodGet) {
				return
			}
			// Gate the status read on server ownership (no existence leak): a
			// non-owner/non-admin gets the same not-found as an unknown id.
			if _, err := s.Portal.GetServer(r.Context(), token, parts[0]); err != nil {
				writePortalServerError(w, err, portal.CodeServerNotFound)
				return
			}
			writeJSON(w, http.StatusOK, s.Benchmarks.Status(parts[0]))
			return
		}
		if len(parts) == 3 && parts[2] == "events" {
			s.handleBenchmarkEvents(w, r, token, parts[0])
			return
		}
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServerNotFound, msgServerNotFound, ""))
		return
	}
	if len(parts) == 3 && parts[1] == "netbird" && parts[2] == "setup-key" && parts[0] != "" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		key, command, err := s.Portal.RegenerateNetbirdKey(r.Context(), token, parts[0])
		if err != nil {
			writePortalServerError(w, err, codeServerUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			SetupKey     string `json:"setup_key"`
			SetupCommand string `json:"netbird_setup_command,omitempty"`
		}{SetupKey: key, SetupCommand: command})
		return
	}
	if len(parts) == 2 && parts[1] == "ping" && parts[0] != "" {
		s.handlePortalServerPing(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "energy" && parts[0] != "" {
		s.handlePortalServerEnergy(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "gpu-budgets" && parts[0] != "" {
		s.handlePortalServerGPUBudgets(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "certificate" && parts[0] != "" {
		s.handlePortalServerCertificate(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "https-switch" && parts[0] != "" {
		s.handlePortalServerHTTPSSwitchOverride(w, r, token, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "admin-groups" && parts[0] != "" {
		s.handlePortalServerAdminGroups(w, r, token, parts[0])
		return
	}
	if len(parts) >= 2 && parts[1] == "resource-groups" && parts[0] != "" {
		s.handlePortalServerResourceGroups(w, r, token, parts)
		return
	}
	id := pathID(r.URL.Path, "/api/portal/servers/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServerNotFound, msgServerNotFound, ""))
		return
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetServer(r.Context(), token, id)
		if err != nil {
			writePortalServerError(w, err, codeServerUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPatch:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateServerRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.UpdateServer(r.Context(), token, id, req)
		if err != nil {
			writePortalServerError(w, err, codeServerUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		deletePeer := r.URL.Query().Get("delete_peer") == "true"
		failed, err := s.Portal.DeleteServer(r.Context(), token, id, deletePeer)
		if err != nil {
			writePortalServerError(w, err, codeServerUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Ok                      bool `json:"ok"`
			NetbirdPeerDeleteFailed bool `json:"netbird_peer_delete_failed,omitempty"`
		}{Ok: true, NetbirdPeerDeleteFailed: failed})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPatch+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handlePortalServerResourceGroups backs the server-owner self-service path
// (spec 2026-08-11-resource-groups-server-owner-self-service):
//
//	GET    /api/portal/servers/{id}/resource-groups        -> eligible groups + member flag
//	PUT    /api/portal/servers/{id}/resource-groups/{rgId} -> join (idempotent)
//	DELETE /api/portal/servers/{id}/resource-groups/{rgId} -> leave (idempotent)
//
// parts[0] = server id, parts[1] = "resource-groups", parts[2] = rgId (item paths).
// Grant + 404-no-leak / 400 mapping live in the portal service + writePortalResourceGroupError.
func (s *Server) handlePortalServerResourceGroups(w http.ResponseWriter, r *http.Request, token auth.Token, parts []string) {
	serverID := parts[0]
	switch {
	case len(parts) == 2:
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		data, err := s.Portal.ServerOwnerResourceGroups(r.Context(), token, serverID)
		if err != nil {
			writePortalResourceGroupError(w, err, portal.CodeServerNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	case len(parts) == 3:
		rgID := parts[2]
		switch r.Method {
		case http.MethodPut:
			if err := s.Portal.AddServerToResourceGroup(r.Context(), token, serverID, rgID); err != nil {
				writePortalResourceGroupError(w, err, portal.CodeServerNotFound)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		case http.MethodDelete:
			if err := s.Portal.RemoveServerFromResourceGroup(r.Context(), token, serverID, rgID); err != nil {
				writePortalResourceGroupError(w, err, portal.CodeServerNotFound)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		default:
			w.Header().Set("Allow", http.MethodPut+", "+http.MethodDelete)
			writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		}
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServerNotFound, msgServerNotFound, ""))
	}
}

// handlePortalServerModels backs GET /api/portal/servers/{id}/models — the
// distinct gateway models one server offers (portal.Service.ServerModels),
// gated on the caller MANAGING that server (AuthorizeServerManage: 404-no-leak,
// same response for a non-manager and a stranger). Used by the frontend to
// narrow a server-override's model dropdown to what the target actually
// serves once one is selected on a token or a chat's run settings.
func (s *Server) handlePortalServerModels(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.ServerModels(r.Context(), token, id)
	if err != nil {
		writePortalServerError(w, err, portal.CodeServerNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handlePortalServerEnergy backs the dedicated "Save" button on the server edit
// form's "Energy & cost" section: PUT /api/portal/servers/{id}/energy writes ONLY
// the five per-server energy-config columns (estimated_watts, idle_watts,
// price_per_kwh, pue, price_unit). Owner/admin-scoped (SetServerEnergyConfig gates
// via authorizeServer → 404 no-leak); a negative numeric value → 400; price_unit
// is normalized (unknown/empty → the default). It is a full-replace of the five
// columns (the section edits them all together), independent of the main
// server PATCH (which leaves energy untouched when omitted).
func (s *Server) handlePortalServerEnergy(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var body struct {
		EstimatedWatts float64 `json:"estimated_watts"`
		IdleWatts      float64 `json:"idle_watts"`
		PricePerKwh    float64 `json:"price_per_kwh"`
		Pue            float64 `json:"pue"`
		PriceUnit      string  `json:"price_unit"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.SetServerEnergyConfig(r.Context(), token, id, body.EstimatedWatts, body.IdleWatts, body.PricePerKwh, body.Pue, body.PriceUnit)
	if err != nil {
		writePortalServerError(w, err, codeServerUpdateFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalServerGPUBudgets backs GET/PUT /api/portal/servers/{id}/gpu-budgets
// (Task 6): the per-GPU VRAM budget rows used by the co-residency admission
// math. Owner/admin-scoped (SetServerGPUBudgets/GetServerGPUBudgets gate via
// authorizeServer -> 404 no-leak). PUT is a full-document replace, mirroring
// handlePortalMappingRuntimeSpec's PUT semantics; both responses wrap the
// slice under "budgets" (matching the request field name and the
// warnings-endpoint envelope convention below).
func (s *Server) handlePortalServerGPUBudgets(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	switch r.Method {
	case http.MethodGet:
		budgets, err := s.Portal.GetServerGPUBudgets(r.Context(), token, id)
		if err != nil {
			writePortalServerError(w, err, codeServerUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"budgets": budgets})
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.SetGPUBudgetsRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		budgets, err := s.Portal.SetServerGPUBudgets(r.Context(), token, id, req)
		if err != nil {
			writePortalServerError(w, err, codeServerUpdateFailed)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"budgets": budgets})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handleServerAdminGroupCandidates backs GET /api/portal/server-admin-group-candidates:
// the admin-tier groups the caller may create/link a server into (system
// scope -> every admin-tier group; anyone else -> the groups they may manage
// servers through). Drives the create-server / linkage-editor picker.
func (s *Server) handleServerAdminGroupCandidates(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	data, err := s.Portal.ServerAdminGroupCandidates(r.Context(), token)
	if err != nil {
		writePortalServerError(w, err, "server.admin_group_candidates_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handlePortalServerAdminGroups backs PUT /api/portal/servers/{id}/admin-groups:
// replaces the server's linked admin-group set (Phase B, spec 2026-08-10).
func (s *Server) handlePortalServerAdminGroups(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var body struct {
		AdminGroupIDs []string `json:"admin_group_ids"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.SetServerAdminGroups(r.Context(), token, id, body.AdminGroupIDs)
	if err != nil {
		writePortalServerError(w, err, codeServerUpdateFailed)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// portalServerErrRows are writePortalServerError's mapper-specific rows
// (checked before sharedErrorMap); portal.ErrPathSuffixInvalid and
// portal.ErrServerNotFound map identically elsewhere and live in
// sharedErrorMap instead. store.ErrNotFound maps to a different code in
// other mappers, so it must stay here.
var portalServerErrRows = []errRow{
	{err: portal.ErrServerNameRequired, status: http.StatusBadRequest, code: "server.name_required", msg: "server name is required"},
	{err: portal.ErrServerDomainRequired, status: http.StatusBadRequest, code: "server.domain_required", msg: "server domain is required"},
	{err: portal.ErrServerStatusInvalid, status: http.StatusBadRequest, code: "server.status_invalid", msg: "server status is invalid"},
	{err: portal.ErrServerOwnerInvalid, status: http.StatusBadRequest, code: "server.owner_invalid", msg: "owner is invalid"},
	{err: portal.ErrServerAgentPresenceTimeoutInvalid, status: http.StatusBadRequest, code: "server.agent_presence_timeout_invalid", msg: "agent presence timeout must be >= 0"},
	{err: portal.ErrServerRuntimeLimitInvalid, status: http.StatusBadRequest, code: "server.runtime_limit_invalid", msg: "runtime_max_processes must be >= 0"},
	{err: portal.ErrGPUBudgetInvalid, status: http.StatusBadRequest, code: "server.gpu_budget_invalid", msg: "gpu budget index/budget_mb must be >= 0 and index must be unique"},
	{err: portal.ErrServerEnergyConfigInvalid, status: http.StatusBadRequest, code: "server.energy_config_invalid", msg: "estimated_watts, idle_watts, price_per_kwh and pue must be >= 0"},
	{err: portal.ErrServerAdminGroupRequired, status: http.StatusBadRequest, code: "server.admin_group_required", msg: "at least one admin group is required"},
	{err: portal.ErrServerAdminGroupInvalid, status: http.StatusBadRequest, code: "server.admin_group_invalid", msg: "admin group is invalid"},
	{err: portal.ErrServerAdminGroupParentMismatch, status: http.StatusBadRequest, code: "server.admin_group_parent_mismatch", msg: "admin groups must share one parent system group"},
	{err: portal.ErrServerForbidden, status: http.StatusForbidden, code: "server.forbidden", msg: "not allowed"},
	{err: portal.ErrNetbirdModuleDisabled, status: http.StatusConflict, code: "netbird.module_disabled", msg: "netbird module is not enabled"},
	{err: portal.ErrNetbirdPeerInUse, status: http.StatusConflict, code: "netbird.peer_in_use", msg: "netbird peer is already linked to another server"},
	{err: portal.ErrNetbirdPeerNotManaged, status: http.StatusConflict, code: "netbird.peer_not_managed", msg: "netbird peer is not managed by the gateway"},
	{err: portal.ErrNetbirdKeyFileNotConfigured, status: http.StatusConflict, code: "netbird.key_file_not_configured", msg: "netbird key file is not configured"},
	{err: portal.ErrNetbirdNetworkRangeInvalid, status: http.StatusBadRequest, code: "system.netbird_network_range_invalid", msg: "network range must be a valid CIDR"},
	{err: netbird.ErrAuth, status: http.StatusBadGateway, code: "netbird.auth_failed", msg: "netbird authentication failed"},
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeServerNotFound, msg: msgServerNotFound},
}

func writePortalServerError(w http.ResponseWriter, err error, defaultCode string) {
	writeMappedError(w, err, portalServerErrRows, http.StatusInternalServerError, defaultCode, "server request failed")
}

func (s *Server) handlePortalServerApplications(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	switch r.Method {
	case http.MethodGet:
		resp, err := s.Portal.ListApplications(r.Context(), token, serverID)
		if err != nil {
			writePortalApplicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.CreateApplicationRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.CreateApplication(r.Context(), token, serverID, req)
		if err != nil {
			writePortalApplicationError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

func (s *Server) handlePortalServerAgentToken(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.AgentTokenStatus(r.Context(), token, serverID)
		if err != nil {
			writePortalAgentTokenError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.decorateAgentTokenStatus(r.Context(), requestOrigin(r), dto))
	case http.MethodPost:
		resp, err := s.Portal.GenerateAgentToken(r.Context(), token, serverID)
		if err != nil {
			writePortalAgentTokenError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, generateAgentTokenResponse{
			Secret: resp.Secret,
			Token:  s.decorateAgentTokenStatus(r.Context(), requestOrigin(r), resp.Token),
		})
	case http.MethodDelete:
		if err := s.Portal.RevokeAgentToken(r.Context(), token, serverID); err != nil {
			writePortalAgentTokenError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

type agentTokenStatusResponse struct {
	portal.AgentTokenDTO
	Config            agentConfigMaterial `json:"config"`
	AgentDownloadBase string              `json:"agent_download_base"`
}

type generateAgentTokenResponse struct {
	Secret string                   `json:"secret"`
	Token  agentTokenStatusResponse `json:"token"`
}

// decorateAgentTokenStatus keeps generated config content and its authenticated
// download route on the same runtime transport decision. The concrete base is
// present even when no binary manifest exists: public origin while unrestricted
// (or while the listener is fail-safe inactive), otherwise the mesh base.
func (s *Server) decorateAgentTokenStatus(ctx context.Context, fallbackOrigin string, dto portal.AgentTokenDTO) agentTokenStatusResponse {
	config, meshTLS := s.resolveAgentConfigMaterial(ctx, fallbackOrigin)
	downloadBase := fallbackOrigin
	downloadOnly := s.Portal != nil && s.Portal.NetbirdAgentDownloadOnly(ctx)
	if meshBase := s.agentDownloadBaseFromMaterial(ctx, downloadOnly, config, meshTLS); meshBase != "" {
		downloadBase = meshBase
	}
	return agentTokenStatusResponse{
		AgentTokenDTO:     dto,
		Config:            config,
		AgentDownloadBase: downloadBase,
	}
}

// portalAgentTokenErrRows are writePortalAgentTokenError's mapper-specific
// rows; portal.ErrServerNotFound maps identically elsewhere and lives in
// sharedErrorMap instead. store.ErrNotFound maps to a different code in
// other mappers, so it must stay here (both it and the shared
// portal.ErrServerNotFound row happen to resolve to the same
// server.not_found response, exactly as the original combined case did).
var portalAgentTokenErrRows = []errRow{
	{err: store.ErrNotFound, status: http.StatusNotFound, code: portal.CodeServerNotFound, msg: msgServerNotFound},
	{err: store.ErrConflict, status: http.StatusConflict, code: "agent_token.conflict", msg: "agent token conflict"},
}

func writePortalAgentTokenError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalAgentTokenErrRows, http.StatusInternalServerError, "agent_token.request_failed", "agent token request failed")
}
