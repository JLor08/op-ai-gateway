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
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"time"
)

// apiFlavorImages is the images endpoint's flavor string. It folds to the coarse
// "openai" through NormalizeAPIFlavor like every other openai* value (ADR-042),
// which is exactly why it cannot carry the capability gate on its own -- the
// gate keys on inference.Request.RequiredCapabilities instead. It exists for
// labelling (usage rows, upstreamPath) and for its own sessionEndpoint case,
// never for filtering.
const apiFlavorImages = "openai_images"

// billingUnitFor returns usage.BillingUnitImage when apiFlavor identifies an
// images request and usage.BillingUnitTokens (the zero value) otherwise. The
// name is deliberately NOT "imagesBillingUnit": proxyNative's three shared
// recordUsage call sites (native_passthrough.go -- the "provider is not a
// NativeProxyClient" branch, the pre-response ProxyNative-error branch, and
// the terminal success/error branch) call this on EVERY native-passthrough
// request, including every openai_responses and anthropic_messages one,
// where it answers "" -- a function whose own name claimed to be about
// images would misdescribe those calls. Each call site must read the unit
// from the CALLER's own endpoint identity, never default it or infer it from
// whatever (if anything) the upstream answered -- see usageMeta's own doc
// comment for why a response-derived unit would record a failed images
// request as token-metered with a zero measure, the exact lie the
// (unit, quantity) pair exists to prevent. relayImages and
// relayImagesUpstreamError below don't need this helper: both are
// images-only functions, so their own recordUsage calls set
// usage.BillingUnitImage directly.
func billingUnitFor(apiFlavor string) string {
	if apiFlavor == apiFlavorImages {
		return usage.BillingUnitImage
	}
	return usage.BillingUnitTokens
}

// imagesDataMarker is the JSON key an images response uses for one produced
// image (sd-server's b64_json response format; see the response_format
// decision below validateImagesRequest). The quotes rule out the marker
// appearing by CHANCE inside base64 image data (the base64 alphabet never
// emits a `"` byte) -- they do NOT, by themselves, distinguish a JSON KEY
// from a matching STRING VALUE: "b64_json" is also OpenAI's own canonical
// response_format value, so the identical quoted bytes appear verbatim in
// request-echoing fields too (response_format:"b64_json", a parameters
// object, even a prompt or revised_prompt whose text happens to be exactly
// "b64_json"). imagesDataCounter.feed's own colon check is what makes that
// distinction; see there.
const imagesDataMarker = `"b64_json"`

var imagesDataMarkerBytes = []byte(imagesDataMarker)

// imagesDataKeyLookahead bounds how many bytes past a matched
// imagesDataMarker occurrence feed looks for the byte that decides KEY vs
// VALUE: a ':' (after skipping ordinary JSON whitespace) means "b64_json" is
// being used as a KEY -- one produced image; anything else means it is a
// STRING VALUE and must not be counted (see imagesDataMarker's own doc
// comment for why quoting alone cannot tell the two apart). A real JSON key
// is followed by its colon with at most incidental whitespace, so this bound
// is ample while still capping a hostile upstream that pads a key occurrence
// with unbounded whitespace to grow feed's carry without limit.
const imagesDataKeyLookahead = 8

// imagesDataCounter counts KEY occurrences of imagesDataMarker AS THE
// RESPONSE BYTES PASS THROUGH THE COPIER (nativeCopier.writeChunk,
// native_passthrough.go), independently of the capture tee's bounded
// respBuf.
//
// That independence is the whole reason this exists rather than reading
// data's length off respBuf after the copy finishes: respBuf is capped at
// captureMaxBytes (defaultCaptureMaxBytes = 1<<20, 1 MiB -- see server.go),
// the SAME budget usageScanner's own doc comment (passthrough_usage_scan.go)
// explains is shared with the capture feature and nothing else. A
// base64-encoded image response routinely exceeds that: base64 inflates the
// raw bytes by ~33%, so a single modest PNG already sits in the hundreds of
// KB, and an n>1 request multiplies that. Reading data's length from a
// silently truncated respBuf would UNDER-REPORT the count on exactly the
// large responses this measure matters most for -- worse than recording no
// count at all (see relayImages' own doc comment on the (unit, quantity)
// pair, and billingUnitFor above). So this scans the FULL byte stream
// instead, keeping only a small, bounded carry across chunk boundaries --
// never the whole body -- the same reason usageScanner is fed independently
// of respBuf's cap.
//
// Failure mode: if a successful (2xx) response never contains the key AS A
// KEY at all -- a malformed/truncated body, or a shape this relay was not
// built against -- the count is 0, indistinguishable from a genuine empty
// data[]. This relay cannot tell "produced nothing" from "said nothing this
// counter recognises" any more precisely than that, so proxyNative logs that
// case at Error rather than recording it silently (native_passthrough.go,
// beside the BillingQuantity assignment) -- the same posture recordUsage
// itself takes on an XOR violation (inference_complete.go): make the
// unmeasurable case LOUD, never quietly indistinguishable from a genuine
// zero.
type imagesDataCounter struct {
	carry []byte
	count int
	// bytesSeen is the TOTAL number of response bytes fed, uncapped -- unlike
	// respBuf, so the "counted zero" log line above can report an honest body
	// size even for a response many times captureMaxBytes.
	bytesSeen int
}

// newImagesDataCounter answers a counter for an images request and nil for
// every other flavor, mirroring newUsageScanner's own nil-for-images
// convention (passthrough_usage_scan.go) -- the two are exact complements, and
// proxyNative builds both the same way so neither flavor test is spelled out at
// the call site. A nil counter is fed and read safely (every method here is
// nil-safe), so a non-images relay pays nothing.
func newImagesDataCounter(apiFlavor string) *imagesDataCounter {
	if apiFlavor != apiFlavorImages {
		return nil
	}
	return &imagesDataCounter{}
}

// markerRole is what resolveMarkerRole decides about ONE quoted
// imagesDataMarker occurrence inside feed's window.
type markerRole int

const (
	markerIsKey      markerRole = iota // resolved by ':' -- one produced image
	markerIsValue                      // resolved by any other byte -- a string value, never counted
	markerUnresolved                   // the window ended before any byte resolved it
)

// resolveMarkerRole decides whether the imagesDataMarker occurrence whose bytes
// end just before `after` in window is being used as a JSON KEY or as a string
// VALUE, by looking for the first byte from `after` on that is not whitespace:
// ':' means KEY, anything else means VALUE. See imagesDataMarker's own doc
// comment for why the marker's quotes alone cannot make that call.
//
// Two bounds cut the search short, and they answer DIFFERENTLY on purpose:
//
//   - Running out of WINDOW having seen nothing but whitespace is
//     markerUnresolved: the deciding byte may be the first byte of the next
//     upstream read, so feed defers the match into its carry rather than guess.
//   - Reaching the imagesDataKeyLookahead bound having seen nothing but
//     whitespace is markerIsValue: a real JSON key never puts that much space
//     before its colon, and this is the bound that stops a hostile upstream
//     padding a key occurrence with unbounded whitespace to grow feed's carry
//     without limit.
func resolveMarkerRole(window []byte, after int) markerRole {
	limit := after + imagesDataKeyLookahead
	ranOutOfWindow := false
	if limit >= len(window) {
		limit = len(window)
		ranOutOfWindow = true
	}
	j := after
	for j < limit && isJSONSpace(window[j]) {
		j++
	}
	switch {
	case j < limit:
		// A resolving (non-whitespace) byte was found within the window and
		// within the lookahead bound.
		if window[j] == ':' {
			return markerIsKey
		}
		return markerIsValue
	case ranOutOfWindow:
		return markerUnresolved
	default:
		return markerIsValue
	}
}

// feed counts new KEY occurrences of imagesDataMarker in chunk -- i.e. ones
// immediately followed, after skipping up to imagesDataKeyLookahead bytes of
// JSON whitespace, by ':' -- folding in the bounded carry retained from the
// previous call so a marker, or its still-unresolved key/value
// determination, split across two upstream reads is neither missed nor
// double-counted. A quoted match that resolves to anything other than ':' is
// a STRING VALUE and is never counted; see imagesDataMarker's own doc
// comment for why the quotes alone cannot make that call. The per-match
// decision itself, and the two bounds that cut it short, are
// resolveMarkerRole's above; feed owns the walk, the count and the carry.
// Nil-safe, matching usageScanner.feed's own convention, so a nativeCopier
// built without a counter (every non-images flavor) pays nothing.
func (c *imagesDataCounter) feed(chunk []byte) {
	if c == nil {
		return
	}
	window := make([]byte, len(c.carry)+len(chunk))
	copy(window, c.carry)
	copy(window[len(c.carry):], chunk)
	c.bytesSeen += len(chunk)

	pos := 0
	deferredFrom := -1
scan:
	for {
		i := bytes.Index(window[pos:], imagesDataMarkerBytes)
		if i < 0 {
			break
		}
		matchStart := pos + i
		after := matchStart + len(imagesDataMarkerBytes)
		switch resolveMarkerRole(window, after) {
		case markerIsKey:
			c.count++
			pos = after
		case markerIsValue:
			pos = after
		case markerUnresolved:
			// Every byte to the end of the CURRENT window was whitespace, and
			// the lookahead bound was not yet reached: the resolving byte may
			// be in the NEXT chunk. Defer this match to the next feed call
			// rather than guess.
			deferredFrom = matchStart
			break scan
		}
	}

	tailLen := len(imagesDataMarkerBytes) - 1
	if len(window) < tailLen {
		tailLen = len(window)
	}
	carryStart := len(window) - tailLen
	if deferredFrom >= 0 && deferredFrom < carryStart {
		carryStart = deferredFrom
	}
	tail := window[carryStart:]
	next := make([]byte, len(tail))
	copy(next, tail)
	c.carry = next
}

// total returns the number of images this response reports having produced.
// Nil-safe: a non-images request's nil counter reports 0, which proxyNative
// never actually reads -- it only consults this when the endpoint identity
// (pfReq.APIFlavor) is images.
func (c *imagesDataCounter) total() int {
	if c == nil {
		return 0
	}
	return c.count
}

// bytesFed returns the total number of response bytes this counter has
// seen, uncapped by captureMaxBytes -- see imagesDataCounter's bytesSeen
// field. Nil-safe, matching total().
func (c *imagesDataCounter) bytesFed() int {
	if c == nil {
		return 0
	}
	return c.bytesSeen
}

// handleOpenAIImages serves POST /v1/images/generations by relaying to a
// natively OpenAI-shaped image backend. There is no translate path: the gateway
// proxies image requests and does not synthesize them, so an application that
// does not serve this shape is simply not a candidate.
//
// The capability gate is what makes that true, and it runs inside the ONE
// existing admission gate: the request declares
// RequiredCapabilities = [routing.CapabilityImage] and inferencePreflight ->
// Resolve refuses a model without a yes verdict. No second admitPrincipal call
// site is added here; there is exactly one in this package and its comment
// records what broke when there were four.
func (s *Server) handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readRawJSONUnlimited(w, r)
	if !ok {
		return
	}
	// sd-server IGNORES the request's model field -- one process serves one
	// model -- so the model here is purely OUR routing input, read with the same
	// tolerant probe every native-passthrough endpoint uses.
	model, _ := sniffRoutingModel(raw)
	if err := validateImagesRequest(raw, model); err != nil {
		writeRequestError(w, err)
		return
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, inferenceShape{
		apiFlavor:            apiFlavorImages,
		endpoint:             endpointImages,
		model:                model,
		requiredCapabilities: []string{routing.CapabilityImage},
	})
	if handled {
		return
	}
	s.relayImages(w, r, token, pf.Req, raw)
}

// imagesResponseFormatUnsupported is validateImagesRequest's own code for a
// response_format this relay cannot honor -- see its check below for the
// decision and the reason.
const imagesResponseFormatUnsupported = "images.response_format_unsupported"

// imagesStreamUnsupported is validateImagesRequest's code for a request asking
// for a streamed response, which this relay does not produce -- same shape and
// same reasoning as imagesResponseFormatUnsupported; see the check below.
const imagesStreamUnsupported = "images.stream_unsupported"

// validateImagesRequest validates the shape this handler owns, instead of
// calling inference.Request.Validate: that validator requires len(Messages) > 0,
// and an images request has none, so calling it would reject every one of them.
// "prompt" (non-empty) and "model" (non-empty, resolved by the caller's own
// tolerant probe -- see sniffRoutingModel) are required. With TWO exceptions
// below (response_format and stream), every other field is the client's own
// business and reaches the upstream unexamined.
//
// response_format decision: only "b64_json" (sd-server's own shape) is
// accepted; an explicit "url" -- or anything else -- is REJECTED here with
// its own code, rather than relayed and billed afterwards. Two reasons, both
// about what happens AFTER this function returns, not before: sd-server has
// no way to host a URL for output it generates in-process, so a "url"
// request could only ever get an error or a shape this relay does not
// expect; and imagesDataCounter (below) is built specifically against the
// b64_json KEY -- a "url"-format response would relay successfully while
// every one of its images counts as 0 produced, which is exactly the
// silently-wrong measurement this feature exists to prevent (see
// imagesDataCounter's own doc comment). Rejecting the request up front turns
// that into a 400 the client can act on, instead of a usage row that quietly
// under-bills a response that streamed just fine.
//
// b64_json is NOT OpenAI's default, and the compatibility cost of this rule
// is therefore real: OpenAI's Images API defaults response_format to "url"
// for dall-e-2/dall-e-3, and gpt-image-1 does not accept the parameter at
// all. So a strict OpenAI client that explicitly sends the documented OpenAI
// default gets a 400 from this gateway. That is accepted deliberately --
// relaying a base64 body to a client that asked for URLs is a silent
// wire-contract violation, and a 400 is the only answer that is neither
// wrong nor silent -- but it is a PRECONDITION for using this endpoint, not
// a preference, and it is documented as such in
// docs/architecture/cross-cutting/compatibility-and-inference.md section 3.4.
//
// stream decision: the SAME reasoning, applied to the other parameter whose
// value this relay cannot honor. The gateway has pinned itself to the buffered
// path for this endpoint (Stream is false in proxyNative's request, and the
// deadline policy that follows from it is documented there), so relaying a
// client's `stream: true` unexamined would send the upstream a flag the gateway
// then ignores and hand the client one `application/json` body where it asked
// for a stream -- the same silent wire-contract violation the response_format
// rule exists to prevent, and 400 is the same only-non-silent answer.
// `stream: false` and an absent stream are both accepted: they describe what
// this endpoint already does. This is reachable rather than theoretical --
// OpenAI's gpt-image-1 does accept `stream`/`partial_images`, so a real client
// has a reason to send it.
//
// Both fields are decoded as json.RawMessage and then type-checked, rather than
// straight into a string/bool. A single struct decode collects the FIRST type
// error and keeps going, and this function discards that error (deliberately --
// it is as tolerant as sniffRoutingModel about everything it does not own), so a
// non-string response_format such as ["url"] or 123 would leave the field at its
// zero value and walk straight past the one check this function makes. The raw
// form has no type to mismatch, so the check below sees the value that was
// actually sent. A JSON `null` decodes into either target as a no-op, which is
// the right reading: an explicit null is an absent value.
func validateImagesRequest(raw []byte, model string) error {
	if strings.TrimSpace(model) == "" {
		return &inference.Error{Code: "images.model_required", Message: "model is required"}
	}
	var body struct {
		Prompt         string          `json:"prompt"`
		ResponseFormat json.RawMessage `json:"response_format"`
		Stream         json.RawMessage `json:"stream"`
	}
	_ = json.Unmarshal(raw, &body)
	if strings.TrimSpace(body.Prompt) == "" {
		return &inference.Error{Code: "images.prompt_required", Message: "prompt is required"}
	}
	if len(body.ResponseFormat) > 0 {
		var format string
		if err := json.Unmarshal(body.ResponseFormat, &format); err != nil {
			return &inference.Error{Code: imagesResponseFormatUnsupported, Message: fmt.Sprintf("response_format must be the string \"b64_json\"; got %s", body.ResponseFormat)}
		}
		if format != "" && format != "b64_json" {
			return &inference.Error{Code: imagesResponseFormatUnsupported, Message: fmt.Sprintf("response_format %q is not supported; only \"b64_json\" is", format)}
		}
	}
	if len(body.Stream) > 0 {
		var stream bool
		if err := json.Unmarshal(body.Stream, &stream); err != nil {
			return &inference.Error{Code: imagesStreamUnsupported, Message: fmt.Sprintf("stream must be the boolean false or absent; got %s", body.Stream)}
		}
		if stream {
			return &inference.Error{Code: imagesStreamUnsupported, Message: "stream is not supported on this endpoint; it always returns a single buffered JSON response"}
		}
	}
	return nil
}

// relayImages resolves the routing target for an already-gated images request
// and relays it via proxyNative. It follows tryProxyNative's resolve-then-relay
// shape, minus the translate fallback: images has none, so unlike
// tryProxyNative -- which returns false on an ordinary routing failure and lets
// the caller's translate path re-resolve and record the error -- EVERY resolve
// failure here is terminal and recorded directly, not just an admission-queue
// rejection. req is pf.Req from the caller's inferencePreflight call, already
// carrying RequiredCapabilities = [routing.CapabilityImage].
func (s *Server) relayImages(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte) {
	start := time.Now()
	target, err := s.resolveTarget(r.Context(), &token, req)
	if err != nil {
		id := nextRequestID()
		capturing := s.capturingEnabled(token)
		status := completionHTTPStatus(err)
		code := completionErrorCode(err)
		// Warn for an admission-queue rejection, mirroring tryProxyNative's own
		// severity split for the same error pair; Debug for every other routing
		// failure (no route, no healthy host, not capable), which is the
		// ordinary, expected shape of this gate doing its job.
		if errors.Is(err, routing.ErrAdmissionQueueTimeout) || errors.Is(err, routing.ErrAdmissionQueueFull) {
			slog.Warn("images request admission rejected", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status)
		} else {
			slog.Debug("images request rejected: routing failed", "path", r.URL.Path, "api_flavor", req.APIFlavor, "model", req.Model, "code", code, "status", status, "err", err)
		}
		body := writeCompletionErrorCaptured(w, err)
		// BillingUnit is set here unconditionally (not via billingUnitFor):
		// relayImages is an images-only function, so req.APIFlavor is always
		// apiFlavorImages -- the unit is endpoint identity, set on EVERY
		// recordUsage call this path makes, success and failure alike (see
		// usageMeta's own doc comment). BillingQuantity is left at its zero
		// value: nothing was produced by a resolve failure.
		s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, code, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: status, ContentType: jsonContentType, BillingUnit: usage.BillingUnitImage}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, status, req.APIFlavor))
		return
	}
	s.proxyNative(w, r, nativeRelay{
		token:    token,
		target:   target,
		path:     upstreamPath(target, req.APIFlavor),
		raw:      raw,
		pfReq:    req,
		endpoint: endpointImages,
	})
}

// relayImagesUpstreamError handles a non-2xx response from the images
// upstream (sd-server): it buffers the (bounded) body, normalises it via
// normalizeImagesUpstreamError, and relays either the normalised OpenAI object
// or -- when the body matches neither recognised shape -- the original bytes
// unchanged, never guessed at. Called from proxyNative (native_passthrough.go)
// behind needsImagesErrorRelay, so no other native-passthrough flavor's error
// handling changes.
//
// ex carries this request's identity -- principal, session-enriched request,
// target, client bytes, and the start/id/capturing bookkeeping -- as one value.
// This function both writes the client's response and records its usage row, so
// it needs everything proxyNative's own terminal step needs; see nativeExchange
// (native_passthrough.go).
//
// Reads the whole body via io.ReadAll rather than proxyNative's streaming
// nativeCopier: images never streams (Stream is pinned false in
// handleOpenAIImages), so sd-server's response is always a single buffered
// payload, and normalising it requires seeing all of it before writing
// anything to the client -- the opposite of the copier's byte-as-it-arrives
// design. The read is bounded by s.captureMaxBytes (the same cap the copier's
// own capture buffer already uses, not widened here): an error body has no
// reason to exceed it.
func (s *Server) relayImagesUpstreamError(w http.ResponseWriter, r *http.Request, ex nativeExchange, resp *provider.ProxyResponse) {
	upstreamBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(s.captureMaxBytes)))
	errorCode := fmt.Sprintf("upstream.%d", resp.StatusCode)
	if readErr != nil {
		// A read failure this late (upstream status line already arrived, body
		// broke mid-read) is the same class nativeTerminalStatus calls
		// provider.stream_copy_error on the streaming path; named the same way
		// here for one consistent code across both.
		errorCode = "provider.stream_copy_error"
		slog.Error("inference native passthrough copy error", "path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "err", readErr)
	} else {
		slog.Warn("inference native passthrough upstream error", "path", r.URL.Path, "api_flavor", ex.req.APIFlavor, "model", ex.req.Model, "server", ex.serverName, "status", resp.StatusCode)
	}

	var written []byte
	var sentContentType string
	if body, ok := normalizeImagesUpstreamError(resp.StatusCode, upstreamBody); ok {
		written = writeJSONCaptured(w, resp.StatusCode, body)
		sentContentType = jsonContentType
	} else {
		// Neither recognised shape: relay upstreamBody unchanged rather than
		// guess at one, per normalizeImagesUpstreamError's own contract.
		sentContentType = resp.Header.Get("Content-Type")
		if sentContentType == "" {
			sentContentType = jsonContentType
		}
		w.Header().Set("Content-Type", sentContentType)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(upstreamBody)
		written = upstreamBody
	}

	// BillingUnit is set unconditionally, as in relayImages above:
	// relayImagesUpstreamError is images-only, so ex.req.APIFlavor is always
	// apiFlavorImages. A non-2xx upstream response is still a non-token
	// request -- BillingQuantity stays 0, nothing was produced.
	s.recordUsage(ex.start, ex.token, ex.req, ex.target, provider.Response{}, errorCode, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: resp.StatusCode, ContentType: sentContentType, BillingUnit: usage.BillingUnitImage}, ex.id, buildCaptureInput(ex.capturing, ex.token.UserID, ex.token.Secret, r, ex.raw, w.Header(), written, resp.StatusCode, ex.req.APIFlavor))
}

// needsImagesErrorRelay reports whether a native-passthrough response must go
// through relayImagesUpstreamError instead of proxyNative's streaming copier: an
// images-flavor request whose upstream answered a non-2xx status, whose error
// SHAPE has to be normalised before the client sees it.
//
// It is deliberately narrow on both axes. The flavor test is what keeps every
// other native-passthrough error (openai_responses, anthropic_messages) on the
// copier, relayed byte-for-byte exactly as before this endpoint existed; the
// status test is the same 2xx window proxyNative's own success path uses, so a
// 2xx images response still streams.
func needsImagesErrorRelay(apiFlavor string, statusCode int) bool {
	return apiFlavor == apiFlavorImages && (statusCode < 200 || statusCode >= 300)
}

// setImagesBillingQuantity records, on meta, how many images a finished relay
// reports having produced -- and does nothing at all for a non-images flavor or
// a request that did not finish cleanly, leaving BillingQuantity at 0 so
// nothing is asserted about what was produced.
//
// The quantity comes from the RESPONSE, not the request: n states what was asked
// for, data[] states what was produced, and a partial failure makes those
// differ. imgCounter's count is only trusted once the FULL body actually reached
// the client without error (status == "success" -- upstream 2xx, no idle
// timeout, no client disconnect, no copy error; a non-2xx images response never
// gets here at all, see needsImagesErrorRelay above).
//
// A successful relay that counted zero produced images is indistinguishable, in
// the recorded row alone, from a genuine empty data[] -- imagesDataCounter's own
// doc comment names this as its failure mode. The (unit, quantity) pair has no
// "unknown" representation to fall back on, so discoverability is the
// alternative: log it at Error, with enough to find the request, exactly as
// recordUsage logs an XOR violation rather than silently repairing the row.
//
// meta.ReqPath is read for that log line rather than taking the request a
// second time: it is the same r.URL.Path the caller already put there.
func setImagesBillingQuantity(meta *usageMeta, ex nativeExchange, counter *imagesDataCounter, status string) {
	if ex.req.APIFlavor != apiFlavorImages || status != "success" {
		return
	}
	n := counter.total()
	if n == 0 {
		slog.Error("images relay succeeded but counted zero produced images", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName, "body_bytes", counter.bytesFed())
	}
	meta.BillingQuantity = float64(n)
}

// imagesUpstreamErrorType and imagesUpstreamErrorCode are the GATEWAY's own
// classification for a normalised sd-server error (see
// normalizeImagesUpstreamError). They are fixed regardless of the upstream's
// HTTP status: sd-server's plain-string body carries no more information than
// the string itself, so inventing a status-dependent value would claim a
// distinction sd-server never drew.
const (
	imagesUpstreamErrorType = "upstream_error"
	imagesUpstreamErrorCode = "images.upstream_error"
)

// normalizeImagesUpstreamError converts sd-server's error shape into OpenAI's.
// sd-server returns {"error": "<plain string>"}; OpenAI clients expect
// {"error": {message, type, code}}.
//
// The type and code in the result are OURS, not the backend's: sd-server
// states neither, and nothing downstream may read them as an upstream
// statement. Recorded here because the alternative -- inventing a plausible
// upstream code -- would make a gateway-authored value indistinguishable from
// an attested one, which is the same class of error as reporting an unmeasured
// zero as a measurement.
//
// ok is false when the body is neither shape, and the caller then relays the
// upstream bytes unchanged rather than guessing.
func normalizeImagesUpstreamError(status int, body []byte) (apierror.Body, bool) {
	// Already OpenAI-shaped: pass through untouched. Re-normalising it would
	// overwrite the upstream's OWN type/code with ours, restating an attested
	// value as a gateway-authored one -- the inverse of the mistake this
	// function exists to avoid. Message is the discriminator: a plain-string
	// body's "error" key does not unmarshal into ErrorBody at all (its JSON
	// value is a string, not an object), so err != nil there and this branch
	// is skipped.
	var already apierror.Body
	if err := json.Unmarshal(body, &already); err == nil && already.Error.Message != "" {
		return already, true
	}

	// sd-server's own shape: {"error": "<plain string>"}.
	var plain struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &plain); err == nil && plain.Error != "" {
		return apierror.Body{Error: apierror.ErrorBody{
			Message: plain.Error,
			Type:    imagesUpstreamErrorType,
			Code:    imagesUpstreamErrorCode,
		}}, true
	}

	return apierror.Body{}, false
}
