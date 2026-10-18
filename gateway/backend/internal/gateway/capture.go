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
	// Endpoint is the client endpoint the exchange arrived on, for the one decision
	// the flavor cannot make: speech and transcription share the openai_audio flavor,
	// and the first answers with one binary WAV while the second answers with JSON
	// (isBinarySpeechResponse). The two 2xx producers that share speech's flavor
	// stamp it: relayTranscriptionResponse for a one-shot transcript, and
	// transcriptionStreamScan.stampCapture for a streamed one. Every other producer
	// leaves it zero (endpointChat), which nothing reads. That is why the one reader carves
	// transcription out of speech instead of requiring endpointSpeech: a speech
	// capture built without an endpoint is still a speech capture, and the way that
	// can go wrong is a binary body kept redacted, not a WAV run through json.Marshal.
	Endpoint sessionEndpoint
	// RespBytes is how many body bytes the relay actually sent the client, and
	// RespMetering the upstream's metering headers. Both are set only by
	// proxyNative's streaming terminal step, and both are read only when the
	// response body is not stored (captureResponseBody): RespBody is a BOUNDED copy
	// of what was relayed, so its length is the size of the clip only while the clip
	// fits the cap, and the client's own response headers carry none of the
	// upstream's metering. RespBytes == 0 means "not counted", and the size then
	// falls back to len(RespBody).
	RespBytes    int64
	RespMetering http.Header
	// Translated* hold the TRANSLATED upstream exchange (the request the gateway
	// actually sent to the Chat-Completions upstream + the raw upstream response),
	// populated only on the translate path when capturing. Empty on native
	// passthrough and on plain same-protocol requests. Request headers are
	// redacted like the client ones.
	//
	// Empty on native passthrough because that path attaches no sink, NOT because
	// its captured bytes are the upstream's. They are not, and have not been since
	// the model rewrite: rewriteModelField re-serializes the whole object whenever
	// a provider-model override applies, which reorders keys and HTML-escapes
	// <>&, and proxyNative may additionally add `timings_per_token` when the
	// operator's Responses live-timings opt-in is on. So a capture on this path
	// shows what the CLIENT sent, which is the right thing to show a tenant and
	// the wrong thing to debug an upstream 400 with — the per-request debug log
	// is what covers the second case.
	TranslatedReqHeaders  http.Header
	TranslatedReqBody     []byte
	TranslatedRespHeaders http.Header
	TranslatedRespBody    []byte
}

// attachTranslatedCapture copies the sink's collected upstream (translated)
// request+response onto ci, for the translate path. Nil-safe: a nil ci or a nil
// sink leaves ci unchanged. Native passthrough passes no sink at all, so nothing
// upstream-side is captured on that path — see the Translated* field doc for why
// that is not the same as the two byte streams being equal.
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
// run-as token, x-api-key -- the header a vendor Anthropic account's upstream
// credential travels in on the translated request -- and x-goog-api-key, the
// native Gemini account's equivalent). The provider's CaptureSink records the
// OUTBOUND request headers, so an unlisted upstream credential header would be
// stored in clear. Compared case-insensitively because net/http canonicalizes
// header names (auth.go:18-20).
var redactedCaptureHeaders = map[string]struct{}{
	"authorization":     {},
	"cookie":            {},
	"x-op-csrf":         {},
	"x-op-run-as-token": {},
	"x-api-key":         {},
	"x-goog-api-key":    {},
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

// captureBinaryBody is the JSON document stored as the response body of a binary
// (audio) response whose bytes the capture does not keep: what the response was,
// how large, and the upstream's metering headers, which are the only thing in it
// the capture viewer can say anything about. The viewer pretty-prints it like any
// JSON body.
type captureBinaryBody struct {
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// Redacted says the content is deliberately absent, as it does on a multipart
	// part (captureMultipartPart): a reader must not take the summary for a body.
	Redacted        bool                `json:"redacted"`
	MeteringHeaders map[string][]string `json:"metering_headers,omitempty"`
}

// isAudioContentType reports whether a Content-Type header value names an audio
// media type (audio/*), in any case and with or without parameters.
func isAudioContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "audio/")
}

// isBinarySpeechResponse reports whether the capture is of a speech request the
// upstream answered 2xx. That response is one audio file by the endpoint's
// contract, so its body must never reach json.Marshal even when its Content-Type
// is missing or wrong. It is narrow on purpose: a speech request answered with an
// error, and every other flavor, are decided by the Content-Type alone.
//
// The flavor alone cannot say "speech": a transcription shares openai_audio, and
// its 2xx answer is a JSON document -- the transcript -- that the capture exists to
// keep. The endpoint carves it out (captureInput.Endpoint). The carve-out is
// negative, as needsSpeechErrorRelay's is, so a speech capture built without an
// endpoint (the zero value is endpointChat) is still redacted: the failure that
// would leave is a transcript lost to a summary, never a WAV in the envelope.
func isBinarySpeechResponse(in *captureInput) bool {
	return in.APIFlavor == apiFlavorSpeech && in.Endpoint != endpointTranscription && in.HTTPStatus >= 200 && in.HTTPStatus < 300
}

// captureResponseBody returns what the envelope stores as the response body, and
// whether that was cut short. For an audio response it is the summary of
// captureBinaryBody; for everything else it is the body itself clipped to
// maxBytes, exactly as before.
//
// An audio body is binary: it is stored as a string that json.Marshal then rewrites
// to valid UTF-8 (every invalid byte becomes U+FFFD) and the cap clips mid-file, so
// what a capture held would be a corrupted fragment of a clip, and its megabyte
// would bloat every capture for nothing the viewer can show.
//
// The gate is the response's OWN Content-Type, the one the client was sent: a
// speech request whose upstream answered an error (application/json) is stored like
// any other JSON body, and the same holds for anything else that is not audio.
// There is one backstop behind it, isBinarySpeechResponse: a speech request that was
// answered 2xx got audio whatever its Content-Type says (proxyNative defaults a
// missing one to application/json), so it is skipped too. A transcription is not
// one: it shares the flavor but answers with a JSON transcript, which is stored like
// any other body. Nothing is cut off by the skip, so truncated is false.
func captureResponseBody(in *captureInput, maxBytes int) (string, bool) {
	contentType := in.RespHeaders.Get("Content-Type")
	if !isAudioContentType(contentType) && !isBinarySpeechResponse(in) {
		body, truncated := capBody(in.RespBody, maxBytes)
		return string(body), truncated
	}
	size := in.RespBytes
	if size == 0 {
		size = int64(len(in.RespBody))
	}
	doc := captureBinaryBody{ContentType: strings.TrimSpace(contentType), Size: size, Redacted: true}
	if len(in.RespMetering) > 0 {
		doc.MeteringHeaders = cloneHeader(in.RespMetering)
	}
	// Marshal cannot fail on this type (strings, an int64, a bool and a map of
	// string slices), so there is no failure to report.
	summary, _ := json.Marshal(doc)
	return string(summary), false
}

// buildCaptureEnvelope redacts request headers and clips both bodies to maxBytes.
// When the translate path collected the upstream exchange, its request headers are
// redacted (an upstream API key never leaks), its response headers stored verbatim,
// and both translated bodies clipped to maxBytes.
//
// A multipart/form-data REQUEST body (an image edit) is not stored as bytes: it is
// binary, a body is stored as a string that json.Marshal then rewrites to valid
// UTF-8 (every invalid byte becomes U+FFFD) and clips mid-file, and the image and
// mask parts have no business in a payload capture anyway. captureRequestBody
// stores its text fields and each part's metadata and redacts the rest.
//
// An audio RESPONSE body (a speech relay's WAV) is skipped for the same reasons,
// on the response side: captureResponseBody stores a metadata summary in its place.
func buildCaptureEnvelope(in *captureInput, maxBytes int) captureEnvelope {
	reqBody, reqTrunc := captureRequestBody(in, maxBytes)
	respBody, respTrunc := captureResponseBody(in, maxBytes)
	env := captureEnvelope{
		ReqHeaders:  redactCaptureHeaders(in.ReqHeaders),
		ReqBody:     reqBody,
		RespHeaders: cloneHeader(in.RespHeaders),
		RespBody:    respBody,
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
