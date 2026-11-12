// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/compat"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// apiFlavorOpenAIDecisions is the OpenAI Decisions endpoint's (/v1/decisions)
// request flavor (ADR-098). NormalizeAPIFlavor folds it to the coarse decisions
// flavor, so candidacy admits exactly the applications that declare decisions,
// while applicationServesEndpoint applies the decisions mode to it and
// upstreamPath answers the route the handler resolved. It labels usage rows,
// which /v1/systemone keeps labelling with the coarse flavor.
const apiFlavorOpenAIDecisions = routing.APIFlavorOpenAIDecisions

// The request-error codes validateDecisionsRequest answers with, and the
// codes of the refusals and upstream failures this endpoint and /v1/systemone
// answer with (ADR-098). They are API surface: clients branch on them.
const (
	decisionsModelRequired     = "decisions.model_required"
	decisionsQuestionsRequired = "decisions.questions_required"
	decisionsStreamUnsupported = "decisions.stream_unsupported"

	codeDecisionsEndpointDisabled = "decisions.endpoint_disabled"
	codeSystemOneEndpointDisabled = "systemone.endpoint_disabled"
	codeDecisionsUpstreamError    = "decisions.upstream_error"
	codeSystemOneUpstreamError    = "systemone.upstream_error"
)

// decisionsInferenceShape is the shape handleOpenAIDecisions hands the ONE
// preflight gate, pinned by its test like systemOneInferenceShape: the
// openai_decisions request flavor, the decisions endpoint, and the derived
// any_decision requirement. The requirement admits a mapping with either
// verdict (decision or decision_openai); which one the request then needs is
// decided after the resolve (decisionsRoute). Like System One's list, it also
// keeps the request off the vendor-account path.
func decisionsInferenceShape(model string) inferenceShape {
	return inferenceShape{
		apiFlavor:            apiFlavorOpenAIDecisions,
		endpoint:             endpointDecisions,
		model:                model,
		requiredCapabilities: []string{routing.CapabilityAnyDecision},
	}
}

// handleOpenAIDecisions serves POST /v1/decisions (and /openai/v1/decisions):
// an OpenAI Decisions request relayed to a decision-capable model, either
// passed through to the upstream's own /v1/decisions or translated to System One
// and back, as the effective decisions mode and the mapping's verdicts decide
// (ADR-098). Bearer only (gateway:use or llm:invoke): a browser session cookie
// and the internal loopback are no way in.
func (s *Server) handleOpenAIDecisions(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONLimit(w, r, s.decisionsBodyLimit())
	if !ok {
		return
	}
	model, _ := sniffRoutingModel(raw)
	if err := validateDecisionsRequest(raw); err != nil {
		writeRequestError(w, err)
		return
	}
	token, ok = s.runAsPrincipal(w, r, token)
	if !ok {
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, decisionsInferenceShape(model))
	if handled {
		return
	}
	s.relayDecisions(w, r, token, pf.Req, raw)
}

// validateDecisionsRequest validates the shape this handler owns: a non-blank
// string "model", a non-empty JSON ARRAY "questions" (System One's object form
// is refused here), and no streaming ("stream" absent, null or false; OpenAI
// Decisions has none). Everything else is the translator's or the upstream's
// business. Like validateSystemOneRequest it returns the concrete
// *inference.Error, nil for a valid request.
func validateDecisionsRequest(raw []byte) *inference.Error {
	var body struct {
		Model     json.RawMessage `json:"model"`
		Questions json.RawMessage `json:"questions"`
		Stream    json.RawMessage `json:"stream"`
	}
	_ = json.Unmarshal(raw, &body)
	var model string
	if json.Unmarshal(body.Model, &model) != nil || strings.TrimSpace(model) == "" {
		return &inference.Error{Code: decisionsModelRequired, Message: "model is required and must be a non-empty string"}
	}
	var questions []json.RawMessage
	if json.Unmarshal(body.Questions, &questions) != nil || len(questions) == 0 {
		return &inference.Error{Code: decisionsQuestionsRequired, Message: "questions is required and must be a non-empty array"}
	}
	if len(body.Stream) > 0 && string(body.Stream) != "null" {
		var stream bool
		if json.Unmarshal(body.Stream, &stream) != nil || stream {
			return &inference.Error{Code: decisionsStreamUnsupported, Message: "decisions do not stream; leave stream out or set it to false"}
		}
	}
	return nil
}

// relayDecisions resolves an already-gated OpenAI Decisions request, settles its
// concrete route (decisionsRoute) and relays it via proxyNative: passthrough
// sends the client's body to the upstream's /v1/decisions (model rewritten),
// translate sends the System One body compat.TranslateDecisionsRequest builds to
// its /v1/systemone. Every refusal is answered and recorded here; a gone
// instance re-runs this whole path (relayAgain).
func (s *Server) relayDecisions(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	c := decisionsCall{token: token, req: req, raw: raw, start: time.Now(), endpoint: endpointDecisions}
	target, ok := s.resolveDecisionsTarget(w, r, &c)
	if !ok {
		return
	}
	if target, ok = s.decisionsRoute(w, r, c, target); !ok {
		return
	}
	rel := nativeRelay{
		token:    c.token,
		target:   target,
		path:     upstreamPath(target, apiFlavorOpenAIDecisions, endpointDecisions),
		raw:      raw,
		pfReq:    c.req,
		endpoint: endpointDecisions,
		reselect: relayAgain(c.req, func(again inference.Request) { s.relayDecisions(w, r, c.token, again, raw) }),
	}
	if target.DecisionsMode == routing.EndpointModeTranslate {
		// The System One body already names the upstream model, the one
		// proxyNative's model rewrite would set (target.ProviderModel), so that
		// rewrite is a no-op on it and the body reaches the upstream as built.
		body, table, derr := compat.TranslateDecisionsRequest(raw, effectiveProviderModel(target, c.req.Model))
		if derr != nil {
			slog.Debug("decisions request refused by the translator", "path", r.URL.Path, "model", c.req.Model, "code", derr.Code, "err", derr.Message)
			s.refuseDecisionsRequest(w, r, c, http.StatusBadRequest, derr.Code, derr.Message)
			return
		}
		rel.upstreamBody = body
		rel.decisionsRewrite = &decisionsRewrite{table: &table, clientModel: c.req.Model, normalize: c.token.DecisionsConfidenceNormalize, dialect: compat.DialectOpenAI, failCode: codeDecisionsUpstreamError}
	} else if c.token.DecisionsConfidenceNormalize {
		rel.decisionsRewrite = &decisionsRewrite{normalize: true, dialect: compat.DialectOpenAI, failCode: codeDecisionsUpstreamError}
	}
	s.proxyNative(w, r, rel)
}

// decisionsRoute settles the concrete route of a resolved OpenAI Decisions
// request (ADR-098): it reads the mapping's two verdicts and hands them, with
// the target's effective decisions mode, to routing.EffectiveDecisionsRoute. A
// disabled endpoint is answered 404 decisions.endpoint_disabled, a route whose
// verdict is missing 404 routing.model_not_capable; otherwise the returned
// target carries the route (translate or passthrough) as its DecisionsMode, which
// is what upstreamPath and the usage row's provider_path read.
func (s *Server) decisionsRoute(w http.ResponseWriter, r *http.Request, c decisionsCall, target routing.Target) (routing.Target, bool) {
	decision, decisionOpenAI, err := s.decisionVerdicts(r.Context(), target.RouteID)
	if err != nil {
		slog.Error("decisions request: the mapping's verdicts could not be read", "path", r.URL.Path, "model", c.req.Model, "route_id", target.RouteID, "err", err)
		s.recordDecisionsRoutingFailure(w, r, c, err)
		return routing.Target{}, false
	}
	route, ok := routing.EffectiveDecisionsRoute(target.DecisionsMode, decision, decisionOpenAI)
	switch {
	case route == routing.EndpointModeDisabled:
		slog.Debug("decisions request rejected: endpoint disabled", "path", r.URL.Path, "model", c.req.Model, "server", s.serverName(target.ServerID), "route_id", target.RouteID)
		s.refuseDecisionsRequest(w, r, c, statusEndpointDisabled, codeDecisionsEndpointDisabled, msgEndpointDisabled)
		return routing.Target{}, false
	case !ok:
		slog.Debug("decisions request rejected: the mapping has no verdict for the route its mode asks for", "path", r.URL.Path, "model", c.req.Model, "mode", string(target.DecisionsMode), "route_id", target.RouteID)
		s.recordDecisionsRoutingFailure(w, r, c, routing.ErrModelNotCapable)
		return routing.Target{}, false
	}
	target.DecisionsMode = route
	return target, true
}

// decisionVerdicts reads whether the mapping carries (decision, yes) and
// (decision_openai, yes): one read of its capability rows.
func (s *Server) decisionVerdicts(ctx context.Context, mappingID string) (decision, decisionOpenAI bool, err error) {
	rows, err := s.Routes.MappingCapabilities(ctx, mappingID)
	if err != nil {
		return false, false, fmt.Errorf("read capability rows of mapping %s: %w", mappingID, err)
	}
	byName := routing.CapabilityRowsByName(rows)
	return byName[routing.CapabilityDecision].Verdict == routing.CapabilityYes,
		byName[routing.CapabilityDecisionOpenAI].Verdict == routing.CapabilityYes, nil
}
