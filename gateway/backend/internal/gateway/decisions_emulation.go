// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/compat"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
)

// The emulated decision route (#380): a decision request the resolver sent to an
// opted-in OpenAI subscription account (Target.DecisionEmulated) is answered by
// its chat model. The OpenAI Decisions body becomes one buffered Responses call
// with a strict JSON schema (compat.BuildDecisionEmulation), the model's JSON
// text becomes the OpenAI Decisions answer (compat.ParseDecisionEmulation), and
// a /v1/systemone request is translated to OpenAI Decisions before and back
// after. Every answer written once the route settled carries
// X-OP-Decision-Emulated: chat, and every usage row the mark (recordUsage reads
// it off the target).

// markDecisionEmulated sets the emulation header on w. It is called before the
// first write of every answer on the emulated route, success or error.
func markDecisionEmulated(w http.ResponseWriter) {
	w.Header().Set(headerDecisionEmulated, decisionEmulatedChat)
}

// relaySystemOneEmulation is /v1/systemone's emulated route: the System One body
// is translated to OpenAI Decisions (compat.TranslateSystemOneRequest, its
// refusals unchanged and answered 400 here) and emulated, the answer translated
// back with the table.
func (s *Server) relaySystemOneEmulation(w http.ResponseWriter, r *http.Request, c decisionsCall, target routing.Target) {
	markDecisionEmulated(w)
	body, table, derr := compat.TranslateSystemOneRequest(c.raw, effectiveProviderModel(target, c.req.Model))
	if derr != nil {
		slog.Debug("emulated systemone request refused by the translator", "path", r.URL.Path, "model", c.req.Model, "code", derr.Code, "err", derr.Message)
		s.refuseDecisionsRequest(w, r, c, target, http.StatusBadRequest, derr.Code, derr.Message)
		return
	}
	s.relayDecisionEmulation(w, r, c, target, body, &table)
}

// relayDecisionEmulation answers a decision request on the emulated route.
// openAIBody is the OpenAI Decisions body (the client's on /v1/decisions, the
// translated one on /v1/systemone); systemOne is the reverse table of a
// /v1/systemone request and nil on /v1/decisions.
//
//   - A body the emulator cannot express is its 400 with no provider call.
//   - The call is the provider client's buffered Complete, with the
//     subscription auth context and the capture sink a chat call gets
//     (completeCaptureContext) and the target's timeout (the Responses client
//     applies it).
//   - A provider error is answered with the chat path's status
//     (completionHTTPStatus) and code (completionErrorCode); the generic
//     provider.unavailable becomes the endpoint's upstream_error. The row keeps
//     the chat path's code.
//   - An answer that is not the schema's JSON is the endpoint's 502
//     upstream_error, recorded with the tokens the vendor reported.
//   - Otherwise the answer (reverse-translated for System One, its confidences
//     normalized for a token that opted in) is written 200 as JSON.
func (s *Server) relayDecisionEmulation(w http.ResponseWriter, r *http.Request, c decisionsCall, target routing.Target, openAIBody []byte, systemOne *compat.SystemOneAnswerTable) {
	markDecisionEmulated(w)
	dialect := compat.DialectOpenAI
	if systemOne != nil {
		dialect = compat.DialectSystemOne
	}
	emuReq, table, derr := compat.BuildDecisionEmulation(openAIBody, effectiveProviderModel(target, c.req.Model))
	if derr != nil {
		slog.Debug("emulated decisions request refused by the emulator", "path", r.URL.Path, "model", c.req.Model, "code", derr.Code, "err", derr.Message)
		s.refuseDecisionsRequest(w, r, c, target, http.StatusBadRequest, derr.Code, derr.Message)
		return
	}
	emuReq.APIFlavor = c.req.APIFlavor

	id := nextRequestID()
	capturing := s.capturingEnabled(c.token)
	s.Active.Add(ActiveRequest{ID: id, UserID: c.token.UserID, TokenID: c.token.ID, TokenName: c.token.Name, ServiceID: c.token.ServiceID, ServiceName: c.token.ServiceName, ServerName: s.serverName(target.ServerID), ServerID: target.ServerID, InstanceKey: target.InstanceKey, Model: c.req.Model, RequestedModel: c.req.RequestedModel, APIFlavor: c.req.APIFlavor, BillingUnit: billingUnitFor(c.req.APIFlavor), ReqPath: r.URL.Path, ProviderPath: upstreamPath(target, c.req.APIFlavor, c.endpoint), ProviderModel: effectiveProviderModel(target, c.req.Model), SessionID: c.req.ClientSessionID, SessionSource: c.req.SessionSource, AgentID: c.req.AgentID, Stream: false, StartedAt: c.start})
	defer s.Active.Remove(id)
	providerReq := s.upstreamRequest(r.Context(), target, emuReq)
	provCtx, sink := s.completeCaptureContext(r.Context(), capturing, target)
	resp, err := s.completeProvider(r.Context(), provCtx, target, providerReq)

	ex := emulationExchange{c: c, target: target, id: id, capturing: capturing, sink: sink}
	if err != nil {
		s.failDecisionEmulationProvider(w, r, ex, dialect, err)
		return
	}
	answer, err := compat.ParseDecisionEmulation(resp.Text, table, c.req.Model, resp.Usage)
	if err == nil && systemOne != nil {
		answer, _, err = compat.TranslateOpenAIDecisionsResponse(answer, *systemOne, c.req.Model)
	}
	if err != nil {
		slog.Warn("emulated decisions answer could not be read", "path", r.URL.Path, "model", c.req.Model, "account", target.VendorAccountID, "err", err)
		code := decisionsFailCode(dialect)
		body := writeJSONCaptured(w, http.StatusBadGateway, apierror.Body{Error: apierror.ErrorBody{
			Code:    code,
			Message: "the emulating chat model's answer could not be read",
			Type:    decisionsUpstreamErrorType,
		}})
		s.recordDecisionEmulation(r, ex, resp, http.StatusBadGateway, "error", code, body, w.Header())
		return
	}
	if c.token.DecisionsConfidenceNormalize {
		answer, _ = compat.NormalizeDecisionsConfidence(answer, dialect)
	}
	w.Header().Set(contentTypeHeader, jsonContentType)
	w.Header().Set(contentLengthHeader, strconv.Itoa(len(answer)))
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(answer)
	status, errorCode := s.nativeTerminalStatus(r, http.StatusOK, c.req, s.serverName(target.ServerID), false, writeErr, c.start)
	s.recordDecisionEmulation(r, ex, resp, http.StatusOK, status, errorCode, answer, w.Header())
}

// emulationExchange is what an emulated call's answer and row need beyond the
// call itself.
type emulationExchange struct {
	c         decisionsCall
	target    routing.Target
	id        string
	capturing bool
	sink      *provider.CaptureSink
}

// failDecisionEmulationProvider answers a provider error on the emulated route:
// the chat path's status and code, the generic provider.unavailable replaced by
// the endpoint's upstream_error (decisionsFailCode of dialect). The row records
// the chat path's code. A vendor group member that answered a limit gets its
// cooldown (noteVendorGroupLimit), so the next request goes to another member;
// this request itself is not retried.
func (s *Server) failDecisionEmulationProvider(w http.ResponseWriter, r *http.Request, ex emulationExchange, dialect compat.DecisionsDialect, err error) {
	if limit, ok := groupLimitError(ex.target, err); ok {
		s.noteVendorGroupLimit(ex.target, limit.Header, limit.RetryAfter)
	}
	status := completionHTTPStatus(err)
	rowCode := completionErrorCode(err)
	envelope := completionErrorResponse(err)
	if rowCode == provider.ErrUnavailable.Error() {
		envelope.Error.Code = decisionsFailCode(dialect)
		envelope.Error.Type = decisionsUpstreamErrorType
	}
	slog.Error("emulated decisions call failed", "path", r.URL.Path, "model", ex.c.req.Model, "account", ex.target.VendorAccountID, "code", rowCode, "err", err)
	body := writeJSONCaptured(w, status, envelope)
	s.recordDecisionEmulation(r, ex, provider.Response{}, status, "error", rowCode, body, w.Header())
}

// recordDecisionEmulation records an emulated call's row: token-metered from the
// Responses usage, on the serving account, provider_path /responses
// (upstreamPath), the emulation mark from the target, and the capture of the
// bytes the client got with the translated exchange the sink holds.
func (s *Server) recordDecisionEmulation(r *http.Request, ex emulationExchange, resp provider.Response, httpStatus int, status, errorCode string, body []byte, respHeader http.Header) {
	c := ex.c
	ci := buildCaptureInput(ex.capturing, c.token.UserID, c.token.Secret, r, c.raw, respHeader, body, httpStatus, c.req.APIFlavor)
	attachTranslatedCapture(ci, ex.sink)
	var upstreamHeaders http.Header
	if ex.sink != nil {
		upstreamHeaders = ex.sink.ResponseHeaders()
	}
	meta := usageMeta{ReqPath: r.URL.Path, HTTPStatus: httpStatus, ContentType: jsonContentType, BillingUnit: billingUnitFor(c.req.APIFlavor), Endpoint: c.endpoint}
	s.recordUsage(c.start, c.token, c.req, ex.target, provider.Response{Usage: emulationRowUsage(resp.Usage)}, errorCode, status, meta, ex.id, ci, upstreamHeaders)
}

// emulationRowUsage is the row's usage of an emulated call: the Responses usage as
// reported (input includes the cached tokens, which recordUsage splits off),
// with a total when the client reported none.
func emulationRowUsage(u inference.Usage) inference.Usage {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}
