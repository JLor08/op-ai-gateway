// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/compat"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// defaultDecisionsBodyBytes is the default bound on the JSON body of
// POST /v1/systemone and POST /v1/decisions, and on a decisions answer the
// gateway buffers (relayDecisionsResponse), an operator setting
// (ServerDeps.MaxDecisionsBodyBytes, config
// OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES). It equals the agent router's JSON
// buffer, but the two are separate checks: this one counts the client's bytes,
// the router the relayed ones, and the shared model rewrite (rewriteModelField)
// re-encodes the body with HTML escaping and the mapping's model name, so a
// body near the cap can grow past the router's buffer and draw the router's own
// 413, relayed like any upstream error.
const defaultDecisionsBodyBytes = 32 << 20

// decisionsBodyLimit is the JSON read cap in force: the configured one, or the
// default for a Server that was never given one (a bare &Server{}).
func (s *Server) decisionsBodyLimit() int64 {
	if s.maxDecisionsBodyBytes > 0 {
		return s.maxDecisionsBodyBytes
	}
	return defaultDecisionsBodyBytes
}

// apiFlavorSystemOne is the System One endpoint's (/v1/systemone) flavor string,
// the same local alias for routing.APIFlavorDecisions that apiFlavorSpeech is for
// the coarse audio flavor: NormalizeAPIFlavor folds it to itself, so it filters
// candidacy (only an application declaring decisions is a candidate), labels
// usage rows, and answers upstreamPath. Like images and speech it has no
// translate path; its one mode, SystemOneMode, only switches it on or off
// (ADR-098). Whether a MODEL can give a verdict is the capability gate's job,
// keyed on routing.CapabilityDecision.
const apiFlavorSystemOne = routing.APIFlavorDecisions

// The request-error codes validateSystemOneRequest answers with. They are API
// surface: clients branch on them.
const (
	systemOneModelRequired     = "systemone.model_required"
	systemOneQuestionsRequired = "systemone.questions_required"
	systemOneStreamUnsupported = "systemone.stream_unsupported"
)

// systemOneInferenceShape is the shape handleSystemOne hands the ONE preflight
// gate (inferencePreflight). It is a function so the values the handler uses are
// the ones its test pins: the exact coarse flavor (upstreamPath matches it by
// string equality), the System One endpoint, and the decision capability. The
// capability list does two jobs: it feeds the capability gate (a model without a
// (decision, yes) verdict is refused, an unprobed one included), and it keeps the
// request off the vendor-account path, which declines a request only because it
// carries a capability list while a vendor model's match reads the model name and
// never the flavor.
func systemOneInferenceShape(model string) inferenceShape {
	return inferenceShape{
		apiFlavor:            apiFlavorSystemOne,
		endpoint:             endpointSystemOne,
		model:                model,
		requiredCapabilities: []string{routing.CapabilityDecision},
	}
}

// handleSystemOne serves POST /v1/systemone: a System One decision request
// ({model, state, questions}) relayed to a decision-capable model and answered
// with the upstream's own body ({model, answers, usage}). Bearer only (gateway:use
// or llm:invoke): a browser session cookie and the internal loopback are no way in.
func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
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
	// The model read here is the client's gateway model name: purely OUR routing
	// input, rewritten to the application's own model id by proxyNative.
	model, _ := sniffRoutingModel(raw)
	if err := validateSystemOneRequest(raw); err != nil {
		writeRequestError(w, err)
		return
	}
	token, ok = s.runAsPrincipal(w, r, token)
	if !ok {
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, systemOneInferenceShape(model))
	if handled {
		return
	}
	s.relaySystemOne(w, r, token, pf.Req, raw)
}

// validateSystemOneRequest validates the shape this handler owns (the chat
// validator requires messages): a non-blank string "model", a non-empty JSON
// object "questions", and no streaming (the protocol has none: "stream" must be
// absent, null or false). state and the questions' contents are the upstream's
// business and reach it unexamined. Fields are decoded as json.RawMessage and
// then type-checked, so a wrongly typed field is not silently zeroed.
//
// The result is the concrete *inference.Error, nil for a valid request, so a
// caller compares it to nil as returned (no typed-nil-in-an-interface trap).
func validateSystemOneRequest(raw []byte) *inference.Error {
	var body struct {
		Model     json.RawMessage `json:"model"`
		Questions json.RawMessage `json:"questions"`
		Stream    json.RawMessage `json:"stream"`
	}
	_ = json.Unmarshal(raw, &body)
	var model string
	if json.Unmarshal(body.Model, &model) != nil || strings.TrimSpace(model) == "" {
		return &inference.Error{Code: systemOneModelRequired, Message: "model is required and must be a non-empty string"}
	}
	var questions map[string]json.RawMessage
	if json.Unmarshal(body.Questions, &questions) != nil || len(questions) == 0 {
		return &inference.Error{Code: systemOneQuestionsRequired, Message: "questions is required and must be a non-empty object"}
	}
	if len(body.Stream) > 0 && string(body.Stream) != "null" {
		var stream bool
		if json.Unmarshal(body.Stream, &stream) != nil || stream {
			return &inference.Error{Code: systemOneStreamUnsupported, Message: "System One does not stream; leave stream out or set it to false"}
		}
	}
	return nil
}

// decisionsCall is the request-scoped identity of a decision request (System
// One or OpenAI Decisions) between its preflight and its relay: what every
// refusal before the upstream call needs to answer and record it. token is
// refreshed by the resolve (resolveTarget updates the last-used-model snapshot),
// which is why resolveDecisionsTarget takes the call by pointer.
type decisionsCall struct {
	token    auth.Token
	req      inference.Request
	raw      []byte
	start    time.Time
	endpoint sessionEndpoint
}

// recordDecisionsRoutingFailure answers and records a decision request refused
// before any upstream call by a routing error (routing, capability or
// effective-flavor refusal), the decisions twin of recordSpeechRoutingFailure.
func (s *Server) recordDecisionsRoutingFailure(w http.ResponseWriter, r *http.Request, c decisionsCall, err error) {
	status := completionHTTPStatus(err)
	code := completionErrorCode(err)
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
		slog.Warn("decisions request admission rejected", "path", r.URL.Path, "api_flavor", c.req.APIFlavor, "model", c.req.Model, "code", code, "status", status)
	} else {
		slog.Debug("decisions request rejected: routing failed", "path", r.URL.Path, "api_flavor", c.req.APIFlavor, "model", c.req.Model, "code", code, "status", status, "err", err)
	}
	body := writeCompletionErrorCaptured(w, err)
	s.recordDecisionsRefusal(w, r, c, status, code, body)
}

// refuseDecisionsRequest answers a decision request the gateway itself refuses
// before any upstream call -- an endpoint its effective mode disables, a body
// the translator cannot express -- with status and an error object carrying
// code and message, and records it like recordDecisionsRoutingFailure.
func (s *Server) refuseDecisionsRequest(w http.ResponseWriter, r *http.Request, c decisionsCall, status int, code, message string) {
	body := writeJSONCaptured(w, status, apierror.Response(code, message, ""))
	s.recordDecisionsRefusal(w, r, c, status, code, body)
}

// recordDecisionsRefusal records the usage row of a refused decision request.
// The row carries the endpoint and no host or route: no server saw the request
// (even a refusal that comes after the resolve is recorded so). The billing
// unit stays the default (tokens), the endpoints' unit.
func (s *Server) recordDecisionsRefusal(w http.ResponseWriter, r *http.Request, c decisionsCall, status int, code string, body []byte) {
	capture := buildCaptureInput(s.capturingEnabled(c.token), c.token.UserID, c.token.Secret, r, c.raw, w.Header(), body, status, c.req.APIFlavor)
	s.recordUsage(c.start, c.token, c.req, routing.Target{}, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, Endpoint: c.endpoint}, nextRequestID(), capture, nil)
}

// resolveDecisionsTarget resolves the routing target for an already-gated
// decision request and applies the effective-served conjunct's flavor half to
// it, as resolveSpeechTarget does: for a server_agent mapping the resolved
// runtime spec is the authority for its model's flavors, and the relay reaches
// proxyNative directly. The mode half is each endpoint's own (System One's
// SystemOneMode in relaySystemOne, the decisions route in decisionsRoute). On a
// refusal it has already answered and recorded, and returns ok == false.
func (s *Server) resolveDecisionsTarget(w http.ResponseWriter, r *http.Request, c *decisionsCall) (routing.Target, bool) {
	target, err := s.resolveTarget(r.Context(), &c.token, c.req)
	if err != nil {
		s.recordDecisionsRoutingFailure(w, r, *c, err)
		return routing.Target{}, false
	}
	if !targetServesFlavor(target, c.req.APIFlavor) {
		slog.Debug("decisions request rejected: resolved target's effective flavors exclude decisions",
			"path", r.URL.Path, "api_flavor", c.req.APIFlavor, "model", c.req.Model,
			"server", s.serverName(target.ServerID), "route_id", target.RouteID)
		s.recordDecisionsRoutingFailure(w, r, *c, routing.ErrNoModelRoute)
		return routing.Target{}, false
	}
	return target, true
}

// relaySystemOne relays an already-gated System One request via proxyNative with
// no translate fallback: every resolve failure is terminal and recorded directly.
// req is pf.Req, already carrying RequiredCapabilities = [decision]. A resolved
// target whose effective System One mode is disabled (a server_agent spec can
// disable it per model; an ordinary application is no candidate then) is
// refused 404 systemone.endpoint_disabled. proxyNative rewrites the model to the
// mapping's own name and relays the upstream's status and body verbatim; usage
// is read from the body's usage object (mergePassthroughUsage), and a body
// without one records zero tokens. A token that opted into confidence
// normalization gets the buffered relay instead, which rewrites the confidence
// of a 2xx answer (ADR-098) and relays everything else as the copier would.
func (s *Server) relaySystemOne(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	c := decisionsCall{token: token, req: req, raw: raw, start: time.Now(), endpoint: endpointSystemOne}
	target, ok := s.resolveDecisionsTarget(w, r, &c)
	if !ok {
		return
	}
	if target.SystemOneMode == routing.EndpointModeDisabled {
		slog.Debug("systemone request rejected: endpoint disabled", "path", r.URL.Path, "model", c.req.Model, "server", s.serverName(target.ServerID), "route_id", target.RouteID)
		s.refuseDecisionsRequest(w, r, c, statusEndpointDisabled, codeSystemOneEndpointDisabled, msgEndpointDisabled)
		return
	}
	rel := nativeRelay{
		token:    c.token,
		target:   target,
		path:     upstreamPath(target, apiFlavorSystemOne, endpointSystemOne),
		raw:      raw,
		pfReq:    c.req,
		endpoint: endpointSystemOne,
		reselect: relayAgain(c.req, func(again inference.Request) { s.relaySystemOne(w, r, c.token, again, raw) }),
	}
	if c.token.DecisionsConfidenceNormalize {
		rel.decisionsRewrite = &decisionsRewrite{normalize: true, dialect: compat.DialectSystemOne, failCode: codeSystemOneUpstreamError}
	}
	s.proxyNative(w, r, rel)
}

// handleSystemOneModels serves GET /v1/systemone/models: the models a System One
// client can name in POST /v1/systemone. It is /v1/models narrowed to the
// decisions flavor and to the rows that carry a (decision, yes) verdict, so a
// model the gate would refuse is never offered. Like /v1/models it is
// bearer-only, reachable to a service token (llm:invoke), and lists the
// portal's caller-visible rows (provisioning visibility, groups and aliases
// included); data is [] rather than null when nothing qualifies.
func (s *Server) handleSystemOneModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	models := modelDTOsOnFlavor(s.Portal.Models(r.Context(), token).Data, apiFlavorSystemOne)
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if !m.Decision {
			continue
		}
		data = append(data, map[string]any{"id": m.ID, "object": "model", "api": "systemone"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
