// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"time"
)

const msgRuntimeSpecNotFound = "runtime spec not found"

// handlePortalMappingRuntimeSpec serves GET/PUT/DELETE
// /api/portal/mappings/{mappingID}/runtime-spec, dispatched from
// handlePortalMappingItem's sub-path guard (portal_mapping_endpoints.go)
// BEFORE its own pathID fallthrough. Auth was already checked once by the
// caller (requireWebScope); every further authorization decision (mapping ->
// application -> server ownership/admin-group chain) happens inside
// portal.Service via authorizeMapping.
func (s *Server) handlePortalMappingRuntimeSpec(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.GetRuntimeSpec(r.Context(), token, mappingID)
		if err != nil {
			writePortalRuntimeSpecError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.PutRuntimeSpecRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.PutRuntimeSpec(r.Context(), token, mappingID, req)
		if err != nil {
			writePortalRuntimeSpecError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteRuntimeSpec(r.Context(), token, mappingID); err != nil {
			writePortalRuntimeSpecError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// portalRuntimeSpecErrRows are writePortalRuntimeSpecError's mapper-specific
// rows (checked before sharedErrorMap); portal.ErrMappingNotFound (the
// 404-no-leak collapse from authorizeMapping) maps identically elsewhere and
// lives in sharedErrorMap instead. Every sentinel here is 400 except
// ErrRuntimeSpecNotFound (404 — nothing to delete).
var portalRuntimeSpecErrRows = []errRow{
	{err: portal.ErrRuntimeSpecNotFound, status: http.StatusNotFound, code: portal.CodeRuntimeSpecNotFound, msg: msgRuntimeSpecNotFound},
	{err: portal.ErrRuntimeSpecBinaryRequired, status: http.StatusBadRequest, code: "runtime_spec.binary_required", msg: "runtime spec binary is required and must be an absolute path"},
	{err: portal.ErrRuntimeSpecArgsInvalid, status: http.StatusBadRequest, code: "runtime_spec.args_invalid", msg: "runtime spec args are invalid"},
	{err: portal.ErrRuntimeSpecEnvInvalid, status: http.StatusBadRequest, code: "runtime_spec.env_invalid", msg: "runtime spec env is invalid"},
	{err: portal.ErrRuntimeSpecGPUInvalid, status: http.StatusBadRequest, code: "runtime_spec.gpu_invalid", msg: "runtime spec gpu configuration is invalid"},
	{err: portal.ErrRuntimeSpecTuningInvalid, status: http.StatusBadRequest, code: "runtime_spec.tuning_invalid", msg: "runtime spec tuning values must be non-negative"},
	{err: portal.ErrRuntimeSpecAdminStateInvalid, status: http.StatusBadRequest, code: "runtime_spec.admin_state_invalid", msg: "runtime spec admin state is invalid"},
	{err: portal.ErrRuntimeSpecNotServerAgent, status: http.StatusBadRequest, code: "runtime_spec.application_not_server_agent", msg: "runtime spec requires a server_agent application"},
}

func writePortalRuntimeSpecError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalRuntimeSpecErrRows, http.StatusInternalServerError, "runtime_spec.request_failed", "runtime spec request failed")
}

// --- Task 9: runtime status SSE + file-mode report view ---------------------

// handleRuntimeReportView serves GET /api/portal/servers/{id}/runtime/report:
// the latest file-mode runtime report for serverID (or {"available":false}
// when the server has never reported one), plus the agent_version/
// agent_features read from its latest telemetry row so a later portal task
// can render a feature-mismatch banner without a new endpoint. All
// authorization (ownership/admin-group, 404-no-leak) happens inside
// Portal.ServerRuntimeReportView via authorizeServer.
func (s *Server) handleRuntimeReportView(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	dto, err := s.Portal.ServerRuntimeReportView(r.Context(), token, serverID)
	if err != nil {
		writePortalServerError(w, err, "server.runtime_report_failed")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// runtimeStatusEventDTO is the SSE payload for BOTH the initial `snapshot`
// frame and every later `update` frame on the runtime-status stream: unlike
// perf/benchmark, a runtime-status publish is always a FULL replacement of a
// server's whole managed-process list (see runtimeStatusRegistry.publish),
// never an incremental delta, so both event names carry the identical shape.
type runtimeStatusEventDTO struct {
	Runtimes []RuntimeStatusDTO `json:"runtimes"`
}

// nonNilRuntimeStatuses returns statuses, or a non-nil empty slice when it is
// nil, so the wire payload serializes as `[]`, never JSON `null`.
func nonNilRuntimeStatuses(statuses []RuntimeStatusDTO) []RuntimeStatusDTO {
	if statuses == nil {
		return []RuntimeStatusDTO{}
	}
	return statuses
}

// handleRuntimeEvents streams GET /api/portal/servers/{id}/runtime/events
// over SSE: a `snapshot` frame with the server's current runtime-status list,
// then an `update` frame per live RuntimeStatus.publish, with a 25s
// heartbeat. EXACTLY mirrors handleBenchmarkEvents: ownership check via
// s.Portal.GetServer BEFORE any stream byte (404-no-leak), flusher check,
// headers, SetWriteDeadline(time.Time{}) to clear the server WriteTimeout for
// this long-lived response, then an atomic snapshot+subscribe so no update
// between the two is lost.
func (s *Server) handleRuntimeEvents(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, err := s.Portal.GetServer(r.Context(), token, serverID); err != nil {
		writePortalServerError(w, err, "server.not_found")
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

	snapshot, ch, unsub := s.RuntimeStatus.subscribe(serverID)
	defer unsub()
	if !writePerfEvent(w, flusher, "snapshot", runtimeStatusEventDTO{Runtimes: nonNilRuntimeStatuses(snapshot)}) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case statuses, open := <-ch:
			if !open {
				return
			}
			if !writePerfEvent(w, flusher, "update", runtimeStatusEventDTO{Runtimes: nonNilRuntimeStatuses(statuses)}) {
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
