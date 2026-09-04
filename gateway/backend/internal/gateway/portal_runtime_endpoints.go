// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
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
	{err: portal.ErrRuntimeSpecVisibleDevicesNoGPUs, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_no_gpus", msg: "set_visible_devices requires at least one gpu row: an empty visible-devices value hides every gpu from the model"},
	{err: portal.ErrRuntimeSpecVisibleDevicesConflict, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_conflict", msg: "set_visible_devices conflicts with a gpu visibility variable set by hand in env"},
	{err: portal.ErrRuntimeSpecVisibleDevicesModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_mode_invalid", msg: "visible_devices_mode must be \"env\" or \"args\""},
	{err: portal.ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder, status: http.StatusBadRequest, code: "runtime_spec.visible_devices_args_no_placeholder", msg: "args mode requires one of ${CUDA_DEVICES}/${VULKAN_DEVICES}/${METAL_DEVICES} in args"},
	{err: portal.ErrRuntimeSpecNotServerAgent, status: http.StatusBadRequest, code: "runtime_spec.application_not_server_agent", msg: "runtime spec requires a server_agent application"},
	{err: portal.ErrRuntimeSpecEndpointModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.endpoint_mode_invalid", msg: "runtime spec endpoint mode is invalid"},
	{err: portal.ErrRuntimeSpecFlavorInvalid, status: http.StatusBadRequest, code: "runtime_spec.flavor_invalid", msg: "runtime spec api flavor is invalid"},
	// Task 8: the four per-spec API-token sentinels from Task 3
	// (validateRuntimeSpecAPIToken) -- code is err.Error() verbatim, per the
	// convention every row above already follows.
	{err: portal.ErrRuntimeSpecAPITokenModeInvalid, status: http.StatusBadRequest, code: "runtime_spec.api_token_mode_invalid", msg: "api_token_mode must be \"app\", \"off\", \"set\" or \"random\""},
	{err: portal.ErrRuntimeSpecAPITokenNoPlaceholder, status: http.StatusBadRequest, code: "runtime_spec.api_token_no_placeholder", msg: "api_token_mode \"set\"/\"random\" requires a \"${API_TOKEN}\" placeholder in env or args"},
	{err: portal.ErrRuntimeSpecAPITokenPlaceholderWithoutMode, status: http.StatusBadRequest, code: "runtime_spec.api_token_placeholder_without_mode", msg: "a \"${API_TOKEN}\" placeholder in env or args requires api_token_mode \"set\" or \"random\""},
	{err: portal.ErrRuntimeSpecAPITokenHeaderInvalid, status: http.StatusBadRequest, code: "runtime_spec.api_token_header_invalid", msg: "api_token_header_source must be \"app\" or \"custom\", with a valid api_token_header when custom"},
	// Task 4's seal-or-400 path: PutRuntimeSpec computes the sealed api_token
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
