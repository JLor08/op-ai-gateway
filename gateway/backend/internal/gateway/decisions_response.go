// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
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

// decisionsNotImplementedGuidance is the sentence a translated request's 501
// adds: both vLLM decision endpoints answer 501 for a model without a read
// strategy, which on the translate route means the model has no System One
// read on that upstream.
const decisionsNotImplementedGuidance = "the model does not answer System One decisions on this upstream"

// decisionsRewrite is what relayDecisionsResponse does to a decisions answer
// (nativeRelay.decisionsRewrite; ADR-098):
//
//   - table != nil: the translate route. The upstream answered System One; a 2xx
//     is translated into the OpenAI Decisions shape (clientModel as its model) and,
//     with normalize, its confidences recomputed; a non-2xx becomes the gateway's
//     decisions.upstream_error envelope under the upstream's status.
//   - table == nil: a passthrough the token asked to normalize. A 2xx JSON body
//     in dialect gets its confidences recomputed; anything else -- a non-2xx, a
//     body that is not JSON -- is relayed as the upstream sent it.
//
// failCode is the code of the gateway's own 502 when the answer cannot be
// relayed at all (over the bound, unreadable, untranslatable):
// decisions.upstream_error, or systemone.upstream_error on /v1/systemone.
type decisionsRewrite struct {
	table       *compat.DecisionsAnswerTable
	clientModel string
	normalize   bool
	dialect     compat.DecisionsDialect
	failCode    string
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
// it. The error is a translation failure only.
func (rw *decisionsRewrite) apply(body []byte, upstreamContentType string) (decisionsAnswer, error) {
	if rw.table != nil {
		out, u, err := compat.TranslateSystemOneResponse(body, *rw.table, rw.clientModel)
		if err != nil {
			return decisionsAnswer{}, err
		}
		if rw.normalize {
			out, _ = compat.NormalizeDecisionsConfidence(out, compat.DialectOpenAI)
		}
		return decisionsAnswer{body: out, contentType: jsonContentType, usage: decisionsInferenceUsage(u)}, nil
	}
	// Only normalization buffers a passthrough; it is best effort, so a body
	// that is not JSON goes out unchanged under the upstream's Content-Type.
	out, isJSON := compat.NormalizeDecisionsConfidence(body, rw.dialect)
	contentType := upstreamContentType
	if isJSON {
		contentType = jsonContentType
	}
	return decisionsAnswer{body: out, contentType: contentType, usage: decisionsInferenceUsage(compat.ParseDecisionsUsage(body, rw.dialect))}, nil
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
// a body over the bound or broken off is the gateway's 502 rw.failCode and
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
		s.failDecisionsRelay(w, r, ex, rw, errorCode, "the decisions upstream's response could not be read in full")
		return
	case int64(len(body)) > limit:
		slog.Error("decisions upstream response exceeds the buffer bound; refused instead of truncated", "id", ex.id, "path", r.URL.Path, "model", ex.req.Model, "server", ex.serverName, "limit_bytes", limit)
		s.failDecisionsRelay(w, r, ex, rw, rw.failCode, "the decisions upstream's response is larger than the gateway buffers")
		return
	}
	upstreamContentType := resp.Header.Get(contentTypeHeader)
	if upstreamContentType == "" {
		upstreamContentType = jsonContentType
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		answer := decisionsAnswer{body: body, contentType: upstreamContentType}
		if rw.table != nil {
			answer = decisionsAnswer{body: decisionsUpstreamErrorBody(resp.StatusCode, body), contentType: jsonContentType}
		}
		s.writeDecisionsAnswer(w, r, ex, resp, answer)
		return
	}
	answer, err := rw.apply(body, upstreamContentType)
	if err != nil {
		slog.Warn("decisions upstream answer could not be translated", "path", r.URL.Path, "model", ex.req.Model, "server", ex.serverName, "err", err)
		s.failDecisionsRelay(w, r, ex, rw, rw.failCode, err.Error())
		return
	}
	s.writeDecisionsAnswer(w, r, ex, resp, answer)
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
// gateway's own 502 (rw.failCode, type upstream_error) carrying message, and
// records it as a failed relay with errorCode on the row and no tokens.
func (s *Server) failDecisionsRelay(w http.ResponseWriter, r *http.Request, ex nativeExchange, rw *decisionsRewrite, errorCode, message string) {
	payload := writeJSONCaptured(w, http.StatusBadGateway, apierror.Body{Error: apierror.ErrorBody{
		Code:    rw.failCode,
		Message: message,
		Type:    decisionsUpstreamErrorType,
	}})
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: http.StatusBadGateway, ContentType: jsonContentType, BillingUnit: billingUnitFor(ex.req.APIFlavor), Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), payload, http.StatusBadGateway, ex.req.APIFlavor), nil)
}

// decisionsUpstreamErrorBody is the gateway envelope a translated request's
// non-2xx System One answer becomes: decisions.upstream_error, type
// upstream_error, and a message naming the upstream's status and -- when one of
// the known shapes yields it -- the upstream's own message; a 501 adds
// decisionsNotImplementedGuidance. The status itself is relayed by the caller.
func decisionsUpstreamErrorBody(status int, body []byte) []byte {
	message := fmt.Sprintf("the System One upstream answered %d", status)
	if upstream := decisionsUpstreamMessage(body); upstream != "" {
		message += ": " + upstream
	}
	if status == http.StatusNotImplemented {
		message += "; " + decisionsNotImplementedGuidance
	}
	// Marshal cannot fail on this type (strings only).
	out, _ := json.Marshal(apierror.Body{Error: apierror.ErrorBody{
		Code:    codeDecisionsUpstreamError,
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
