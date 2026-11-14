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
	"net/textproto"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// defaultMaxImageEditBodyBytes is the default bound on the multipart read of POST
// /v1/images/edits, an operator setting (ServerDeps.MaxImageEditBodyBytes, config
// OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES; the Server's value is
// imageEditBodyLimit). The body is held in memory whole -- the handler peeks the
// form before routing and may rebuild it afterwards (a mask conversion, a model
// rewrite) -- so, unlike the JSON endpoints (readRawJSONUnlimited is uncapped on
// purpose, to match llama-swap), an unbounded read here is a memory exposure the
// endpoint has to answer for. The default is the agent runtime router's own
// request cap (agentRouterMaxBodyBytes).
//
// The relation between the two caps: this one bounds what the CLIENT sends, and
// an edit to an agent-launched model must also fit what the agent accepts after
// the gateway has prepared it -- and preparing can GROW a body (the converted
// mask of a highly compressed one, the longer model name, the new boundary). So
// the body actually handed to an agent target is checked against the agent's cap
// (relayImageEdit, errImageEditTooLarge), and a client body under this cap but
// whose prepared form is over that one is refused with a 413 of the gateway's
// own instead of being sent to come back as the agent's. Raising this setting
// above the agent's cap therefore never lets an oversized body reach an agent;
// it only lets larger ones through to external edit upstreams.
const defaultMaxImageEditBodyBytes = 32 << 20

// agentRouterMaxBodyBytes is the largest request the server agent's runtime router
// accepts (maxBodyBytes in server-agent/internal/runtime/router.go, a separate
// module this one does not import; it answers a larger one 413
// runtime.request_too_large). Keep the two equal.
const agentRouterMaxBodyBytes = 32 << 20

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

// malformedMultipartBodyMessage is the message of the invalidImageEditRequest
// refusal for a body whose parts cannot be walked to their end: the one the form
// parse (parseImageEditForm, readImageEditField) and the mask conversion
// (convertImageEditMask) both give for it.
const malformedMultipartBodyMessage = "malformed multipart body"

// imageEditForm is what the handler needs out of an edit's multipart form before
// it can route the request: the routing model, the fields the validation twin
// checks, and whether a source image and a mask arrived. It is deliberately NOT a
// general form decoding -- every other field (n, size, output_format, the
// extra-args prompt block that carries strength, ...) is the client's own business
// and reaches the upstream unexamined, exactly as the generations endpoint's JSON
// does. (The mask is the one part with a meaning of its own: its alpha is
// converted, see convertImageEditMask.)
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
	// masks counts the NON-EMPTY `mask` parts: 0 or 1 (a second is refused at
	// parse). Whether there is one is all the parse says; convertImageEditMask
	// reads it.
	masks int
}

func (f imageEditForm) routingModel() string { return strings.TrimSpace(f.model) }

// imageEditBodyLimit is the multipart read cap in force: the configured one, or
// the default for a Server that was never given one (a bare &Server{}).
func (s *Server) imageEditBodyLimit() int64 {
	if s.maxImageEditBodyBytes > 0 {
		return s.maxImageEditBodyBytes
	}
	return defaultMaxImageEditBodyBytes
}

// errImageEditTooLarge is the refusal of an edit whose prepared body is past the
// request limit of the agent it is bound for (agentRouterMaxBodyBytes). Like
// errImageEditUnavailable it is mapped to a status, code and message in one place
// (completionHTTPStatus, completionErrorCode, completionErrorResponse): 413
// request.body_too_large, the code the client's own oversized body gets.
var errImageEditTooLarge = errors.New(codeRequestBodyTooLarge)

// codeImageEditUnavailable is the code an edit is refused with when its resolved
// target is an agent that has not declared multipart_proxy: the request is well
// formed and the model is edit-capable, but this server cannot take an edit.
// errImageEditUnavailable is its sentinel -- like the routing refusals it is
// mapped to a status, code and message in one place (completionHTTPStatus,
// completionErrorCode, completionErrorResponse), so the response and the usage
// row recordImagesRoutingFailure writes cannot disagree about it.
const codeImageEditUnavailable = "images.edit_unavailable"

var errImageEditUnavailable = errors.New(codeImageEditUnavailable)

// imageEditTargetServed reports whether target can be sent an image edit. An
// agent-launched (server_agent) target is sent the edit through the agent's
// runtime router, which peeks the model out of the body to find the child to
// start; only an agent that has DECLARED multipart_proxy peeks it out of a
// multipart one, and a router that has not answers 404 for every edit -- a
// failure no later signal distinguishes from an unknown model. So an agent target
// is served only on positive evidence: AgentFeatures.Has is false for a server
// that has never reported (a gateway restart empties the registry until the next
// telemetry sample), for one whose last sample lacked the name, and for a nil
// registry.
//
// Every other target is served: an external openai_images target (an sd-server
// the operator registered as an application) has no agent in front of it, and its
// relay is exactly what it was before this gate existed. The decision is on the
// TARGET's provider, not on whether its server happens to run an agent.
func (s *Server) imageEditTargetServed(target routing.Target) bool {
	if target.Provider != routing.ProviderServerAgent {
		return true
	}
	return s.AgentFeatures.Has(target.ServerID, multipartProxyFeature)
}

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
//   - A mask is converted (convertImageEditMask) with the rest of the validation:
//     OpenAI's transparent-means-edit alpha mask becomes the luminance mask
//     sd-server reads, and a mask that cannot be used is a 400 of its own
//     (images.mask_invalid, images.edit_unsupported_format) before the gate.
//   - The relay names the edits endpoint (nativeRelay.endpoint) and its upstream
//     path, and carries the body's own content type -- the client's, verbatim,
//     unless there is a mask or a model name to change (relayImageEdit).
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
	raw, ok := readImageEditBody(w, r, s.imageEditBodyLimit())
	if !ok {
		return
	}
	form, err := parseImageEditForm(r.Header.Get(contentTypeHeader), raw)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if err := validateImageEditRequest(form); err != nil {
		writeRequestError(w, err)
		return
	}
	// A mask is converted here, with the rest of the validation: its refusals
	// (size, format, no region) are 400s answered before the gate like any other,
	// so a bad mask spends no admission. The conversion is bounded work -- the
	// mask's pixel count is read off its header and capped before anything is
	// decoded -- and what it produces rides to the relay beside the client's own
	// bytes, which stay what capture records.
	var maskPNG []byte
	if form.masks > 0 {
		if maskPNG, err = convertImageEditMask(form.boundary, raw); err != nil {
			writeRequestError(w, err)
			return
		}
	}
	token, ok = s.runAsPrincipal(w, r, token)
	if !ok {
		return
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
	s.relayImageEdit(w, r, token, pf.Req, form, raw, maskPNG)
}

// readImageEditBody reads the whole multipart body, bounded by limit (the
// server's imageEditBodyLimit). A body past the cap is a 413 whether its length
// was declared (refused without reading a byte) or only discovered while
// reading. The JSON readers cannot be used here: readRawJSONLimit decodes one
// JSON value and refuses anything else.
func readImageEditBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
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
// maxImageEditFieldBytes; the image and mask parts are counted and skipped, and
// every other part is skipped untouched. Each of model, prompt, response_format,
// stream and mask may appear ONCE. The model decides routing and the allowlist
// and may be peeked again downstream (the agent's router), so two of them are two
// answers to "which model" and one must not be silently picked over the other;
// the rest are refused for the same uniformity rather than inventing a
// precedence (and which of two masks is THE mask is not a question to guess at).
//
// The whole body is walked, and EVERY part read to its end -- the skipped ones
// too, because mime/multipart swallows the read error of a part it merely steps
// over, so a part cut off mid-way would pass here and fail later, in the rebuild,
// after the request had been admitted. A repeated field or a malformed tail is
// caught here rather than discovered by a consumer later.
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
			return form, invalidImageEdit(malformedMultipartBodyMessage)
		}
		if err := form.readPart(part, seen); err != nil {
			return form, err
		}
	}
}

// readPart reads one part of the form into f, by the field it is: the four text
// fields are read and kept, the image and mask parts are counted and skipped, and
// any other part is skipped untouched. seen is the set of the single-use fields
// (the four text ones and mask) already met.
func (f *imageEditForm) readPart(part *multipart.Part, seen map[string]bool) error {
	name := part.FormName()
	switch name {
	case "model", "prompt", "response_format", "stream":
		return f.readTextField(part, name, seen)
	case "image", "image[]":
		return countNonEmptyPart(part, &f.images)
	case "mask":
		if err := markImageEditFieldSeen(seen, name); err != nil {
			return err
		}
		return countNonEmptyPart(part, &f.masks)
	default:
		_, err := discardImageEditPart(part)
		return err
	}
}

// readTextField reads the value of one of the four text fields into f, once: a
// repeat is refused before its value is read.
func (f *imageEditForm) readTextField(part *multipart.Part, name string, seen map[string]bool) error {
	if err := markImageEditFieldSeen(seen, name); err != nil {
		return err
	}
	value, err := readImageEditField(part)
	if err != nil {
		return err
	}
	switch name {
	case "model":
		f.model = value
	case "prompt":
		f.prompt = value
	case "response_format":
		f.responseFormat = value
	case "stream":
		f.stream = value
	}
	return nil
}

// markImageEditFieldSeen records that the single-use field name has been met, and
// refuses a second one.
func markImageEditFieldSeen(seen map[string]bool, name string) error {
	if seen[name] {
		return invalidImageEdit(fmt.Sprintf("form field %q must appear only once", name))
	}
	seen[name] = true
	return nil
}

// discardImageEditPart reads a part to its end without keeping it, and returns how
// many bytes it held: a part that breaks off is a malformed body.
func discardImageEditPart(part *multipart.Part) (int64, error) {
	n, err := io.Copy(io.Discard, part)
	if err != nil {
		return 0, invalidImageEdit(malformedMultipartBodyMessage)
	}
	return n, nil
}

// countNonEmptyPart reads a part to its end without keeping it and adds one to
// *count if it held any bytes (an empty image or mask part is not one).
func countNonEmptyPart(part *multipart.Part, count *int) error {
	n, err := discardImageEditPart(part)
	if err != nil {
		return err
	}
	if n > 0 {
		*count++
	}
	return nil
}

// readImageEditField reads one text field, bounded.
func readImageEditField(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxImageEditFieldBytes+1))
	if err != nil {
		return "", invalidImageEdit(malformedMultipartBodyMessage)
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
// be replaced with the mapping's own name for the model (routing.Target.
// ProviderModel, the mapping's AppModelName): for ANY target whose name is set
// and is not the one the form already carries. The client names the GATEWAY's
// model, and the two are independent fields of a mapping. An agent-launched
// target's router keys the child it starts on the mapped name; an external
// upstream is handed it exactly as the JSON generations relay hands it
// (rewriteModelField sets it for every provider). For the sd-server this endpoint
// exists for the field changes nothing -- it ignores it, one process serves one
// model -- but an external edit upstream that routes on the model (a proxy in
// front of several) would otherwise route on the gateway's name, and a client
// could name any model there, past a token's model override or redirect pin.
// The comparison is against the field as the client sent it, so a request that
// reached the mapping under a differently-spelled name -- or through an override
// or redirect that replaced it -- is rewritten too; a form that already carries
// the mapped name is left alone.
func imageEditNeedsModelRewrite(target routing.Target, formModel string) bool {
	return target.ProviderModel != "" && formModel != target.ProviderModel
}

// imageEditRebuild says what rebuildImageEditBody changes in a form: the value of
// its model field when model is set, the content of its mask part when mask is
// set, and -- when canonical is set (an sglang_omni target) -- the headers of every
// other part, rebuilt by sglangOmniEditPartHeader. Whatever it leaves unset is
// copied as the client sent it.
type imageEditRebuild struct {
	model     string
	mask      []byte
	canonical bool
}

// sglangOmniEditSourceRefusal walks an edit form bound for an sglang_omni target and
// returns the refusal of its first part sglangOmniEditPartHeader refuses, nil when
// every part may be relayed. raw is a body parseImageEditForm accepted under boundary.
func sglangOmniEditSourceRefusal(raw []byte, boundary string) *imageSourceRefusal {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// Not expected: parseImageEditForm walked this body to its end.
			return &imageSourceRefusal{field: "body", message: malformedMultipartBodyMessage}
		}
		if _, refusal := sglangOmniEditPartHeader(part); refusal != nil {
			return refusal
		}
	}
}

// sglangOmniEditPartHeader returns the header a part of an edit bound for an
// sglang_omni target is relayed with, or the refusal of the part. sglang-omni v0.2.0
// (sglang_omni/serve/openai_api.py, edits) takes the source image from the form fields
// image, image[], url and url[], and a source that is no uploaded file is a string it
// resolves itself: a bare path read from the model host, a file:// path or a URL it
// fetches. sglang-omni authenticates nothing, so the gateway relays a source only as an
// uploaded file:
//
//   - a part must carry exactly one Content-Disposition, which parses and is form-data.
//     Starlette keeps the LAST Content-Disposition of a part while Go's FormName and
//     FileName read the first, and a duplicated parameter (two names) is one more way
//     for the two to disagree; mime.ParseMediaType refuses that one.
//
//   - url and url[] are refused: the upstream resolves their value whatever its shape.
//
//   - image and image[] must be file parts (a non-empty filename): Starlette makes a
//     part an uploaded file only when its Content-Disposition names a filename.
//
//   - the name matches sglangOmniPartNamePattern and the filename holds no backslash,
//     double quote, semicolon or control character. python-multipart splits the
//     parameters with a copy of CPython's email.message._parseparam, which counts
//     quotes as count('"') - count('\\"'), so a quoted value ending in a backslash
//     misparses and can turn the rest of the line into a smuggled parameter (a part the
//     gateway saw as an unknown file field becomes the text field url). Inside the
//     charset the quoting the gateway writes is read back identically.
//
// The header returned carries a Content-Disposition the gateway built from what it
// read (form-data, the name and, when there is one, the filename) and the part's
// Content-Type, and nothing else the client sent. The refusal names the field and
// never echoes a value.
func sglangOmniEditPartHeader(part *multipart.Part) (textproto.MIMEHeader, *imageSourceRefusal) {
	dispositions := part.Header.Values(contentDispositionHeader)
	if len(dispositions) != 1 {
		return nil, &imageSourceRefusal{field: contentDispositionHeader, message: "every form part must carry exactly one Content-Disposition header for this model server"}
	}
	disposition, params, err := mime.ParseMediaType(dispositions[0])
	if err != nil || disposition != "form-data" {
		return nil, &imageSourceRefusal{field: contentDispositionHeader, message: "every form part's Content-Disposition must be a well-formed form-data disposition for this model server"}
	}
	name, filename := params["name"], part.FileName()
	switch name {
	case "url", "url[]":
		return nil, &imageSourceRefusal{field: name, message: fmt.Sprintf("form field %q is not accepted by this model server, which would read its value as a server-side path or fetch it as a URL; send the source image as an uploaded file under \"image\"", name)}
	case "image", "image[]":
		if filename == "" {
			return nil, &imageSourceRefusal{field: name, message: fmt.Sprintf("form field %q must be an uploaded file with a filename; this model server would read a text value as a server-side path or fetch it as a URL", name)}
		}
	}
	if refusal := sglangOmniEditPartNameRefusal(name, params["filename"]); refusal != nil {
		return nil, refusal
	}
	h := textproto.MIMEHeader{}
	value := fmt.Sprintf("form-data; name=%q", name)
	if filename != "" {
		value += fmt.Sprintf("; filename=%q", filename)
	}
	h.Set(contentDispositionHeader, value)
	if ct := part.Header.Get(contentTypeHeader); ct != "" {
		h.Set(contentTypeHeader, ct)
	}
	return h, nil
}

// contentDispositionHeader is the multipart part header that names a part and its file.
const contentDispositionHeader = "Content-Disposition"

// sglangOmniPartNamePattern is the charset of a part name relayed to an sglang_omni
// target: what the form fields of /v1/images/edits (image, image[], model, n, ...) use.
var sglangOmniPartNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]+$`)

// sglangOmniEditPartNameRefusal refuses a part whose name or filename is outside the
// charset sglang-omni's parameter parser reads the same way the gateway does (see
// sglangOmniEditPartHeader). It names the header and never echoes the value.
func sglangOmniEditPartNameRefusal(name, filename string) *imageSourceRefusal {
	if !sglangOmniPartNamePattern.MatchString(name) {
		return &imageSourceRefusal{field: contentDispositionHeader, message: "every form part's Content-Disposition name must consist only of letters, digits and _ . [ ] - for this model server"}
	}
	if strings.ContainsFunc(filename, func(r rune) bool {
		return r == '\\' || r == '"' || r == ';' || unicode.IsControl(r)
	}) {
		return &imageSourceRefusal{field: contentDispositionHeader, message: "a form part's Content-Disposition filename must not contain a backslash, double quote, semicolon or control character for this model server"}
	}
	return nil
}

// imageSourceRefusal is an images.source_unsupported refusal: the field it names (a
// form field, or the Content-Disposition header) and its message, which never
// carries a value from the form.
type imageSourceRefusal struct {
	field   string
	message string
}

// rebuildImageEditBody returns raw with the changes of rb applied, as a new
// multipart body and its Content-Type (a fresh boundary). Every part the changes
// do not touch keeps its bytes and its position; its headers are copied as the
// client sent them, except under rb.canonical, where the gateway rebuilds them
// (sglangOmniEditPartHeader). The model change replaces the value of the model field,
// the one the agent's router (and, now, any model-routing upstream) reads; the
// mask change replaces the content of the mask part IN PLACE with the converted
// mask, as a PNG file part (imageEditMaskFilename). A mask edit is therefore
// always a rebuild, and a rebuild always keeps the model field: dropping it
// would leave the agent's router unable to find the child to start.
//
// It reads raw with the boundary parseImageEditForm already validated the body
// against -- every part of which parseImageEditForm walked to its end -- so an
// error here is not expected for a body that function accepted.
//
// The body is built in memory at roughly the client's size; the size cap
// (imageEditBodyLimit) is applied to what the client sends, not to this, which is
// why relayImageEdit checks what it hands an agent against the agent's own.
func rebuildImageEditBody(raw []byte, boundary string, rb imageEditRebuild) ([]byte, string, error) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	var out bytes.Buffer
	out.Grow(len(raw) + len(rb.model) + len(rb.mask) + 512)
	mw := multipart.NewWriter(&out)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read part: %w", err)
		}
		if err := writeRebuiltPart(mw, part, rb); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return out.Bytes(), mw.FormDataContentType(), nil
}

// writeRebuiltPart writes one part of the client's form into the rebuilt body: the
// model field with rb.model for its value when that is set, the mask part with the
// converted rb.mask for its content when that is set, and every other part with its
// bytes as the client sent them and its headers as the client sent them -- or, under
// rb.canonical, rebuilt by sglangOmniEditPartHeader.
func writeRebuiltPart(mw *multipart.Writer, part *multipart.Part, rb imageEditRebuild) error {
	if rb.model != "" && part.FormName() == "model" {
		if err := mw.WriteField("model", rb.model); err != nil {
			return fmt.Errorf("write model field: %w", err)
		}
		return nil
	}
	if rb.mask != nil && part.FormName() == "mask" {
		return writeConvertedMaskPart(mw, rb.mask)
	}
	header := part.Header
	if rb.canonical {
		h, refusal := sglangOmniEditPartHeader(part)
		if refusal != nil {
			// Not expected: relayImageEdit refused such a form before the rebuild.
			return fmt.Errorf("part %q: %s", part.FormName(), refusal.message)
		}
		header = h
	}
	pw, err := mw.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create part %q: %w", part.FormName(), err)
	}
	if _, err := io.Copy(pw, part); err != nil {
		return fmt.Errorf("copy part %q: %w", part.FormName(), err)
	}
	return nil
}

// writeConvertedMaskPart writes the converted mask as the form's mask part: a PNG
// file part named imageEditMaskFilename.
func writeConvertedMaskPart(mw *multipart.Writer, mask []byte) error {
	h := textproto.MIMEHeader{}
	h.Set(contentDispositionHeader, fmt.Sprintf(`form-data; name="mask"; filename=%q`, imageEditMaskFilename))
	h.Set(contentTypeHeader, mediaTypePNG)
	pw, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create mask part: %w", err)
	}
	if _, err := pw.Write(mask); err != nil {
		return fmt.Errorf("write mask part: %w", err)
	}
	return nil
}

// relayImageEdit resolves the routing target for an already-gated edit and
// relays it via proxyNative. The resolve and the effective-served conjunct are
// the generations relay's own (resolveImagesTarget), so an agent child whose spec
// excludes images refuses an edit exactly as it refuses a generation. An agent
// target whose agent has not declared multipart_proxy is then refused
// images.edit_unavailable (imageEditTargetServed) instead of being relayed. req is
// pf.Req from the caller's inferencePreflight call, already carrying
// RequiredCapabilities = [routing.CapabilityImageEdit].
//
// What differs from relayImages is the request the upstream is sent. The client's
// multipart body is relayed VERBATIM, with the client's own Content-Type, unless
// there is something to change in it: the model field, when the target needs a
// different name in it than the form carries (imageEditNeedsModelRewrite --
// every target, agent or external), or the mask, when the client sent one and
// maskPNG is its sd luminance conversion (convertImageEditMask). Either makes the
// body be rebuilt ONCE with all of its changes (rebuildImageEditBody), sent with
// its own Content-Type; a no-mask edit whose model needs no rewrite stays
// verbatim. An sglang_omni target is the exception: its form is checked for a
// source the upstream would resolve itself (sglangOmniEditSourceRefusal, 400
// images.source_unsupported) and always rebuilt, every part's headers written by
// the gateway (sglangOmniEditPartHeader). nativeRelay.raw stays the client's bytes either way: that is what
// capture records. The relay names the edits endpoint, so the upstream path AND
// the usage row's provider_path are /v1/images/edits -- left unset, the endpoint
// defaults to the generations one in both places.
//
// The body that is finally sent to an agent-launched target must fit the agent
// router's own request limit (agentRouterMaxBodyBytes), which the cap on what the
// client sends does not guarantee (imageEditBodyLimit says why): one that does
// not is refused here, recorded, instead of being sent.
func (s *Server) relayImageEdit(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, form imageEditForm, raw, maskPNG []byte) {
	start := time.Now()
	target, ok := s.resolveImagesTarget(w, r, &token, req, raw, start)
	if !ok {
		return
	}
	// The multipart_proxy gate (imageEditTargetServed): after the resolve and the
	// served-flavor conjunct, so a refusal those make still wins, and before
	// anything is built or sent, so an agent that cannot take the edit is never
	// handed a multipart body (it would 404). Like the conjunct it is NOT retried
	// against another application that might serve the model: the resolve has
	// already picked this target.
	if !s.imageEditTargetServed(target) {
		slog.Debug("image edit refused: the target's agent has not declared multipart_proxy",
			"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID),
			"route_id", target.RouteID, "feature", multipartProxyFeature)
		s.recordImagesRoutingFailure(w, r, token, req, raw, start, errImageEditUnavailable)
		return
	}
	// An sglang_omni target resolves a source that is no uploaded file on its own
	// (sglangOmniEditPartHeader): such a form is refused here, after the gates and
	// before anything is sent, and every form it accepts is rebuilt with headers the
	// gateway wrote.
	omni := target.EffectiveKind() == string(routing.RuntimeSpecTypeSGLangOmni)
	if omni {
		if refusal := sglangOmniEditSourceRefusal(raw, form.boundary); refusal != nil {
			s.recordImageSourceRefusal(w, r, token, req, raw, start, refusal)
			return
		}
	}
	rel := nativeRelay{
		token:       token,
		target:      target,
		path:        upstreamPath(target, req.APIFlavor, endpointImagesEdit),
		raw:         raw,
		pfReq:       req,
		endpoint:    endpointImagesEdit,
		contentType: r.Header.Get(contentTypeHeader),
		reselect:    relayAgain(req, func(again inference.Request) { s.relayImageEdit(w, r, token, again, form, raw, maskPNG) }),
	}
	sent := len(raw)
	rebuild := imageEditRebuild{mask: maskPNG, canonical: omni}
	if imageEditNeedsModelRewrite(target, form.model) {
		rebuild.model = target.ProviderModel
	}
	if rebuild.model != "" || rebuild.mask != nil || rebuild.canonical {
		body, contentType, err := rebuildImageEditBody(raw, form.boundary, rebuild)
		if err != nil {
			// Not expected for a body parseImageEditForm accepted: it walked every
			// part of it to its end with the same reader and boundary. Were it to
			// happen it is answered as a failed relay (and recorded) rather than
			// sending the upstream a body whose model or mask it would misread.
			slog.Error("image edit: rebuilding the multipart body failed",
				"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID), "err", err)
			s.recordImagesRoutingFailure(w, r, token, req, raw, start, fmt.Errorf("rebuild multipart body: %w", err))
			return
		}
		rel.upstreamBody, rel.contentType = body, contentType
		sent = len(body)
	}
	if target.Provider == routing.ProviderServerAgent && sent > agentRouterMaxBodyBytes {
		slog.Debug("image edit refused: the body to send is past the agent router's request limit",
			"path", r.URL.Path, "model", req.Model, "server", s.serverName(target.ServerID),
			"bytes", sent, "limit", agentRouterMaxBodyBytes)
		s.recordImagesRoutingFailure(w, r, token, req, raw, start, errImageEditTooLarge)
		return
	}
	s.proxyNative(w, r, rel)
}

// recordImageSourceRefusal answers an edit whose source an sglang_omni target cannot be
// given safely (sglangOmniEditSourceRefusal) with 400 and the refusal, and records the
// usage row recordImagesRoutingFailure records (no upstream call, billed as image, 0).
// It logs at Warn: a path or URL aimed at an sglang_omni target is probing.
func (s *Server) recordImageSourceRefusal(w http.ResponseWriter, r *http.Request, token auth.Token, req inference.Request, raw []byte, start time.Time, refusal *imageSourceRefusal) {
	slog.Warn("image edit rejected: the source is not an uploaded file for an sglang_omni target",
		"path", r.URL.Path, "model", req.Model, "field", refusal.field)
	id := nextRequestID()
	capturing := s.capturingEnabled(token)
	body := writeJSONCaptured(w, http.StatusBadRequest, apierror.Response(imagesSourceUnsupported, refusal.message, ""))
	s.recordUsage(start, token, req, routing.Target{}, provider.Response{}, imagesSourceUnsupported, "error", usageMeta{ReqPath: r.URL.Path, HTTPStatus: http.StatusBadRequest, ContentType: jsonContentType, BillingUnit: usage.BillingUnitImage}, id, buildCaptureInput(capturing, token.UserID, token.Secret, r, raw, w.Header(), body, http.StatusBadRequest, req.APIFlavor), nil)
}
