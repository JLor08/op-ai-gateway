// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"slices"
)

// portalRPCMemberErrRows are the manual RPC member spec rows. The runtime
// spec and server rows follow them in portalRPCMemberErrors, so a member
// write reports the same codes as the equivalent runtime spec write.
var portalRPCMemberErrRows = []errRow{
	{err: portal.ErrRPCMemberSpecNotFound, status: http.StatusNotFound, code: "rpc_member_spec.not_found", msg: "rpc member spec not found"},
	{err: portal.ErrRPCMemberFlagReserved, status: http.StatusBadRequest, code: "runtime_spec.member_flag_reserved", msg: "-H/--host, -p/--port, -d/--device and -c/--cache are set by the agent and cannot be used in member args"},
	{err: portal.ErrRPCMemberDeviceBackendInvalid, status: http.StatusBadRequest, code: "runtime_spec.device_backend_invalid", msg: "device_backend must be one of auto, cuda, rocm, vulkan, metal, sycl"},
	{err: portal.ErrRPCMemberVRAMEstimateRequired, status: http.StatusBadRequest, code: "runtime_spec.member_vram_estimate_required", msg: "a vram estimate is required for every gpu on a server without NVIDIA GPUs"},
	{err: portal.ErrRPCMemberInUse, status: http.StatusConflict, code: "runtime.cluster_member_in_use", msg: "member is used by a cluster head", msgFn: rpcMemberInUseMessage},
}

// portalRPCMemberErrors is the lookup table writePortalRPCMemberError uses.
var portalRPCMemberErrors = slices.Concat(portalRPCMemberErrRows, portalRuntimeSpecErrRows, portalServerErrRows)

func writePortalRPCMemberError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalRPCMemberErrors, http.StatusInternalServerError, "rpc_member_spec.request_failed", "rpc member spec request failed")
}

// handlePortalServerRPCMembers serves /api/portal/servers/{id}/rpc-members
// (GET list, POST create) and /api/portal/servers/{id}/rpc-members/{specID}
// (PUT replace, DELETE). specID is "" for the collection route. Authorization
// happens inside portal.Service via authorizeServer.
func (s *Server) handlePortalServerRPCMembers(w http.ResponseWriter, r *http.Request, token auth.Token, serverID, specID string) {
	if specID == "" {
		switch r.Method {
		case http.MethodGet:
			specs, err := s.Portal.RPCMemberSpecs(r.Context(), token, serverID)
			if err != nil {
				writePortalRPCMemberError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": specs})
		case http.MethodPost:
			s.putRPCMemberSpec(w, r, token, serverID, "", http.StatusCreated)
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.putRPCMemberSpec(w, r, token, serverID, specID, http.StatusOK)
	case http.MethodDelete:
		if err := s.Portal.DeleteRPCMemberSpec(r.Context(), token, serverID, specID); err != nil {
			writePortalRPCMemberError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", http.MethodPut+", "+http.MethodDelete)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

func (s *Server) putRPCMemberSpec(w http.ResponseWriter, r *http.Request, token auth.Token, serverID, specID string, okStatus int) {
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req portal.PutRPCMemberSpecRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.PutRPCMemberSpec(r.Context(), token, serverID, specID, req)
	if err != nil {
		writePortalRPCMemberError(w, err)
		return
	}
	writeJSON(w, okStatus, dto)
}

// rpcMemberInUseMessage appends the head's server name (present only when the
// caller may see it) to the generic message.
func rpcMemberInUseMessage(err error) string {
	const generic = "member is used by a cluster head"
	var inUse *portal.RPCMemberInUseError
	if errors.As(err, &inUse) {
		return generic + " (server " + inUse.HeadServerName + ")"
	}
	return generic
}
