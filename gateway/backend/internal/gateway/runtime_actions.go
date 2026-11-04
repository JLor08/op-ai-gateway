// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// The portal's manual start and stop of a managed model's instance:
//
//	POST /api/portal/mappings/{mappingID}/runtime/start
//	POST /api/portal/mappings/{mappingID}/runtime/stop   {"instance": k}
//
// Both are transient. Neither writes the spec: a start asks the agent for one
// instance through its ensure route and a stop asks it to drain one, and
// afterwards the instance follows the normal idle, eviction and demand rules.
// The persistent override is the spec's admin_state (the runtime-spec PUT).
// Both authorize as the runtime-spec PUT does and are refused while a
// benchmark run holds the server (portal.Service.AuthorizeRuntimeAction).

// runtimeStopFeature is the agent-DECLARED capability naming the router's
// POST /stop/{model} route (agent 0.17.0). It is on the agent's list only, not
// on gatewayAgentFeatures: the gateway reads it, fail-closed, before it asks an
// agent to stop an instance, because an older router hands the bodiless POST
// to its proxy, which answers 404 runtime.model_not_managed -- the answer for
// an unknown address too, so the two could not be told apart.
//
// The agent module cannot import this package, so the agent names the
// identical string in its own registry (server-agent/internal/agent/features.go).
// The two spellings must stay equal; TestRuntimeStopFeatureIsAgentDeclaredOnly
// pins this side.
const runtimeStopFeature = "runtime_stop"

const (
	// defaultRuntimeStartWindow is how long a manual start waits for the agent's
	// answer before it answers 202 and lets the start complete in the
	// background. Most refusals (no room, an unmeasured sibling, an instance
	// already running) arrive at once; a real start takes as long as the model
	// loads, which the status stream shows.
	defaultRuntimeStartWindow = 5 * time.Second

	// runtimeStopTimeout bounds a stop call: the agent answers as soon as a
	// drain began, without waiting for the process to exit.
	runtimeStopTimeout = 15 * time.Second
)

// The manual start's and stop's own error codes. The agent's refusals keep the
// agent's codes (runtimeAgentCodes).
const (
	codeRuntimeAdminStopped        = "runtime.admin_stopped"
	codeRuntimeMaxInstancesReached = "runtime.max_instances_reached"
	codeRuntimeAgentFeatureMissing = "runtime.agent_feature_missing"
	codeRuntimeAgentUnavailable    = "runtime.agent_unavailable"
	codeRuntimeAgentTimeout        = "runtime.agent_timeout"
	codeRuntimeAgentError          = "runtime.agent_error"
	codeRuntimeInstanceInvalid     = "runtime.instance_invalid"
	codeRuntimeActionFailed        = "runtime.action_failed"
)

// runtimeAgentCodes are the agent router's refusals a manual start or stop
// answers with the agent's own code, at the status the router answered. Any
// other router code is codeRuntimeAgentError: a code this gateway does not
// know is not one a client can be told to act on.
var runtimeAgentCodes = map[string]struct {
	status int
	msg    string
}{
	runtimeInstanceNoCapacityCode:    {http.StatusConflict, "the agent has no free room for another instance of this model"},
	runtimeInstanceDemandUnknownCode: {http.StatusConflict, "a running instance's demand is not measured yet; try again in a few seconds"},
	"runtime.instance_protected":     {http.StatusConflict, "this instance is kept up by the spec (min instances, pinned or force running); the agent would start it again at once"},
	"runtime.instance_not_highest":   {http.StatusConflict, "a higher instance of this model is running; instances stop highest first"},
	"runtime.model_not_managed":      {http.StatusNotFound, "the agent does not manage this model or instance"},
	"runtime.admission_blocked":      {http.StatusServiceUnavailable, "the agent cannot admit this model now"},
	"runtime.start_timeout":          {http.StatusGatewayTimeout, "the model did not become healthy within its startup timeout"},
	"runtime.start_failed":           {http.StatusBadGateway, "the model failed to start"},
	"runtime.not_permitted":          {http.StatusBadGateway, "the agent did not permit the model to start"},
	"runtime.upstream_gone":          {http.StatusBadGateway, "the agent could not complete the request"},
}

// handlePortalMappingRuntimeAction dispatches POST
// /api/portal/mappings/{mappingID}/runtime/{start,stop}.
func (s *Server) handlePortalMappingRuntimeAction(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID, action string) {
	var run func(http.ResponseWriter, *http.Request, auth.Token, string)
	switch action {
	case "start":
		run = s.startRuntimeInstance
	case "stop":
		run = s.stopRuntimeInstance
	default:
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeMappingNotFound, msgMappingNotFound, ""))
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	run(w, r, token, mappingID)
}

// startRuntimeInstance serves POST .../runtime/start: it picks the instance
// (claimStartInstance), asks the agent for it in the background, and waits up
// to the early-answer window for the agent's answer -- 200 running, the
// agent's refusal, or else 202 starting while the start completes in the
// background (runRuntimeStart). Refused, in this order, after authorization:
// a force_stopped spec (409 runtime.admin_stopped: the agent refuses every
// start while it is set), an agent not heard from since the gateway started
// (503 runtime.agent_unavailable), an agent without runtime_ensure, no free
// index below max_instances (409 runtime.max_instances_reached) and an
// instance k >= 1 on an agent without runtime_instances (409
// runtime.agent_feature_missing naming the feature). It does not apply the
// scaler's performance ceiling, its hold-offs or its starting-sibling rule: a
// manual start is the operator's explicit choice, and the agent still has the
// final say on room.
func (s *Server) startRuntimeInstance(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	scope, err := s.Portal.AuthorizeRuntimeAction(r.Context(), token, mappingID)
	if err != nil {
		writePortalRuntimeSpecError(w, err)
		return
	}
	if scope.Spec.AdminState == vramAdminStateForceStopped {
		writeJSON(w, http.StatusConflict, apierror.Response(codeRuntimeAdminStopped, "this model's runtime spec is pinned as stopped; clear the admin override before starting it", ""))
		return
	}
	serverID := scope.Server.ID
	if !s.refuseAgentWithout(w, serverID, runtimeEnsureFeature) {
		return
	}
	ensurer, ok := s.Provider.(provider.RuntimeEnsurer)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apierror.Response(codeRuntimeAgentUnavailable, "this gateway's provider cannot start a model through the agent", ""))
		return
	}
	k, claimed, err := s.claimStartInstance(r.Context(), scope)
	if err != nil {
		if errors.Is(err, errRuntimeMaxInstances) {
			writeJSON(w, http.StatusConflict, apierror.Response(codeRuntimeMaxInstancesReached, "every instance this spec allows is running or starting", ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response(codeRuntimeActionFailed, "could not pick an instance to start", ""))
		return
	}
	key := scaleKey{serverID: serverID, mappingID: scope.Mapping.ID}
	if k >= 1 && !s.AgentFeatures.Has(serverID, runtimeInstancesFeature) {
		if claimed {
			s.instanceClaims.release(key, k)
		}
		writeFeatureMissing(w, runtimeInstancesFeature)
		return
	}
	start := &runtimeStart{answer: make(chan error, 1), waiting: true}
	s.runtimeStarts.Add(1)
	go func() {
		defer s.runtimeStarts.Done()
		s.runRuntimeStart(ensurer, scope, k, claimed, start)
	}()
	s.awaitRuntimeStart(w, r, k, start)
}

// errRuntimeMaxInstances: no index below the spec's max_instances (or the
// highest an address can carry) is free.
var errRuntimeMaxInstances = errors.New(codeRuntimeMaxInstancesReached)

// claimStartInstance picks the instance a manual start asks for and claims it
// in the claim set the scaler shares (instance_claims.go), and reports whether
// it claimed. A spec that is not replicable runs instance 0 alone, which is
// asked for unclaimed: an ensure of a running instance is a no-op. For a
// replicable spec the index rule is the scaler's, with instance 0 included:
// instance 0 is free while the agent reports it neither starting nor running,
// and k >= 1 while it is not reported in a state that keeps the index
// (specInstanceRows), not the address of another mapping's upstream model, and
// not claimed by an ensure under way. The lowest free index below
// max_instances (0: no cap of its own) is claimed in the same step.
func (s *Server) claimStartInstance(ctx context.Context, scope portal.RuntimeActionScope) (int, bool, error) {
	if !scope.Spec.Replicable {
		return 0, false, nil
	}
	rows := s.RuntimeStatus.statusRows(scope.Server.ID)
	_, reported := specInstanceRows(rows, scope.Spec.ID)
	mappings, err := s.Routes.MappingsByApplication(ctx, scope.Application.ID)
	if err != nil {
		return 0, false, err
	}
	taken := otherMappingIndexes(mappings, scope.Mapping)
	for _, k := range reported {
		taken[k] = true
	}
	for i := range rows {
		row := &rows[i]
		if row.SpecID == scope.Spec.ID && row.Instance == 0 && (row.State == agentSpecStateRunning || row.State == agentSpecStateStarting) {
			taken[0] = true
		}
	}
	limit := routing.MaxInstanceIndex + 1
	if scope.Spec.MaxInstances > 0 {
		limit = min(limit, scope.Spec.MaxInstances)
	}
	key := scaleKey{serverID: scope.Server.ID, mappingID: scope.Mapping.ID}
	k, ok := s.instanceClaims.claimLowest(key, 0, limit, taken)
	if !ok {
		return 0, false, errRuntimeMaxInstances
	}
	return k, true, nil
}

// runtimeStart is one manual start's hand-off between the handler, which
// waits for the agent's answer for the early-answer window, and the background
// ensure. waiting is true while the handler still takes the answer; once it
// has left (the window ran out, or its client went away) the background ensure
// logs its outcome instead.
type runtimeStart struct {
	mu      sync.Mutex
	waiting bool
	answer  chan error // buffered 1
}

// deliver hands err to the waiting handler and reports whether it did.
func (st *runtimeStart) deliver(err error) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.waiting {
		return false
	}
	st.answer <- err
	return true
}

// detach marks the handler gone and returns an answer that arrived meanwhile.
func (st *runtimeStart) detach() (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.waiting = false
	select {
	case err := <-st.answer:
		return true, err
	default:
		return false, nil
	}
}

// runRuntimeStart is a manual start's ensure, run in the background. It is
// bounded by the spec's startup timeout plus the scaler's margin
// (instanceEnsureTimeout) and by the server's lifetime, so no start outlives
// the shutdown. Its answer goes to the waiting handler; an answer nobody waits
// for any more is logged -- a failure at warn, unless the shutdown cut it
// short. A claimed index stays claimed until the agent's status reports the
// instance, bounded by the visible wait, so neither the scaler nor another
// manual start asks for it before routing can see it.
func (s *Server) runRuntimeStart(ensurer provider.RuntimeEnsurer, scope portal.RuntimeActionScope, k int, claimed bool, start *runtimeStart) {
	key := scaleKey{serverID: scope.Server.ID, mappingID: scope.Mapping.ID}
	if claimed {
		defer s.instanceClaims.release(key, k)
	}
	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, instanceEnsureTimeout(scope.Spec))
	defer cancel()
	target := runtimeInstanceTarget(scope, k)
	err := ensurer.EnsureRuntimeModel(s.upstreamAuthCtx(ctx, target), target)
	if !start.deliver(err) {
		attrs := []any{"mapping_id", scope.Mapping.ID, "server_id", scope.Server.ID, "instance", target.ProviderModel}
		switch {
		case err == nil:
			slog.Info("runtime start: started an instance", attrs...)
		case base.Err() != nil:
			slog.Debug("runtime start: the gateway's shutdown ended a start", attrs...)
		default:
			slog.Warn("runtime start: starting an instance failed", append(attrs, "err", err)...)
		}
	}
	if err == nil && claimed {
		s.awaitInstanceReported(base, scope.Server.ID, scope.Spec.ID, k)
	}
}

// awaitRuntimeStart answers the manual start: the agent's answer when it
// arrives within the early-answer window, else 202 starting. A client that
// leaves first gets nothing; the start goes on either way.
func (s *Server) awaitRuntimeStart(w http.ResponseWriter, r *http.Request, k int, start *runtimeStart) {
	window := s.runtimeStartWindow
	if window <= 0 {
		window = defaultRuntimeStartWindow
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	var err error
	select {
	case err = <-start.answer:
	case <-timer.C:
		var answered bool
		if answered, err = start.detach(); !answered {
			writeJSON(w, http.StatusAccepted, runtimeActionAnswer{Instance: k, State: "starting"})
			return
		}
	case <-r.Context().Done():
		if answered, err := start.detach(); answered && err != nil {
			slog.Debug("runtime start: the agent refused a start whose client had left", "instance", k, "err", err)
		}
		return
	}
	if err != nil {
		writeRuntimeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeActionAnswer{Instance: k, State: agentSpecStateRunning})
}

// awaitInstanceReported returns once serverID's status reports instance k of
// spec specID starting or running, after the visible wait, or when ctx ends. It
// subscribes before it looks, so a sample published in between is not missed.
func (s *Server) awaitInstanceReported(ctx context.Context, serverID, specID string, k int) {
	wait := s.runtimeStartVisibleWait
	if wait <= 0 {
		wait = instanceVisibleWait
	}
	rows, updates, unsubscribe := s.RuntimeStatus.subscribe(serverID)
	defer unsubscribe()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for !instanceReported(rows, specID, k) {
		select {
		case next, open := <-updates:
			if !open {
				updates = nil
				continue
			}
			rows = next
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// instanceReported reports whether rows carry instance k of spec specID
// starting or running.
func instanceReported(rows []RuntimeStatusDTO, specID string, k int) bool {
	for i := range rows {
		row := &rows[i]
		if row.SpecID == specID && row.Instance == k && (row.State == agentSpecStateRunning || row.State == agentSpecStateStarting) {
			return true
		}
	}
	return false
}

// runtimeStopRequest is POST .../runtime/stop's body. Instance is required.
type runtimeStopRequest struct {
	Instance *int `json:"instance"`
}

// runtimeActionAnswer is a manual start's or stop's success body.
type runtimeActionAnswer struct {
	Instance int    `json:"instance"`
	State    string `json:"state"`
}

// stopRuntimeInstance serves POST .../runtime/stop: it asks the agent to drain
// instance k now and answers 200 with the agent's state, stopping or stopped.
// It changes nothing in the config, so demand can start the instance again.
// The gateway checks only what the agent cannot: the body (400
// runtime.instance_invalid for a missing index or one outside 0..63), an agent
// not heard from (503) and the agent feature runtime_stop (409
// runtime.agent_feature_missing). Whether the instance may stop -- the
// protected floor, highest index first -- is the agent's to decide, and its
// 409 runtime.instance_protected / runtime.instance_not_highest and 404
// runtime.model_not_managed pass through.
func (s *Server) stopRuntimeInstance(w http.ResponseWriter, r *http.Request, token auth.Token, mappingID string) {
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req runtimeStopRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	scope, err := s.Portal.AuthorizeRuntimeAction(r.Context(), token, mappingID)
	if err != nil {
		writePortalRuntimeSpecError(w, err)
		return
	}
	if req.Instance == nil || *req.Instance < 0 || *req.Instance > routing.MaxInstanceIndex {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRuntimeInstanceInvalid, "instance must be an index from 0 to 63", ""))
		return
	}
	if !s.refuseAgentWithout(w, scope.Server.ID, runtimeStopFeature) {
		return
	}
	stopper, ok := s.Provider.(provider.RuntimeStopper)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apierror.Response(codeRuntimeAgentUnavailable, "this gateway's provider cannot stop a model through the agent", ""))
		return
	}
	k := *req.Instance
	target := runtimeInstanceTarget(scope, k)
	ctx, cancel := context.WithTimeout(r.Context(), runtimeStopTimeout)
	defer cancel()
	state, err := stopper.StopRuntimeModel(s.upstreamAuthCtx(ctx, target), target)
	if err != nil {
		writeRuntimeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeActionAnswer{Instance: k, State: state})
}

// refuseAgentWithout answers when serverID's agent cannot serve an action that
// needs feature, and reports whether the action may go on: 503
// runtime.agent_unavailable for an agent not heard from since the gateway
// started (its declared features are not known), 409
// runtime.agent_feature_missing naming feature for one that does not declare
// it.
func (s *Server) refuseAgentWithout(w http.ResponseWriter, serverID, feature string) bool {
	if !s.AgentFeatures.Known(serverID) {
		writeJSON(w, http.StatusServiceUnavailable, apierror.Response(codeRuntimeAgentUnavailable, "the server's agent has not reported since the gateway started", ""))
		return false
	}
	if !s.AgentFeatures.Has(serverID, feature) {
		writeFeatureMissing(w, feature)
		return false
	}
	return true
}

// featureMissingBody is runtime.agent_feature_missing's body: the error
// envelope with the missing feature's name beside its code.
type featureMissingBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Feature string `json:"feature"`
	} `json:"error"`
}

func writeFeatureMissing(w http.ResponseWriter, feature string) {
	var body featureMissingBody
	body.Error.Code = codeRuntimeAgentFeatureMissing
	body.Error.Message = "the server's agent does not declare the feature " + feature + "; update the agent"
	body.Error.Feature = feature
	writeJSON(w, http.StatusConflict, body)
}

// writeRuntimeAgentError answers an agent call's failure: a known router
// refusal with the agent's code at its status (runtimeAgentCodes), a deadline
// as 504 runtime.agent_timeout, an agent the gateway could not reach as 503
// runtime.agent_unavailable, and anything else as 502 runtime.agent_error.
func writeRuntimeAgentError(w http.ResponseWriter, err error) {
	var re *provider.RouterError
	if errors.As(err, &re) {
		if known, ok := runtimeAgentCodes[re.Code]; ok {
			writeJSON(w, known.status, apierror.Response(re.Code, known.msg, ""))
			return
		}
		writeJSON(w, http.StatusBadGateway, apierror.Response(codeRuntimeAgentError, "the agent refused the request: "+re.Error(), ""))
		return
	}
	switch {
	case errors.Is(err, provider.ErrTimeout):
		writeJSON(w, http.StatusGatewayTimeout, apierror.Response(codeRuntimeAgentTimeout, "the agent did not answer in time", ""))
	case errors.Is(err, provider.ErrUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, apierror.Response(codeRuntimeAgentUnavailable, "the gateway could not reach the server's agent", ""))
	default:
		writeJSON(w, http.StatusBadGateway, apierror.Response(codeRuntimeAgentError, "the agent request failed", ""))
	}
}

// runtimeInstanceTarget is the agent target of instance k of scope's mapping:
// the application's endpoint, the instance's address and key, and the spec's
// upstream credential -- what routing's target builder makes of the same
// mapping (routing.Resolver.InstanceTarget), from the scope already read.
func runtimeInstanceTarget(scope portal.RuntimeActionScope, k int) routing.Target {
	apiToken, apiTokenHeader := routing.SpecUpstreamAuth(scope.Spec, scope.Application)
	return routing.Target{
		RouteID:         scope.Mapping.ID,
		ServerID:        scope.Server.ID,
		Provider:        scope.Application.Type,
		Endpoint:        routing.ApplicationEndpoint(scope.Server, scope.Application),
		Model:           scope.Mapping.GatewayModelName,
		ProviderModel:   routing.InstanceAddress(scope.Mapping.AppModelName, k),
		InstanceKey:     routing.InstanceKey(scope.Mapping.ID, k),
		InstanceCapable: scope.Spec.Replicable,
		Timeout:         time.Duration(scope.Application.TimeoutMS) * time.Millisecond,
		APIToken:        apiToken,
		APITokenHeader:  apiTokenHeader,
	}
}

// waitRuntimeStarts returns once every manual start's background ensure has
// ended.
func (s *Server) waitRuntimeStarts() { s.runtimeStarts.Wait() }
