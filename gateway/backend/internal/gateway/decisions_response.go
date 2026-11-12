// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/compat"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"strconv"
	"strings"
)

// decisionsUpstreamErrorType is the OpenAI-client-facing type of the gateway's
// own decisions error envelopes (decisions.upstream_error,
// systemone.upstream_error), the type the transcription twin uses.
const decisionsUpstreamErrorType = "upstream_error"

// decisionsRefusalErrorType is the type of the 422 systemone.refused envelope:
// the upstream answered, and refused to answer a question, which is not an
// upstream failure.
const decisionsRefusalErrorType = "refusal"

// decisionsNotImplementedGuidance is the sentence a translated /v1/decisions
// request's 501 adds: both vLLM decision endpoints answer 501 for a model
// without a read strategy, which on the forward translate route means the model
// has no System One read on that upstream.
const decisionsNotImplementedGuidance = "the model does not answer System One decisions on this upstream"

// systemOneNotImplementedGuidance is the reverse route's twin: a translated
// /v1/systemone request's 501 means the model has no OpenAI Decisions read on
// that upstream (ADR-100).
const systemOneNotImplementedGuidance = "the model does not answer OpenAI Decisions on this upstream"

// decisionsDirection is which translation, if any, a decisions answer takes on
// its way back to the client.
type decisionsDirection int

const (
	// decisionsNormalizeOnly: a passthrough a token asked to normalize; the
	// answer is in the client's own dialect already.
	decisionsNormalizeOnly decisionsDirection = iota
	// decisionsForward: /v1/decisions translated to a System One upstream
	// (ADR-098); its answer is translated System One -> OpenAI Decisions.
	decisionsForward
	// decisionsReverse: /v1/systemone translated to an OpenAI Decisions
	// upstream (ADR-100); its answer is translated OpenAI Decisions -> System
	// One.
	decisionsReverse
)

// upstreamAPI names the API a translated request's upstream speaks, the one
// the gateway's error envelope blames.
func (d decisionsDirection) upstreamAPI() string {
	if d == decisionsReverse {
		return "OpenAI Decisions"
	}
	return "System One"
}

// clientDialect is the dialect a translated route's client speaks, the one its
// answer is translated into.
func (d decisionsDirection) clientDialect() compat.DecisionsDialect {
	if d == decisionsReverse {
		return compat.DialectSystemOne
	}
	return compat.DialectOpenAI
}

// decisionsFailCode is the code of the gateway's own error envelopes for a
// client of dialect: its endpoint's upstream_error, systemone.upstream_error on
// /v1/systemone and decisions.upstream_error on /v1/decisions. It is the one
// source of that code, for a translated route and a normalized passthrough
// alike.
func decisionsFailCode(dialect compat.DecisionsDialect) string {
	if dialect == compat.DialectSystemOne {
		return codeSystemOneUpstreamError
	}
	return codeDecisionsUpstreamError
}

// notImplementedGuidance is the sentence a translated request's 501 adds.
func (d decisionsDirection) notImplementedGuidance() string {
	if d == decisionsReverse {
		return systemOneNotImplementedGuidance
	}
	return decisionsNotImplementedGuidance
}

// decisionsRewrite is what relayDecisionsResponse does to a decisions answer
// (nativeRelay.decisionsRewrite; ADR-098, ADR-100):
//
//   - direction decisionsForward or decisionsReverse: a translate route. A 2xx
//     is translated into the client's dialect (clientModel as its model) and,
//     with normalize, its confidences recomputed in that dialect; a non-2xx
//     becomes the gateway's <endpoint>.upstream_error envelope under the
//     upstream's status. A reverse answer whose upstream refused a question is
//     the gateway's 422 systemone.refused.
//   - decisionsNormalizeOnly: a passthrough the token asked to normalize. A 2xx
//     JSON body in dialect gets its confidences recomputed; anything else -- a
//     non-2xx, a body that is not JSON -- is relayed as the upstream sent it.
//
// dialect is always the CLIENT's dialect, the one the written answer is in;
// it also names the code of the gateway's own 502 when the answer cannot be
// relayed at all (failCode).
type decisionsRewrite struct {
	direction   decisionsDirection
	forward     *compat.DecisionsAnswerTable
	reverse     *compat.SystemOneAnswerTable
	clientModel string
	normalize   bool
	dialect     compat.DecisionsDialect
}

// failCode is the code of the gateway's own 502 when the answer cannot be
// relayed at all (over the bound, unreadable, untranslatable):
// decisionsFailCode of the client's dialect.
func (rw *decisionsRewrite) failCode() string {
	return decisionsFailCode(rw.dialect)
}

// forwardDecisionsRewrite is the rewrite of a /v1/decisions request translated
// to System One.
func forwardDecisionsRewrite(table compat.DecisionsAnswerTable, clientModel string, normalize bool) *decisionsRewrite {
	return &decisionsRewrite{direction: decisionsForward, forward: &table, clientModel: clientModel, normalize: normalize, dialect: decisionsForward.clientDialect()}
}

// reverseDecisionsRewrite is the rewrite of a /v1/systemone request translated
// to OpenAI Decisions.
func reverseDecisionsRewrite(table compat.SystemOneAnswerTable, clientModel string, normalize bool) *decisionsRewrite {
	return &decisionsRewrite{direction: decisionsReverse, reverse: &table, clientModel: clientModel, normalize: normalize, dialect: decisionsReverse.clientDialect()}
}

// normalizeDecisionsRewrite is the rewrite of a passthrough answer in dialect
// for a token that opted into confidence normalization.
func normalizeDecisionsRewrite(dialect compat.DecisionsDialect) *decisionsRewrite {
	return &decisionsRewrite{direction: decisionsNormalizeOnly, normalize: true, dialect: dialect}
}

// translates reports whether the answer changes dialect on its way back.
func (rw *decisionsRewrite) translates() bool {
	return rw.direction != decisionsNormalizeOnly
}

// decisionsAnswer is a rewritten 2xx answer: the client's bytes, their
// Content-Type and the usage they carry.
type decisionsAnswer struct {
	body        []byte
	contentType string
	usage       inference.Usage
}

// apply rewrites a 2xx upstream body. upstreamContentType is the upstream's
// own (or JSON when it named none); a passthrough body that is not JSON keeps
// it. The error is a translation failure only (a reverse refusal included).
func (rw *decisionsRewrite) apply(body []byte, upstreamContentType string) (decisionsAnswer, error) {
	if rw.translates() {
		out, u, err := rw.translate(body)
		if err != nil {
			return decisionsAnswer{}, err
		}
		if rw.normalize {
			out, _ = compat.NormalizeDecisionsConfidence(out, rw.dialect)
		}
		return decisionsAnswer{body: out, contentType: jsonContentType, usage: decisionsInferenceUsage(u)}, nil
	}
	// Only normalization buffers a passthrough; it is best effort, so a body
	// that is not JSON goes out unchanged under the upstream's Content-Type.
	// A body that was rewritten is the gateway's JSON; one left byte for byte
	// keeps the upstream's Content-Type.
	out, isJSON := compat.NormalizeDecisionsConfidence(body, rw.dialect)
	contentType := upstreamContentType
	if isJSON && !bytes.Equal(out, body) {
		contentType = jsonContentType
	}
	return decisionsAnswer{body: out, contentType: contentType, usage: decisionsInferenceUsage(compat.ParseDecisionsUsage(body, rw.dialect))}, nil
}

// translate turns a translated route's 2xx upstream body into the client's
// dialect. The usage is the UPSTREAM body's: the reverse translator reads it
// with compat.ParseDecisionsUsage(body, DialectOpenAI), so the cached and
// cache-write tokens an OpenAI Decisions answer reports reach the row.
func (rw *decisionsRewrite) translate(body []byte) ([]byte, compat.DecisionsUsage, error) {
	if rw.direction == decisionsReverse {
		return compat.TranslateOpenAIDecisionsResponse(body, *rw.reverse, rw.clientModel)
	}
	return compat.TranslateSystemOneResponse(body, *rw.forward, rw.clientModel)
}

// decisionsInferenceUsage is u in the gateway's usage type: input_tokens
// includes both cache subsets (OpenAI semantics, which recordUsage splits).
func decisionsInferenceUsage(u compat.DecisionsUsage) inference.Usage {
	return inference.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
		CachedTokens:     u.CachedTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

// relayDecisionsResponse finishes a decisions request whose answer must be
// rewritten (rw; see decisionsRewrite). Like relayTranscriptionResponse it reads,
// decides, then writes: the body is read up to s.decisionsBodyLimit() through
// io.LimitReader(limit+1), so exceeding the bound is detected rather than cut;
// a body over the bound or broken off is the gateway's 502 rw.failCode() and
// nothing of it is written (failDecisionsRelay). Otherwise the answer is
// rewritten (2xx) or relayed (non-2xx) as decisionsRewrite describes, written
// with its Content-Length, and recorded once, the capture holding the bytes the
// client got. The memory exposure is that bound per in-flight request, paid
// only by translated requests and tokens that opted into normalization.
func (s *Server) relayDecisionsResponse(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse, rw *decisionsRewrite) {
	limit := s.decisionsBodyLimit()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case readErr != nil:
		_, errorCode := s.nativeTerminalStatus(r, resp.StatusCode, ex.req, ex.serverName, false, readErr, ex.start)
		s.failDecisionsRelay(w, r, ex, resp, rw, errorCode, "the decisions upstream's response could not be read in full")
		return
	case int64(len(body)) > limit:
		slog.Error("decisions upstream response exceeds the buffer bound; refused instead of truncated", "id", ex.id, "path", r.URL.Path, "model", ex.req.Model, "server", ex.serverName, "limit_bytes", limit)
		s.failDecisionsRelay(w, r, ex, resp, rw, rw.failCode(), "the decisions upstream's response is larger than the gateway buffers")
		return
	}
	upstreamContentType := resp.Header.Get(contentTypeHeader)
	if upstreamContentType == "" {
		upstreamContentType = jsonContentType
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		answer := decisionsAnswer{body: body, contentType: upstreamContentType}
		if rw.translates() {
			answer = decisionsAnswer{body: decisionsUpstreamErrorBody(rw.direction, resp.StatusCode, body), contentType: jsonContentType}
		}
		s.writeDecisionsAnswer(w, r, ex, resp, answer)
		return
	}
	answer, err := rw.apply(body, upstreamContentType)
	if err != nil {
		s.failDecisionsTranslation(w, r, ex, resp, rw, body, err)
		return
	}
	s.writeDecisionsAnswer(w, r, ex, resp, answer)
}

// failDecisionsTranslation answers a 2xx upstream answer the translator could
// not express in the client's dialect. A reverse answer whose upstream refused
// one or more questions is the gateway's 422 systemone.refused naming their
// keys, recorded with the tokens the upstream reports (it answered, and is paid
// for); anything else is the gateway's 502 rw.failCode(). Either row carries
// resp's headers, so a vendor account's rate-limit scrape still runs.
func (s *Server) failDecisionsTranslation(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse, rw *decisionsRewrite, body []byte, err error) {
	if rw.direction == decisionsReverse && errors.Is(err, compat.ErrSystemOneRefused) {
		slog.Debug("decisions upstream refused to answer", "path", r.URL.Path, "model", ex.req.Model, "server", ex.serverName, "err", err)
		usage := decisionsInferenceUsage(compat.ParseDecisionsUsage(body, compat.DialectOpenAI))
		s.writeDecisionsGatewayError(w, r, ex, resp.Header, decisionsGatewayError{
			status:    http.StatusUnprocessableEntity,
			code:      codeSystemOneRefused,
			errorCode: codeSystemOneRefused,
			message:   err.Error(),
			usage:     usage,
		})
		return
	}
	slog.Warn("decisions upstream answer could not be translated", "path", r.URL.Path, "model", ex.req.Model, "server", ex.serverName, "err", err)
	s.failDecisionsRelay(w, r, ex, resp, rw, rw.failCode(), err.Error())
}

// writeDecisionsAnswer writes answer under the upstream's status and records
// the request: success for a 2xx written whole, upstream.<status> for a
// non-2xx, the client-gone or copy-error class when the write failed (the same
// classification the streaming relay gives).
func (s *Server) writeDecisionsAnswer(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse, answer decisionsAnswer) {
	w.Header().Set(contentTypeHeader, answer.contentType)
	w.Header().Set(contentLengthHeader, strconv.Itoa(len(answer.body)))
	w.WriteHeader(resp.StatusCode)
	written, writeErr := w.Write(answer.body)

	status, errorCode := s.nativeTerminalStatus(r, resp.StatusCode, ex.req, ex.serverName, false, writeErr, ex.start)
	meta := usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: answer.contentType, BillingUnit: billingUnitFor(ex.req.APIFlavor), Endpoint: ex.endpoint}
	capIn := buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), answer.body, resp.StatusCode, ex.req.APIFlavor)
	if capIn != nil {
		capIn.RespBytes = int64(written)
	}
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{Usage: answer.usage}, errorCode, status, meta, ex.id, capIn, resp.Header)
}

// failDecisionsRelay answers a decisions request whose upstream answer cannot be
// relayed -- over the buffer bound, broken off, or untranslatable -- with the
// gateway's own 502 (rw.failCode(), type upstream_error) carrying message, and
// records it as a failed relay with errorCode on the row, no tokens and resp's
// headers (the upstream did answer).
func (s *Server) failDecisionsRelay(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse, rw *decisionsRewrite, errorCode, message string) {
	s.writeDecisionsGatewayError(w, r, ex, resp.Header, decisionsGatewayError{
		status:    http.StatusBadGateway,
		code:      rw.failCode(),
		errorCode: errorCode,
		message:   message,
	})
}

// decisionsGatewayError is the gateway's own error answer to a relayed
// decisions request: the HTTP status, the envelope's code and message, and what
// the usage row records -- errorCode and the tokens (zero when the upstream
// reported none).
type decisionsGatewayError struct {
	status    int
	code      string
	errorCode string
	message   string
	usage     inference.Usage
}

// writeDecisionsGatewayError answers a relayed decisions request with the
// gateway's own error envelope (code, type upstream_error -- refusal for
// systemone.refused -- and message) under
// e.status, and records it as a failed relay with e.errorCode and e.usage on
// the row. upstreamHeaders are the upstream answer's headers, which recordUsage
// scrapes for a vendor account's rate limits.
func (s *Server) writeDecisionsGatewayError(w http.ResponseWriter, r *http.Request, ex nativeExchange, upstreamHeaders http.Header, e decisionsGatewayError) {
	status := e.status
	errType := decisionsUpstreamErrorType
	if e.code == codeSystemOneRefused {
		errType = decisionsRefusalErrorType
	}
	payload := writeJSONCaptured(w, status, apierror.Body{Error: apierror.ErrorBody{
		Code:    e.code,
		Message: e.message,
		Type:    errType,
	}})
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{Usage: e.usage}, e.errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, BillingUnit: billingUnitFor(ex.req.APIFlavor), Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), payload, status, ex.req.APIFlavor), upstreamHeaders)
}

// decisionsUpstreamErrorBody is the gateway envelope a translated request's
// non-2xx answer becomes: decisionsFailCode of dir's client dialect
// (decisions.upstream_error forward, systemone.upstream_error reverse), type
// upstream_error, and a message naming
// the upstream dir translates to, its status and -- when one of the known
// shapes yields it -- the upstream's own message; a 501 adds dir's
// notImplementedGuidance. The status itself is relayed by the caller.
func decisionsUpstreamErrorBody(dir decisionsDirection, status int, body []byte) []byte {
	message := fmt.Sprintf("the %s upstream answered %d", dir.upstreamAPI(), status)
	if upstream := decisionsUpstreamMessage(body); upstream != "" {
		message += ": " + upstream
	}
	if status == http.StatusNotImplemented {
		message += "; " + dir.notImplementedGuidance()
	}
	// Marshal cannot fail on this type (strings only).
	out, _ := json.Marshal(apierror.Body{Error: apierror.ErrorBody{
		Code:    decisionsFailCode(dir.clientDialect()),
		Message: message,
		Type:    decisionsUpstreamErrorType,
	}})
	return out
}

// decisionsUpstreamMessage extracts an upstream error's own message from the
// shapes a decisions upstream answers with: an OpenAI error object
// ({"error":{"message":...}}, vLLM's ErrorResponse), or FastAPI's detail -- a
// string ({"detail":"..."}) or a validation list ({"detail":[{"msg":...}]},
// joined with "; "). Anything else yields "".
func decisionsUpstreamMessage(body []byte) string {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return ""
	}
	var errObj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(top["error"], &errObj) == nil {
		if m := strings.TrimSpace(errObj.Message); m != "" {
			return m
		}
	}
	return decisionsDetailMessage(top["detail"])
}

// decisionsDetailMessage reads FastAPI's detail: a string, or a list of
// objects whose msg fields are joined with "; ".
func decisionsDetailMessage(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var list []struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return ""
	}
	msgs := make([]string, 0, len(list))
	for _, d := range list {
		if m := strings.TrimSpace(d.Msg); m != "" {
			msgs = append(msgs, m)
		}
	}
	return strings.Join(msgs, "; ")
}
