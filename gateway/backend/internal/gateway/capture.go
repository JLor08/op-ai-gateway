// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/store"
	"strings"
	"time"
)

// captureInput carries everything recordUsage needs to persist a capture of
// one request/response exchange. complete / completeStream build it only when
// capturing is enabled; a nil *captureInput means "no capture". HTTPStatus,
// APIFlavor, and OwnerUserID describe the exchange but are NOT stored in the
// gzipped envelope itself — persistCapture (SP-C+ P4) copies them onto
// store.Capture instead, because persistCapture runs detached with no token in
// scope, and MemoryCaptureStore has no usage_events JOIN to resolve them from.
type captureInput struct {
	ReqHeaders  http.Header
	RawReq      []byte
	RespHeaders http.Header
	RespBody    []byte
	HTTPStatus  int
	APIFlavor   string
	OwnerUserID string
	Secret      bool
	// Translated* hold the TRANSLATED upstream exchange (the request the gateway
	// actually sent to the Chat-Completions upstream + the raw upstream response),
	// populated only on the translate path when capturing. Empty on native
	// passthrough (its client bytes already equal the upstream bytes) and on plain
	// same-protocol requests. Request headers are redacted like the client ones.
	TranslatedReqHeaders  http.Header
	TranslatedReqBody     []byte
	TranslatedRespHeaders http.Header
	TranslatedRespBody    []byte
}

// attachTranslatedCapture copies the sink's collected upstream (translated)
// request+response onto ci, for the translate path. Nil-safe: a nil ci or a nil
// sink leaves ci unchanged. Native passthrough passes no sink because the bytes it
// already captures ARE the upstream bytes.
func attachTranslatedCapture(ci *captureInput, sink *provider.CaptureSink) {
	if ci == nil || sink == nil {
		return
	}
	ci.TranslatedReqHeaders = sink.RequestHeaders()
	ci.TranslatedReqBody = sink.RequestBody()
	ci.TranslatedRespHeaders = sink.ResponseHeaders()
	ci.TranslatedRespBody = sink.ResponseBody()
}

// capturingEnabled reports whether this request should be captured: the
// global switch is on, the token opted in OR the capture_override system
// setting forces capture on for all requests, AND a store is wired. Unlike
// SP-C, a nil Cipher does NOT gate this off (SP-C+ P4) — it is instead the
// signal persistCapture uses to fall back to the volatile RAM path (KeyVersion
// 0, plain gzip) instead of sealing (KeyVersion capture.KeyVersion).
func (s *Server) capturingEnabled(token auth.Token) bool {
	return s.captureEnabled() && (token.LogCommunication || s.captureOverride()) && s.Captures != nil
}

// captureEnabled reports whether new captures are globally allowed right now.
// Returns true when ServerDeps.CaptureEnabled was never set (default-on), so
// every existing caller/test that builds a bare *Server keeps capturing.
func (s *Server) captureEnabled() bool {
	if s.CaptureEnabled == nil {
		return true
	}
	return s.CaptureEnabled()
}

// captureOverride reports whether capture is forced on for all requests
// regardless of the per-token log_communication flag. Nil hook => false, so a
// bare *Server preserves the opt-in-only behavior.
func (s *Server) captureOverride() bool {
	if s.CaptureOverride == nil {
		return false
	}
	return s.CaptureOverride()
}

// redactedMarker replaces the value of a sensitive request header while the key
// is kept. It stores the marker "[redacted]"; the CaptureDialog renders this
// stored value directly (no client-side synthesis).
const redactedMarker = "[redacted]"

// redactedCaptureHeaders lists request headers whose VALUE must be replaced with
// redactedMarker before a capture is stored (token secret, session cookie, CSRF,
// run-as token). Compared case-insensitively because net/http canonicalizes
// header names (auth.go:18-20).
var redactedCaptureHeaders = map[string]struct{}{
	"authorization":     {},
	"cookie":            {},
	"x-op-csrf":         {},
	"x-op-run-as-token": {},
}

// redactCaptureHeaders copies h, replacing the sensitive request-header VALUES with
// redactedMarker while keeping the header key.
func redactCaptureHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, values := range h {
		if _, redacted := redactedCaptureHeaders[strings.ToLower(name)]; redacted {
			out[name] = []string{redactedMarker}
			continue
		}
		vs := make([]string, len(values))
		copy(vs, values)
		out[name] = vs
	}
	return out
}

// cloneHeader copies h into a plain map. Response headers are stored verbatim; only
// request headers are redacted.
func cloneHeader(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, values := range h {
		vs := make([]string, len(values))
		copy(vs, values)
		out[name] = vs
	}
	return out
}

// captureEnvelope is the plaintext JSON that is gzipped then AES-GCM sealed. It is
// exactly five fields; http_status/api_flavor are sourced by the detail endpoint
// from the joined usage_events row, not from here.
type captureEnvelope struct {
	ReqHeaders  map[string][]string `json:"req_headers"`
	ReqBody     string              `json:"req_body"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RespBody    string              `json:"resp_body"`
	Truncated   bool                `json:"truncated"`
	// Translated* are present only when the built-in translation ran (else omitted,
	// so old captures and native-passthrough/same-protocol captures decode with them
	// empty). They carry the upstream request the gateway sent + the raw upstream
	// response, letting the capture view show both the client and the translated
	// communication.
	TranslatedReqHeaders  map[string][]string `json:"translated_req_headers,omitempty"`
	TranslatedReqBody     string              `json:"translated_req_body,omitempty"`
	TranslatedRespHeaders map[string][]string `json:"translated_resp_headers,omitempty"`
	TranslatedRespBody    string              `json:"translated_resp_body,omitempty"`
}

// capBody clips b to maxBytes, reporting whether it was truncated. maxBytes <= 0
// disables the cap.
func capBody(b []byte, maxBytes int) ([]byte, bool) {
	if maxBytes > 0 && len(b) > maxBytes {
		return b[:maxBytes], true
	}
	return b, false
}

// buildCaptureEnvelope redacts request headers and clips both bodies to maxBytes.
// When the translate path collected the upstream exchange, its request headers are
// redacted (an upstream API key never leaks), its response headers stored verbatim,
// and both translated bodies clipped to maxBytes.
func buildCaptureEnvelope(in *captureInput, maxBytes int) captureEnvelope {
	reqBody, reqTrunc := capBody(in.RawReq, maxBytes)
	respBody, respTrunc := capBody(in.RespBody, maxBytes)
	env := captureEnvelope{
		ReqHeaders:  redactCaptureHeaders(in.ReqHeaders),
		ReqBody:     string(reqBody),
		RespHeaders: cloneHeader(in.RespHeaders),
		RespBody:    string(respBody),
		Truncated:   reqTrunc || respTrunc,
	}
	if len(in.TranslatedReqBody) > 0 || len(in.TranslatedReqHeaders) > 0 || len(in.TranslatedRespBody) > 0 || len(in.TranslatedRespHeaders) > 0 {
		tReq, tReqTrunc := capBody(in.TranslatedReqBody, maxBytes)
		tResp, tRespTrunc := capBody(in.TranslatedRespBody, maxBytes)
		env.TranslatedReqHeaders = redactCaptureHeaders(in.TranslatedReqHeaders)
		env.TranslatedReqBody = string(tReq)
		env.TranslatedRespHeaders = cloneHeader(in.TranslatedRespHeaders)
		env.TranslatedRespBody = string(tResp)
		env.Truncated = env.Truncated || tReqTrunc || tRespTrunc
	}
	return env
}

// persistCapture builds, compresses, and stores the capture for id, sealing
// it when a cipher is configured (KeyVersion capture.KeyVersion) or, in RAM
// fallback mode (SP-C+ P4, s.Cipher == nil), storing plain gzip (KeyVersion
// 0). It runs on a DETACHED context (context.Background) because recordUsage
// runs after the response is written, when r.Context may already be canceled
// (client-gone / long streams). All errors are logged and discarded — a
// capture failure must never fail the request (mirrors usage.Record).
func (s *Server) persistCapture(id string, in *captureInput) {
	plain, err := json.Marshal(buildCaptureEnvelope(in, s.captureMaxBytes))
	if err != nil {
		log.Printf("capture: marshal envelope for %s: %v", id, err)
		return
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(plain); err != nil {
		log.Printf("capture: gzip for %s: %v", id, err)
		return
	}
	if err := zw.Close(); err != nil {
		log.Printf("capture: gzip close for %s: %v", id, err)
		return
	}
	rec := store.Capture{
		UsageEventID: id,
		OwnerUserID:  in.OwnerUserID,
		APIFlavor:    in.APIFlavor,
		HTTPStatus:   in.HTTPStatus,
		Secret:       in.Secret,
		CreatedAt:    time.Now().UTC(),
	}
	if s.Cipher != nil {
		rec.KeyVersion = capture.KeyVersion
		rec.Blob = s.Cipher.Seal(gz.Bytes())
	} else {
		rec.KeyVersion = 0
		rec.Blob = gz.Bytes()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Captures.SaveCapture(ctx, rec); err != nil {
		log.Printf("capture: save for %s: %v", id, err)
	}
}

// buildCaptureInput assembles a *captureInput when capturing, else nil.
// respBody is the exact bytes written to the client (serialize-once),
// respHeaders is the live client header map at write time. owner is
// token.UserID and secret is token.Secret, both threaded from the same
// principal so persistCapture (which runs detached, with no token in scope)
// can fill store.Capture.OwnerUserID and Secret — the capture inherits the
// principal's secret flag at write time (SP-2b).
func buildCaptureInput(capturing bool, owner string, secret bool, r *http.Request, raw []byte, respHeaders http.Header, respBody []byte, httpStatus int, apiFlavor string) *captureInput {
	if !capturing {
		return nil
	}
	return &captureInput{
		ReqHeaders:  r.Header,
		RawReq:      raw,
		RespHeaders: respHeaders,
		RespBody:    respBody,
		HTTPStatus:  httpStatus,
		APIFlavor:   apiFlavor,
		OwnerUserID: owner,
		Secret:      secret,
	}
}
