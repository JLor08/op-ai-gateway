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
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"time"
)

// maxImageEditBodyBytes bounds the multipart read of POST /v1/images/edits. The
// body is held in memory whole -- the handler peeks the form before routing and
// may rebuild it afterwards -- so, unlike the JSON endpoints (readRawJSONUnlimited
// is uncapped on purpose, to match llama-swap), an unbounded read here is a
// memory exposure the endpoint has to answer for. The value matches the agent
// runtime router's own request cap, which an edit to an agent-launched child
// still has to fit under, rebuilt or not. The cap applies to what the client
// sends, so a rebuild that grows the body (the model-name rewrite grows it by
// at most the difference of two names) is not itself bounded by it. It is a
// constant, not yet an operator setting.
const maxImageEditBodyBytes = 32 << 20

// maxImageEditFieldBytes bounds ONE text field the handler reads out of the form
// (model, prompt, response_format, stream). The image and mask parts are never
// read into a field: they are counted and skipped.
const maxImageEditFieldBytes = 1 << 20

// invalidImageEditRequest is the code for a structurally broken edit body: not
// multipart, no boundary, truncated or malformed parts, a field repeated where
// one answer is required. It is the generic request.invalid writeRequestError
// itself falls back to, named here so the handler's own refusals read as what
// they are.
const invalidImageEditRequest = "request.invalid"

// imageEditForm is what the handler needs out of an edit's multipart form before
// it can route the request: the routing model, the fields the validation twin
// checks, and whether any source image arrived. It is deliberately NOT a general
// form decoding -- every other field (n, size, mask, strength, output_format,
// the extra-args prompt block, ...) is the client's own business and reaches the
// upstream unexamined, exactly as the generations endpoint's JSON does.
type imageEditForm struct {
	// boundary is the multipart boundary of the CLIENT's body, kept so a body
	// that has to be rebuilt can be read back with the same reader.
	boundary string
	// model is the model field exactly as the client sent it. Routing uses the
	// trimmed value (routingModel), like sniffRoutingModel does for JSON; the
	// untrimmed one is what a verbatim relay would hand an agent's router, so it
	// is what the rewrite decision compares.
	model          string
	prompt         string
	responseFormat string
	stream         string
	// images counts the NON-EMPTY parts under `image` or `image[]`, the two
	// spellings OpenAI's clients use for the source image(s).
	images int
}

func (f imageEditForm) routingModel() string { return strings.TrimSpace(f.model) }

func invalidImageEdit(message string) error {
	return &inference.Error{Code: invalidImageEditRequest, Message: message}
}

// handleOpenAIImagesEdit serves POST /v1/images/edits (and its
// /openai/v1/images/edits alias, server.go): an OpenAI-shaped image EDIT,
// relayed as multipart to a natively OpenAI-shaped image backend (sd-server).
// It is the gateway's first multipart request path, and it serves the public
// bearer endpoint and the portal (reached server-side over loopback) alike.
//
// Everything about the gate is handleOpenAIImages' -- the same auth, the same
// run-as block, the same preflight and relay stages -- with these differences:
//
//   - The body is multipart/form-data, so the model the gate needs is peeked out
//     of the FORM rather than a JSON probe (parseImageEditForm). That happens,
//     with the rest of validation, BEFORE inferencePreflight: a request with no
//     model, prompt or image is refused 400 without spending an admission.
//   - The required capability is routing.CapabilityImageEdit, set here from the
//     handler's identity and never from anything in the body. It is derived (a
//     model that generates images may be edited unless an operator opted out),
//     which is also what lets the unknown-model redirect find a target.
//   - The relay names the edits endpoint (nativeRelay.endpoint) and its upstream
//     path, and carries the body's own content type.
//
// Auth is requireInternalOrBearerAnyScope for the reasons handleOpenAIImages
// gives: a bearer or the internal loopback pair, never a browser session cookie.
func (s *Server) handleOpenAIImagesEdit(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireInternalOrBearerAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)
	if !ok {
		return
	}
	liftInferenceDeadlines(w)
	raw, ok := readImageEditBody(w, r)
	if !ok {
		return
	}
	form, err := parseImageEditForm(r.Header.Get("Content-Type"), raw)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if err := validateImageEditRequest(form); err != nil {
		writeRequestError(w, err)
		return
	}
	// Run-as: handleOpenAIImages' block, which is handleOpenAIChat's -- copied for
	// the same reason (the surrounding code differs too much to factor out) and
	// with the same effect: only the loopback principal (token.ID == "") ever
	// reaches it, and without it a portal edit would bill and capture against the
	// bare session instead of the token the run executes as.
	if token.ID == "" {
		if runAsID := strings.TrimSpace(r.Header.Get(runAsHeaderName)); runAsID != "" {
			if s.Portal == nil {
				// Fail closed: the caller explicitly asked to run as a specific
				// token and, with no Portal, there is no way to authorize it.
				writePortalTokenError(w, portal.ErrTokenForbidden)
				return
			}
			runAs, rErr := s.Portal.AuthorizeRunAsToken(r.Context(), token, runAsID)
			if rErr != nil {
				writePortalTokenError(w, rErr)
				return
			}
			token = runAs
		}
	}
	pf, handled := s.inferencePreflight(w, r, token, raw, inferenceShape{
		apiFlavor:            apiFlavorImages,
		endpoint:             endpointImagesEdit,
		model:                form.routingModel(),
		requiredCapabilities: []string{routing.CapabilityImageEdit},
	})
	if handled {
		return
	}
	s.relayImageEdit(w, r, token, pf.Req, form, raw)
}

// readImageEditBody reads the whole multipart body, bounded by
// maxImageEditBodyBytes. A body past the cap is a 413 whether its length was
// declared (refused without reading a byte) or only discovered while reading.
// The JSON readers cannot be used here: readRawJSONLimit decodes one JSON value
// and refuses anything else.
func readImageEditBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.ContentLength > maxImageEditBodyBytes {
		writeBodyTooLarge(w)
		return nil, false
	}
	body := http.MaxBytesReader(w, r.Body, maxImageEditBodyBytes)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeBodyTooLarge(w)
			return nil, false
		}
		writeRequestError(w, invalidImageEdit("request body could not be read"))
		return nil, false
	}
	return raw, true
}

// parseImageEditForm walks the multipart body once with mime/multipart and the
// request's own boundary, and returns what routing and validation need. It does
// NOT use http.Request.ParseMultipartForm: that spills parts over a memory
// threshold to temp files and consumes the request body, and this handler needs
// the body intact afterwards (to relay it verbatim or to rebuild it).
//
// Only the fields the handler owns are read into memory, each bounded by
// maxImageEditFieldBytes; the image parts are counted and skipped, and every
// other part is skipped untouched. Each of model, prompt, response_format and
// stream may appear ONCE. The model decides routing and the allowlist and may be
// peeked again downstream (the agent's router), so two of them are two answers
// to "which model" and one must not be silently picked over the other; the rest
// are refused for the same uniformity rather than inventing a precedence.
//
// The whole body is walked even once everything needed has been seen, so a
// repeated field or a malformed tail is caught here rather than discovered by a
// consumer later.
func parseImageEditForm(contentType string, raw []byte) (imageEditForm, error) {
	var form imageEditForm
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return form, invalidImageEdit("request body must be multipart/form-data with a boundary")
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
			return form, invalidImageEdit("malformed multipart body")
		}
		name := part.FormName()
		switch name {
		case "model", "prompt", "response_format", "stream":
			if seen[name] {
				return form, invalidImageEdit(fmt.Sprintf("form field %q must appear only once", name))
			}
			seen[name] = true
			value, err := readImageEditField(part)
			if err != nil {
				return form, err
			}
			switch name {
			case "model":
				form.model = value
			case "prompt":
				form.prompt = value
			case "response_format":
				form.responseFormat = value
			case "stream":
				form.stream = value
			}
		case "image", "image[]":
			n, err := io.Copy(io.Discard, part)
			if err != nil {
				return form, invalidImageEdit("malformed multipart body")
			}
			if n > 0 {
				form.images++
			}
		}
	}
}

// readImageEditField reads one text field, bounded.
func readImageEditField(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxImageEditFieldBytes+1))
	if err != nil {
		return "", invalidImageEdit("malformed multipart body")
	}
	if len(b) > maxImageEditFieldBytes {
		return "", invalidImageEdit(fmt.Sprintf("form field %q is too large", part.FormName()))
	}
	return string(b), nil
}

// validateImageEditRequest is validateImagesRequest's twin for the multipart
// shape: a non-empty model and prompt and at least one source image are
// required, response_format may only be "b64_json", and stream may not be on.
// The reasons for the last two rules are validateImagesRequest's, and apply
// unchanged -- the edits response is the same b64_json document, counted the
// same way, and the relay is the same buffered one -- so a request that fails
// either is refused 400 rather than relayed and mis-measured. As there, a
// stream of false and an absent or empty one are both accepted: they describe
// what this endpoint already does.
//
// Every other field reaches the upstream unexamined.
func validateImageEditRequest(form imageEditForm) error {
	if form.routingModel() == "" {
		return &inference.Error{Code: "images.model_required", Message: "model is required"}
	}
	if strings.TrimSpace(form.prompt) == "" {
		return &inference.Error{Code: "images.prompt_required", Message: "prompt is required"}
	}
	if form.images == 0 {
		return &inference.Error{Code: imagesImageRequired, Message: "at least one non-empty image is required (form field \"image\" or \"image[]\")"}
	}
	if format := strings.TrimSpace(form.responseFormat); format != "" && format != "b64_json" {
		return &inference.Error{Code: imagesResponseFormatUnsupported, Message: fmt.Sprintf("response_format %q is not supported; only \"b64_json\" is", format)}
	}
	if v := strings.TrimSpace(form.stream); v != "" {
		stream, err := strconv.ParseBool(v)
		if err != nil {
			return &inference.Error{Code: imagesStreamUnsupported, Message: fmt.Sprintf("stream must be the boolean false or absent; got %q", v)}
		}
		if stream {
			return &inference.Error{Code: imagesStreamUnsupported, Message: "stream is not supported on this endpoint; it always returns a single buffered JSON response"}
		}
	}
	return nil
}

// imageEditNeedsModelRewrite reports whether the relayed form's model field must
// be replaced with the mapping's AppModelName: only for an agent-launched
// (server_agent) target, whose agent router keys the child it starts on that
// name -- routing.Target.ProviderModel -- and which is not the name the client
// sent (the client names the GATEWAY's model; the two are independent fields of
// a mapping). The comparison is against the field as the client sent it, so a
// request that reached the mapping under a differently-spelled name is rewritten
// too. An EXTERNAL target is never rewritten: sd-server ignores the model field
// (one process serves one model), so the client's bytes are relayed verbatim,
// which is also the cheapest and the least damaging thing to do to a body that
// can carry several megabytes of binary.
func imageEditNeedsModelRewrite(target routing.Target, formModel string) bool {
	return target.Provider == routing.ProviderServerAgent &&
		target.ProviderModel != "" &&
		formModel != target.ProviderModel
}

// rebuildImageEditBody returns raw with the value of its model field replaced by
// model, as a new multipart body and its Content-Type (a fresh boundary). Every
// other part -- its headers, its bytes, and its position -- is copied exactly as
// the client sent it, so only the field the agent's router reads changes. It
// reads raw with the boundary parseImageEditForm already validated the body
// against, so an error here is a defect rather than a bad request.
//
// The body is built in memory at roughly the client's size; the size cap
// (maxImageEditBodyBytes) is applied to what the client sends, not to this.
func rebuildImageEditBody(raw []byte, boundary, model string) ([]byte, string, error) {
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
		if part.FormName() == "model" {
			if err := mw.WriteField("model", model); err != nil {
				return nil, "", fmt.Errorf("write model field: %w", err)
			}
			continue
		}
		pw, err := mw.CreatePart(part.Header)
		if err != nil {
			return nil, "", fmt.Errorf("create part %q: %w", part.FormName(), err)
		}
		if _, err := io.Copy(pw, part); err != nil {
			return nil, "", fmt.Errorf("copy part %q: %w", part.FormName(), err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return out.Bytes(), mw.FormDataContentType(), nil
}

// relayImageEdit resolves the routing target for an already-gated edit and
// relays it via proxyNative. The resolve and the effective-served conjunct are
// the generations relay's own (resolveImagesTarget), so an agent child whose spec
// excludes images refuses an edit exactly as it refuses a generation. req is
// pf.Req from the caller's inferencePreflight call, already carrying
// RequiredCapabilities = [routing.CapabilityImageEdit].
//
// What differs from relayImages is the request the upstream is sent. The client's
// multipart body is relayed VERBATIM, with the client's own Content-Type, unless
// the target is an agent whose router needs a different model name in the form
// (imageEditNeedsModelRewrite), in which case the body is rebuilt with that one
// field changed and sent with its own Content-Type. nativeRelay.raw stays the
// client's bytes either way: that is what capture records. The relay names the
// edits endpoint, so the upstream path AND the usage row's provider_path are
// /v1/images/edits -- left unset, the endpoint defaults to the generations one in
// both places.
func (s *Server) relayImageEdit(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, form imageEditForm, raw []byte) {
	start := time.Now()
	target, ok := s.resolveImagesTarget(w, r, &token, req, raw, start)
	if !ok {
		return
	}
	rel := nativeRelay{
		token:       token,
		target:      target,
		path:        upstreamPath(target, req.APIFlavor, endpointImagesEdit),
		raw:         raw,
		pfReq:       req,
		endpoint:    endpointImagesEdit,
		contentType: r.Header.Get("Content-Type"),
	}
	if imageEditNeedsModelRewrite(target, form.model) {
		body, contentType, err := rebuildImageEditBody(raw, form.boundary, target.ProviderModel)
		if err != nil {
			// Unreachable for a body parseImageEditForm accepted: it walked the same
			// parts with the same boundary. Answered as a failed relay (and
			// recorded) rather than sending the agent a body whose model it would
			// not recognise.
			slog.Error("image edit: rebuilding the multipart body for the agent target failed",
				"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID), "err", err)
			s.recordImagesRoutingFailure(w, r, token, req, raw, start, fmt.Errorf("rebuild multipart body: %w", err))
			return
		}
		rel.upstreamBody, rel.contentType = body, contentType
	}
	s.proxyNative(w, r, rel)
}
