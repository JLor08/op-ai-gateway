// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
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

// defaultMaxAudioTranscriptionBodyBytes is the default bound on the multipart read
// of POST /v1/audio/transcriptions, an operator setting
// (ServerDeps.MaxAudioTranscriptionBodyBytes, config
// OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES; the Server's value is
// transcriptionBodyLimit). Like the image edit, the upload is held in memory --
// the handler peeks the form before routing and may rebuild it afterwards (a
// model-name rewrite) -- so an unbounded read is a memory exposure the endpoint has
// to answer for. It is a setting of its own, not the image-edit one, so an operator
// can size audio independently. The default is the agent runtime router's own
// request cap (agentRouterMaxBodyBytes): a body bound for an agent-launched model
// must also fit what the agent accepts after the gateway has prepared it.
const defaultMaxAudioTranscriptionBodyBytes = 32 << 20

// maxTranscriptionFieldBytes bounds ONE text field the handler reads out of the
// form (model, response_format, stream, ...). The audio file part is never read
// into a field: it is counted and relayed.
const maxTranscriptionFieldBytes = 1 << 20

// transcriptionBodyLimit is the multipart read cap in force: the configured one, or
// the default for a Server that was never given one (a bare &Server{}).
func (s *Server) transcriptionBodyLimit() int64 {
	if s.maxAudioTranscriptionBodyBytes > 0 {
		return s.maxAudioTranscriptionBodyBytes
	}
	return defaultMaxAudioTranscriptionBodyBytes
}

// The request-error codes the transcription endpoint answers its own refusals
// with. They are API surface: clients branch on them.
const (
	transcriptionModelRequired = "transcription.model_required"
	transcriptionFileRequired  = "transcription.file_required"
	// transcriptionResponseFormatUnsupported is the code for a response_format this
	// relay cannot honor -- every value but "json"; see validateTranscriptionRequest.
	transcriptionResponseFormatUnsupported = "transcription.response_format_unsupported"
	// transcriptionStreamUnsupported is the code for a request whose streaming this
	// relay will not honor -- a `stream` that is no boolean, or any stream_format;
	// see validateTranscriptionRequest. (A boolean stream=true is served.)
	transcriptionStreamUnsupported = "transcription.stream_unsupported"
	// invalidTranscriptionRequest is the code for a structurally broken form: not
	// multipart, no boundary, truncated or malformed parts, a field too large to
	// read, a field repeated where one answer is required, a body that cannot be
	// read. (The image edit's twin is the generic request.invalid; this endpoint
	// names its own.)
	invalidTranscriptionRequest = "transcription.invalid"
)

// transcriptionForm is what the handler needs out of a transcription's multipart
// form before it can route the request: the routing model, the fields the
// validation twin checks, and whether an audio file arrived. It is deliberately NOT
// a general form decoding -- every other field (temperature,
// timestamp_granularities[], ...) is the client's own business and reaches the
// upstream unexamined.
type transcriptionForm struct {
	// boundary is the multipart boundary of the CLIENT's body, kept so a body that
	// has to be rebuilt can be read back with the same reader.
	boundary string
	// model is the model field exactly as the client sent it. Routing uses the
	// trimmed value (routingModel); the untrimmed one is what a verbatim relay would
	// hand an agent's router, so it is what the rewrite decision compares.
	model          string
	language       string
	prompt         string
	responseFormat string
	stream         string
	// streamFormat is the stream_format field. The gateway relays the upstream's own
	// event shape and translates none, so the only question about it is whether it
	// was set at all.
	streamFormat string
	// files counts the NON-EMPTY parts named `file`, the one field OpenAI's API and
	// the audio.cpp server read the upload from. (There is no `file[]` spelling: a
	// part under it is just another part the handler does not own, so a form that
	// carries only one has no file.) Validation requires exactly one.
	files int
	// emptyFiles counts the EMPTY parts named `file`. They are no file (validation
	// ignores them, so one beside the real upload is accepted), but they are in the
	// body: the relay drops them (rebuildTranscriptionBody) so the upstream sees
	// exactly the one file the gateway validated, and a form that has any is
	// therefore rebuilt even when its model needs no rewrite.
	emptyFiles int
}

func (f transcriptionForm) routingModel() string { return strings.TrimSpace(f.model) }

// wantsStream reports whether the client asked for a streamed answer: a stream field
// that reads as the boolean true (validateTranscriptionRequest has already refused one
// that reads as no boolean, so false and absent are the only other cases).
func (f transcriptionForm) wantsStream() bool {
	v := strings.TrimSpace(f.stream)
	if v == "" {
		return false
	}
	stream, err := strconv.ParseBool(v)
	return err == nil && stream
}

func invalidTranscription(message string) *inference.Error {
	return &inference.Error{Code: invalidTranscriptionRequest, Message: message}
}

// readTranscriptionBody reads the whole multipart body, bounded by limit (the
// server's transcriptionBodyLimit). A body past the cap is a 413 whether its length
// was declared (refused without reading a byte) or only discovered while reading.
// The JSON readers cannot be used here: readRawJSONLimit decodes one JSON value
// and refuses anything else.
func readTranscriptionBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.ContentLength > limit {
		writeBodyTooLarge(w)
		return nil, false
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeBodyTooLarge(w)
			return nil, false
		}
		writeRequestError(w, invalidTranscription("request body could not be read"))
		return nil, false
	}
	return raw, true
}

// parseTranscriptionForm walks the multipart body once with mime/multipart and the
// request's own boundary, and returns what routing and validation need. It is
// parseImageEditForm's twin and does NOT use http.Request.ParseMultipartForm, for
// the same reasons: that spills parts over a memory threshold to temp files and
// consumes the request body, and this handler needs the body intact afterwards (to
// relay it verbatim or to rebuild it).
//
// Only the fields the handler owns are read into memory, each bounded by
// maxTranscriptionFieldBytes; the `file` parts are counted and skipped, and every
// other part is skipped untouched. Each of model, language, prompt,
// response_format, stream and stream_format may appear ONCE: the model decides
// routing and the allowlist and is peeked again downstream (the agent's router), so
// two of them are two answers to "which model" and one must not be silently picked
// over the other; the rest are refused for the same uniformity rather than
// inventing a precedence.
//
// SECURITY: the model is read EXACTLY as the agent router's peekMultipartModel
// reads it (server-agent/internal/runtime/router.go): a part of
// mime/multipart.NewReader over the buffered body whose FormName() is "model" --
// not a scan for the text name="model" -- single-use, and with the WHOLE body
// walked to its end even once the model is in hand. Were the two to read different
// models out of one body, the gateway would allowlist one name and the router
// start another. Keep them in step.
//
// The whole body is walked, and EVERY part read to its end -- the skipped ones
// too, because mime/multipart swallows the read error of a part it merely steps
// over, so a part cut off mid-way would pass here and fail later, in a rebuild,
// after the request had been admitted. A repeated field or a malformed tail is
// caught here rather than discovered by a consumer later.
func parseTranscriptionForm(contentType string, raw []byte) (transcriptionForm, error) {
	var form transcriptionForm
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return form, invalidTranscription("request body must be multipart/form-data with a boundary")
	}
	form.boundary = params["boundary"]
	mr := multipart.NewReader(bytes.NewReader(raw), form.boundary)
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return form, nil
		}
		if err != nil {
			return form, invalidTranscription(malformedMultipartBodyMessage)
		}
		if err := form.readPart(part, seen); err != nil {
			return form, err
		}
	}
}

// readPart reads one part of the form into f, by the field it is: the six text
// fields are read and kept, the `file` parts are counted and skipped, and any other
// part is skipped untouched. seen is the set of the single-use fields already met.
func (f *transcriptionForm) readPart(part *multipart.Part, seen map[string]bool) error {
	name := part.FormName()
	switch name {
	case "model", "language", "prompt", "response_format", "stream", "stream_format":
		return f.readTextField(part, name, seen)
	case "file":
		return f.readFilePart(part)
	default:
		_, err := discardTranscriptionPart(part)
		return err
	}
}

// readTextField reads the value of one of the text fields into f, once: a repeat
// is refused before its value is read.
func (f *transcriptionForm) readTextField(part *multipart.Part, name string, seen map[string]bool) error {
	if seen[name] {
		return invalidTranscription(fmt.Sprintf("form field %q must appear only once", name))
	}
	seen[name] = true
	value, err := readTranscriptionField(part)
	if err != nil {
		return err
	}
	switch name {
	case "model":
		f.model = value
	case "language":
		f.language = value
	case "prompt":
		f.prompt = value
	case "response_format":
		f.responseFormat = value
	case "stream":
		f.stream = value
	case "stream_format":
		f.streamFormat = value
	}
	return nil
}

// discardTranscriptionPart reads a part to its end without keeping it, and returns
// how many bytes it held: a part that breaks off is a malformed body.
func discardTranscriptionPart(part *multipart.Part) (int64, error) {
	n, err := io.Copy(io.Discard, part)
	if err != nil {
		return 0, invalidTranscription(malformedMultipartBodyMessage)
	}
	return n, nil
}

// readFilePart reads a `file` part to its end without keeping it and counts it: a
// part that held any bytes is a file (files), an empty one is not (emptyFiles).
func (f *transcriptionForm) readFilePart(part *multipart.Part) error {
	n, err := discardTranscriptionPart(part)
	if err != nil {
		return err
	}
	if n > 0 {
		f.files++
	} else {
		f.emptyFiles++
	}
	return nil
}

// readTranscriptionField reads one text field, bounded.
func readTranscriptionField(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxTranscriptionFieldBytes+1))
	if err != nil {
		return "", invalidTranscription(malformedMultipartBodyMessage)
	}
	if len(b) > maxTranscriptionFieldBytes {
		return "", invalidTranscription(fmt.Sprintf("form field %q is too large", part.FormName()))
	}
	return string(b), nil
}

// validateTranscriptionRequest is validateImageEditRequest's twin for the
// transcription form, and validateSpeechRequest's in its policy: a non-empty model
// and EXACTLY ONE non-empty `file` part are required, and every other field
// (language, prompt, temperature, ...) is the client's own business and reaches the
// upstream unexamined, with two exceptions.
//
// file decision: none is transcriptionFileRequired; more than one is refused with
// invalidTranscriptionRequest (a form with two audio files is two answers to "which
// audio", not one request, and the upstream reads a single file). Only the `file`
// field counts, as in OpenAI's API and audio.cpp's own server: a `file[]` part is
// not a file here, so a form with only that one gets transcriptionFileRequired.
//
// response_format decision: only "json" (or an absent value) is accepted, and EVERY
// other value -- verbose_json, text, srt, vtt, and anything that is no
// transcription format at all -- is refused with
// transcriptionResponseFormatUnsupported. audio.cpp ignores response_format and
// always answers its own {"text","timing"} JSON document, so accepting verbose_json
// (which promises segments and a language it does not produce) or the plain-text
// formats would hand a client a shape it did not ask for without telling it, and a
// non-JSON answer would carry no duration to bill the usage row from. A 400 the
// client can act on beats both silent outcomes. The match is exact (case-sensitive,
// as OpenAI's is) after trimming surrounding whitespace.
//
// stream decision: a boolean `stream` is accepted, true included -- the relay then
// passes the upstream's event stream through unbuffered (relayTranscription,
// proxyNative). A `stream` that is no boolean is refused with
// transcriptionStreamUnsupported rather than guessed at, and so is ANY stream_format:
// audio.cpp answers a streamed transcription in its own event shape and the gateway
// translates it into no other, so a client asking for another shape would be handed
// one it did not ask for. An absent or empty stream, `stream` false and an absent or
// empty stream_format describe what the endpoint always did and are accepted (a
// multipart form has no null: an empty value is the closest thing to one, and is
// treated as absent, as the image edit treats an empty stream).
//
// The result is the concrete *inference.Error, nil for a valid request. Callers
// compare it to nil as returned and only then hand it on as an error, which avoids
// the typed-nil-in-an-interface trap.
func validateTranscriptionRequest(form transcriptionForm) *inference.Error {
	if form.routingModel() == "" {
		return &inference.Error{Code: transcriptionModelRequired, Message: "model is required"}
	}
	if form.files == 0 {
		return &inference.Error{Code: transcriptionFileRequired, Message: "a non-empty audio \"file\" field is required"}
	}
	if form.files > 1 {
		return invalidTranscription("exactly one audio file is required")
	}
	if format := strings.TrimSpace(form.responseFormat); format != "" && format != "json" {
		return &inference.Error{Code: transcriptionResponseFormatUnsupported, Message: fmt.Sprintf("response_format %q is not supported; only \"json\" is", format)}
	}
	if v := strings.TrimSpace(form.stream); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return &inference.Error{Code: transcriptionStreamUnsupported, Message: fmt.Sprintf("stream must be a boolean or absent; got %q", v)}
		}
	}
	if strings.TrimSpace(form.streamFormat) != "" {
		return &inference.Error{Code: transcriptionStreamUnsupported, Message: "stream_format is not supported on this endpoint; a streamed transcription is relayed in the upstream's own event format"}
	}
	return nil
}

// codeTranscriptionUnavailable is the code a transcription is refused with when its
// resolved target is an agent that has not declared multipart_proxy: the request is
// well formed and the model is transcription-capable, but this server cannot take a
// multipart body. errTranscriptionUnavailable is its sentinel -- like the routing
// refusals it is mapped to a status, code and message in one place
// (completionHTTPStatus, completionErrorCode, completionErrorResponse), so the
// response and the usage row recordTranscriptionRoutingFailure writes cannot
// disagree about it. It is the image edit's errImageEditUnavailable twin, and a
// value of its own so the message names the right endpoint.
const codeTranscriptionUnavailable = "transcription.unavailable"

var errTranscriptionUnavailable = errors.New(codeTranscriptionUnavailable)

// errTranscriptionTooLarge is the refusal of a transcription whose prepared body is
// past the request limit of the agent it is bound for (agentRouterMaxBodyBytes): 413
// request.body_too_large, the code the client's own oversized body gets. It is
// errImageEditTooLarge's twin, mapped in the same three places.
var errTranscriptionTooLarge = errors.New(codeRequestBodyTooLarge)

// transcriptionInferenceShape is the shape handleOpenAITranscription hands the ONE
// preflight gate (inferencePreflight). It is a function, not an inline literal, so
// the exact values the handler uses are the ones its test pins:
//
//   - apiFlavor is exactly routing.APIFlavorOpenAIAudio (the string apiFlavorSpeech
//     aliases), never a fine variant: transcription shares the flavor with speech, and
//     the coarse flavor is what candidacy, targetServesFlavor, billingUnitFor
//     (audio_second) and the usage row key on.
//   - endpoint is endpointTranscription, because the flavor cannot tell a
//     transcription from a speech request: the endpoint is what picks the upstream
//     path (upstreamPath), the session-extraction case and the usage row's
//     provider_path. A shape that dropped it would label the row with the speech path.
//   - requiredCapabilities is [routing.CapabilityTranscription], which does two jobs,
//     as speech's does. It feeds the capability gate (a model without a
//     (transcription, yes) verdict is refused, an unprobed one included). And it keeps
//     the request off the vendor-account path: vendorAccountRoutingEligible has no
//     openai_audio twin of its images exclusion and declines a request only because it
//     carries a capability list, while a vendor account's model match reads the model
//     name and never the flavor. A handler that dropped the list would skip the gate
//     AND become routable to a vendor model of the same name.
func transcriptionInferenceShape(model string) inferenceShape {
	return inferenceShape{
		apiFlavor:            routing.APIFlavorOpenAIAudio,
		endpoint:             endpointTranscription,
		model:                model,
		requiredCapabilities: []string{routing.CapabilityTranscription},
	}
}

// handleOpenAITranscription serves POST /v1/audio/transcriptions (and its
// /openai/v1/audio/transcriptions alias, server.go) by relaying the client's
// multipart upload to a natively OpenAI-shaped speech-to-text backend (audio.cpp). It
// is handleOpenAISpeech's twin on the gate, the flavor and the preflight, and
// handleOpenAIImagesEdit's on the multipart relay: there is no translate path, the
// gateway proxies the upload and does not transcribe it, so an application that does
// not serve openai_audio is simply not a candidate.
//
// Auth: requireInternalOrBearerAnyScope admits a bearer token (every ordinary API
// client) OR the internal trusted-loopback header pair, DELIBERATELY not the browser
// session cookie -- see authenticateInternalOrBearer's own doc comment.
//
// The body is multipart/form-data, so the model the gate needs is peeked out of the
// FORM (parseTranscriptionForm). That happens, with the rest of validation
// (validateTranscriptionRequest), BEFORE inferencePreflight: a request with no model,
// no audio or a format this relay cannot honor is refused 400 without spending an
// admission. validateTranscriptionRequest answers the concrete *inference.Error, nil
// for a valid request, and is compared to nil before being handed on as an error.
//
// The capability gate runs inside the ONE existing admission gate: the request
// declares RequiredCapabilities = [routing.CapabilityTranscription]
// (transcriptionInferenceShape) and inferencePreflight -> Resolve refuses a model
// without a yes verdict. No second admitPrincipal call site is added here.
//
// Run-as: the shared runAsPrincipal helper. token.ID == "" only for the loopback
// principal, so this fires only for the run executor's own loopback calls.
func (s *Server) handleOpenAITranscription(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readTranscriptionBody(w, r, s.transcriptionBodyLimit())
	if !ok {
		return
	}
	form, err := parseTranscriptionForm(r.Header.Get(contentTypeHeader), raw)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if verr := validateTranscriptionRequest(form); verr != nil {
		writeRequestError(w, verr)
		return
	}
	token, ok = s.runAsPrincipal(w, r, token)
	if !ok {
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, transcriptionInferenceShape(form.routingModel()))
	if handled {
		return
	}
	s.relayTranscription(w, r, token, pf.Req, form, raw)
}

// transcriptionTargetServed reports whether target can be sent a transcription. An
// agent-launched (server_agent) target is sent the upload through the agent's runtime
// router, which peeks the model out of the body to find the child to start; only an
// agent that has DECLARED multipart_proxy peeks it out of a multipart one, and a
// router that has not answers 404 for every upload -- a failure no later signal
// distinguishes from an unknown model. So an agent target is served only on positive
// evidence: AgentFeatures.Has is false for a server that has never reported (a
// gateway restart empties the registry until the next telemetry sample), for one
// whose last sample lacked the name, and for a nil registry.
//
// Every other target is served: an external openai_audio target (an audio.cpp the
// operator registered as an application) has no agent in front of it. The decision is
// on the TARGET's provider, not on whether its server happens to run an agent. It is
// imageEditTargetServed's twin, on the same feature.
func (s *Server) transcriptionTargetServed(target routing.Target) bool {
	if target.Provider != routing.ProviderServerAgent {
		return true
	}
	return s.AgentFeatures.Has(target.ServerID, multipartProxyFeature)
}

// recordTranscriptionRoutingFailure writes the client-facing error response for a
// terminal transcription routing failure and records its usage row, so the relay's
// refusal sites -- an outright resolveTarget failure, the post-resolve
// effective-flavor conjunct, the multipart_proxy gate, a failed rebuild and the agent
// body limit -- cannot drift in what they record or capture. It is
// recordSpeechRoutingFailure's twin: the row is recorded against routing.Target{}
// because none of them reached proxyNative, so no upstream call was made, and a
// populated Host/RouteID/ProviderPath would claim one that never happened (and make
// the energy reconciler price the row against a server that never saw the request).
//
// The row carries usage.BillingUnitAudioSecond with a zero quantity, set here
// unconditionally: the unit is the ENDPOINT's identity, and the default "" would
// record a refused transcription as token-metered with a zero measure, the lie the
// (unit, quantity) pair exists to prevent. Endpoint is endpointTranscription for the
// row's sake, though with an empty target no provider_path is derived from it.
func (s *Server) recordTranscriptionRoutingFailure(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte, start time.Time, err error) {
	id := nextRequestID()
	capturing := s.capturingEnabled(token)
	status := completionHTTPStatus(err)
	code := completionErrorCode(err)
	// Warn for an admission-queue rejection, mirroring tryProxyNative's own severity
	// split for the same error pair; Debug for every other routing failure (no route,
	// no healthy host, not capable), which is the ordinary, expected shape of the gate
	// doing its job.
	if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
		slog.Warn("transcription request admission rejected", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status)
	} else {
		slog.Debug("transcription request rejected: routing failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status, "err", err)
	}
	body := writeCompletionErrorCaptured(w, err)
	s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: endpointTranscription}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, status, req.APIFlavor), nil)
}

// resolveTranscriptionTarget is resolveSpeechTarget's twin: it resolves the routing
// target for an already-gated transcription and applies the effective-served
// conjunct to it. On a refusal it has already written the client's response and
// recorded the usage row, and answers ok == false.
//
// token is a pointer for the reason tryProxyNative's is: resolveTarget refreshes the
// caller's last-used-model snapshot through it, and the relay that follows must carry
// that refreshed token.
func (s *Server) resolveTranscriptionTarget(w http.ResponseWriter, r *http.Request, token *auth.Token, req inference.Request, raw []byte, start time.Time) (routing.Target, bool) {
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		s.recordTranscriptionRoutingFailure(w, r, *token, req, raw, start, err)
		return routing.Target{}, false
	}
	// The effective-served rule is a CONJUNCT (§6 / ADR-033): flavor ∈ APIFlavors AND
	// the endpoint is not disabled. For a server_agent mapping the resolved runtime
	// spec -- not the application -- is the authority for its model's flavors
	// (targetFrom), and candidacy gated that app type on its coarser app-level flavors
	// only. The transcription relay reaches proxyNative directly, so without this an
	// agent-managed child whose spec lists only text would still serve a transcription.
	if !targetServesFlavor(target, req.APIFlavor) {
		// The refusal looks like candidacy's own no-route answer WHEN THIS WAS THE ONLY
		// ROUTE for the model, and is not retried against a sibling application that
		// might still serve the request -- the same limitation resolveSpeechTarget and
		// resolveImagesTarget carry.
		slog.Debug("transcription request rejected: resolved target's effective flavors exclude audio",
			"path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model,
			"server", s.serverName(target.ServerID), "route_id", target.RouteID)
		s.recordTranscriptionRoutingFailure(w, r, *token, req, raw, start, routing.ErrNoModelRoute)
		return routing.Target{}, false
	}
	return target, true
}

// transcriptionNeedsModelRewrite reports whether the relayed form's model field must
// be replaced with the mapping's own name for the model (routing.Target.ProviderModel,
// the mapping's AppModelName): for ANY target whose name is set and is not the one the
// form already carries. The client names the GATEWAY's model, and the two are
// independent fields of a mapping. An agent-launched target's router keys the child it
// starts on the mapped name; an external upstream is handed it exactly as the JSON
// relay hands it (rewriteModelField sets it for every provider). The comparison is
// against the field as the client sent it (untrimmed), so a request that reached the
// mapping under a differently-spelled name -- or through an override or redirect that
// replaced it -- is rewritten too; a form that already carries the mapped name is left
// alone. It is imageEditNeedsModelRewrite's twin.
func transcriptionNeedsModelRewrite(target routing.Target, formModel string) bool {
	return target.ProviderModel != "" && formModel != target.ProviderModel
}

// streamEdit is what rebuildTranscriptionBody does to the client's `stream` part.
type streamEdit int

const (
	// streamKeep copies the part exactly as the client sent it.
	streamKeep streamEdit = iota
	// streamDrop omits it: the request is answered buffered, a plain one-shot
	// transcription.
	streamDrop
	// streamSetTrue writes it, in its place, as the literal true. A client may spell a
	// boolean stream any way strconv.ParseBool reads (1, t, TRUE, ...), but audio.cpp is
	// not known to read more than true; a spelling it ignores would be answered one-shot,
	// and the relay -- which expects a stream -- would be left running it under the idle
	// watchdog instead of the first-byte grace a stream gets.
	streamSetTrue
)

// rebuildTranscriptionBody returns raw as a new multipart body and its Content-Type (a
// fresh boundary), with up to three changes: the value of the model field is replaced
// by model when that is non-empty (the field the agent's router, and any model-routing
// upstream, reads), every EMPTY `file` part is dropped, and the `stream` part is edited
// as stream says: dropped (a streamed request that is answered buffered: up front, when
// the route is not known to stream (relayTranscription), or as the retry of a refused
// stream, see bufferedTranscriptionBody) or stated as the literal true (a stream the
// route serves, whatever the client's spelling of the boolean). Every other part -- its
// headers, its bytes and its position -- is copied exactly as the client sent it.
//
// The empty parts are dropped because validation counts only the non-empty ones
// (validateTranscriptionRequest accepts one beside the real upload): audio.cpp reads
// the upload from `file`, and handed two parts of that name it could take the empty
// one. Dropping them makes the upstream see exactly the one file the gateway
// validated. A rebuild always keeps the model field (an empty model leaves it as
// sent): dropping it would leave the agent's router unable to find the child to start.
//
// It reads raw with the boundary parseTranscriptionForm already validated the body
// against -- every part of which that function walked to its end -- so an error here
// is not expected for a body it accepted. It is rebuildImageEditBody's twin, without a
// mask.
//
// The body is built in memory at roughly the client's size; the size cap
// (transcriptionBodyLimit) is applied to what the client sends, not to this, which is
// why relayTranscription checks what it hands an agent against the agent's own.
func rebuildTranscriptionBody(raw []byte, boundary, model string, stream streamEdit) ([]byte, string, error) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	var out bytes.Buffer
	out.Grow(len(raw) + len(model) + 512)
	mw := multipart.NewWriter(&out)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read part: %w", err)
		}
		if err := writeRebuiltTranscriptionPart(mw, part, model, stream); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return out.Bytes(), mw.FormDataContentType(), nil
}

// writeRebuiltTranscriptionPart writes one part of the client's form into the rebuilt
// body: the model field with model for its value when that is set, the `stream` part as
// stream says (nothing at all, or the literal true), nothing at all for an empty `file`
// part, and every other part -- its headers and its bytes -- as the client sent it.
func writeRebuiltTranscriptionPart(mw *multipart.Writer, part *multipart.Part, model string, stream streamEdit) error {
	name := part.FormName()
	if name == "stream" {
		switch stream {
		case streamDrop:
			return nil
		case streamSetTrue:
			if err := mw.WriteField("stream", "true"); err != nil {
				return fmt.Errorf("write stream field: %w", err)
			}
			return nil
		}
	}
	if model != "" && name == "model" {
		if err := mw.WriteField("model", model); err != nil {
			return fmt.Errorf("write model field: %w", err)
		}
		return nil
	}
	// The first byte of a `file` part says whether it is empty, without reading a
	// large upload into a second buffer. A part that breaks off before it is an error.
	var first []byte
	if name == "file" {
		var peek [1]byte
		n, err := io.ReadFull(part, peek[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read part %q: %w", name, err)
		}
		first = peek[:n]
	}
	pw, err := mw.CreatePart(part.Header)
	if err != nil {
		return fmt.Errorf("create part %q: %w", name, err)
	}
	if _, err := pw.Write(first); err != nil {
		return fmt.Errorf("write part %q: %w", name, err)
	}
	if _, err := io.Copy(pw, part); err != nil {
		return fmt.Errorf("copy part %q: %w", name, err)
	}
	return nil
}

// relayTranscription resolves the routing target for an already-gated transcription
// and relays it via proxyNative. The resolve and the effective-served conjunct are
// resolveTranscriptionTarget's, so an agent child whose spec excludes audio refuses a
// transcription exactly as it refuses speech. An agent target whose agent has not
// declared multipart_proxy is then refused transcription.unavailable
// (transcriptionTargetServed) instead of being relayed. req is pf.Req from the
// caller's inferencePreflight call, already carrying RequiredCapabilities =
// [routing.CapabilityTranscription].
//
// The client's multipart body is relayed VERBATIM, with the client's own
// Content-Type, unless there is something to change in it: the model field, when the
// target needs a different name in it than the form carries
// (transcriptionNeedsModelRewrite -- every target, agent or external), an empty
// `file` part the upstream must not see (form.emptyFiles), or the `stream` part when
// it is to be dropped or restated (below). Any of them makes the body be rebuilt ONCE
// (rebuildTranscriptionBody), sent with its own Content-Type; any other form stays
// verbatim. The Content-Type is ALWAYS set, the client's or the rebuilt
// one's: left empty, proxyNative would treat the body as JSON and try to rewrite a
// model field out of a form. nativeRelay.raw stays the client's bytes either way: that
// is what capture records. The relay names the transcriptions endpoint, so the
// upstream path AND the usage row's provider_path are /v1/audio/transcriptions.
//
// The body that is finally sent to an agent-launched target must fit the agent
// router's own request limit (agentRouterMaxBodyBytes), which the cap on what the
// client sends does not guarantee (rebuilding can grow a body): one that does not is
// refused here, recorded, instead of being sent.
//
// A form that asks for a stream (form.wantsStream) is relayed as one only when the
// mapped route is KNOWN to stream: its transcription_streaming verdict is yes
// (mappingStreamsTranscription). Then the relay is marked a stream
// (nativeRelay.pfReq.Stream) and carries bufferedRetry, the builder of the request
// proxyNative sends once more, buffered, when the upstream refuses to stream
// (peekStreamingRefusal) -- the net for a yes that has gone stale, a model swapped to
// offline mode behind the verdict's back. Any other answer (a no, no verdict at all,
// one that cannot be read) decides up front that the request is answered buffered:
// the client's form is rebuilt without its `stream` part and sent as a plain one-shot
// transcription, so the upstream is never asked for what it would refuse and the
// client's answer costs one upstream call. A stream the route serves is sent with the
// literal true as its value whatever the client's spelling of the boolean (1, TRUE, ...:
// strconv.ParseBool reads them, audio.cpp is not known to), so a spelling the upstream
// would ignore cannot turn a call the relay treats as a stream into a one-shot answer.
//
// proxyNative then finishes the exchange through this file's response side, keyed on
// the endpoint (the openai_audio flavor is speech's too): a non-2xx answer is
// normalised (relayTranscriptionUpstreamError); a 2xx event stream of a streamed
// request is relayed unbuffered by the native copier (isStreamedTranscription), read on
// its way by transcriptionStreamScan, billed from the upload's WAV header once the
// stream completed (setStreamedTranscriptionBillingQuantity) and captured as its
// transcript; any other 2xx one is buffered, metered from the timing block of its body
// and written verbatim (relayTranscriptionResponse).
func (s *Server) relayTranscription(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, form transcriptionForm, raw []byte) {
	start := time.Now()
	target, ok := s.resolveTranscriptionTarget(w, r, &token, req, raw, start)
	if !ok {
		return
	}
	// The multipart_proxy gate (transcriptionTargetServed): after the resolve and the
	// served-flavor conjunct, so a refusal those make still wins, and before anything
	// is built or sent, so an agent that cannot take the upload is never handed a
	// multipart body (it would 404). Like the conjunct it is NOT retried against
	// another application that might serve the model: the resolve has already picked
	// this target.
	if !s.transcriptionTargetServed(target) {
		slog.Debug("transcription refused: the target's agent has not declared multipart_proxy",
			"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID),
			"route_id", target.RouteID, "feature", multipartProxyFeature)
		s.recordTranscriptionRoutingFailure(w, r, token, req, raw, start, errTranscriptionUnavailable)
		return
	}
	rel := nativeRelay{
		token:       token,
		target:      target,
		path:        upstreamPath(target, req.APIFlavor, endpointTranscription),
		raw:         raw,
		pfReq:       req,
		endpoint:    endpointTranscription,
		contentType: r.Header.Get(contentTypeHeader),
	}
	// The stream-versus-buffered decision is made HERE, before the upstream body is
	// built, because answering buffered means a body without the client's `stream` part.
	streaming := form.wantsStream() && s.mappingStreamsTranscription(r.Context(), target.RouteID)
	dropStream := form.wantsStream() && !streaming
	stream := streamKeep
	switch {
	case dropStream:
		stream = streamDrop
	case streaming && form.stream != "true":
		// Any other spelling of the boolean is stated to the upstream as the literal.
		stream = streamSetTrue
	}
	if dropStream {
		slog.Debug("transcription asked to stream but the route is not known to: answering buffered",
			"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID),
			"route_id", target.RouteID, "capability", routing.CapabilityTranscriptionStreaming)
	}
	sent := len(raw)
	rewrite := transcriptionNeedsModelRewrite(target, form.model)
	model := ""
	if rewrite {
		model = target.ProviderModel
	}
	if rewrite || form.emptyFiles > 0 || stream != streamKeep {
		body, contentType, err := rebuildTranscriptionBody(raw, form.boundary, model, stream)
		if err != nil {
			// Not expected for a body parseTranscriptionForm accepted: it walked every
			// part of it to its end with the same reader and boundary. Were it to
			// happen it is answered as a failed relay (and recorded) rather than
			// sending the upstream a body whose model it would misread.
			slog.Error("transcription: rebuilding the multipart body failed",
				"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID), "err", err)
			s.recordTranscriptionRoutingFailure(w, r, token, req, raw, start, fmt.Errorf("rebuild multipart body: %w", err))
			return
		}
		rel.upstreamBody, rel.contentType = body, contentType
		sent = len(body)
	}
	if target.Provider == routing.ProviderServerAgent && sent > agentRouterMaxBodyBytes {
		slog.Debug("transcription refused: the body to send is past the agent router's request limit",
			"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID),
			"bytes", sent, "limit", agentRouterMaxBodyBytes)
		s.recordTranscriptionRoutingFailure(w, r, token, req, raw, start, errTranscriptionTooLarge)
		return
	}
	if streaming {
		// A client that asked for a stream, on a route known to stream, is sent one:
		// stream=true reaches the upstream, as the literal true whatever the client's
		// spelling of the boolean was. A yes that went stale (the model
		// was swapped to offline mode while the verdict stood) is answered with the
		// refusal proxyNative turns into the buffered retry built here. The relay's
		// request (pfReq) is marked a stream for the deadline policy, the active row
		// and the usage row; the preflight request is not, so the gate and the resolve
		// above judged the same request a plain transcription would be.
		rel.pfReq.Stream = true
		rel.bufferedRetry = func() ([]byte, string, error) {
			return bufferedTranscriptionBody(target, raw, form.boundary, model)
		}
	}
	s.proxyNative(w, r, rel)
}

// bufferedTranscriptionBody builds the request that is sent INSTEAD of a streamed one
// the upstream refused (see peekStreamingRefusal): the client's form, still
// held whole in memory, rebuilt with the `stream` part dropped and, as for the first
// attempt, the mapped model name and no empty `file` part. It is built only when the
// retry happens, so the common streamed call never holds a second copy of the upload.
//
// The body must fit the agent router's own request limit, as the first attempt's did
// (relayTranscription): dropping `stream` shrinks the form, but a client boundary
// shorter than the rebuilt one's can make the whole larger than what was checked. One
// that does not fit is errTranscriptionTooLarge, and the caller then relays the
// upstream's refusal instead.
func bufferedTranscriptionBody(target routing.Target, raw []byte, boundary, model string) ([]byte, string, error) {
	body, contentType, err := rebuildTranscriptionBody(raw, boundary, model, streamDrop)
	if err != nil {
		return nil, "", err
	}
	if target.Provider == routing.ProviderServerAgent && len(body) > agentRouterMaxBodyBytes {
		return nil, "", errTranscriptionTooLarge
	}
	return body, contentType, nil
}

// mappingStreamsTranscription reports whether the mapped route is known to answer a
// streamed transcription: its stored transcription_streaming verdict, keyed by the
// route (target.RouteID, the mapping id), is yes. It is the up-front half of the
// stream-versus-buffered decision; peekStreamingRefusal and the buffered retry remain
// the net under a yes that has gone stale.
//
// Everything but a yes is false: a no, no row at all (the verdict is "unknown" until a
// probe states the model's mode, and the answer for unknown is the buffered one-shot,
// which every model serves), a route that is no mapping (a vendor account's route id
// names none), no route store, and a read that fails. A failed read is logged at
// Warn: it is a store error and not an expected outcome, and it silently changes what
// the client was answered with.
//
// The verdict is read per request with a keyed MappingCapabilities, not carried on the
// routing.Target: the resolver builds a target for every inference shape and this is
// the one reader.
func (s *Server) mappingStreamsTranscription(ctx context.Context, routeID string) bool {
	if s.Routes == nil || routeID == "" {
		return false
	}
	rows, err := s.Routes.MappingCapabilities(ctx, routeID)
	if err != nil {
		slog.Warn("transcription streaming verdict could not be read; answering buffered",
			"route_id", routeID, "capability", routing.CapabilityTranscriptionStreaming, "err", err)
		return false
	}
	return routing.CapabilityRowsByName(rows)[routing.CapabilityTranscriptionStreaming].Verdict == routing.CapabilityYes
}

// transcriptionStreamingRefusal is the sentence audio.cpp's error carries when
// stream=true reaches a model that is not loaded with mode=streaming (an offline
// model). It answers HTTP 500, as it does for every error, so the sentence -- not the
// status alone -- is what tells this refusal from a server that is really failing.
const transcriptionStreamingRefusal = "requires a model configured with mode=streaming"

// transcriptionRefusalPeekBytes bounds how much of a 500 body is read looking for
// transcriptionStreamingRefusal: audio.cpp puts the sentence in a one-line error
// object, so the head of the body is where it is.
const transcriptionRefusalPeekBytes = 64 << 10

// replayedBody is a response body some of whose bytes have already been read: the
// reader yields them again in front of the rest, and Close closes the original.
type replayedBody struct {
	io.Reader
	io.Closer
}

// peekStreamingRefusal reports whether resp is the upstream's refusal to
// stream a transcription: HTTP 500 whose body names transcriptionStreamingRefusal.
// Any other status, and any other 500, is false -- those are the upstream's errors and
// reach the client as they always did.
//
// It is a peek, not a pure predicate: it reads the head of the body to look, and puts
// what it read back -- resp.Body is replaced by a body that still yields every byte, so
// an error that is not the refusal reaches relayTranscriptionUpstreamError whole.
func peekStreamingRefusal(resp *provider.ProxyResponse) bool {
	if resp.StatusCode != http.StatusInternalServerError {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, transcriptionRefusalPeekBytes))
	resp.Body = replayedBody{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), Closer: resp.Body}
	return err == nil && bytes.Contains(head, []byte(transcriptionStreamingRefusal))
}

// isStreamedTranscription reports whether a transcription's 2xx answer is relayed as
// a stream rather than buffered: the request asked for one (the relay's pfReq.Stream)
// AND the upstream answered with an event stream. Either alone is not enough. An
// upstream that answered a stream request with a plain JSON document did not stream,
// and its document is buffered, read for its timing block and written verbatim like
// any one-shot transcription; and an event stream nobody asked for is no stream this
// relay was set up for.
func isStreamedTranscription(endpoint sessionEndpoint, streamRequested bool, header http.Header) bool {
	if endpoint != endpointTranscription || !streamRequested {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header.Get(contentTypeHeader))
	return err == nil && mediaType == "text/event-stream"
}

// transcriptionDurationSeconds reads the billable quantity out of a transcription
// response body: the input audio's duration, timing.audio_duration_ms, in seconds. It
// reports whether the body stated a usable one -- a finite number greater than zero.
// An absent timing block or duration, a null, a non-number (a numeric STRING
// included: the document says "number", and guessing at "2000" would bill on the
// strength of a shape the upstream does not produce), zero, a negative, a value past
// float range and a body that is no JSON object all answer false -- none states how
// much audio was transcribed, and a NaN or infinite quantity would poison every sum
// the usage views take. (encoding/json refuses the bare NaN and Infinity spellings,
// so the explicit check is the second line of defence, not the first.)
//
// It is speechDurationSeconds' twin for a body instead of a header. audio.cpp writes
// the timing block AFTER the text, so the whole body has to be in hand
// (relayTranscriptionResponse buffers it); only the one numeric field is decoded, the
// transcript itself is skipped, not copied.
func transcriptionDurationSeconds(body []byte) (float64, bool) {
	var doc struct {
		Timing struct {
			AudioDurationMs *float64 `json:"audio_duration_ms"`
		} `json:"timing"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Timing.AudioDurationMs == nil {
		return 0, false
	}
	ms := *doc.Timing.AudioDurationMs
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 {
		return 0, false
	}
	return ms / 1000, true
}

// setTranscriptionBillingQuantity records, on meta, how many seconds of audio a
// finished transcription relay transcribed -- and does nothing at all for any other
// endpoint or a relay that did not finish cleanly, leaving BillingQuantity at 0 so
// nothing is asserted about what was transcribed. It is setSpeechBillingQuantity's
// twin and shares its reasoning: the quantity is the RESPONSE's own statement (never
// the request's), and is only trusted once the full body reached the client
// (status == "success"; a non-2xx answer, an unreadable or over-limit body and a
// client that went away mid-write all leave it 0).
//
// It is keyed on ex.endpoint, not the flavor: the openai_audio flavor is speech's
// too, and speech's quantity comes from a header this endpoint does not have.
//
// body is the upstream's FULL response body, the very bytes the client was sent. A
// successful relay with no usable duration is indistinguishable, in the recorded row
// alone, from a genuinely empty recording, and the (unit, quantity) pair has no
// "unknown" representation to fall back on. So the gap is logged once at Error with
// enough to find the request, instead of silently recording a bare zero (ADR-041).
// Only the body's size is logged: the body is the transcript, which is the user's
// content and has no business in a log line.
func setTranscriptionBillingQuantity(meta *usageMeta, ex nativeExchange, body []byte, status string) {
	if ex.endpoint != endpointTranscription || status != "success" {
		return
	}
	seconds, ok := transcriptionDurationSeconds(body)
	if !ok {
		slog.Error("transcription relay succeeded but the upstream reported no usable audio duration", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName, "field", "timing.audio_duration_ms", "body_bytes", len(body))
		return
	}
	meta.BillingQuantity = seconds
}

// transcriptionUpstreamErrorType and transcriptionUpstreamErrorCode are the GATEWAY's
// own classification for a transcription upstream error whose shape is not already an
// OpenAI error object (see normalizeTranscriptionUpstreamError), and for a 2xx answer
// the relay could not deliver whole (over the buffer bound, or broken off mid-body).
// They are fixed regardless of the upstream's HTTP status, for the reason the speech
// pair documents. The code is API surface.
const (
	transcriptionUpstreamErrorType = "upstream_error"
	transcriptionUpstreamErrorCode = "transcription.upstream_error"
)

// needsTranscriptionErrorRelay reports whether a native-passthrough response must go
// through relayTranscriptionUpstreamError instead of the buffered success path: a
// transcription whose upstream answered a non-2xx status. It is needsSpeechErrorRelay's
// twin and is keyed on the endpoint, not the flavor string the two share; the status
// test is the 2xx window proxyNative's own success path uses.
func needsTranscriptionErrorRelay(endpoint sessionEndpoint, statusCode int) bool {
	return endpoint == endpointTranscription && (statusCode < 200 || statusCode >= 300)
}

// normalizeTranscriptionUpstreamError turns a transcription upstream's non-2xx body
// into the bytes the client is sent, and reports whether it recognised the shape. It
// is normalizeSpeechUpstreamError's twin: an OpenAI error object
// ({"error":{"message":...}}) passes through untouched, byte for byte -- audio.cpp
// answers this way, with HTTP 500 and its own attested type "server_error" even for a
// client error, and re-normalising would restate that as a gateway-authored value --
// and a plain string ({"error":"..."}) is wrapped into an object whose message is that
// string verbatim and whose type and code are the gateway's own
// (transcriptionUpstreamErrorType, transcriptionUpstreamErrorCode). status is the
// upstream's HTTP status; it is not consulted, because the classification is
// deliberately independent of it.
//
// ok is false for anything else -- HTML from an intermediary, an empty body, JSON of
// another shape -- and the caller then relays the upstream bytes unchanged instead of
// guessing. The upstream's status is relayed as it came.
func normalizeTranscriptionUpstreamError(body []byte, status int) ([]byte, bool) {
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
			Type:    transcriptionUpstreamErrorType,
			Code:    transcriptionUpstreamErrorCode,
		}})
		return wrapped, true
	}
	return nil, false
}

// relayTranscriptionUpstreamError handles a non-2xx response from the transcription
// upstream (audio.cpp): it buffers the (bounded) body, normalises it via
// normalizeTranscriptionUpstreamError, and relays either the normalised body or -- when
// the shape is not recognised -- the original bytes unchanged, under the upstream's own
// status either way. It is relaySpeechUpstreamError's twin and is called from
// proxyNative behind needsTranscriptionErrorRelay.
//
// The whole body is read before anything is written, as speech's is: normalising needs
// all of it, and an error is one small payload -- a streamed request's too: a non-2xx
// status is the answer INSTEAD of a stream, not a part of one. The
// read is bounded by s.captureMaxBytes, the cap an ERROR body has no reason to exceed
// (the success path's bound is the transcription cap instead: a transcript is not an
// error body, and is never clipped).
//
// A failed relay produced no transcript: the row is an audio_second row with quantity
// 0, as recordTranscriptionRoutingFailure leaves it for a refused request.
func (s *Server) relayTranscriptionUpstreamError(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse) {
	upstreamBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
	errorCode := fmt.Sprintf("upstream.%d", resp.StatusCode)
	if readErr != nil {
		// A read failure this late (the status line arrived, the body broke) is the
		// class nativeTerminalStatus names provider.stream_copy_error on the streaming
		// path; named the same way here for one consistent code.
		errorCode = "provider.stream_copy_error"
		slog.Error("inference native passthrough copy error", "path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "err", readErr)
	} else {
		slog.Warn("inference native passthrough upstream error", "path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "status", resp.StatusCode)
	}

	written, sentContentType := upstreamBody, resp.Header.Get(contentTypeHeader)
	if body, ok := normalizeTranscriptionUpstreamError(upstreamBody, resp.StatusCode); ok {
		written, sentContentType = body, jsonContentType
	} else if sentContentType == "" {
		// Neither recognised shape: the upstream's bytes go out unchanged, under its own
		// Content-Type, or JSON when it named none (the relay's default).
		sentContentType = jsonContentType
	}
	w.Header().Set(contentTypeHeader, sentContentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(written)

	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: sentContentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), written, resp.StatusCode, ex.req.APIFlavor), nil)
}

// relayTranscriptionResponse finishes a transcription whose upstream answered 2xx with
// a one-shot JSON document -- every 2xx but the event stream of a streamed request
// (isStreamedTranscription). It is the one relay in proxyNative that BUFFERS a success
// instead of streaming it, and that is forced, not chosen: the billable duration is
// the timing block of the JSON body, written AFTER the transcript, so it cannot be
// known before the body has been read to its end -- and the gateway must not have sent
// a byte by then, or a body that turns out unreadable or too large could not be turned
// into an error.
//
// So the sequence is read, decide, write:
//
//  1. The body is read up to s.transcriptionBodyLimit() (the transcription cap, a
//     generous bound: a transcript of a 32 MiB upload is a few MB of text), through
//     io.LimitReader(limit+1) so that exceeding it is DETECTED instead of silently
//     cut. It is not captureMaxBytes: that bound is for error bodies, and clipping a
//     transcript there would hand the client a truncated success.
//  2. A body past the bound, or one that cannot be read in full, is an upstream error:
//     nothing of it is written (failTranscriptionRelay), nothing is billed.
//  3. Otherwise the buffered bytes go to the client VERBATIM -- the upstream's status
//     and Content-Type, the timing block kept (the gateway proxies, it does not
//     translate) -- and the quantity is read off the same bytes once they were
//     written (setTranscriptionBillingQuantity), so a client that went away mid-write
//     bills 0.
//
// The memory exposure under concurrency is therefore bounded by this cap per in-flight
// request, which is the price of metering a trailing field.
//
// ex carries this request's identity (see nativeExchange); the function both writes
// the client's response and records its usage row, as relayTranscriptionUpstreamError
// does. There is no idle watchdog and no usage scanner here: a one-shot document is
// not a stream (proxyNative stops the watchdog of a streamed request that was answered
// with one), and the row's token columns stay 0.
func (s *Server) relayTranscriptionResponse(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse) {
	limit := s.transcriptionBodyLimit()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case readErr != nil:
		// Same classification (client gone, copy error) the streaming path gives a body
		// that breaks off; the nil idle flag is its watchdog, which a non-stream never arms.
		_, errorCode := s.nativeTerminalStatus(r, resp.StatusCode, ex.req, ex.serverName, false, readErr, ex.start)
		s.failTranscriptionRelay(w, r, ex, errorCode, "the transcription upstream's response could not be read in full")
		return
	case int64(len(body)) > limit:
		slog.Error("transcription upstream response exceeds the buffer bound; refused instead of truncated", "id", ex.id, "path", r.URL.Path, "model", ex.req.Model, "server", ex.serverName, "limit_bytes", limit)
		s.failTranscriptionRelay(w, r, ex, transcriptionUpstreamErrorCode, "the transcription upstream's response is larger than the gateway buffers")
		return
	}

	contentType := resp.Header.Get(contentTypeHeader)
	if contentType == "" {
		contentType = jsonContentType
	}
	w.Header().Set(contentTypeHeader, contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	written, writeErr := w.Write(body)

	status, errorCode := s.nativeTerminalStatus(r, resp.StatusCode, ex.req, ex.serverName, false, writeErr, ex.start)
	meta := usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: contentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: ex.endpoint}
	setTranscriptionBillingQuantity(&meta, ex, body, status)
	capIn := buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), body, resp.StatusCode, ex.req.APIFlavor)
	if capIn != nil {
		capIn.RespBytes = int64(written)
		// The endpoint is what tells this JSON transcript from speech's binary WAV: both
		// carry the openai_audio flavor, and a 2xx speech capture is stored as a redacted
		// summary (isBinarySpeechResponse). The error captures of this file need no stamp:
		// the carve-out matters only for a 2xx, and theirs are all non-2xx.
		capIn.Endpoint = ex.endpoint
	}
	// resp.Header is the UPSTREAM response's headers, threaded in for the best-effort
	// vendor rate-limit scrape every native relay passes it for (a no-op here: a
	// capability-carrying request never resolves to a vendor account).
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, errorCode, status, meta, ex.id, capIn, resp.Header)
}

// failTranscriptionRelay answers a transcription whose 2xx upstream response could not
// be delivered whole -- over the buffer bound, or broken off mid-body -- with the
// gateway's own 502 transcription.upstream_error (an OpenAI error object of type
// upstream_error), and records it as a failed relay: errorCode on the row, an
// audio_second unit and quantity 0. Nothing of the upstream's body is written: a
// transcript that cannot be delivered whole is not delivered in part.
func (s *Server) failTranscriptionRelay(w http.ResponseWriter, r *http.Request, ex nativeExchange, errorCode, message string) {
	payload := writeJSONCaptured(w, http.StatusBadGateway, apierror.Body{Error: apierror.ErrorBody{
		Code:    transcriptionUpstreamErrorCode,
		Message: message,
		Type:    transcriptionUpstreamErrorType,
	}})
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: http.StatusBadGateway, ContentType: jsonContentType, BillingUnit: usage.BillingUnitAudioSecond, Endpoint: ex.endpoint}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), payload, http.StatusBadGateway, ex.req.APIFlavor), nil)
}
