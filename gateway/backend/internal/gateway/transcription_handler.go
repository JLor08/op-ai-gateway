// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
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
	// transcriptionStreamUnsupported is the code for a request asking for a streamed
	// response, which v1 does not produce; see validateTranscriptionRequest.
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
	// streamFormat is the stream_format field. The endpoint has no streamed answer,
	// so the only question about it is whether it was set at all.
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
// stream decision: `stream` on and ANY stream_format are refused with
// transcriptionStreamUnsupported. The relay's buffering and billing contract is
// that of a single complete JSON response, and relaying a streaming request would
// hand the client a shape the gateway has not been built, metered or tested for.
// An absent or empty stream, `stream` false and an absent or empty stream_format
// describe what the endpoint already does and are accepted (a multipart form has no
// null: an empty value is the closest thing to one, and is treated as absent, as the
// image edit treats an empty stream). A stream that is no boolean is refused too,
// rather than guessed at.
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
		stream, err := strconv.ParseBool(v)
		if err != nil {
			return &inference.Error{Code: transcriptionStreamUnsupported, Message: fmt.Sprintf("stream must be the boolean false or absent; got %q", v)}
		}
		if stream {
			return &inference.Error{Code: transcriptionStreamUnsupported, Message: "stream is not supported on this endpoint; it always returns a single complete JSON response"}
		}
	}
	if strings.TrimSpace(form.streamFormat) != "" {
		return &inference.Error{Code: transcriptionStreamUnsupported, Message: "stream_format is not supported on this endpoint; it always returns a single complete JSON response"}
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

// rebuildTranscriptionBody returns raw as a new multipart body and its Content-Type (a
// fresh boundary), with two changes: the value of the model field is replaced by
// model when that is non-empty (the field the agent's router, and any model-routing
// upstream, reads), and every EMPTY `file` part is dropped. Every other part -- its
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
func rebuildTranscriptionBody(raw []byte, boundary, model string) ([]byte, string, error) {
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
		if err := writeRebuiltTranscriptionPart(mw, part, model); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return out.Bytes(), mw.FormDataContentType(), nil
}

// writeRebuiltTranscriptionPart writes one part of the client's form into the rebuilt
// body: the model field with model for its value when that is set, nothing at all for
// an empty `file` part, and every other part -- its headers and its bytes -- as the
// client sent it.
func writeRebuiltTranscriptionPart(mw *multipart.Writer, part *multipart.Part, model string) error {
	name := part.FormName()
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
// (transcriptionNeedsModelRewrite -- every target, agent or external), or an empty
// `file` part the upstream must not see (form.emptyFiles). Either makes the body be
// rebuilt ONCE (rebuildTranscriptionBody), sent with its own Content-Type; any other
// form stays verbatim. The Content-Type is ALWAYS set, the client's or the rebuilt
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
// proxyNative relays the upstream's answer as it stands. What it does NOT yet do for
// this endpoint is meter the duration out of the response body or normalise an
// upstream error.
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
		path:        upstreamPath(target, apiFlavorSpeech, endpointTranscription),
		raw:         raw,
		pfReq:       req,
		endpoint:    endpointTranscription,
		contentType: r.Header.Get(contentTypeHeader),
	}
	sent := len(raw)
	rewrite := transcriptionNeedsModelRewrite(target, form.model)
	if rewrite || form.emptyFiles > 0 {
		model := ""
		if rewrite {
			model = target.ProviderModel
		}
		body, contentType, err := rebuildTranscriptionBody(raw, form.boundary, model)
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
	s.proxyNative(w, r, rel)
}
