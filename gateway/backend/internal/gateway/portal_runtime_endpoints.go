// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
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

// handlePortalClusterCandidates serves GET
// /api/portal/mappings/{mappingID}/runtime/cluster-candidates?mode=automatic|manual&binary=<path>:
// the servers the cluster head on that mapping may add as members, as
// {"data": [...], "head_binary_known": bool}. binary is the head binary the
// operator's form has (empty: the stored spec's). Authorization (the head's
// mapping, each candidate server, member consent) happens inside
// portal.Service.ClusterCandidates.
func (s *Server) handlePortalClusterCandidates(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	list, err := s.Portal.ClusterCandidates(r.Context(), token, mappingID, q.Get("mode"), q.Get("binary"))
	if err != nil {
		writePortalRuntimeSpecError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list.Candidates, "head_binary_known": list.HeadBinaryKnown})
}

// handlePortalClusterMemberResetUsage serves POST
// /api/portal/mappings/{mappingID}/runtime/cluster-members/{serverID}/reset-usage:
// it clears the stored usage of the member on serverID of the cluster head on
// that mapping (the way out of a stored peak above the member's budget, which
// its next start would be refused for) and answers the head's runtime spec.
// Authorization -- the head's mapping and the member's server -- happens
// inside portal.Service.AuthorizeClusterMemberUsageReset; the reset itself
// runs beside the telemetry ingest that writes the same row (clusterUsage),
// which also pushes the member server's document.
func (s *Server) handlePortalClusterMemberResetUsage(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID, serverID string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	m, err := s.Portal.AuthorizeClusterMemberUsageReset(r.Context(), token, mappingID, serverID)
	if err != nil {
		writePortalRuntimeSpecError(w, err)
		return
	}
	if err := s.clusterUsage.reset(r.Context(), m.HeadSpecID, m.ServerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The member left between the authorization and the reset.
			err = portal.ErrClusterMemberNotFound
		}
		writePortalRuntimeSpecError(w, err)
		return
	}
	dto, err := s.Portal.GetRuntimeSpec(r.Context(), token, mappingID)
	if err != nil {
		writePortalRuntimeSpecError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// portalRuntimeSpecErrRows are writePortalRuntimeSpecError's mapper-specific
// rows (checked before sharedErrorMap); portal.ErrMappingNotFound (the
// 404-no-leak collapse from authorizeMapping) maps identically elsewhere and
// lives in sharedErrorMap instead. Every sentinel here is 400 except two:
// ErrRuntimeSpecNotFound is 404 (nothing to delete) and
// ErrRuntimeSpecServerBenchmarking is 409 (well-formed, but a benchmark run
// holds the server — see that row's own note). Deliberately no count: a
// number here goes stale the next time a row is added.
var portalRuntimeSpecErrRows = []errRow{
	{err: portal.ErrRuntimeSpecNotFound, status: http.StatusNotFound, code: portal.CodeRuntimeSpecNotFound, msg: msgRuntimeSpecNotFound},
	{err: portal.ErrRuntimeSpecBinaryRequired, status: http.StatusBadRequest, code: "runtime_spec.binary_required", msg: "runtime spec binary is required and must be an absolute path"},
	{err: portal.ErrRuntimeSpecArgsInvalid, status: http.StatusBadRequest, code: "runtime_spec.args_invalid", msg: "runtime spec args are invalid"},
	{err: portal.ErrRuntimeSpecEnvInvalid, status: http.StatusBadRequest, code: "runtime_spec.env_invalid", msg: "runtime spec env is invalid"},
	{err: portal.ErrRuntimeSpecGPUInvalid, status: http.StatusBadRequest, code: "runtime_spec.gpu_invalid", msg: "runtime spec gpu configuration is invalid"},
	{err: portal.ErrRuntimeSpecTuningInvalid, status: http.StatusBadRequest, code: "runtime_spec.tuning_invalid", msg: "runtime spec tuning values must be non-negative"},
	{err: portal.ErrRuntimeSpecAdminStateInvalid, status: http.StatusBadRequest, code: "runtime_spec.admin_state_invalid", msg: "runtime spec admin state is invalid"},
	{err: portal.ErrRuntimeSpecVisibleDevicesNoGPUs, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_no_gpus", msg: "set_visible_devices requires at least one gpu row: an empty visible-devices value hides every gpu from the model"},
	{err: portal.ErrRuntimeSpecVisibleDevicesConflict, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_conflict", msg: "set_visible_devices conflicts with a gpu visibility variable set by hand in env"},
	{err: portal.ErrRuntimeSpecVisibleDevicesModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_mode_invalid", msg: "visible_devices_mode must be \"env\" or \"args\""},
	{err: portal.ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_args_no_placeholder", msg: "args mode requires one of ${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES}/${DEVICES} in args"},
	{err: portal.ErrRuntimeSpecNotServerAgent, status: http.StatusBadRequest, code: "runtime_spec.application_not_server_agent", msg: "runtime spec requires a server_agent application"},
	{err: portal.ErrRuntimeSpecEndpointModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.endpoint_mode_invalid", msg: "runtime spec endpoint mode is invalid"},
	{err: portal.ErrRuntimeSpecFlavorInvalid, status: http.StatusBadRequest, code: "runtime_spec.flavor_invalid", msg: "runtime spec api flavor is invalid"},
	{err: portal.ErrRuntimeSpecTypeInvalid, status: http.StatusBadRequest, code: "runtime_spec.type_invalid", msg: "runtime spec type must be one of vllm, llama_cpp, tgi, ollama, stable_diffusion_cpp, audio_cpp, vllm_sr, custom (or empty for auto)"},
	// The one row in this table with a dynamic message. The service wraps the
	// sentinel with the offending EFFECTIVE kind (fmt.Errorf("%w: ...")), and
	// naming it is the entire point of refusing the write instead of storing
	// something else -- doubly so here, where an empty "type" means the kind
	// was detected from the binary and the caller never typed it. msgFn exists
	// for exactly this ("a row that must surface the underlying error's own
	// text", see errRow in error_map.go); the sentinel's own text IS the API
	// code, per the convention above, so it is trimmed rather than repeated.
	//
	// 400 and never 409: the application table splits this refusal in two
	// (portal_application_endpoints.go) because a PATCH can assert the flag
	// against a type it never sent, and the request is then well-formed. A
	// spec PUT always states its own Type and Binary, so the kind named here
	// always came from this body.
	{
		err:    portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported,
		status: http.StatusBadRequest,
		code:   "runtime_spec.responses_live_timings_unsupported",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported.Error()+": ")
		},
	},
	{
		err:    portal.ErrRuntimeSpecDecisionsPassthroughUnsupported,
		status: http.StatusBadRequest,
		code:   "runtime_spec.decisions_passthrough_unsupported",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrRuntimeSpecDecisionsPassthroughUnsupported.Error()+": ")
		},
	},
	{
		err:    portal.ErrRuntimeSpecSystemOneTranslateUnsupported,
		status: http.StatusBadRequest,
		code:   "runtime_spec.systemone_translate_unsupported",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrRuntimeSpecSystemOneTranslateUnsupported.Error()+": ")
		},
	},
	// Dynamic message, same shape as the row above: the service wraps the
	// sentinel with the offending file's name and the rule it broke (never its
	// content), and the operator needs that to fix the entry.
	{
		err:    portal.ErrRuntimeSpecConfigFilesInvalid,
		status: http.StatusBadRequest,
		code:   "runtime_spec.config_files_invalid",
		msgFn: func(err error) string {
			return strings.TrimPrefix(err.Error(), portal.ErrRuntimeSpecConfigFilesInvalid.Error()+": ")
		},
	},
	{err: portal.ErrRuntimeSpecMetricsPathInvalid, status: http.StatusBadRequest, code: "runtime_spec.metrics_path_invalid", msg: "metrics_path must be a relative path beginning with a single \"/\" (no scheme, host, or whitespace)"},
	{err: portal.ErrRuntimeSpecContextProbePathInvalid, status: http.StatusBadRequest, code: "runtime_spec.context_probe_path_invalid", msg: "context_probe_path must be a relative path beginning with a single \"/\" (no scheme, host, or whitespace)"},
	// The two instance-settings refusals (#340); a negative count answers with
	// the tuning row above.
	{err: portal.ErrRuntimeSpecInstancesInvalid, status: http.StatusBadRequest, code: "runtime_spec.instances_invalid", msg: "min_instances must not exceed max_instances when both are set"},
	{err: portal.ErrRuntimeSpecInstancesNeedEphemeralPort, status: http.StatusBadRequest, code: "runtime_spec.instances_need_ephemeral_port", msg: "a replicable runtime spec needs listen_port 0: every instance listens on its own port, picked by the agent"},
	// The four per-spec API-token sentinels from validateRuntimeSpecAPIToken
	// -- code is err.Error() verbatim, per the convention every row above
	// already follows.
	{err: portal.ErrRuntimeSpecAPITokenModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.api_token_mode_invalid", msg: "api_token_mode must be \"app\", \"off\", \"set\" or \"random\""},
	{err: portal.ErrRuntimeSpecAPITokenNoPlaceholder, status: http.StatusBadRequest, code: "runtime_spec.api_token_no_placeholder", msg: "api_token_mode \"set\"/\"random\" requires a \"${API_TOKEN}\" placeholder in env or args"},
	{err: portal.ErrRuntimeSpecAPITokenPlaceholderWithoutMode, status: http.StatusBadRequest, code: "runtime_spec.api_token_placeholder_without_mode", msg: "a \"${API_TOKEN}\" placeholder in env or args requires api_token_mode \"set\" or \"random\""},
	{err: portal.ErrRuntimeSpecAPITokenHeaderInvalid, status: http.StatusBadRequest, code: "runtime_spec.api_token_header_invalid", msg: "api_token_header_source must be \"app\" or \"custom\", with a valid api_token_header when custom"},
	// The seal-or-400 path: PutRuntimeSpec computes the sealed api_token
	// BEFORE any store write, so a keyless disk store (no cipher, not the
	// volatile settings store) fails this closed -- capture.SealSecret
	// returns capture.ErrKeyRequired instead of ever persisting a plaintext
	// token. Mirrors system.smtp_key_required/system.netbird_key_required's
	// code+msg convention (system_settings_endpoints.go).
	{err: capture.ErrKeyRequired, status: http.StatusBadRequest, code: "runtime_spec.api_token_key_required", msg: "an encryption key is required to store a runtime spec api token on a disk-backed store"},
	// 409, not 400: the request is well-formed and would be accepted a moment
	// later. It conflicts with the server's current state -- a benchmark run
	// holds it -- which is the same reasoning the VRAM run's own precondition
	// refusals answer 409 on.
	{err: portal.ErrRuntimeSpecServerBenchmarking, status: http.StatusConflict, code: "runtime_spec.server_benchmarking", msg: "a benchmark run is holding this server; a launch-spec change now would contaminate its measurement"},
}

// portalClusterErrRows are the cluster head refusals of a runtime-spec write
// and of the cluster-candidates listing. Every row is 400 except the three 409s
// (a member spec another head uses, a member server without a free RPC
// port, a listen_port a member holds: well-formed, but in conflict with
// other specs). Rows whose
// refusal names the server or binary at fault surface the service's detail
// through clusterErrDetail; the server-not-found answer for a member server
// the caller may not use is the shared server row.
var portalClusterErrRows = []errRow{
	{err: portal.ErrClusterMemberNotFound, status: http.StatusNotFound, code: "runtime_spec.cluster_member_not_found", msg: "the server is no member of this cluster head"},
	{err: portal.ErrClusterMemberModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_mode_invalid", msg: "member_mode must be \"automatic\" or \"manual\""},
	{err: portal.ErrClusterNetworkInvalid, status: http.StatusBadRequest, code: "runtime_spec.cluster_network_invalid", msg: "cluster_network must be \"mesh\" or \"lan\" (or empty for the default)"},
	{err: portal.ErrClusterTransportInvalid, status: http.StatusBadRequest, code: "runtime_spec.cluster_transport_invalid", msg: "rpc_transport must be \"direct\" or \"relay\" (or empty for the default)"},
	{err: portal.ErrClusterTypeInvalid, status: http.StatusBadRequest, code: "runtime_spec.cluster_type_invalid", msg: "a cluster head must be a llama_cpp runtime spec"},
	{err: portal.ErrClusterFlagEqualsForm, status: http.StatusBadRequest, code: "runtime_spec.cluster_flag_equals_form", msg: "llama-server refuses --flag=value: write --rpc, --device/-dev, --tensor-split/-ts and --mmproj-device/-mmdev with their value as the next argument"},
	{err: portal.ErrClusterRPCPlaceholderMissing, status: http.StatusBadRequest, code: "runtime_spec.cluster_rpc_placeholder_missing", msg: "a cluster head needs ${RPC_SERVERS} as the value of --rpc in args or of LLAMA_ARG_RPC in env"},
	{err: portal.ErrClusterRPCTwice, status: http.StatusBadRequest, code: "runtime_spec.cluster_rpc_twice", msg: "set the RPC servers exactly once: ${RPC_SERVERS} once, through either --rpc or LLAMA_ARG_RPC, never both"},
	{err: portal.ErrClusterRPCPlaceholderWithoutCluster, status: http.StatusBadRequest, code: "runtime_spec.cluster_rpc_placeholder_without_cluster", msg: "${RPC_SERVERS} and ${RPC_DEVICES} need cluster mode"},
	{err: portal.ErrClusterDeviceListMissingRPC, status: http.StatusBadRequest, code: "runtime_spec.cluster_device_list_missing_rpc", msg: "a cluster head's device list must name the RPC devices with ${DEVICES} or ${RPC_DEVICES}, else the members are never used"},
	{err: portal.ErrClusterDeviceOrder, status: http.StatusBadRequest, code: "runtime_spec.cluster_device_order", msg: "the device list is parsed before the RPC servers exist: put --device after --rpc in args, or set the RPC servers through LLAMA_ARG_RPC"},
	{err: portal.ErrClusterMembersRequired, status: http.StatusBadRequest, code: "runtime_spec.cluster_members_required", msg: "a cluster head needs at least one member"},
	{err: portal.ErrClusterHeadReplicable, status: http.StatusBadRequest, code: "runtime_spec.cluster_head_replicable", msg: "a cluster head cannot be replicable: each member serves one head"},
	{err: portal.ErrClusterStartupTimeoutRequired, status: http.StatusBadRequest, code: "runtime_spec.cluster_startup_timeout_required", msg: "a cluster head needs an explicit startup_timeout_seconds"},
	{err: portal.ErrClusterMemberServerTwice, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_server_twice", msg: "a server can be a member of a head only once; combine its GPUs into one member"},
	{err: portal.ErrClusterMemberOnHeadServer, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_on_head_server", msg: "a member must run on another server than its head"},
	{err: portal.ErrClusterMemberSpecTaken, status: http.StatusConflict, code: "runtime_spec.cluster_member_spec_taken", msgFn: clusterErrDetail(portal.ErrClusterMemberSpecTaken, "the member spec serves another cluster head")},
	{err: portal.ErrClusterMemberSpecDisabled, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_spec_disabled", msgFn: clusterErrDetail(portal.ErrClusterMemberSpecDisabled, "the member spec is disabled")},
	{err: portal.ErrClusterMemberRPCDisabled, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_rpc_disabled", msgFn: clusterErrDetail(portal.ErrClusterMemberRPCDisabled, "the server does not offer RPC membership in its runtime settings")},
	{err: portal.ErrClusterMemberNoMatchingBinary, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_no_matching_binary", msgFn: clusterErrDetail(portal.ErrClusterMemberNoMatchingBinary, "the member server has no ggml-rpc-server of the head binary's version")},
	{err: portal.ErrClusterVersionMismatch, status: http.StatusBadRequest, code: "runtime_spec.cluster_version_mismatch", msgFn: clusterErrDetail(portal.ErrClusterVersionMismatch, "the member binary's version differs from the head binary's")},
	{err: portal.ErrClusterMeshUnavailable, status: http.StatusBadRequest, code: "runtime_spec.cluster_mesh_unavailable", msgFn: clusterErrDetail(portal.ErrClusterMeshUnavailable, "a mesh cluster needs NetBird with a tracking group and a known mesh IP on the head and every member")},
	{err: portal.ErrClusterLANAddressRequired, status: http.StatusBadRequest, code: "runtime_spec.cluster_lan_address_required", msgFn: clusterErrDetail(portal.ErrClusterLANAddressRequired, "a lan cluster needs a LAN address on every server, and lan_address on NetBird-enabled ones")},
	{err: portal.ErrClusterDirectUnavailable, status: http.StatusBadRequest, code: "runtime_spec.cluster_direct_unavailable", msg: "direct transport in a mesh cluster needs NetBird policy management with deny-by-default on and enforced, and the Default policy seen disabled; use relay"},
	{err: portal.ErrClusterLANAckRequired, status: http.StatusBadRequest, code: "runtime.cluster_lan_ack_required", msg: "direct transport in a lan cluster exposes each member's RPC port unauthenticated; acknowledge it with lan_exposure_ack"},
	{err: portal.ErrClusterMemberBudgetRequired, status: http.StatusBadRequest, code: "runtime_spec.cluster_member_budget_required", msgFn: clusterErrDetail(portal.ErrClusterMemberBudgetRequired, "a member on an Apple GPU needs a GPU budget on its server")},
	{err: portal.ErrClusterTensorSplitRequired, status: http.StatusBadRequest, code: "runtime_spec.cluster_tensor_split_required", msgFn: clusterErrDetail(portal.ErrClusterTensorSplitRequired, "a head with a non-NVIDIA member needs --tensor-split")},
	{err: portal.ErrClusterPortExhausted, status: http.StatusConflict, code: "runtime_spec.cluster_port_exhausted", msg: "no free port in the RPC range 26000-26999 could be claimed on the member server"},
	{err: portal.ErrRuntimeSpecPortConflictsRPCMember, status: http.StatusConflict, code: "runtime_spec.port_conflicts_rpc_member", msg: "listen_port is the RPC port of a cluster member on this server"},
	{err: portal.ErrClusterHeadBinaryVersionUnknown, status: http.StatusBadRequest, code: "runtime_spec.cluster_head_binary_version_unknown", msgFn: clusterErrDetail(portal.ErrClusterHeadBinaryVersionUnknown, "the version of the head binary is unknown; add it in the Binaries tab")},
}

// clusterErrDetail surfaces the detail a cluster refusal was wrapped with
// (fmt.Errorf("%w: ...")), else fallback.
func clusterErrDetail(sentinel error, fallback string) func(error) string {
	return func(err error) string {
		if detail, ok := strings.CutPrefix(err.Error(), sentinel.Error()+": "); ok {
			return detail
		}
		return fallback
	}
}

// portalRuntimeSpecErrors is writePortalRuntimeSpecError's table: the
// runtime-spec rows, the cluster rows, and the member-spec rows a cluster
// write shares with the member-spec API (an unknown member spec, an invalid
// device backend, a missing VRAM estimate).
var portalRuntimeSpecErrors = slices.Concat(portalRuntimeSpecErrRows, portalClusterErrRows, portalRPCMemberErrRows)

func writePortalRuntimeSpecError(w http.ResponseWriter, err error) {
	writeMappedError(w, err, portalRuntimeSpecErrors, http.StatusInternalServerError, "runtime_spec.request_failed", "runtime spec request failed")
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
	// RPC members are named for this reader: what the name may say depends
	// on the heads the reader may see.
	namer := &rpcMemberNamer{
		resolve: func(ctx context.Context, ids []string) (map[string]string, error) {
			return s.Portal.RPCMemberDisplayNames(ctx, token, serverID, ids)
		},
		now: time.Now,
	}
	ctx := r.Context()
	if !writePerfEvent(w, flusher, "snapshot", runtimeStatusEventDTO{Runtimes: nonNilRuntimeStatuses(namer.apply(ctx, snapshot))}) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case statuses, open := <-ch:
			if !open {
				return
			}
			if !writePerfEvent(w, flusher, "update", runtimeStatusEventDTO{Runtimes: nonNilRuntimeStatuses(namer.apply(ctx, statuses))}) {
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

// --- T3: live managed-process log stream ------------------------------------

// runtimeLogStatusEventDTO is the SSE `status` payload: whether this server's
// agent can actually deliver a live log stream right now, and if not, WHY --
// see the runtimeLogState* constants (runtime_logs.go). It is sent once before
// any log frame and again whenever the answer changes, because "the window is
// empty" needs a reason attached to it: an empty view with no explanation is
// indistinguishable from a model that prints nothing, which is precisely the
// question the operator opened it to answer.
type runtimeLogStatusEventDTO struct {
	State string `json:"state"`
}

// runtimeLogStatusInterval is how often an open log stream re-evaluates
// whether the agent can serve it. Cheap (two in-memory map reads) and worth
// it: an agent that reconnects, or whose first telemetry sample lands a moment
// after the view opened, must flip the banner off by itself rather than
// leaving the operator looking at a stale "offline" over a stream that is now
// working. A package-level var so tests need not wait it out.
var runtimeLogStatusInterval = 5 * time.Second

// runtimeLogState answers "can this server's agent stream logs right now", and
// distinguishes the two silences that need different action from the operator:
// no live connection at all (a stopped agent, an unreachable one, or one
// configured with the POST transport, which has no gateway->agent direction)
// versus a connection whose agent binary does not declare the runtime_logs
// feature and will therefore ignore the request forever.
//
// The connection check comes FIRST deliberately. The feature registry is fed
// by telemetry, so a server whose agent is long gone can still have a stale
// declared feature set; reporting "streaming" off that would be the exact
// empty-window lie this state exists to prevent.
func (s *Server) runtimeLogState(serverID string) string {
	if !s.AgentStreams.hasConn(serverID) {
		return runtimeLogStateOffline
	}
	if !s.AgentFeatures.Has(serverID, runtimeLogsFeature) {
		return runtimeLogStateUnsupported
	}
	return runtimeLogStateStreaming
}

// handleRuntimeLogEvents streams GET
// /api/portal/servers/{id}/runtime/logs?spec_id=... over SSE: a `status` frame
// stating whether a live stream is possible at all, then a `log` frame per
// agent flush -- the spec's retained scrollback first (one batch with
// scrollback=true, possibly empty), then live output.
//
// AUTHORIZATION is the same boundary as the rest of this feature, not a laxer
// one because it is "just logs": ownership/admin-group via s.Portal.GetServer
// BEFORE any stream byte, with the 404-no-leak collapse, exactly as
// handleRuntimeEvents does. spec_id needs no AUTHORIZATION check of its own:
// fan-out is keyed by (server, spec) and an agent only ever reports its OWN
// server's specs, so a spec id belonging to another server can only ever
// receive nothing -- there is no id an authorized caller could name to reach
// output they are not already entitled to.
//
// It does need a VALIDITY check, and that reasoning is why it did not have one.
// The id is not only a fan-out key: it is a value the gateway SHIPS TO THE AGENT
// inside the outbound runtime_log_config command, where every subscribed id for
// the server is marshaled into one frame. MaxHeaderBytes is 1 MiB, so one
// authorized caller holding two GETs with ~600 KiB of spec_id each produced a
// ~1.2 MiB frame that failed the agent's own SetReadLimit -- and because a fresh
// connection is answered with an unconditional restate, the agent's WebSocket
// then flapped for as long as those requests were held open. The agent's
// maxWatchedSpecs guard runs only after the frame has been read, so it never
// got the chance.
//
// Both bounds REJECT rather than clamp. ingestRuntimeLog clamps a reported id to
// runtimeLogMaxSpecIDLen, so a subscription on a longer one could never match a
// published batch: clamping here would hand back a window guaranteed to stay
// empty forever, which is the outcome this whole feature exists to eliminate. An
// id that cannot match is an error, and is answered as one.
//
// SUBSCRIBING IS WHAT STARTS THE STREAM. The registry tells the agent the new
// watch set on the first subscriber for a spec and again on the last
// unsubscribe, so the agent produces output only while this handler (or a
// sibling) is running. The deferred unsubscribe is therefore not merely
// cleanup: it is the stop command.
func (s *Server) handleRuntimeLogEvents(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	specID := strings.TrimSpace(r.URL.Query().Get("spec_id"))
	if specID == "" {
		writeJSON(w, http.StatusBadRequest, apierror.Response("runtime_logs.spec_required", "spec_id is required", ""))
		return
	}
	if len(specID) > runtimeLogMaxSpecIDLen {
		writeJSON(w, http.StatusBadRequest, apierror.Response("runtime_logs.spec_invalid", "spec_id is too long", ""))
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
	// Subscribe BEFORE the 200: past the header there is no status left to
	// answer a refusal with, and a refusal here has to be visible. The agent
	// silently truncates a watch set past its own maxWatchedSpecs, so a
	// subscription the gateway accepted past the same ceiling would be a window
	// that streams nothing, forever, with nothing said.
	sub, unsub, ok := s.RuntimeLogs.subscribe(serverID, specID)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apierror.Response("runtime_logs.too_many_specs",
			"too many specs are being streamed for this server", ""))
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Clear the server WriteTimeout for this long-lived response. An unsupported
	// writer (httptest recorder) returns an error we intentionally ignore.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	state := s.runtimeLogState(serverID)
	if !writePerfEvent(w, flusher, "status", runtimeLogStatusEventDTO{State: state}) {
		return
	}

	statusTicker := time.NewTicker(runtimeLogStatusInterval)
	defer statusTicker.Stop()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.resync:
			// This reader fell so far behind that the next thing lost would be
			// a generation boundary. Ending the stream is the honest move: the
			// browser's EventSource reconnects on its own, and a reconnect now
			// re-snapshots, so it comes back with a complete history instead of
			// a silently incomplete one.
			return
		case batch := <-sub.ch:
			// take() stamps on the markers rescued from batches this
			// subscriber's queue could not hold and on the bytes those batches
			// were carrying, so the gap is reported where it happened rather
			// than showing up as silence.
			if !writePerfEvent(w, flusher, "log", sub.take(batch)) {
				return
			}
		case <-statusTicker.C:
			if next := s.runtimeLogState(serverID); next != state {
				state = next
				if !writePerfEvent(w, flusher, "status", runtimeLogStatusEventDTO{State: state}) {
					return
				}
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
