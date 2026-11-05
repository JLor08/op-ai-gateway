// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"slices"
)

// portalBinaryErrRows are the binary inventory rows; the server rows follow
// them in portalBinaryErrors so an unknown or foreign server reports the same
// 404 as everywhere else.
var portalBinaryErrRows = []errRow{
	{err: portal.ErrBinaryPathInvalid, status: http.StatusBadRequest, code: "binary.path_invalid", msg: "path must be an absolute path of at most 4096 characters"},
	{err: portal.ErrBinaryKindInvalid, status: http.StatusBadRequest, code: "binary.kind_invalid", msg: "kind must be one of llama_server, rpc_server, other"},
	{err: portal.ErrBinaryVersionInvalid, status: http.StatusBadRequest, code: "binary.version_invalid", msg: "version, build and commit must be at most 256 characters"},
	{err: portal.ErrBinaryNotFound, status: http.StatusNotFound, code: "binary.not_found", msg: "binary not found on this server"},
	{err: portal.ErrBinaryAgentOwned, status: http.StatusConflict, code: "binary.agent_owned", msg: "the agent reports this binary; only its version override can be changed"},
}

var portalBinaryErrors = slices.Concat(portalBinaryErrRows, portalServerErrRows)

func writePortalBinaryError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalBinaryErrors, http.StatusInternalServerError, "binary.request_failed", "binary request failed")
}

// handlePortalServerBinaries serves /api/portal/servers/{id}/binaries: GET
// lists, POST adds a manual row, PUT edits a manual row or sets a version
// override, DELETE removes a manual row. The binary path is never part of the
// URL path (it would need encoding): PUT carries it in the JSON body and
// DELETE in the "path" query parameter. Authorization happens inside
// portal.Service via authorizeServer.
func (s *Server) handlePortalServerBinaries(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.Portal.ServerBinaries(r.Context(), token, serverID)
		if err != nil {
			writePortalBinaryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": rows})
	case http.MethodPost, http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.PutServerBinaryRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		var (
			dto    portal.ServerBinaryDTO
			err    error
			status = http.StatusOK
		)
		if r.Method == http.MethodPost {
			dto, err = s.Portal.CreateManualServerBinary(r.Context(), token, serverID, req)
			status = http.StatusCreated
		} else {
			dto, err = s.Portal.UpdateServerBinary(r.Context(), token, serverID, req)
		}
		if err != nil {
			writePortalBinaryError(w, err)
			return
		}
		writeJSON(w, status, dto)
	case http.MethodDelete:
		if err := s.Portal.DeleteManualServerBinary(r.Context(), token, serverID, r.URL.Query().Get("path")); err != nil {
			writePortalBinaryError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost+", "+http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}
