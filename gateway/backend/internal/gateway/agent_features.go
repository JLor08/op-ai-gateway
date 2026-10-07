// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
)

// gatewayAgentFeatures is the gateway's declared feature set for agents
// (design spec §9, feature negotiation): behavior hangs exclusively on
// string-equal feature NAMES, never on version comparison -- a version gate
// breaks under forks, backports, and custom builds, while a name says what
// the binary can actually do. A name both sides declare is ACTIVE iff BOTH
// the gateway and the agent declare it; each side computes that intersection
// independently. A name that states a fact about one side alone need only be
// on that side's list: capability_source_sdcpp is on this list only, and the
// agent-declared gpu_selection, runtime_api_token, runtime_model_probe,
// runtime_upstream_props (RuntimeUpstreamPropsFeature) and runtime_ensure
// (runtimeEnsureFeature) are on the agent's only.
// Append-only: once shipped, a name is never removed or renamed here, only
// added to.
// runtime_logs is declared here for completeness of the negotiation contract
// -- this gateway does understand runtime_log frames and does issue
// runtime_log_config commands -- but note where the decision it gates actually
// lives. Unlike runtime_manager, which the AGENT checks against this list
// before managing anything (runtime.Driver.featureActive), runtime_logs is
// checked in the other direction: the gateway consults the agent's DECLARED
// set before telling a portal log view that a live stream is possible
// (Server.runtimeLogState). An agent needs no permission to answer a command
// it was sent.
//
// runtime_config_ack is the same shape as runtime_logs: declared for
// completeness, decided in the other direction. The gateway consults the
// AGENT's declared set before it will wait for an applied-config
// acknowledgement instead of blindly waiting out the agent's poll interval
// (runtimeConfigAckFeature, agent_runtime.go).
//
// capability_source_sdcpp is one-sided, so the intersection rule above does
// not apply to it: the agent's declared set has no counterpart. It is this
// gateway's statement of what its capability ingest accepts, the
// sdcpp_capabilities source for the image row only (rowSource,
// runtimeSampleCapabilityRows), and the AGENT reads it as a precondition. It
// checks this list for the name, as it does for runtime_manager, before it
// reports a stable_diffusion_cpp child's image verdict under that source at
// all. The ingest's acceptance does not consult the name; a gateway build
// without the name simply has no such acceptance, which is why the agent
// does not send the source there.
var gatewayAgentFeatures = []string{"runtime_manager", runtimeLogsFeature, runtimeConfigAckFeature, capabilitySourceSdcppFeature}

// capabilitySourceSdcppFeature says this gateway's capability ingest accepts
// the sdcpp_capabilities source from an agent (rowSource), for the image row
// only. The agent sends that source only when this name is declared (its
// gatewayFeatureCapabilitySourceSdcpp).
const capabilitySourceSdcppFeature = "capability_source_sdcpp"

// RuntimeUpstreamPropsFeature is the agent-declared capability naming the
// runtime router's GET /upstream/{model}/props passthrough
// (routing.AgentRouterContextProbePath). It is on the agent's list only, not
// on gatewayAgentFeatures. A probe through that route goes only to an agent
// with positive evidence the route exists -- fail-closed, the
// PushRuntimeConfig precedent -- because an older agent answers 404
// runtime.model_not_managed for every such probe, forever, and silent no-op
// traffic each cadence tick is exactly what a capability gate exists to
// prevent. It is exported for the app-health loop in cmd/gateway, which
// checks it too.
const RuntimeUpstreamPropsFeature = "runtime_upstream_props"

// agentFeaturesDTO is the GET /api/agent/v1/features response body. Unlike
// AgentProxyRoutesDTO/AgentRuntimeConfigDTO, the etag is carried ONLY in the
// ETag header (see handleAgentFeatures) -- there is no in-body etag field,
// since the body is nothing but the static feature list itself.
type agentFeaturesDTO struct {
	Features []string `json:"features"`
}

// handleAgentFeatures serves the gateway's declared feature list (spec §9) so
// the agent can compute the feature intersection at startup and on every WS
// reconnect. Deliberately no hello frame: this works identically for POST and
// WS agents, is cacheable, and needs no connection state machine.
//
// Like every other agent-token endpoint, the target server comes ONLY from
// the agent token (authenticateAgent); the response itself does not depend on
// serverID at all (the feature list is a gateway-wide constant), but auth is
// still required so an unauthenticated caller cannot probe the endpoint.
//
// A conditional GET (If-None-Match against the sha256 hex digest of the
// marshaled body) answers 304 with no body -- the steady state, so an agent
// polling before/after every reconnect does not re-fetch an unchanged list.
func (s *Server) handleAgentFeatures(w http.ResponseWriter, r *http.Request) {
	// Set on EVERY response path (before method/auth checks), matching every
	// other Bearer-token agent endpoint (agent_ca.go, agent_proxy_routes.go):
	// this traffic flows through the public listener behind fronting infra by
	// default and must never be cached there.
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if _, ok := s.authenticateAgent(w, r); !ok {
		return
	}
	dto := agentFeaturesDTO{Features: gatewayAgentFeatures}
	raw, err := json.Marshal(dto)
	if err != nil {
		// gatewayAgentFeatures is a package-level []string constant: this can
		// only fail if that invariant is ever violated, never on live input.
		writeJSON(w, http.StatusInternalServerError, apierror.Response("agent_features.encode_failed", "could not encode feature list", ""))
		return
	}
	sum := sha256.Sum256(raw)
	etag := hex.EncodeToString(sum[:])
	w.Header().Set("ETag", `"`+etag+`"`)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}
