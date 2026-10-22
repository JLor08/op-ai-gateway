// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"time"
)

// apiFlavorSpeech is the speech endpoint's (/v1/audio/speech) flavor string, the
// same local alias for the coarse routing.APIFlavorOpenAIAudio that
// apiFlavorImages is for routing.APIFlavorOpenAIImages: NormalizeAPIFlavor folds
// it to itself, so it filters candidacy (only an application declaring
// openai_audio is a speech candidate), labels usage rows, and answers
// upstreamPath. Like images, speech is a single-purpose flavor with no
// EndpointMode and no translate path; what the flavor cannot express -- whether
// a MODEL can synthesize speech -- is the capability gate's job, keyed on
// routing.CapabilitySpeech.
const apiFlavorSpeech = "openai_audio"

// The request-error codes validateSpeechRequest answers with. They are API
// surface: clients branch on them.
const (
	speechModelRequired = "speech.model_required"
	speechInputRequired = "speech.input_required"
	// speechResponseFormatUnsupported is the code for a response_format this relay
	// cannot honor -- every value but "wav" and "pcm" on a one-shot request, or but
	// "pcm" on a stream; see validateSpeechRequest.
	speechResponseFormatUnsupported = "speech.response_format_unsupported"
	// speechStreamUnsupported is the code for a request whose stream fields the relay
	// cannot honor: a stream that is no boolean, a stream_format on a request that is
	// no stream or outside the two audio.cpp emits, or a stream asking for wav; see
	// validateSpeechRequest.
	speechStreamUnsupported = "speech.stream_unsupported"
	// speechPcmUnavailable is the code for a one-shot request that asked for
	// response_format "pcm" whose synthesized clip cannot be served as such: the mapped
	// model's native format is not 16-bit mono PCM. Unlike the codes above it is a
	// POST-synthesis code -- validateSpeechRequest never returns it, because the format
	// of the audio is known only once the upstream has answered -- so the relay raises
	// it after the upstream call, not before.
	speechPcmUnavailable = "speech.pcm_unavailable"
)

// speechInferenceShape is the shape handleOpenAISpeech hands the ONE preflight
// gate (inferencePreflight). It is a function, not an inline literal, so the
// exact values the handler uses are the ones its test pins:
//
//   - apiFlavor is exactly apiFlavorSpeech, never a fine variant: upstreamPath
//     answers by string equality, and the coarse flavor is what candidacy,
//     targetServesFlavor and the usage row key on.
//   - requiredCapabilities is [routing.CapabilitySpeech], which does two jobs.
//     It feeds the capability gate (a model without a (speech, yes) verdict is
//     refused, an unprobed one included). And it keeps a speech request off the
//     vendor-account path: vendorAccountRoutingEligible excludes the images
//     flavor by name but has no openai_audio twin, and declines a request only
//     because it carries a capability list, while a vendor account's model match
//     reads the model name and never the flavor. A handler that dropped the list
//     would skip the gate AND become routable to a vendor model of the same name.
//   - requiredVoice is the voice the body names (sniffSpeechVoice), "" for none. It is
//     a routing PREFERENCE that fails open -- see inference.Request.RequiredVoice -- so
//     unlike the capability list it never refuses a request on its own.
func speechInferenceShape(model, voice string) inferenceShape {
	return inferenceShape{
		apiFlavor:            apiFlavorSpeech,
		endpoint:             endpointSpeech,
		model:                model,
		requiredCapabilities: []string{routing.CapabilitySpeech},
		requiredVoice:        voice,
	}
}

// sniffSpeechVoice extracts the TTS voice a speech request names, tolerantly and
// exactly as the client sent it: the sibling of sniffRoutingModel for the one other
// field that steers routing (#251). Anything that is not a non-blank JSON string --
// an absent field, null, a number, an OpenAI-style custom-voice object, a body that is
// not JSON at all -- is "" (no voice named), which is the fail-open answer: the request
// then routes exactly as it did before voices were routable. The name is NOT trimmed:
// the body is relayed to the upstream byte for byte, so " alba" is a different voice
// from "alba", and the filter matches what the upstream will be asked for.
func sniffSpeechVoice(raw []byte) string {
	var probe struct {
		Voice json.RawMessage `json:"voice"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	var voice string
	if json.Unmarshal(probe.Voice, &voice) != nil || strings.TrimSpace(voice) == "" {
		return ""
	}
	return voice
}

// sniffSpeechResponseFormat extracts the response_format a speech request names,
// tolerantly and exactly as the client sent it -- the sibling of sniffSpeechVoice for
// the field that decides whether a one-shot relay strips the upstream WAV to raw PCM.
// Anything that is not a non-blank JSON string -- an absent field, null, a number, an
// array, a body that is not JSON at all -- is "" (none named), the fail-open answer that
// relaySpeech reads as "relay the WAV as it is". The value is NOT trimmed: it is compared
// against the exact strings validateSpeechResponseFormat already admitted, so a value that
// validation refused ("pcm " say) never reaches relaySpeech to be sniffed here.
func sniffSpeechResponseFormat(raw []byte) string {
	var probe struct {
		ResponseFormat json.RawMessage `json:"response_format"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	var format string
	if json.Unmarshal(probe.ResponseFormat, &format) != nil || strings.TrimSpace(format) == "" {
		return ""
	}
	return format
}

// handleOpenAISpeech serves POST /v1/audio/speech (and its /openai/v1/audio/speech
// alias, server.go) by relaying to a natively OpenAI-shaped speech backend
// (audio.cpp). It mirrors handleOpenAIImages step for step: there is no
// translate path, the gateway proxies speech requests and does not synthesize
// them, so an application that does not serve openai_audio is simply not a
// candidate. The response is binary audio, relayed by proxyNative without
// buffering it.
//
// Auth: requireInternalOrBearerAnyScope admits a bearer token (every ordinary API
// client) OR the internal trusted-loopback header pair, DELIBERATELY not the
// browser session cookie -- see authenticateInternalOrBearer's own doc comment
// (auth_internal_or_bearer.go).
//
// The capability gate is what makes the "not a candidate" claim true, and it
// runs inside the ONE existing admission gate: the request declares
// RequiredCapabilities = [routing.CapabilitySpeech] (speechInferenceShape) and
// inferencePreflight -> Resolve refuses a model without a yes verdict. No second
// admitPrincipal call site is added here.
//
// Run-as: the shared runAsPrincipal helper. token.ID == "" only for the loopback
// principal, so this fires only for the run executor's own loopback calls; without
// it a run-as speech call would bill and capture under the bare session instead of
// the token.
func (s *Server) handleOpenAISpeech(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// The model read here is the client's gateway model name: purely OUR routing
	// input (proxyNative rewrites it to the application's own model id before the
	// relay), read with the same tolerant probe every native-passthrough endpoint
	// uses.
	model, _ := sniffRoutingModel(raw)
	if err := validateSpeechRequest(raw, model); err != nil {
		writeRequestError(w, err)
		return
	}
	token, ok = s.runAsPrincipal(w, r, token)
	if !ok {
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, speechInferenceShape(model, sniffSpeechVoice(raw)))
	if handled {
		return
	}
	s.relaySpeech(w, r, token, pf.Req, raw)
}

// validateSpeechRequest validates the shape this handler owns, instead of
// calling inference.Request.Validate (which requires messages, and a speech
// request has none). "model" (non-empty, resolved by the caller's tolerant probe
// -- see sniffRoutingModel) and "input" (a non-empty string) are required, and
// refused here with a gateway 400 rather than relayed: audio.cpp answers a client
// error such as a missing input or an unknown model with HTTP 500, which the
// gateway would otherwise hand on as a server failure. Every other field (voice,
// instructions, speed, ...) is the client's own business and reaches the
// upstream unexamined, with three exceptions: response_format, stream and
// stream_format, which decide what KIND of response the relay has to carry.
//
// stream decision: `stream` must be a JSON boolean (or absent or null); anything
// else is refused with speechStreamUnsupported. `stream: true` is admitted -- whether
// the call is then relayed as a stream or answered as a one-shot WAV is the relay's
// decision (relaySpeech), made from the mapped model's speech_streaming verdict.
// `stream: false` and an absent stream describe what the endpoint always did.
//
// response_format decision, which depends on whether the request streams:
//
//   - A request that does NOT stream accepts "wav" or "pcm" (or an absent value); EVERY other
//     value -- mp3, opus, flac, aac, but also json and b64_json -- is refused with
//     speechResponseFormatUnsupported. The relay of a one-shot request is a purely binary
//     body: audio.cpp answers an unsupported value such as mp3 with raw WAV labelled
//     audio/wav (a client asking for mp3 would be handed WAV without being told), and
//     json/b64_json are not OpenAI speech formats at all and, unlike the wav response,
//     carry none of the metering headers the usage row's billing quantity is read from --
//     relayed, they would be billed as zero seconds. A 400 the client can act on beats both
//     silent outcomes. "pcm" is the one value the gateway can honor honestly on top of the
//     upstream's WAV: the relay strips the WAV header down to its data chunk. Whether the
//     clip IS 16-bit mono PCM is known only once the upstream has answered, so that refusal
//     (speechPcmUnavailable) is the relay's, not this validation's.
//   - A request that streams accepts "pcm" (or an absent value) -- the only format
//     audio.cpp's streaming mode serves. "wav" is refused with speechStreamUnsupported,
//     because audio.cpp answers a wav stream with a 500 ("streaming speech currently
//     supports response_format=pcm") and the gateway says so itself before relaying it;
//     every other value is refused with speechResponseFormatUnsupported as above.
//
// stream_format ("sse", the default shape, or "audio", headerless raw PCM) is meaningful
// only to a stream: on one it is relayed as the client wrote it -- the gateway translates
// no event shape into another -- and any other value, or a stream_format on a request
// that is not a stream, is refused with speechStreamUnsupported. A null stream_format is
// absent.
//
// response_format, stream and stream_format are decoded as json.RawMessage and
// then type-checked, for the reason validateImagesRequest gives: a typed decode
// would discard the type error and leave the field at its zero value, walking a
// non-string response_format such as ["mp3"] or 123 past the check. A JSON null
// is an absent value throughout. The checks run in the order response_format, stream,
// stream_format, so a request wrong in two ways is told about the first.
//
// The result is the concrete *inference.Error, nil for a valid request. Callers
// compare it to nil as returned and only then hand it on as an error, which
// avoids the typed-nil-in-an-interface trap.
func validateSpeechRequest(raw []byte, model string) *inference.Error {
	if strings.TrimSpace(model) == "" {
		return &inference.Error{Code: speechModelRequired, Message: "model is required"}
	}
	var body struct {
		Input          string          `json:"input"`
		ResponseFormat json.RawMessage `json:"response_format"`
		Stream         json.RawMessage `json:"stream"`
		StreamFormat   json.RawMessage `json:"stream_format"`
	}
	_ = json.Unmarshal(raw, &body)
	if strings.TrimSpace(body.Input) == "" {
		return &inference.Error{Code: speechInputRequired, Message: "input is required and must be a non-empty string"}
	}
	// Whether the request streams is read first because the response_format rule depends
	// on it, but a stream that is no boolean is only reported after response_format.
	var stream bool
	streamErr := false
	if len(body.Stream) > 0 {
		streamErr = json.Unmarshal(body.Stream, &stream) != nil
		stream = stream && !streamErr
	}
	if len(body.ResponseFormat) > 0 {
		if err := validateSpeechResponseFormat(body.ResponseFormat, stream); err != nil {
			return err
		}
	}
	if streamErr {
		return &inference.Error{Code: speechStreamUnsupported, Message: fmt.Sprintf("stream must be a boolean or absent; got %s", body.Stream)}
	}
	if len(body.StreamFormat) > 0 && string(body.StreamFormat) != "null" {
		if !stream {
			return &inference.Error{Code: speechStreamUnsupported, Message: "stream_format applies only to a streamed request; set stream to true or leave stream_format out"}
		}
		var format string
		if err := json.Unmarshal(body.StreamFormat, &format); err != nil || (format != speechStreamFormatSSE && format != speechStreamFormatAudio) {
			return &inference.Error{Code: speechStreamUnsupported, Message: fmt.Sprintf("stream_format must be \"sse\" or \"audio\"; got %s", body.StreamFormat)}
		}
	}
	return nil
}

// The two values of a streamed speech request's stream_format that audio.cpp serves:
// an event stream (its default) and headerless raw PCM.
const (
	speechStreamFormatSSE   = "sse"
	speechStreamFormatAudio = "audio"
)

// validateSpeechResponseFormat applies validateSpeechRequest's response_format rule to a
// present value: "wav", "pcm" or absent for a request that does not stream, "pcm" or absent
// for one that does (and "wav" on a stream is the stream's own refusal).
func validateSpeechResponseFormat(rawFormat json.RawMessage, stream bool) *inference.Error {
	mustBe, only := `the string "wav" or "pcm"`, `only "wav" and "pcm" are`
	if stream {
		mustBe, only = `the string "pcm"`, `only "pcm" is`
	}
	var format string
	if err := json.Unmarshal(rawFormat, &format); err != nil {
		return &inference.Error{Code: speechResponseFormatUnsupported, Message: fmt.Sprintf("response_format must be %s; got %s", mustBe, rawFormat)}
	}
	switch {
	case format == "" || format == "pcm" || (!stream && format == "wav"):
		return nil
	case stream && format == "wav":
		return &inference.Error{Code: speechStreamUnsupported, Message: "a streamed response is headerless PCM; response_format must be \"pcm\" (or absent) when stream is true"}
	default:
		return &inference.Error{Code: speechResponseFormatUnsupported, Message: fmt.Sprintf("response_format %q is not supported; %s", format, only)}
	}
}

// recordSpeechRoutingFailure writes the client-facing error response for a
// terminal speech routing failure and records its usage row, so
// resolveSpeechTarget's two refusal sites -- an outright resolveTarget failure
// and the post-resolve effective-flavor conjunct -- cannot drift in what they
// record or capture. Mirrors recordImagesRoutingFailure: the row is recorded
// against routing.Target{} for BOTH callers because neither reached proxyNative,
// so no upstream call was made, and a populated Host/RouteID/ProviderPath would
// claim one that never happened (and make the energy reconciler price the row
// against a server that never saw the request).
//
// The row carries usage.BillingUnitAudioSecond with a zero quantity, set here
// unconditionally as recordImagesRoutingFailure does for images: the unit is the
// ENDPOINT's identity, and the default "" would record a refused speech request
// as token-metered with a zero measure, the lie the (unit, quantity) pair exists
// to prevent. A refused request produced no audio, so the quantity stays 0; it
// is only ever set where a relay finishes (setSpeechBillingQuantity).
func (s *Server) recordSpeechRoutingFailure(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte, start time.Time, err error) {
	id := nextRequestID()
	capturing := s.capturingEnabled(token)
	status := completionHTTPStatus(err)
	code := completionErrorCode(err)
	// Warn for an admission-queue rejection, mirroring tryProxyNative's own
	// severity split for the same error pair; Debug for every other routing
	// failure (no route, no healthy host, not capable, and routing.voice_unavailable
	// -- the requested voice's servers are all down or busy, answered 503 with its
	// own code by completionHTTPStatus / completionErrorCode), which is the
	// ordinary, expected shape of the gate doing its job.
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
		slog.Warn("speech request admission rejected", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status)
	} else {
		slog.Debug("speech request rejected: routing failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status, "err", err)
	}
	body := writeCompletionErrorCaptured(w, err)
	s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: endpointSpeech}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, status, req.APIFlavor), nil)
}

// resolveSpeechTarget is resolveImagesTarget's speech twin: it resolves the
// routing target for an already-gated speech request and applies the
// effective-served conjunct to it. On a refusal it has already written the
// client's response and recorded the usage row, and answers ok == false.
//
// token is a pointer for the reason tryProxyNative's is: resolveTarget refreshes
// the caller's last-used-model snapshot through it, and the relay that follows
// must carry that refreshed token.
func (s *Server) resolveSpeechTarget(w http.ResponseWriter, r *http.Request, token *auth.Token, req inference.Request, raw []byte, start time.Time) (routing.Target, bool) {
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		s.recordSpeechRoutingFailure(w, r, *token, req, raw, start, err)
		return routing.Target{}, false
	}
	// The effective-served rule is a CONJUNCT (§6 / ADR-033): flavor ∈ APIFlavors
	// AND the endpoint is not disabled. For a server_agent mapping the resolved
	// runtime spec -- not the application -- is the authority for its model's
	// flavors (targetFrom), and candidacy gated that app type on its coarser
	// app-level flavors only. The speech relay reaches proxyNative directly, so
	// without this an agent-managed child whose spec lists only text would still
	// serve a speech request.
	if !targetServesFlavor(target, req.APIFlavor) {
		// The refusal looks like candidacy's own no-route answer WHEN THIS WAS THE
		// ONLY ROUTE for the model, and is not retried against a sibling
		// application that might still serve the request -- the same limitation
		// resolveImagesTarget and tryProxyNative's own equivalent carry.
		slog.Debug("speech request rejected: resolved target's effective flavors exclude speech",
			"path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model,
			"server", s.serverName(target.ServerID), "route_id", target.RouteID)
		s.recordSpeechRoutingFailure(w, r, *token, req, raw, start, routing.ErrNoModelRoute)
		return routing.Target{}, false
	}
	return target, true
}

// relaySpeech relays an already-gated speech request via proxyNative, following
// relayImages' resolve-then-relay shape with no translate fallback: EVERY resolve
// failure is terminal and recorded directly (resolveSpeechTarget). req is pf.Req
// from the caller's inferencePreflight call, already carrying
// RequiredCapabilities = [routing.CapabilitySpeech].
//
// proxyNative streams the upstream body to the client chunk by chunk and relays
// the upstream Content-Type verbatim, which is what carries an audio/wav body.
//
// A request that asks for a stream (`stream: true`; validateSpeechRequest has checked it
// is a boolean) is relayed as one only when the mapped route is KNOWN to stream: its
// speech_streaming verdict is yes (mappingStreamsSpeech). Then the relay is marked a
// stream (nativeRelay.pfReq.Stream) and carries bufferedRetry, the builder of the request
// proxyNative sends once more, buffered, when the upstream refuses to stream
// (nativeRelay.streamRefusal) -- the net for a yes that has gone stale, a model swapped to
// offline mode behind the verdict's back. Any other answer (a no, no verdict at all, one
// that cannot be read) decides up front that the request is answered buffered: the body
// is sent without the stream-only fields (speechBufferedBody), so the upstream is never
// asked for what it would refuse and the client's answer, a one-shot WAV, costs one
// upstream call. The preflight request is not marked a stream, so the gate and the
// resolve judged the same request a one-shot call would be.
//
// proxyNative then finishes the exchange: a 2xx event stream (or raw PCM) of a streamed
// request is relayed unbuffered by the native copier, read on its way by
// speechStreamScan, billed on its completion (setStreamedSpeechBillingQuantity) and
// captured as the redacted audio summary every speech response gets; any other 2xx one
// is the one-shot WAV, billed from its duration header (setSpeechBillingQuantity).
func (s *Server) relaySpeech(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	start := time.Now()
	target, ok := s.resolveSpeechTarget(w, r, &token, req, raw, start)
	if !ok {
		return
	}
	rel := nativeRelay{
		token:    token,
		target:   target,
		path:     upstreamPath(target, apiFlavorSpeech, endpointSpeech),
		raw:      raw,
		pfReq:    req,
		endpoint: endpointSpeech,
	}
	_, wantsStream := sniffRoutingModel(raw)
	if wantsStream {
		if s.mappingStreamsSpeech(r.Context(), target.RouteID) {
			rel.pfReq.Stream = true
			rel.streamRefusal = speechStreamingRefusal
			rel.bufferedRetry = func() ([]byte, string, error) {
				body, err := speechBufferedBody(raw)
				return body, jsonContentType, err
			}
		} else {
			slog.Debug("speech asked to stream but the route is not known to: answering buffered",
				"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID),
				"route_id", target.RouteID, "capability", routing.CapabilitySpeechStreaming)
			body, err := speechBufferedBody(raw)
			if err != nil {
				// Not expected for a body validateSpeechRequest accepted: it found an input in
				// it, which only a JSON object has. Were it to happen it is answered as a failed
				// relay (and recorded) rather than sending the upstream a stream request it would
				// refuse.
				slog.Error("speech: building the buffered body failed",
					"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID), "err", err)
				s.recordSpeechRoutingFailure(w, r, token, req, raw, start, fmt.Errorf("build buffered speech body: %w", err))
				return
			}
			rel.upstreamBody = body
		}
	}
	// A one-shot pcm request (#253): the upstream is forced to wav -- audio.cpp's only
	// one-shot format, which it answers whatever response_format it is handed -- and
	// proxyNative strips the WAV header to relay the raw data chunk. Gated on the CLIENT's
	// own stream intent (wantsStream), not rel.pfReq.Stream, so a stream request downgraded
	// to a one-shot WAV (the route is not known to stream) is NOT stripped: it kept
	// response_format off the upstream body (speechBufferedBody) and is relayed as the WAV
	// it asked to become.
	if !wantsStream && sniffSpeechResponseFormat(raw) == "pcm" {
		body, err := speechPCMUpstreamBody(raw)
		if err != nil {
			// Not expected for a body validateSpeechRequest accepted (it found a string input,
			// so the body is a JSON object). Answered as a failed relay rather than guessing a
			// body, exactly as the buffered-body-error branch above does.
			slog.Error("speech: building the pcm upstream body failed",
				"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID), "err", err)
			s.recordSpeechRoutingFailure(w, r, token, req, raw, start, fmt.Errorf("build pcm speech body: %w", err))
			return
		}
		rel.speechPCMStrip = true
		rel.upstreamBody = body
	}
	s.proxyNative(w, r, rel)
}

// speechStreamingRefusal is the sentence audio.cpp's error carries when stream=true
// reaches a speech model that is not loaded with mode=streaming (an offline model). It
// answers HTTP 500, as it does for every error, so the sentence -- not the status alone
// -- is what tells this refusal from a server that is really failing. It is speech's own:
// a transcription's refusal says "transcription streaming ..." (transcriptionStreamingRefusal,
// whose words this one happens to end in).
const speechStreamingRefusal = "speech streaming requires a model configured with mode=streaming"

// speechStreamOnlyFields are the request fields of a speech body that only a stream has a
// meaning for: stream itself, stream_format (sse or audio) and response_format, which on
// a stream is pcm and on a one-shot request is wav or pcm. The buffered twin of a streamed
// request leaves them all out, so audio.cpp answers it with the one-shot WAV it always
// has: a pcm response_format kept on it would ask for a shape the buffered relay's
// billing (a WAV's duration header) does not carry. A one-shot pcm request's own upstream
// body is built by speechPCMUpstreamBody instead, which forces response_format to wav
// rather than dropping it.
var speechStreamOnlyFields = []string{"stream", "stream_format", "response_format"}

// speechBufferedBody builds the body of the one-shot request answered INSTEAD of a
// streamed one -- either decided up front, when the route is not known to stream, or
// sent as the retry when the upstream refused to (nativeRelay.bufferedRetry): the
// client's JSON without speechStreamOnlyFields, every other field as it was written
// (values are kept as raw JSON, so a number keeps its full precision). Names are matched
// without regard to case, as encoding/json matched them when the request was validated,
// so a "Stream" the validation read as a stream is dropped too. The model name is not
// touched here: proxyNative rewrites it to the mapped one, as for any speech body.
//
// It is an error only for a body that is no JSON object, which validateSpeechRequest
// has already ruled out.
func speechBufferedBody(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("speech body is not valid JSON: %w", err)
	}
	if fields == nil {
		return nil, errors.New("speech body is not a JSON object")
	}
	for name := range fields {
		for _, streamOnly := range speechStreamOnlyFields {
			if strings.EqualFold(name, streamOnly) {
				delete(fields, name)
				break
			}
		}
	}
	return json.Marshal(fields)
}

// speechPCMUpstreamBody builds the upstream body for a one-shot pcm request: the client's
// JSON with response_format forced to "wav", every other field kept exactly as it was
// written (values travel as raw JSON, so a number keeps its full precision). audio.cpp
// ignores response_format and always answers a one-shot request with a 44100/mono/16-bit
// WAV, so forcing wav asks for the one format proxyNative can then strip to the raw data
// chunk it relays as application/octet-stream; its duration header -- the billing quantity
// -- is unchanged. The model name is not touched here: proxyNative rewrites it to the
// mapped one, as for any speech body.
//
// It is an error only for a body that is no JSON object, which validateSpeechRequest has
// already ruled out (it found a string input), mirroring speechBufferedBody.
func speechPCMUpstreamBody(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("speech body is not valid JSON: %w", err)
	}
	if fields == nil {
		return nil, errors.New("speech body is not a JSON object")
	}
	fields["response_format"] = json.RawMessage(`"wav"`)
	return json.Marshal(fields)
}

// mappingStreamsSpeech reports whether the mapped route is known to answer a streamed
// synthesis: its stored speech_streaming verdict, keyed by the route (target.RouteID,
// the mapping id), is yes. It is the up-front half of the stream-versus-buffered
// decision, mappingStreamsTranscription's twin; the upstream's refusal and the buffered
// retry remain the net under a yes that has gone stale.
//
// Everything but a yes is false (see mappingStreamingVerdictIsYes): a no, no row at all
// (the verdict is "unknown" until a probe states the model's mode, and the answer for
// unknown is the one-shot WAV, which every model serves), a route that is no mapping, no
// route store, and a read that fails.
func (s *Server) mappingStreamsSpeech(ctx context.Context, routeID string) bool {
	return s.mappingStreamingVerdictIsYes(ctx, routeID, routing.CapabilitySpeechStreaming, "speech streaming verdict could not be read; answering buffered")
}

// speechDurationHeader is the response header audio.cpp reports a synthesized
// clip's length in, in MILLISECONDS as a decimal string ("2000.000000"). It is
// the only metering signal a speech response carries: the body is binary audio
// with no usage object, which is why the speech row's measure is a duration
// (usage.BillingUnitAudioSecond) and not a token count.
const speechDurationHeader = "X-AudioCPP-Audio-Duration-Ms"

// speechDurationSeconds parses a speechDurationHeader value into seconds, and
// reports whether it was a usable duration: a finite number greater than zero.
// An absent or empty value, a non-number (ParseFloat rejects "1500ms"), zero,
// a negative and "NaN"/"Inf" (which ParseFloat does accept) all answer false --
// none states how much audio was produced, and a NaN or infinite quantity would
// poison every sum the usage views take.
func speechDurationSeconds(header string) (float64, bool) {
	ms, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 {
		return 0, false
	}
	return ms / 1000, true
}

// setSpeechBillingQuantity records, on meta, how many seconds of audio a
// finished speech relay produced -- and does nothing at all for a non-speech
// flavor or a request that did not finish cleanly, leaving BillingQuantity at 0
// so nothing is asserted about what was produced. It is setImagesBillingQuantity's
// twin and shares its reasoning: the quantity comes from the RESPONSE (the
// upstream's own statement of the clip it synthesized, never the request's
// input or speed), and is only trusted once the FULL body reached the client
// (status == "success"; a non-2xx upstream answer, an idle timeout, a client
// disconnect or a copy error all leave it 0).
//
// upstream is the UPSTREAM response's headers. Unlike the images count, which
// the copier accumulates from the body, the duration arrives on the response
// header, and nativeExchange is built before any response exists, so the one
// caller passes resp.Header alongside it rather than the exchange growing a
// field that is empty for every other flavor and for the whole pre-response
// phase.
//
// A successful relay with no usable duration is indistinguishable, in the
// recorded row alone, from a genuinely empty clip, and the (unit, quantity) pair
// has no "unknown" representation to fall back on. So, exactly as for a counted
// zero image, the gap is logged once at Error with enough to find the request,
// instead of silently recording a bare zero (or repairing the row, which would
// destroy the evidence). The raw header value is logged because an unparseable
// one is the very thing an operator needs to see; it is the upstream's own
// string, never client input.
//
// Nothing else is set: the usage scanner is nil for speech (newUsageScanner), so
// no token count or rate exists to leak onto the row and ValidateBillingXOR
// holds. meta.ReqPath is read for the log line rather than taking the request a
// second time: it is the same r.URL.Path the caller already put there.
//
// A transcription shares the openai_audio flavor (apiFlavorSpeech) but not the
// header: audio.cpp reports its duration in the response BODY, so the exchange of a
// transcription (ex.endpoint == endpointTranscription) is left alone here --
// setTranscriptionBillingQuantity is its setter. Without that endpoint test a
// flavor-only guard would log an Error for every successful transcription (no
// duration header, by design) and still leave its quantity at 0.
func setSpeechBillingQuantity(meta *usageMeta, ex nativeExchange, upstream http.Header, status string) {
	if ex.req.APIFlavor != apiFlavorSpeech || ex.endpoint == endpointTranscription || status != "success" {
		return
	}
	value := upstream.Get(speechDurationHeader)
	seconds, ok := speechDurationSeconds(value)
	if !ok {
		slog.Error("speech relay succeeded but the upstream reported no usable audio duration", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName, "header", speechDurationHeader, "value", value)
		return
	}
	meta.BillingQuantity = seconds
}

// cachedOutputByteRate returns the PCM byte rate cached for the mapping routeID
// (routing.ModelMappingCacheOutputByteRate, the scope-"" row) and true. It returns
// (0, false) when the rate is not known: no route store, no such row, a payload that
// is not a positive integer fitting a uint32 (the caller divides by it, so a zero is
// no rate), or a store failure (logged at Warn). Every unknown reads the same
// because the caller has one fallback for all of them -- bill the streamed bytes
// themselves -- and a billing path has no business failing a response over a missing
// optimization.
func (s *Server) cachedOutputByteRate(ctx context.Context, routeID string) (uint32, bool) {
	if s.Routes == nil || routeID == "" {
		return 0, false
	}
	rows, err := s.Routes.ModelMappingCache(ctx, routeID, routing.ModelMappingCacheOutputByteRate)
	if err != nil {
		slog.Warn("output byte rate cache read failed; the streamed speech call falls back to bytes", "mapping_id", routeID, "err", err)
		return 0, false
	}
	for _, row := range rows {
		if row.ScopeKey != "" {
			continue
		}
		return routing.ParseOutputByteRate(row.Payload) // the one definition of a usable rate, shared with the health loop's gate
	}
	return 0, false
}

// speechMeteringHeaderPrefix is the lower-cased prefix of the headers audio.cpp
// meters a synthesized clip with: speechDurationHeader (the billing quantity) and
// its siblings, wall time and real-time factor.
const speechMeteringHeaderPrefix = "x-audiocpp-"

// speechMeteringHeaders returns the upstream's metering headers (every header in
// the speechMeteringHeaderPrefix family) and nothing else, or nil when there are
// none. The relay hands the client neither these nor a body it can read them
// back from, so they reach the capture only through this: the one place the
// capture of a skipped audio body (captureResponseBody) can still say what the
// upstream measured.
func speechMeteringHeaders(upstream http.Header) http.Header {
	var metering http.Header
	for name, values := range upstream {
		if !strings.HasPrefix(strings.ToLower(name), speechMeteringHeaderPrefix) {
			continue
		}
		if metering == nil {
			metering = http.Header{}
		}
		metering[name] = append([]string(nil), values...)
	}
	return metering
}

// speechUpstreamErrorType and speechUpstreamErrorCode are the GATEWAY's own
// classification for a speech upstream error whose shape is not already an
// OpenAI error object (see normalizeSpeechUpstreamError). They are fixed
// regardless of the upstream's HTTP status, for the reason the images pair
// documents: the upstream stated neither, so a status-dependent value would claim
// a distinction it never drew. The code is API surface.
const (
	speechUpstreamErrorType = "upstream_error"
	speechUpstreamErrorCode = "speech.upstream_error"
)

// needsSpeechErrorRelay reports whether a native-passthrough response must go
// through relaySpeechUpstreamError instead of proxyNative's streaming copier: a
// speech request whose upstream answered a non-2xx status. It is
// needsImagesErrorRelay's twin and exactly as narrow: the flavor test keeps every
// other flavor's errors on the copier, byte for byte, and the status test is the
// 2xx window proxyNative's own success path uses, so a 2xx audio/wav still
// streams.
//
// The endpoint test carves the transcription out of the flavor: it shares
// openai_audio with speech, and its non-2xx answers belong to its own relay
// (needsTranscriptionErrorRelay), whose normalised code is transcription.upstream_error
// and not speech.upstream_error. The test is negative (everything but a
// transcription) so a speech relay built without an endpoint -- the zero value is
// endpointChat -- is still a speech relay.
func needsSpeechErrorRelay(apiFlavor string, endpoint sessionEndpoint, statusCode int) bool {
	return apiFlavor == apiFlavorSpeech && endpoint != endpointTranscription && (statusCode < 200 || statusCode >= 300)
}

// normalizeSpeechUpstreamError turns a speech upstream's non-2xx body into the
// bytes the client is sent, and reports whether it recognised the shape. status
// is the upstream's HTTP status; it is not consulted, because the classification
// is deliberately independent of it (speechUpstreamErrorType).
//
// Two shapes are recognised, in this order:
//
//   - An OpenAI error object ({"error":{"message":...}}, the message being what
//     identifies one) passes through untouched, byte for byte. audio.cpp answers
//     this way -- {"error":{"message":...,"type":"server_error"}} -- so this is
//     the ordinary case. Re-normalising it would overwrite the upstream's own
//     type with the gateway's, restating an attested value as a gateway-authored
//     one, and would drop whatever else the object carries.
//   - A plain string ({"error":"..."}) is wrapped into an OpenAI object whose
//     message is that string verbatim. Its type and code are the gateway's own
//     (speechUpstreamErrorType, speechUpstreamErrorCode), the one case the
//     upstream attested neither.
//
// ok is false for anything else -- audio, HTML from an intermediary, an empty
// body, JSON of another shape -- and the caller then relays the upstream bytes
// unchanged instead of guessing. The status audio.cpp answers with is relayed by
// the caller as it came (audio.cpp uses 500 for client errors too; correcting
// that is a decision the gateway makes before the relay, in validateSpeechRequest,
// not here).
func normalizeSpeechUpstreamError(body []byte, status int) ([]byte, bool) {
	var already apierror.Body
	if err := json.Unmarshal(body, &already); err == nil && already.Error.Message != "" {
		return body, true
	}
	var plain struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &plain); err == nil && plain.Error != "" {
		// Marshal cannot fail on this type (three strings), so there is no failure
		// to report.
		wrapped, _ := json.Marshal(apierror.Body{Error: apierror.ErrorBody{
			Message: plain.Error,
			Type:    speechUpstreamErrorType,
			Code:    speechUpstreamErrorCode,
		}})
		return wrapped, true
	}
	return nil, false
}

// relaySpeechUpstreamError handles a non-2xx response from the speech upstream
// (audio.cpp): it buffers the (bounded) body, normalises it via
// normalizeSpeechUpstreamError, and relays either the normalised body or -- when
// the shape is not recognised -- the original bytes unchanged, under the
// upstream's own status either way. It is relayImagesUpstreamError's twin and is
// called from proxyNative behind needsSpeechErrorRelay, so no other flavor's error
// handling changes.
//
// ex carries this request's identity -- principal, session-enriched request,
// target, client bytes and the start/id/capturing bookkeeping -- as one value:
// the function both writes the client's response and records its usage row, so it
// needs everything proxyNative's own terminal step does (see nativeExchange).
//
// The whole body is read before anything is written, unlike the copier's
// byte-as-it-arrives design: normalising needs all of it, and an error is one small
// payload -- a streamed request's too: a non-2xx status is the answer INSTEAD of a
// stream, not a part of one. The read is bounded by s.captureMaxBytes, the cap the
// copier's capture buffer already uses; an error body has no reason to exceed it.
func (s *Server) relaySpeechUpstreamError(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse) {
	upstreamBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
	errorCode := fmt.Sprintf("upstream.%d", resp.StatusCode)
	if readErr != nil {
		// A read failure this late (the status line arrived, the body broke) is the
		// class nativeTerminalStatus names provider.stream_copy_error on the
		// streaming path; named the same way here for one consistent code.
		errorCode = "provider.stream_copy_error"
		slog.Error("inference native passthrough copy error", "path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "err", readErr)
	} else {
		slog.Warn("inference native passthrough upstream error", "path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "status", resp.StatusCode)
	}

	written, sentContentType := upstreamBody, resp.Header.Get("Content-Type")
	if body, ok := normalizeSpeechUpstreamError(upstreamBody, resp.StatusCode); ok {
		written, sentContentType = body, jsonContentType
	} else if sentContentType == "" {
		// Neither recognised shape: the upstream's bytes go out unchanged, under its
		// own Content-Type, or JSON when it named none (the relay's default).
		sentContentType = jsonContentType
	}
	w.Header().Set("Content-Type", sentContentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(written)

	// The unit is set unconditionally: this is speech-only, so the flavor is always
	// apiFlavorSpeech, and a failed relay produced no audio -- BillingQuantity stays
	// 0, as recordSpeechRoutingFailure leaves it for a refused request.
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: sentContentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), written, resp.StatusCode, ex.req.APIFlavor), nil)
}

// recordSpeechPCMUnavailable answers a one-shot pcm request whose synthesized clip cannot
// be served as raw PCM: the mapped model's native WAV is not 16-bit mono PCM (or its data
// chunk does not fit the upstream's declared length -- a garbled/streaming writer's header,
// never trusted audio.cpp's). It is a POST-synthesis refusal -- the format is known only
// once the upstream has answered -- so validateSpeechRequest cannot raise it; proxyNative
// does, from its 2xx strip path, which is why this takes the request's identity as the one
// nativeExchange (ex) that relaySpeechUpstreamError does.
//
// The client gets an OpenAI-shaped error object under HTTP 502 (a backend limitation, not
// the client's request): type speechUpstreamErrorType ("upstream_error") and the API-surface
// code speechPcmUnavailable. The row is an error row with the audio_second unit and a zero
// quantity -- no audio was served -- exactly as relaySpeechUpstreamError leaves a failed
// relay. The operator's server name stays out of the client text (it names only the model
// and the format), and the WAV never reaches the copier: this returns in its place.
func (s *Server) recordSpeechPCMUnavailable(w http.ResponseWriter, r *http.Request, ex nativeExchange) {
	const status = http.StatusBadGateway
	slog.Warn("speech pcm request refused: the model's synthesized audio is not 16-bit mono PCM",
		"path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "code", speechPcmUnavailable, "status", status)
	// Marshal cannot fail on this type (three strings), so there is no failure to report.
	body, _ := json.Marshal(apierror.Body{Error: apierror.ErrorBody{
		Message: fmt.Sprintf("response_format \"pcm\" is unavailable for model %q: its native audio is not 16-bit mono PCM", ex.req.Model),
		Type:    speechUpstreamErrorType,
		Code:    speechPcmUnavailable,
	}})
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, speechPcmUnavailable, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), body, status, ex.req.APIFlavor), nil)
}
