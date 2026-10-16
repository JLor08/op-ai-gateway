// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"op-ai-gateway/internal/inference"
	"strconv"
	"strings"
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
	// files counts the NON-EMPTY parts under `file` or `file[]`, the two spellings
	// multipart clients use for the audio upload.
	files int
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
// maxTranscriptionFieldBytes; the audio parts are counted and skipped, and every
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
// fields are read and kept, the audio parts are counted and skipped, and any other
// part is skipped untouched. seen is the set of the single-use fields already met.
func (f *transcriptionForm) readPart(part *multipart.Part, seen map[string]bool) error {
	name := part.FormName()
	switch name {
	case "model", "language", "prompt", "response_format", "stream", "stream_format":
		return f.readTextField(part, name, seen)
	case "file", "file[]":
		return countNonEmptyTranscriptionPart(part, &f.files)
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

// countNonEmptyTranscriptionPart reads a part to its end without keeping it and
// adds one to *count if it held any bytes (an empty file part is not a file).
func countNonEmptyTranscriptionPart(part *multipart.Part, count *int) error {
	n, err := discardTranscriptionPart(part)
	if err != nil {
		return err
	}
	if n > 0 {
		*count++
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
// and at least one non-empty audio file are required, and every other field (language,
// prompt, temperature, ...) is the client's own business and reaches the upstream
// unexamined, with two exceptions.
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
		return &inference.Error{Code: transcriptionFileRequired, Message: "at least one non-empty audio file is required (form field \"file\" or \"file[]\")"}
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
