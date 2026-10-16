// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// transcriptionTestAudio is deliberately NOT valid UTF-8 and carries CRLF, NUL and
// a "--" run, like editTestPNG: the bytes a line-oriented or string-based reading
// of the form would damage.
var transcriptionTestAudio = []byte("RIFF\x00\x01\xfe\xff\r\n--not-a-boundary\r\n\x00WAVEfmt ")

// validTranscriptionParts is a well-formed transcription request for model: the
// model field and one audio file.
func validTranscriptionParts(model string) []editPart {
	return []editPart{
		{name: "model", body: []byte(model)},
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
	}
}

// requireTranscriptionInvalid asserts err is the transcription endpoint's own
// malformed-form refusal: a *inference.Error carrying invalidTranscriptionRequest.
func requireTranscriptionInvalid(t *testing.T, err error) {
	t.Helper()
	var ie *inference.Error
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v (%T), want an *inference.Error", err, err)
	}
	if ie.Code != "transcription.invalid" {
		t.Fatalf("code = %q, want transcription.invalid (message %q)", ie.Code, ie.Message)
	}
}

// The error codes are API surface: clients branch on them.
func TestTranscriptionErrorCodes(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{transcriptionModelRequired, "transcription.model_required"},
		{transcriptionFileRequired, "transcription.file_required"},
		{transcriptionResponseFormatUnsupported, "transcription.response_format_unsupported"},
		{transcriptionStreamUnsupported, "transcription.stream_unsupported"},
		{invalidTranscriptionRequest, "transcription.invalid"},
		{invalidTranscription("m").Code, "transcription.invalid"},
	} {
		if tc.got != tc.want {
			t.Fatalf("code = %q, want %q", tc.got, tc.want)
		}
	}
}

// parseTranscriptionForm reads what routing needs from anywhere in the form,
// counts the audio parts under the `file` field, keeps the model as sent
// (untrimmed) for the rewrite decision while routing on the trimmed one, and
// keeps the boundary of the client's own body.
func TestParseTranscriptionForm(t *testing.T) {
	t.Run("a well-formed body yields the model and one file", func(t *testing.T) {
		body, ct := buildEditMultipart(t, validTranscriptionParts("asr-model"))

		form, err := parseTranscriptionForm(ct, body)
		if err != nil {
			t.Fatalf("parseTranscriptionForm: %v", err)
		}
		if form.model != "asr-model" || form.routingModel() != "asr-model" {
			t.Fatalf("model = %q / routing %q, want asr-model", form.model, form.routingModel())
		}
		if form.files != 1 {
			t.Fatalf("files = %d, want 1", form.files)
		}
		if form.boundary != mustBoundary(t, ct) {
			t.Fatalf("boundary = %q, want the request's own %q", form.boundary, mustBoundary(t, ct))
		}
	})
	t.Run("fields in any order, every text field kept, only the file field counted", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "file[]", filename: "a.wav", body: transcriptionTestAudio},
			{name: "temperature", body: []byte("0.2")},
			{name: "model", body: []byte("  asr-model \n")},
			{name: "language", body: []byte("de")},
			{name: "file", filename: "b.wav", body: []byte("x")},
			{name: "prompt", body: []byte("names: Müller")},
			{name: "response_format", body: []byte("json")},
			{name: "stream", body: []byte("false")},
			{name: "stream_format", body: []byte("sse")},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil {
			t.Fatalf("parseTranscriptionForm: %v", err)
		}
		if form.model != "  asr-model \n" || form.routingModel() != "asr-model" {
			t.Fatalf("model = %q / routing %q, want the untrimmed field and its trimmed routing value", form.model, form.routingModel())
		}
		if form.language != "de" || form.prompt != "names: Müller" || form.responseFormat != "json" || form.stream != "false" || form.streamFormat != "sse" {
			t.Fatalf("form = %+v", form)
		}
		if form.files != 1 {
			t.Fatalf("files = %d, want 1 (the file part; the file[] part is no file)", form.files)
		}
	})
	t.Run("a file part before the model still finds the model", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
			{name: "model", body: []byte("asr-model")},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.routingModel() != "asr-model" || form.files != 1 {
			t.Fatalf("err = %v, form = %+v, want model asr-model and one file", err, form)
		}
	})
	t.Run("an empty file part does not count", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "model", body: []byte("asr-model")},
			{name: "file", filename: "empty.wav", contentType: "audio/wav", body: nil},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.files != 0 {
			t.Fatalf("err = %v, files = %d, want no error and 0 files", err, form.files)
		}
	})
	t.Run("a file[] part is no file", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "model", body: []byte("asr-model")},
			{name: "file[]", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.files != 0 || form.routingModel() != "asr-model" {
			t.Fatalf("err = %v, form = %+v, want no error, the model and 0 files", err, form)
		}
	})
	t.Run("an empty file part beside a non-empty one counts the non-empty one", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "model", body: []byte("asr-model")},
			{name: "file", filename: "empty.wav", body: nil},
			{name: "file", filename: "clip.wav", body: transcriptionTestAudio},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.files != 1 {
			t.Fatalf("err = %v, files = %d, want 1", err, form.files)
		}
	})
	t.Run("two non-empty file parts are both counted (validation refuses them)", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "model", body: []byte("asr-model")},
			{name: "file", filename: "a.wav", body: transcriptionTestAudio},
			{name: "file", filename: "b.wav", body: []byte("x")},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.files != 2 {
			t.Fatalf("err = %v, files = %d, want no error and 2 files", err, form.files)
		}
	})
	t.Run("a model-less form parses (validation refuses it)", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{{name: "file", filename: "clip.wav", body: transcriptionTestAudio}})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.routingModel() != "" || form.files != 1 {
			t.Fatalf("err = %v, form = %+v, want no error, no model and one file", err, form)
		}
	})
	t.Run("a field past its bound is refused", func(t *testing.T) {
		for _, name := range []string{"model", "language", "prompt", "response_format", "stream", "stream_format"} {
			body, ct := buildEditMultipart(t, []editPart{{name: name, body: bytes.Repeat([]byte("a"), maxTranscriptionFieldBytes+1)}})

			_, err := parseTranscriptionForm(ct, body)

			requireTranscriptionInvalid(t, err)
			if !strings.Contains(err.Error(), "too large") {
				t.Fatalf("%s: err = %v, want a too-large refusal", name, err)
			}
		}
	})
	t.Run("a field at its bound is read", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{{name: "prompt", body: bytes.Repeat([]byte("a"), maxTranscriptionFieldBytes)}})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || len(form.prompt) != maxTranscriptionFieldBytes {
			t.Fatalf("err = %v, prompt length = %d, want %d", err, len(form.prompt), maxTranscriptionFieldBytes)
		}
	})
	t.Run("the audio part is not bound by the field bound", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "model", body: []byte("asr-model")},
			{name: "file", filename: "big.wav", body: bytes.Repeat([]byte{0xAB}, maxTranscriptionFieldBytes+1)},
		})

		form, err := parseTranscriptionForm(ct, body)
		if err != nil || form.files != 1 {
			t.Fatalf("err = %v, files = %d, want the large file counted and not refused", err, form.files)
		}
	})
}

// A body that is not a usable multipart form is refused with the endpoint's own
// code. Two of the ways in are the ones mime/multipart would otherwise let
// through silently: a part the handler only steps over, which the Reader swallows
// a read error of, and a field repeated where one answer is required. The WHOLE
// body is walked, and every part read to its end, so a malformed tail is caught
// here -- before routing and admission -- rather than later in a rebuild.
func TestParseTranscriptionFormRefusesAMalformedBody(t *testing.T) {
	validBody, validCT := buildEditMultipart(t, validTranscriptionParts("asr-model"))
	// A body whose LAST part is one the handler never reads (neither a field it
	// owns nor the audio) and which is cut off inside that part.
	extraBody, extraCT := buildEditMultipart(t, append(validTranscriptionParts("asr-model"),
		editPart{name: "reference", filename: "ref.wav", contentType: "audio/wav", body: bytes.Repeat([]byte("r"), 200)}))
	// A body whose LAST part is the audio, cut off inside it.
	audioLastBody, audioLastCT := buildEditMultipart(t, []editPart{
		{name: "model", body: []byte("asr-model")},
		{name: "file", filename: "clip.wav", body: bytes.Repeat([]byte("a"), 200)},
	})
	// The model is found first, and then repeated after the audio part.
	lateRepeatBody, lateRepeatCT := buildEditMultipart(t, append(validTranscriptionParts("asr-model"),
		editPart{name: "model", body: []byte("another-model")}))

	cases := []struct {
		name string
		body []byte
		ct   string
	}{
		{"json body", []byte(`{"model":"asr-model"}`), "application/json"},
		{"no content type", validBody, ""},
		{"multipart without a boundary", validBody, "multipart/form-data"},
		{"another multipart type", validBody, strings.Replace(validCT, "form-data", "mixed", 1)},
		{"malformed content type parameter", validBody, "multipart/form-data; boundary"},
		{"truncated before the closing boundary", validBody[:len(validBody)-16], validCT},
		{"truncated inside the audio part", audioLastBody[:len(audioLastBody)-100], audioLastCT},
		{"truncated inside a part the handler does not read", extraBody[:len(extraBody)-100], extraCT},
		{"a model repeated after the audio part", lateRepeatBody, lateRepeatCT},
	}
	for _, field := range []string{"model", "language", "prompt", "response_format", "stream", "stream_format"} {
		body, ct := buildEditMultipart(t, []editPart{
			{name: field, body: []byte("1")},
			{name: "file", filename: "clip.wav", body: transcriptionTestAudio},
			{name: field, body: []byte("2")},
		})
		cases = append(cases, struct {
			name string
			body []byte
			ct   string
		}{"repeated " + field, body, ct})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTranscriptionForm(tc.ct, tc.body)

			requireTranscriptionInvalid(t, err)
		})
	}
	// mime/multipart reports a body it finds no part in -- an empty one, one that
	// never mentions the request's boundary -- as the end of the form (the EOF a
	// finished form ends with, wrapped), and the agent router's peekMultipartModel
	// takes it the same way. Both end up with no model, so the refusal is the
	// validation's model_required, still 400 and still before routing.
	t.Run("a body with no part to find is a form with no model", func(t *testing.T) {
		for name, tc := range map[string]struct {
			body []byte
			ct   string
		}{
			"empty body":                           {nil, validCT},
			"a boundary that does not match":       {validBody, "multipart/form-data; boundary=SOMETHINGELSE"},
			"a body that ends in the first header": {[]byte("--" + mustBoundary(t, validCT) + "\r\nContent-Disposition: form-data; na"), validCT},
		} {
			form, err := parseTranscriptionForm(tc.ct, tc.body)
			if err != nil {
				requireTranscriptionInvalid(t, err)
				continue
			}
			if form.routingModel() != "" || form.files != 0 {
				t.Fatalf("%s: form = %+v, want no model and no file", name, form)
			}
			if verr := validateTranscriptionRequest(form); verr == nil || verr.Code != "transcription.model_required" {
				t.Fatalf("%s: validation = %v, want transcription.model_required", name, verr)
			}
		}
	})
}

// rawFormBody is a hand-crafted multipart body with a fixed boundary, written with
// LF line ends (crlfBody converts them): the shapes the router's differential tests
// use to pull the model reader apart from a text scan.
type rawFormBody struct {
	name string
	body string
	// refused is, for the bodies of noModelBodies, true when the parse itself must
	// fail; otherwise the form must come back with no model, which validation
	// refuses. (A body the Reader finds no part in at all reads as an empty form, as
	// in the router.)
	refused bool
}

const (
	rawFormContentType = "multipart/form-data; boundary=BOUNDARY"
	rawFormTail        = "--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"prompt\"\n\nsome words\n" +
		"--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"in.wav\"\n\nWAVDATA\n"
)

func crlfBody(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

// modelLookalikeBodies are the bodies in which a model-shaped lookalike sits next to
// a real form-data model part: every reader that walks the form like mime/multipart
// finds the real one, "asr-model", and a scan for the text name="model" does not.
func modelLookalikeBodies() []rawFormBody {
	return []rawFormBody{
		{name: "attachment-named model before the real one", body: "--BOUNDARY\n" +
			"Content-Disposition: attachment; name=\"model\"\n\nghost-model\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			rawFormTail + "--BOUNDARY--\n"},
		{name: "attachment-named model after the real one", body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: attachment; name=\"model\"\n\nghost-model\n" +
			rawFormTail + "--BOUNDARY--\n"},
		{name: "fake model in the preamble", body: "preamble text\n" +
			"--ignored\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			rawFormTail + "--BOUNDARY--\n"},
		{name: "fake model in the epilogue", body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			rawFormTail + "--BOUNDARY--\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
			"--BOUNDARY--\n"},
		{name: "model text inside the prompt part", body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"prompt\"\n\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"file\"; filename=\"in.wav\"\n\nWAVDATA\n" +
			"--BOUNDARY--\n"},
		{name: "model after the file part", body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"file\"; filename=\"in.wav\"\n\nWAVDATA\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			"--BOUNDARY--\n"},
	}
}

// noModelBodies are the bodies the agent router routes nowhere (no model part it
// would start, a repeat, a part it cannot get to).
func noModelBodies() []rawFormBody {
	const rest = rawFormTail + "--BOUNDARY--\n"
	return []rawFormBody{
		{name: "no model part", body: rest},
		{name: "empty model value", body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\n\n" + rest},
		{name: "model part with an attachment disposition", body: "--BOUNDARY\n" +
			"Content-Disposition: attachment; name=\"model\"\n\nghost-model\n" + rest},
		{name: "model-shaped text in the preamble", body: "--ignored\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
			"\n" + rest},
		{name: "model-shaped text in the epilogue", body: rawFormTail + "--BOUNDARY--\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
			"--BOUNDARY--\n"},
		{name: "model named twice", refused: true, body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			"--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nother-model\n" + rest},
		{name: "model part never terminated", refused: true, body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model"},
		{name: "body ends inside the first part's headers", body: "--BOUNDARY\nContent-Disposition: form-data; name=\"mod"},
		{name: "unparseable part headers after the model", refused: true, body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\nasr-model\n" +
			"--BOUNDARY\n" +
			"this is not a header line\n\nx\n" +
			"--BOUNDARY--\n"},
		{name: "model field over the size bound", refused: true, body: "--BOUNDARY\n" +
			"Content-Disposition: form-data; name=\"model\"\n\n" +
			strings.Repeat("m", maxTranscriptionFieldBytes+1) + "\n" + rest},
	}
}

// The model is the one field two readers must agree on: the gateway decides
// routing and the operator's model allowlist on it, and the server agent's
// runtime router (peekMultipartModel, server-agent/internal/runtime/router.go)
// then starts a process on its own reading of the same bytes. The cases mirror the
// router's differential tests (router_multipart_test.go): where a model-shaped
// lookalike sits next to a real form-data model part, the gateway reads the real
// one, never the lookalike; and every body the router refuses (no model part, a
// repeat, a part it cannot get to) the gateway refuses or reads as having no model.
func TestParseTranscriptionFormReadsTheModelLikeTheAgentRouter(t *testing.T) {
	t.Run("the model the router would start", func(t *testing.T) {
		for _, tc := range modelLookalikeBodies() {
			t.Run(tc.name, func(t *testing.T) {
				form, err := parseTranscriptionForm(rawFormContentType, crlfBody(tc.body))
				if err != nil || form.model != "asr-model" {
					t.Fatalf("err = %v, model = %q, want asr-model", err, form.model)
				}
			})
		}
	})
	t.Run("the bodies the router routes nowhere", func(t *testing.T) {
		for _, tc := range noModelBodies() {
			t.Run(tc.name, func(t *testing.T) {
				form, err := parseTranscriptionForm(rawFormContentType, crlfBody(tc.body))
				if tc.refused {
					requireTranscriptionInvalid(t, err)
					return
				}
				if err != nil || form.routingModel() != "" {
					t.Fatalf("err = %v, model = %q, want a form with no model", err, form.model)
				}
				if verr := validateTranscriptionRequest(form); verr == nil || verr.Code != "transcription.model_required" {
					t.Fatalf("validation = %v, want transcription.model_required", verr)
				}
			})
		}
	})
}

// parseImageEditForm and parseTranscriptionForm are two copies of the same
// security-critical model read (the image edit's is the one the agent router's
// peekMultipartModel documents itself against). This pins the copies together: over
// the same bodies, they either both refuse or both return the same model (and the
// same boundary), so a change to one that is not made to the other fails here.
// The bodies are the router's differential set plus form-built ones for the
// orderings, repeats, and malformed tails.
func TestTranscriptionAndImageEditFormsReadTheSameModel(t *testing.T) {
	type body struct {
		name string
		ct   string
		raw  []byte
	}
	var bodies []body
	for _, tc := range append(modelLookalikeBodies(), noModelBodies()...) {
		bodies = append(bodies, body{"raw: " + tc.name, rawFormContentType, crlfBody(tc.body)})
	}
	// Form-built bodies carry both an `image` and a `file` part, so each parser has
	// its own upload to count; the model is the only thing compared.
	built := func(name string, parts []editPart, cut int) {
		raw, ct := buildEditMultipart(t, parts)
		bodies = append(bodies, body{"built: " + name, ct, raw[:len(raw)-cut]})
	}
	upload := []editPart{
		{name: "image", filename: "a.png", body: editTestPNG},
		{name: "file", filename: "a.wav", body: transcriptionTestAudio},
	}
	model := func(v string) editPart { return editPart{name: "model", body: []byte(v)} }
	with := func(parts ...editPart) []editPart { return parts }
	built("well-formed", with(model("asr-model"), upload[0], upload[1]), 0)
	built("untrimmed model", with(model("  asr-model \n"), upload[0], upload[1]), 0)
	built("model after the uploads", with(upload[0], upload[1], model("asr-model")), 0)
	built("model between the uploads", with(upload[0], model("asr-model"), upload[1]), 0)
	built("no model", with(upload[0], upload[1]), 0)
	built("empty model", with(model(""), upload[0], upload[1]), 0)
	built("repeated model", with(model("asr-model"), upload[0], upload[1], model("other-model")), 0)
	built("repeated model, equal values", with(model("asr-model"), model("asr-model"), upload[0], upload[1]), 0)
	built("model over the bound", with(model(strings.Repeat("m", maxTranscriptionFieldBytes+1)), upload[0], upload[1]), 0)
	built("model at the bound", with(model(strings.Repeat("m", maxTranscriptionFieldBytes)), upload[0], upload[1]), 0)
	built("unrelated parts around the model", with(
		editPart{name: "temperature", body: []byte("0.1")}, model("asr-model"),
		editPart{name: "reference", filename: "r.bin", body: bytes.Repeat([]byte("r"), 300)}, upload[0], upload[1]), 0)
	built("truncated before the closing boundary", with(model("asr-model"), upload[0], upload[1]), 16)
	built("truncated inside the last part", with(model("asr-model"), upload[0], upload[1]), 40)
	built("truncated inside a skipped part", with(model("asr-model"), upload[0], upload[1],
		editPart{name: "reference", filename: "r.bin", body: bytes.Repeat([]byte("r"), 300)}), 100)
	validRaw, validCT := buildEditMultipart(t, with(model("asr-model"), upload[0], upload[1]))
	bodies = append(bodies,
		body{"not multipart", "application/json", []byte(`{"model":"asr-model"}`)},
		body{"no content type", "", validRaw},
		body{"no boundary", "multipart/form-data", validRaw},
		body{"another multipart type", strings.Replace(validCT, "form-data", "mixed", 1), validRaw},
		body{"malformed parameter", "multipart/form-data; boundary", validRaw},
		body{"boundary that does not match", "multipart/form-data; boundary=SOMETHINGELSE", validRaw},
		body{"empty body", validCT, nil},
	)

	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			edit, editErr := parseImageEditForm(tc.ct, tc.raw)
			trans, transErr := parseTranscriptionForm(tc.ct, tc.raw)

			if (editErr == nil) != (transErr == nil) {
				t.Fatalf("image edit err = %v, transcription err = %v, want both to refuse or both to read", editErr, transErr)
			}
			if editErr != nil {
				return
			}
			if edit.model != trans.model || edit.routingModel() != trans.routingModel() {
				t.Fatalf("image edit read model %q, transcription read %q", edit.model, trans.model)
			}
			if edit.boundary != trans.boundary {
				t.Fatalf("image edit boundary %q, transcription boundary %q", edit.boundary, trans.boundary)
			}
		})
	}
}

// validateTranscriptionRequest is the multipart shape's validation twin: a
// model and exactly one non-empty file are required, response_format may only be
// "json" (or absent), and neither stream nor stream_format may be on.
func TestValidateTranscriptionRequest(t *testing.T) {
	valid := transcriptionForm{model: "asr-model", files: 1}
	with := func(mut func(*transcriptionForm)) transcriptionForm {
		f := valid
		mut(&f)
		return f
	}
	cases := []struct {
		name     string
		form     transcriptionForm
		wantCode string // "" = accepted
	}{
		{"a model and one file", valid, ""},
		{"optional fields set", with(func(f *transcriptionForm) { f.language, f.prompt = "de", "words" }), ""},

		{"no model", with(func(f *transcriptionForm) { f.model = "" }), "transcription.model_required"},
		{"a blank model", with(func(f *transcriptionForm) { f.model = " \t\n" }), "transcription.model_required"},
		{"no file", with(func(f *transcriptionForm) { f.files = 0 }), "transcription.file_required"},
		{"two files", with(func(f *transcriptionForm) { f.files = 2 }), "transcription.invalid"},
		{"several files", with(func(f *transcriptionForm) { f.files = 3 }), "transcription.invalid"},
		{"two files and no model: the model is checked first", transcriptionForm{files: 2}, "transcription.model_required"},
		{"two files and a bad response_format: the files are checked first", with(func(f *transcriptionForm) { f.files, f.responseFormat = 2, "text" }), "transcription.invalid"},
		{"the model is checked before the file", transcriptionForm{}, "transcription.model_required"},

		{"response_format json", with(func(f *transcriptionForm) { f.responseFormat = "json" }), ""},
		{"response_format json, padded", with(func(f *transcriptionForm) { f.responseFormat = " json\n" }), ""},
		{"response_format empty", with(func(f *transcriptionForm) { f.responseFormat = "" }), ""},
		{"response_format blank", with(func(f *transcriptionForm) { f.responseFormat = "  " }), ""},
		{"response_format verbose_json", with(func(f *transcriptionForm) { f.responseFormat = "verbose_json" }), "transcription.response_format_unsupported"},
		{"response_format text", with(func(f *transcriptionForm) { f.responseFormat = "text" }), "transcription.response_format_unsupported"},
		{"response_format srt", with(func(f *transcriptionForm) { f.responseFormat = "srt" }), "transcription.response_format_unsupported"},
		{"response_format vtt", with(func(f *transcriptionForm) { f.responseFormat = "vtt" }), "transcription.response_format_unsupported"},
		{"response_format mp3", with(func(f *transcriptionForm) { f.responseFormat = "mp3" }), "transcription.response_format_unsupported"},
		{"response_format b64_json", with(func(f *transcriptionForm) { f.responseFormat = "b64_json" }), "transcription.response_format_unsupported"},
		{"response_format in another case", with(func(f *transcriptionForm) { f.responseFormat = "JSON" }), "transcription.response_format_unsupported"},

		{"stream absent", with(func(f *transcriptionForm) { f.stream = "" }), ""},
		{"stream false", with(func(f *transcriptionForm) { f.stream = "false" }), ""},
		{"stream 0, padded", with(func(f *transcriptionForm) { f.stream = " 0 " }), ""},
		{"stream true", with(func(f *transcriptionForm) { f.stream = "true" }), "transcription.stream_unsupported"},
		{"stream TRUE", with(func(f *transcriptionForm) { f.stream = "TRUE" }), "transcription.stream_unsupported"},
		{"stream 1", with(func(f *transcriptionForm) { f.stream = "1" }), "transcription.stream_unsupported"},
		{"stream that is no boolean", with(func(f *transcriptionForm) { f.stream = "maybe" }), "transcription.stream_unsupported"},

		{"stream_format absent", with(func(f *transcriptionForm) { f.streamFormat = "" }), ""},
		{"stream_format blank", with(func(f *transcriptionForm) { f.streamFormat = " " }), ""},
		{"stream_format sse", with(func(f *transcriptionForm) { f.streamFormat = "sse" }), "transcription.stream_unsupported"},
		{"stream_format audio", with(func(f *transcriptionForm) { f.streamFormat = "audio" }), "transcription.stream_unsupported"},
		{"stream_format with stream false", with(func(f *transcriptionForm) { f.stream, f.streamFormat = "false", "sse" }), "transcription.stream_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateTranscriptionRequest(tc.form)

			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("validate = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Code != tc.wantCode {
				t.Fatalf("validate = %+v, want code %q", got, tc.wantCode)
			}
			if got.Message == "" {
				t.Fatalf("refusal %q carries no message", tc.wantCode)
			}
		})
	}
}

// The end of the pipeline the handler runs, parse then validate, on real bodies.
func TestParseThenValidateTranscriptionRequest(t *testing.T) {
	cases := []struct {
		name     string
		parts    []editPart
		wantCode string
	}{
		{"valid", validTranscriptionParts("asr-model"), ""},
		{"no model", replaceEditPart(validTranscriptionParts("asr-model"), "model", nil), "transcription.model_required"},
		{"no file", replaceEditPart(validTranscriptionParts("asr-model"), "file", nil), "transcription.file_required"},
		{"empty file", replaceEditPart(validTranscriptionParts("asr-model"), "file", &editPart{name: "file", filename: "e.wav"}), "transcription.file_required"},
		{"a file[] part only", replaceEditPart(validTranscriptionParts("asr-model"), "file", &editPart{name: "file[]", filename: "clip.wav", body: transcriptionTestAudio}), "transcription.file_required"},
		{"two files", append(validTranscriptionParts("asr-model"), editPart{name: "file", filename: "b.wav", body: []byte("more audio")}), "transcription.invalid"},
		{"a file and a file[] part: the file[] part is no file", append(validTranscriptionParts("asr-model"), editPart{name: "file[]", filename: "b.wav", body: []byte("more audio")}), ""},
		{"a file and an empty second file", append(validTranscriptionParts("asr-model"), editPart{name: "file", filename: "e.wav"}), ""},
		{"verbose_json", append(validTranscriptionParts("asr-model"), editPart{name: "response_format", body: []byte("verbose_json")}), "transcription.response_format_unsupported"},
		{"stream", append(validTranscriptionParts("asr-model"), editPart{name: "stream", body: []byte("true")}), "transcription.stream_unsupported"},
		{"stream_format", append(validTranscriptionParts("asr-model"), editPart{name: "stream_format", body: []byte("sse")}), "transcription.stream_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := buildEditMultipart(t, tc.parts)

			form, err := parseTranscriptionForm(ct, body)
			if err != nil {
				t.Fatalf("parseTranscriptionForm: %v", err)
			}
			got := validateTranscriptionRequest(form)

			switch {
			case tc.wantCode == "" && got != nil:
				t.Fatalf("validate = %+v, want nil", got)
			case tc.wantCode != "" && (got == nil || got.Code != tc.wantCode):
				t.Fatalf("validate = %+v, want code %q", got, tc.wantCode)
			}
		})
	}
}

// The multipart read is bounded. A body past the cap is a 413 whether its length
// was declared (refused without reading a byte) or only discovered while reading
// (chunked, where the read stops at the cap); a body under it is returned whole;
// a body that cannot be read is the endpoint's own malformed-form 400.
func TestReadTranscriptionBody(t *testing.T) {
	const limit = 1 << 10
	big := bytes.Repeat([]byte{0xAB}, limit+1)

	t.Run("a body under the cap is returned whole", func(t *testing.T) {
		body := bytes.Repeat([]byte("a"), limit)
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		raw, ok := readTranscriptionBody(rec, req, limit)

		if !ok || !bytes.Equal(raw, body) {
			t.Fatalf("ok = %v, %d bytes, want the %d-byte body back", ok, len(raw), len(body))
		}
	})
	for _, declared := range []bool{true, false} {
		name := "chunked"
		if declared {
			name = "declared length"
		}
		t.Run("past the cap, "+name, func(t *testing.T) {
			counted := &countingReader{r: bytes.NewReader(big)}
			req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", io.NopCloser(counted))
			if declared {
				req.ContentLength = int64(len(big))
			} else {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()

			raw, ok := readTranscriptionBody(rec, req, limit)

			if ok || raw != nil {
				t.Fatalf("ok = %v, %d bytes, want the read refused", ok, len(raw))
			}
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
			requireErrorCode(t, rec.Body.String(), "request.body_too_large")
			if declared && counted.n != 0 {
				t.Fatalf("read %d bytes of a body whose declared length was already past the cap", counted.n)
			}
		})
	}
	t.Run("a body that cannot be read", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", io.NopCloser(failingReader{}))
		rec := httptest.NewRecorder()

		raw, ok := readTranscriptionBody(rec, req, limit)

		if ok || raw != nil {
			t.Fatalf("ok = %v, %d bytes, want the read refused", ok, len(raw))
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		requireErrorCode(t, rec.Body.String(), "transcription.invalid")
	})
}

// failingReader fails every read with an error that is not the size cap's.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// --- The handler: validation, gate, routing and relay -----------------------

const (
	// transcriptionGatewayModel is the name a client sends; transcriptionUpstreamModel
	// the model id audio.cpp serves (the speech fixture's route-asr mapping). They
	// differ so the relay's model rewrite is observable at the upstream.
	transcriptionGatewayModel  = "asr-model"
	transcriptionUpstreamModel = "whisper_x"
	// transcriptionOKBody is the document audio.cpp answers a transcription with.
	transcriptionOKBody = `{"text":"hello there","timing":{"audio_duration_ms":2000}}`
	// agentTranscriptionServerID is the server newServerAgentTranscriptionTestServer
	// places its one agent application on.
	agentTranscriptionServerID = "srv-agent-audio"
)

// transcriptionUpstreamCall is one request an audio.cpp stub received. The stub
// keeps the method, the path and the Content-Type as well as the body: a stub that
// threw the *http.Request away could not tell a relay that reached the wrong path,
// or sent JSON, or lost the boundary, from a correct one (#54).
type transcriptionUpstreamCall struct {
	method      string
	path        string
	contentType string
	body        []byte
}

// newTranscriptionUpstream starts an audio.cpp-shaped upstream that answers every
// request with status and respBody (application/json) and records what it was
// sent, over a channel so the test reads it race-free after the response.
func newTranscriptionUpstream(t *testing.T, status int, respBody string) (*httptest.Server, <-chan transcriptionUpstreamCall) {
	t.Helper()
	calls := make(chan transcriptionUpstreamCall, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- transcriptionUpstreamCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(up.Close)
	return up, calls
}

// nextTranscriptionUpstreamCall returns the request the stub saw, failing when it
// saw none.
func nextTranscriptionUpstreamCall(t *testing.T, calls <-chan transcriptionUpstreamCall) transcriptionUpstreamCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	default:
		t.Fatal("the upstream was never called")
		return transcriptionUpstreamCall{}
	}
}

// requireNoTranscriptionUpstreamCall fails when the stub saw a request.
func requireNoTranscriptionUpstreamCall(t *testing.T, calls <-chan transcriptionUpstreamCall) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("the upstream received %s %s, want no call", c.method, c.path)
	default:
	}
}

// newTranscriptionCapableTestServer is the speech fixture (one audio.cpp
// application, the qwen-coder chat seed, the dev bearer and the loopback pair)
// with the transcription verdicts the gate tells apart. On top of the speech
// fixture's own mappings -- tts-voice (speech yes, NO transcription row),
// asr-model -> whisper_x and unprobed-voice (no row at all) -- it gives:
//
//   - asr-model: a (transcription, yes) verdict, the only kind that serves; its
//     upstream name differs, so the rewrite is observable;
//   - asr-same: a (transcription, yes) verdict whose gateway and upstream names
//     are EQUAL, so a body that needs no rewrite is relayed verbatim;
//   - asr-denied: an explicit (transcription, no), the verdict a probed model whose
//     task is not asr gets.
//
// The Provider is the real OpenAI-compatible client making a genuine HTTP round
// trip to the stub.
func newTranscriptionCapableTestServer(t *testing.T, upstreamURL string) (*Server, *portal.MemoryDirectory) {
	t.Helper()
	srv, directory := newSpeechCapableTestServer(t, upstreamURL)
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, m := range []struct{ id, gateway, upstream string }{
		{"route-asr-same", "asr-same", "asr-same"},
		{"route-asr-denied", "asr-denied", "whisper_denied"},
	} {
		if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: "app-audio", GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
	}
	for id, verdict := range map[string]string{"route-asr": routing.CapabilityYes, "route-asr-same": routing.CapabilityYes, "route-asr-denied": routing.CapabilityNo} {
		if err := srv.Routes.UpsertMappingCapabilities(ctx, id, []routing.CapabilityRow{{Capability: routing.CapabilityTranscription, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", id, err)
		}
	}
	return srv, directory
}

// newServerAgentTranscriptionTestServer is the server_agent speech fixture with its
// one mapping renamed so the client-facing name (gatewayName) and the name the
// agent's router keys the child on (appName) can differ, and given a
// (transcription, yes) verdict. specFlavors is the runtime spec's flavor list, the
// per-model authority the relay's effective-flavor check reads. The agent has NOT
// declared multipart_proxy: a test that wants one that has calls
// declareTranscriptionMultipartProxy.
func newServerAgentTranscriptionTestServer(t *testing.T, prov provider.Client, gatewayName, appName string, specFlavors []string) *Server {
	t.Helper()
	srv := newServerAgentSpeechSpecTestServer(t, prov, specFlavors)
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.UpdateMapping(ctx, routing.ModelMapping{ID: "route-agent-audio", ApplicationID: "app-agent-audio", GatewayModelName: gatewayName, AppModelName: appName, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, "route-agent-audio", []routing.CapabilityRow{{Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	return srv
}

// declareTranscriptionMultipartProxy records, the way a telemetry sample does,
// that the fixture's agent serves multipart bodies. The feature name is a LITERAL
// on purpose: it is what an agent puts on the wire, so a typo in the gateway's
// constant must fail here rather than agree with itself.
func declareTranscriptionMultipartProxy(srv *Server) {
	srv.AgentFeatures.Set(agentTranscriptionServerID, []string{"runtime_manager", "multipart_proxy"})
}

// postTranscriptionParts sends a bearer-authenticated multipart transcription
// built from parts through the real mux.
func postTranscriptionParts(t *testing.T, srv *Server, parts []editPart) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildEditMultipart(t, parts)
	return postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})
}

// requireSameParts fails unless got and want are the same parts in the same order:
// name, filename, content type and bytes.
func requireSameParts(t *testing.T, got, want []editPart) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("body has %d parts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].filename != want[i].filename || got[i].contentType != want[i].contentType || !bytes.Equal(got[i].body, want[i].body) {
			t.Fatalf("part %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// transcriptionBodyOfSize builds a valid transcription form for model whose whole
// body is exactly size bytes, by padding the audio part. (A multipart.Writer's
// boundary has a fixed length, so the overhead is the same for every build.)
func transcriptionBodyOfSize(t *testing.T, model string, size int) ([]byte, string) {
	t.Helper()
	build := func(n int) ([]byte, string) {
		return buildEditMultipart(t, []editPart{
			{name: "model", body: []byte(model)},
			{name: "file", filename: "long.wav", contentType: "audio/wav", body: bytes.Repeat([]byte{0xAB}, n)},
		})
	}
	probe, _ := build(0)
	body, ct := build(size - len(probe))
	if len(body) != size {
		t.Fatalf("built a %d-byte body, want exactly %d", len(body), size)
	}
	return body, ct
}

// The shape the handler hands the ONE preflight gate. Pinned as values because
// each one is load-bearing: the coarse flavor is what candidacy, targetServesFlavor,
// billingUnitFor and the usage row key on (and upstreamPath answers by string
// equality), the endpoint is what tells a transcription from a speech request that
// shares the flavor (the session extractor, the upstream path, the usage row's
// provider_path), and the capability list does TWO jobs -- it feeds the capability
// gate AND keeps the request off the vendor-account path, which declines a request
// only because it carries one (vendorAccountRoutingEligible has no openai_audio
// twin of its images exclusion, and a vendor account's model match reads the model
// name, never the flavor). A handler that dropped the list would skip the gate and
// become routable to a vendor model of the same name.
func TestTranscriptionInferenceShape(t *testing.T) {
	shape := transcriptionInferenceShape("asr-model")

	// apiFlavorSpeech and routing.APIFlavorOpenAIAudio are pinned to the same string
	// by TestApiFlavorSpeechIsTheRoutingAudioFlavor; the literal is the one wire value.
	if shape.apiFlavor != "openai_audio" {
		t.Fatalf("apiFlavor = %q, want exactly openai_audio (routing.APIFlavorOpenAIAudio = apiFlavorSpeech)", shape.apiFlavor)
	}
	if shape.endpoint != endpointTranscription {
		t.Fatalf("endpoint = %v, want endpointTranscription (it shares the flavor with speech, so only the endpoint tells the two apart)", shape.endpoint)
	}
	if shape.model != "asr-model" {
		t.Fatalf("model = %q, want the model the caller named", shape.model)
	}
	if len(shape.requiredCapabilities) != 1 || shape.requiredCapabilities[0] != routing.CapabilityTranscription {
		t.Fatalf("requiredCapabilities = %v, want exactly [%s]: the list feeds the capability gate AND keeps the request off the vendor-account path", shape.requiredCapabilities, routing.CapabilityTranscription)
	}
	if shape.stream {
		t.Fatal("stream = true, want false: the endpoint never streams")
	}
}

// A transcription is relayed as the multipart request the client sent, to the
// upstream's TRANSCRIPTIONS endpoint, and the JSON answer comes back unchanged.
// When the mapped upstream name differs from the one the client sent (as here), the
// model field -- and only the model field -- is rewritten, in a body of its own
// boundary; every other part keeps its headers, its bytes and its position. Each
// claim is also a regression a mutation would produce: a relay that reached
// /v1/audio/speech, one that sent application/json (which would make proxyNative
// try a JSON model rewrite on a form), one that forgot the rewrite, and one whose
// usage row records the speech path (the flavor is shared, so only the endpoint
// threaded through usageMeta tells them apart).
func TestTranscriptionRelaysMultipartToTheTranscriptionsPathWithTheModelRewritten(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	parts := []editPart{
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
		{name: "model", body: []byte(transcriptionGatewayModel)},
		{name: "language", body: []byte("de")},
		{name: "temperature", body: []byte("0.2")},
		{name: "timestamp_granularities[]", body: []byte("word")},
	}
	body, ct := buildEditMultipart(t, parts)

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want the upstream's application/json", got)
	}
	if rec.Body.String() != transcriptionOKBody {
		t.Fatalf("body = %s, want the upstream's document byte for byte", rec.Body.String())
	}

	call := nextTranscriptionUpstreamCall(t, calls)
	if call.method != http.MethodPost || call.path != "/v1/audio/transcriptions" {
		t.Fatalf("upstream call = %s %s, want POST /v1/audio/transcriptions", call.method, call.path)
	}
	mediaType, params, err := mime.ParseMediaType(call.contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("upstream Content-Type = %q (%v), want multipart/form-data with a boundary", call.contentType, err)
	}
	if call.contentType == ct {
		t.Fatalf("upstream Content-Type = %q is the client's: the rebuilt body must be sent with its own boundary", call.contentType)
	}
	want := append([]editPart(nil), parts...)
	want[1].body = []byte(transcriptionUpstreamModel)
	requireSameParts(t, readEditParts(t, call.body, call.contentType), want)

	ev := lastUsageEvent(t, srv)
	if ev.APIFlavor != apiFlavorSpeech {
		t.Fatalf("APIFlavor = %q, want exactly %q", ev.APIFlavor, apiFlavorSpeech)
	}
	if ev.ReqPath != "/v1/audio/transcriptions" || ev.ProviderPath != "/v1/audio/transcriptions" {
		t.Fatalf("ReqPath/ProviderPath = %q/%q, want /v1/audio/transcriptions for both (the endpoint must reach recordUsage, else the row records the speech path)", ev.ReqPath, ev.ProviderPath)
	}
	if ev.Model != transcriptionGatewayModel || ev.ProviderModel != transcriptionUpstreamModel {
		t.Fatalf("Model/ProviderModel = %q/%q, want %q/%q", ev.Model, ev.ProviderModel, transcriptionGatewayModel, transcriptionUpstreamModel)
	}
	if ev.HTTPStatus != http.StatusOK || ev.Status != "success" || ev.ContentType != "application/json" {
		t.Fatalf("usage row = status %d/%q, content type %q, want 200/success/application/json", ev.HTTPStatus, ev.Status, ev.ContentType)
	}
	if ev.BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("BillingUnit = %q, want %q: a transcription is metered in audio seconds", ev.BillingUnit, usage.BillingUnitAudioSecond)
	}
}

// A form that already carries the name the upstream keys on is relayed VERBATIM:
// the client's bytes and its own Content-Type, boundary included -- the cheapest and
// least damaging thing to do to a body that can carry megabytes of audio.
func TestTranscriptionRelaysVerbatimWhenTheFormCarriesTheMappedName(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	body, ct := buildEditMultipart(t, validTranscriptionParts("asr-same"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	call := nextTranscriptionUpstreamCall(t, calls)
	if call.method != http.MethodPost || call.path != "/v1/audio/transcriptions" {
		t.Fatalf("upstream call = %s %s, want POST /v1/audio/transcriptions", call.method, call.path)
	}
	if call.contentType != ct {
		t.Fatalf("upstream Content-Type = %q, want the client's %q verbatim, boundary included", call.contentType, ct)
	}
	if !bytes.Equal(call.body, body) {
		t.Fatalf("upstream body differs from the client's (%d vs %d bytes): a form that needs no rewrite must be relayed verbatim", len(call.body), len(body))
	}
}

// The rewrite decision is against what the CLIENT sent, not against the mapping's
// own name pair: a token whose model override sends every request to "asr-same"
// reaches a mapping whose two names are equal, yet the form still says whatever the
// client wrote, which the upstream does not know.
func TestTranscriptionRewritesTheModelAnOverrideReplaced(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{
		ID: "tok_ovr", UserID: "usr_dev", Name: "Override Token", Active: true,
		Scopes: []string{"gateway:use"}, ModelOverride: "asr-same",
	}, "ovr-secret")
	body, ct := buildEditMultipart(t, validTranscriptionParts("whisper-1"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer ovr-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	call := nextTranscriptionUpstreamCall(t, calls)
	if got := modelFieldOf(t, call.body, call.contentType); got != "asr-same" {
		t.Fatalf("relayed model = %q, want the mapped name asr-same (the client's whisper-1 is no name the upstream knows)", got)
	}
}

// An empty `file` part beside the one real file is accepted (validation counts the
// NON-EMPTY parts), so the relay must hand the upstream exactly the one file the
// gateway validated: audio.cpp reads the upload from `file`, and given two parts of
// that name it could pick the empty one. The strip happens whether or not the model
// needed rewriting -- a form that carries an empty file part is rebuilt even when
// the names are equal, which a verbatim relay would not do.
func TestTranscriptionRelayDropsAnEmptyFilePart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
	}{
		{"the model needs a rewrite too", transcriptionGatewayModel},
		{"the names are equal, only the empty part forces the rebuild", "asr-same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)
			parts := []editPart{
				{name: "model", body: []byte(tc.model)},
				{name: "file", filename: "empty.wav", contentType: "audio/wav", body: nil},
				{name: "language", body: []byte("de")},
				{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
			}
			body, ct := buildEditMultipart(t, parts)

			rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			call := nextTranscriptionUpstreamCall(t, calls)
			if call.contentType == ct {
				t.Fatal("the form was relayed verbatim although it carries an empty file part: it must be rebuilt without it")
			}
			sent := readEditParts(t, call.body, call.contentType)
			var files int
			for _, p := range sent {
				if p.name == "file" {
					files++
				}
			}
			if files != 1 {
				t.Fatalf("the upstream was sent %d file parts, want exactly the one real one: %+v", files, sent)
			}
			wantModel := transcriptionUpstreamModel
			if tc.model == "asr-same" {
				wantModel = "asr-same"
			}
			requireSameParts(t, sent, []editPart{
				{name: "model", body: []byte(wantModel)},
				{name: "language", body: []byte("de")},
				{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
			})
		})
	}
}

// An upstream's own refusal is relayed under its own status (the error
// normalisation of an unrecognised shape is a separate step), and the usage row
// still records the transcriptions path -- the non-2xx branch records through a
// different site than the success one.
func TestTranscriptionUpstreamErrorRecordsTheTranscriptionsPath(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusInternalServerError, `{"error":{"message":"boom","type":"server_error"}}`)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream's 500; body = %s", rec.Code, rec.Body.String())
	}
	if call := nextTranscriptionUpstreamCall(t, calls); call.path != "/v1/audio/transcriptions" {
		t.Fatalf("upstream path = %q, want /v1/audio/transcriptions", call.path)
	}
	ev := lastUsageEvent(t, srv)
	if ev.Status != "error" || ev.ProviderPath != "/v1/audio/transcriptions" || ev.BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("usage row = %+v, want an error row on /v1/audio/transcriptions billed in audio seconds", ev)
	}
}

// A request this endpoint cannot honor is a gateway 400 BEFORE anything is
// resolved or relayed: audio.cpp answers a client error with a 500, which must not
// be relayed as a server failure, and a refused request spends no admission and
// records no usage.
func TestTranscriptionRejectsAnInvalidRequestBeforeRouting(t *testing.T) {
	valid := validTranscriptionParts(transcriptionGatewayModel)
	with := func(extra ...editPart) []editPart { return append(append([]editPart(nil), valid...), extra...) }
	cases := []struct {
		name     string
		parts    []editPart
		wantCode string
	}{
		{"missing model", replaceEditPart(valid, "model", nil), "transcription.model_required"},
		{"missing file", replaceEditPart(valid, "file", nil), "transcription.file_required"},
		{"empty file", replaceEditPart(valid, "file", &editPart{name: "file", filename: "e.wav"}), "transcription.file_required"},
		{"two files", with(editPart{name: "file", filename: "b.wav", body: []byte("x")}), "transcription.invalid"},
		{"verbose_json", with(editPart{name: "response_format", body: []byte("verbose_json")}), "transcription.response_format_unsupported"},
		{"srt", with(editPart{name: "response_format", body: []byte("srt")}), "transcription.response_format_unsupported"},
		{"stream", with(editPart{name: "stream", body: []byte("true")}), "transcription.stream_unsupported"},
		{"stream_format", with(editPart{name: "stream_format", body: []byte("sse")}), "transcription.stream_unsupported"},
		{"a repeated model", with(editPart{name: "model", body: []byte("other")}), "transcription.invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, tc.parts)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoTranscriptionUpstreamCall(t, calls)
			if events := srv.Usage.All(); len(events) != 0 {
				t.Fatalf("a rejected request recorded usage %+v, want none", events)
			}
		})
	}
	t.Run("not multipart", func(t *testing.T) {
		up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", []byte(`{"model":"asr-model"}`), "application/json", map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "transcription.invalid")
		requireNoTranscriptionUpstreamCall(t, calls)
	})
	t.Run("a truncated body", func(t *testing.T) {
		up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)
		body, ct := buildEditMultipart(t, valid)

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body[:len(body)-40], ct, map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "transcription.invalid")
		requireNoTranscriptionUpstreamCall(t, calls)
	})
}

// A form the client sent with an explicit response_format "json" and stream "false"
// is what the endpoint already does, so it is accepted rather than refused.
func TestTranscriptionAcceptsTheValuesItAlreadyDoes(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, append(validTranscriptionParts(transcriptionGatewayModel),
		editPart{name: "response_format", body: []byte("json")},
		editPart{name: "stream", body: []byte("false")},
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	nextTranscriptionUpstreamCall(t, calls)
}

// The capability gate, end to end through the handler: only a (transcription, yes)
// mapping is served. A speech-only model (it has no transcription row: the gate
// refuses on unknown, it does not fail open), an explicit no, and a model never
// probed are refused as not capable; a chat model on a text application and an
// unknown model are no route at all. No refusal reaches the upstream, and each
// records a usage row that claims no upstream call -- billed in audio seconds with a
// zero quantity, since the unit is the endpoint's identity.
func TestTranscriptionRefusesAModelWithoutATranscriptionVerdict(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		wantCode string
	}{
		{"a speech-only model", "tts-voice", "routing.model_not_capable"},
		{"an explicit no", "asr-denied", "routing.model_not_capable"},
		{"an audio model that was never probed", "unprobed-voice", "routing.model_not_capable"},
		{"a chat model on a text application", "qwen-coder", "routing.no_model_route"},
		{"an unknown model", "no-such-asr", "routing.no_model_route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, validTranscriptionParts(tc.model))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoTranscriptionUpstreamCall(t, calls)

			events := srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage events = %+v, want exactly one refusal row", events)
			}
			ev := events[0]
			if ev.ReqPath != "/v1/audio/transcriptions" || ev.APIFlavor != apiFlavorSpeech {
				t.Fatalf("ReqPath/APIFlavor = %q/%q, want /v1/audio/transcriptions and %q", ev.ReqPath, ev.APIFlavor, apiFlavorSpeech)
			}
			if ev.HTTPStatus != http.StatusNotFound || ev.Status != "error" || ev.ErrorCode != tc.wantCode {
				t.Fatalf("row = status %d/%q/%q, want 404/error/%s", ev.HTTPStatus, ev.Status, ev.ErrorCode, tc.wantCode)
			}
			if ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 0 {
				t.Fatalf("row billing = %q/%v, want an audio_second row with quantity 0", ev.BillingUnit, ev.BillingQuantity)
			}
			// No upstream call was made, so the row must not claim one: a populated
			// Host would make the energy reconciler price it against a server that
			// never saw the request.
			if ev.Host != "" || ev.RouteID != "" || ev.ProviderPath != "" {
				t.Fatalf("row = %+v, want Host/RouteID/ProviderPath empty", ev)
			}
		})
	}
}

// The two audio capabilities are orthogonal at the handler too: the transcription
// model serves transcriptions and is refused speech, the speech model the other
// way round (TestTranscriptionRefusesAModelWithoutATranscriptionVerdict has the
// tts-voice half).
func TestTranscriptionAndSpeechGatesAreOrthogonal(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"asr-model","input":"hi"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("speech to a transcription-only model: status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	requireNoTranscriptionUpstreamCall(t, calls)

	if rec := postTranscriptionParts(t, srv, validTranscriptionParts("asr-model")); rec.Code != http.StatusOK {
		t.Fatalf("transcription to the same model: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// A route affinity pin is the one resolve branch with no candidate filter of its
// own: its mapping is read straight off the pinned application. A pin to a mapping
// WITHOUT a transcription verdict must therefore not be honored for a transcription
// -- the request is refused as not capable, not served by the pinned (speech-only)
// mapping, and the upstream is never called. The pin is seeded under the request's
// own key (token, model, the coarse openai_audio flavor, the explicit session
// header), the only key the resolve looks up for it; a pin under any other key
// would never be found and the test would pass without the gate running at all.
// The refusal is non-destructive: the pin survives for the endpoint it does serve.
func TestTranscriptionDoesNotHonourAnAffinityPinToANonTranscriptionMapping(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	// A pin is only honored for an application with an affinity TTL (anything else is
	// dropped as stale before the capability gate is even reached), so the audio
	// application gets one: the test is about the gate, not about that precondition.
	ctx := context.Background()
	app, err := srv.Routes.ApplicationByID(ctx, "app-audio")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.AffinityTTLSeconds = 1800
	if err := srv.Routes.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	now := time.Now()
	pin := routing.RouteAffinity{
		ID: "aff_transcription_pin", APITokenID: "tok_dev", Model: "tts-voice", APIFlavor: routing.APIFlavorOpenAIAudio, SessionID: "sess-pin",
		ApplicationID: "app-audio", ServerID: "srv-audio", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := srv.Routes.UpsertAffinity(ctx, pin); err != nil {
		t.Fatalf("seed affinity: %v", err)
	}
	body, ct := buildEditMultipart(t, validTranscriptionParts("tts-voice"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret", sessionHeaderName: "sess-pin"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (the pinned mapping has no transcription verdict); body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
	requireNoTranscriptionUpstreamCall(t, calls)
	key := routing.AffinityKey{APITokenID: "tok_dev", Model: "tts-voice", APIFlavor: routing.APIFlavorOpenAIAudio, SessionID: "sess-pin"}
	if _, ok, err := srv.Routes.Affinity(ctx, key); err != nil || !ok {
		t.Fatalf("the affinity row did not survive the refusal (ok=%v, err=%v): it was dropped before the capability gate, or by it", ok, err)
	}
}

// The method is checked before anything else.
func TestTranscriptionRejectsAGet(t *testing.T) {
	srv, _ := newSpeechCapableTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/v1/audio/transcriptions", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d, Allow = %q, want 405 with Allow: POST", rec.Code, rec.Header().Get("Allow"))
	}
}

// A request with no credentials is refused before any body handling.
func TestTranscriptionRequiresAuthentication(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireNoTranscriptionUpstreamCall(t, calls)
}

// A bearer token and the internal trusted-loopback pair are the two ways in, on the
// endpoint's path and on its /openai alias alike.
func TestTranscriptionAcceptsABearerAndTheInternalLoopback(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, _ := newTranscriptionCapableTestServer(t, up.URL)
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))

	for _, path := range []string{"/v1/audio/transcriptions", "/openai/v1/audio/transcriptions"} {
		for name, hdr := range map[string]map[string]string{
			"bearer":   {"Authorization": "Bearer dev-secret"},
			"loopback": {internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"},
		} {
			rec := postEditRaw(t, srv, http.MethodPost, path, body, ct, hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s via %s: status = %d, want 200; body = %s", path, name, rec.Code, rec.Body.String())
			}
			if call := nextTranscriptionUpstreamCall(t, calls); call.path != "/v1/audio/transcriptions" {
				t.Fatalf("%s via %s: upstream path = %q, want /v1/audio/transcriptions", path, name, call.path)
			}
		}
	}
}

// THE BOUNDARY: a browser session cookie must NOT reach this endpoint (a logged-in
// browser never calls an inference endpoint directly; AGENTS.md). A 401 proves auth
// refused: had the cookie been accepted the request would have failed later and
// differently (the capability gate, a 404), never with a 401.
func TestTranscriptionRefusesASessionCookie(t *testing.T) {
	srv, dir := newChatTestServer(t)
	seedLoginUser(t, dir, "usr_transcription", "transcription@example.test", "password-1", "user")
	cookie := loginCookie(t, srv, "transcription@example.test", "password-1")
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct,
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, csrfHeaderName: "1"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from a real session cookie: %s", rec.Code, rec.Body.String())
	}
}

// The run-as block is the shared helper's: a loopback request naming a token the
// loopback user owns is attributed to that token, not to the bare loopback
// principal.
func TestTranscriptionRunAsTokenAttributesUsage(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, dir := newTranscriptionCapableTestServer(t, up.URL)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ra_transcription", UserID: "usr_dev", Name: "RA Transcription", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "ra-secret"); err != nil {
		t.Fatalf("seed run-as token: %v", err)
	}
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev", runAsHeaderName: "tok_ra_transcription"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := lastUsageEvent(t, srv).TokenID; got != "tok_ra_transcription" {
		t.Fatalf("TokenID = %q, want tok_ra_transcription (the run-as token, not the bare loopback principal)", got)
	}
}

// Naming a run-as token the loopback user does not own is a 403, refused rather
// than downgraded to the bare principal, and nothing reaches the upstream or the
// usage log.
func TestTranscriptionRunAsTokenForbiddenForAnUnownedToken(t *testing.T) {
	up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, dir := newTranscriptionCapableTestServer(t, up.URL)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_other_transcription", UserID: "usr_other", Name: "Other", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "other-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct,
		map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev", runAsHeaderName: "tok_other_transcription"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "portal.token_forbidden")
	requireNoTranscriptionUpstreamCall(t, calls)
	if events := srv.Usage.All(); len(events) != 0 {
		t.Fatalf("a forbidden run-as recorded usage %+v, want none", events)
	}
}

// A bare &Server{} (no Portal) must refuse a run-as request rather than panic or
// proceed as the bare loopback principal.
func TestTranscriptionRunAsWithoutPortalIsRefused(t *testing.T) {
	s := &Server{internalAuthSecret: "s3cret", users: fakeUserLookup{
		"usr_1": {ID: "usr_1", Role: "user"},
	}}
	body, ct := buildEditMultipart(t, validTranscriptionParts(transcriptionGatewayModel))
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	r.Header.Set(runAsHeaderName, "tok_whatever")
	w := httptest.NewRecorder()

	s.handleOpenAITranscription(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body = %s, want 403", w.Code, w.Body.String())
	}
	requireErrorCode(t, w.Body.String(), "portal.token_forbidden")
}

// The multipart read is bounded, end to end: a body past the server's cap is a 413
// whether its size was declared up front -- refused without reading a byte of it --
// or only discovered while reading (chunked), when the read stops at the cap. The
// cap is the server's setting (maxAudioTranscriptionBodyBytes), lowered here so the
// fixture stays small; the default and its wiring are TestTranscriptionBodyCapIsASetting's.
func TestTranscriptionRefusesABodyPastTheCap(t *testing.T) {
	const limit = 1 << 20
	body, ct := transcriptionBodyOfSize(t, transcriptionGatewayModel, limit+1)
	for _, declared := range []bool{true, false} {
		name := "chunked"
		if declared {
			name = "declared length"
		}
		t.Run(name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)
			srv.maxAudioTranscriptionBodyBytes = limit
			counted := &countingReader{r: bytes.NewReader(body)}
			req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", io.NopCloser(counted))
			if declared {
				req.ContentLength = int64(len(body))
			} else {
				req.ContentLength = -1
			}
			req.Header.Set("Content-Type", ct)
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "request.body_too_large")
			requireNoTranscriptionUpstreamCall(t, calls)
			switch {
			case declared && counted.n != 0:
				t.Fatalf("read %d bytes of a body whose declared length was already past the cap", counted.n)
			case !declared && counted.n > limit+(64<<10):
				t.Fatalf("read %d bytes of a chunked body, want the read to stop at the %d cap", counted.n, limit)
			}
		})
	}
	t.Run("a body exactly at the cap is read", func(t *testing.T) {
		up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
		srv, _ := newTranscriptionCapableTestServer(t, up.URL)
		srv.maxAudioTranscriptionBodyBytes = limit
		atCap, atCapCT := transcriptionBodyOfSize(t, "asr-same", limit)

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", atCap, atCapCT, map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for a body exactly at the cap; body = %s", rec.Code, rec.Body.String())
		}
		nextTranscriptionUpstreamCall(t, calls)
	})
}

// --- The agent gate ---------------------------------------------------------

// A transcription relays as MULTIPART, and an agent's router that predates the
// multipart branch peeks the model out of a JSON body: it cannot read one out of a
// form, so it answers 404 runtime.model_not_managed -- the same status and code it
// answers an unmanaged model with, which is why trying the route cannot tell the two
// apart. The gateway therefore refuses a transcription to an agent target unless
// that agent has DECLARED multipart_proxy, and refuses it as itself
// (transcription.unavailable) instead of relaying a request that is known to 404 --
// and NOT as the unknown-model 404 it would otherwise look like.
//
// Every case is an agent the gateway has no positive evidence about, so each is
// refused: fail-closed is the registry's own default (AgentFeatures.Has). Each is
// falsifiable against a gate that is too loose -- "any features reported", "the
// feature on some server", a prefix match -- and against one that is not a gate at
// all (ProxyNative must not be reached, and no upstream call may be recorded).
func TestTranscriptionRefusesAnAgentTargetWithoutMultipartProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared func(*Server)
	}{
		{"the agent has never reported", func(*Server) {}},
		{"the agent reports other features but not multipart_proxy", func(srv *Server) {
			srv.AgentFeatures.Set(agentTranscriptionServerID, []string{"runtime_manager", "runtime_ensure", "runtime_model_probe"})
		}},
		{"only another server declares multipart_proxy", func(srv *Server) {
			srv.AgentFeatures.Set("srv-elsewhere", []string{"multipart_proxy"})
		}},
		{"a name that merely resembles it", func(srv *Server) {
			srv.AgentFeatures.Set(agentTranscriptionServerID, []string{"multipart", "multipart-proxy", "multipart_proxy_v2"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: transcriptionOKBody}
			srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})
			tc.declared(srv)

			rec := postTranscriptionParts(t, srv, validTranscriptionParts("agent-asr"))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "transcription.unavailable")
			body := rec.Body.String()
			for _, want := range []string{"not available", "multipart_proxy", "older than 0.9.0", "has not reported"} {
				if !strings.Contains(body, want) {
					t.Fatalf("body = %s, want a message containing %q (an agent too old or not yet reported, not an unknown model)", body, want)
				}
			}
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0 -- a transcription must never be sent multipart to an agent that did not declare multipart_proxy", prov.proxyCalls)
			}
			events := srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage events = %d, want exactly one error row", len(events))
			}
			ev := events[0]
			if ev.ErrorCode != "transcription.unavailable" || ev.HTTPStatus != http.StatusNotFound || ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 0 {
				t.Fatalf("usage row = %+v, want a transcription.unavailable 404 billed as audio_second with quantity 0", ev)
			}
			if ev.ReqPath != "/v1/audio/transcriptions" || ev.APIFlavor != apiFlavorSpeech {
				t.Fatalf("usage row ReqPath/APIFlavor = %q/%q, want /v1/audio/transcriptions and %q", ev.ReqPath, ev.APIFlavor, apiFlavorSpeech)
			}
			if ev.Host != "" || ev.RouteID != "" || ev.ProviderPath != "" {
				t.Fatalf("usage row = %+v, want Host/RouteID/ProviderPath empty: no upstream call was made", ev)
			}
		})
	}
}

// The positive control of the gate, and its edge: an agent that declares
// multipart_proxy is relayed to (the transcriptions path, the multipart
// Content-Type), and the registry is a FULL snapshot per sample, so an agent that
// stops declaring it -- a downgrade, an older binary reconnecting -- is refused
// again on its very next request rather than keeping a stale grant.
func TestTranscriptionRelaysToAnAgentTargetThatDeclaresMultipartProxyAndStopsWhenItDoesNot(t *testing.T) {
	prov := &recordingProxyProvider{respBody: transcriptionOKBody}
	srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})
	declareTranscriptionMultipartProxy(srv)
	body, ct := buildEditMultipart(t, validTranscriptionParts("agent-asr"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/audio/transcriptions" || prov.gotContentType != ct || !bytes.Equal(prov.gotBody, body) {
		t.Fatalf("ProxyNative calls/path/Content-Type = %d/%q/%q, want 1, /v1/audio/transcriptions and the client's multipart form verbatim", prov.proxyCalls, prov.gotPath, prov.gotContentType)
	}

	// The agent reconnects as a build that no longer declares it.
	srv.AgentFeatures.Set(agentTranscriptionServerID, []string{"runtime_manager"})

	rec = postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status after the agent stopped declaring the feature = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "transcription.unavailable")
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want still 1 -- the refused request must not be relayed", prov.proxyCalls)
	}
}

// The gate is on the TARGET being an agent, not on the server having one: an
// EXTERNAL audio.cpp application (one the operator registered, with no agent and no
// router in between) has nothing to gate and relays a transcription whatever the
// feature registry says. The second case tells "the target is an agent" apart from
// "the server has an agent": the server DID report features, none of them
// multipart_proxy, and the request still goes through.
func TestTranscriptionToAnExternalTargetIsNotGatedOnMultipartProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared func(*Server)
	}{
		{"the server never reported features", func(*Server) {}},
		{"the server reports features without multipart_proxy", func(srv *Server) {
			srv.AgentFeatures.Set("srv-audio", []string{"runtime_manager"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, calls := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)
			tc.declared(srv)

			rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			nextTranscriptionUpstreamCall(t, calls)
		})
	}
}

// The effective-served conjunct (ADR-044 b): an agent-launched child whose runtime
// spec lists only text cannot be picked to serve a transcription, and the refusal
// looks like candidacy's own no-route answer (and is not the multipart_proxy
// refusal, which comes after it). The positive control is the same fixture with a
// spec that lists openai_audio.
func TestTranscriptionRefusesAServerAgentChildWhoseSpecExcludesAudio(t *testing.T) {
	t.Run("a text-only spec is refused", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: transcriptionOKBody}
		srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAI})
		declareTranscriptionMultipartProxy(srv)

		rec := postTranscriptionParts(t, srv, validTranscriptionParts("agent-asr"))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
		if prov.proxyCalls != 0 {
			t.Fatalf("ProxyNative calls = %d, want 0 -- a text-only spec must never be sent a transcription", prov.proxyCalls)
		}
		events := srv.Usage.All()
		if len(events) != 1 || events[0].ErrorCode != "routing.no_model_route" || events[0].BillingUnit != usage.BillingUnitAudioSecond {
			t.Fatalf("usage events = %+v, want one routing.no_model_route error billed as audio_second", events)
		}
		if ev := events[0]; ev.Host != "" || ev.RouteID != "" || ev.ProviderPath != "" || ev.ReqPath != "/v1/audio/transcriptions" {
			t.Fatalf("usage row = %+v, want an upstream-less row on /v1/audio/transcriptions", ev)
		}
	})
	t.Run("an audio spec is served", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: transcriptionOKBody}
		srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "agent-asr", []string{routing.APIFlavorOpenAIAudio})
		declareTranscriptionMultipartProxy(srv)

		rec := postTranscriptionParts(t, srv, validTranscriptionParts("agent-asr"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if prov.proxyCalls != 1 {
			t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
		}
	})
}

// For an agent-launched target the agent's router keys the child on the mapping's
// AppModelName, which is not the name the client sent: the relayed form carries the
// app name in its model field, and the request goes out with the rebuilt body's own
// Content-Type.
func TestTranscriptionRewritesTheModelForAnAgentTarget(t *testing.T) {
	prov := &recordingProxyProvider{respBody: transcriptionOKBody}
	srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "asr-q8-gguf", []string{routing.APIFlavorOpenAIAudio})
	declareTranscriptionMultipartProxy(srv)
	parts := []editPart{
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
		{name: "model", body: []byte("agent-asr")},
		{name: "language", body: []byte("de")},
	}
	body, ct := buildEditMultipart(t, parts)

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/audio/transcriptions" {
		t.Fatalf("ProxyNative calls/path = %d/%q, want 1 and /v1/audio/transcriptions", prov.proxyCalls, prov.gotPath)
	}
	if prov.gotContentType == ct {
		t.Fatalf("Content-Type = %q is the client's: the rebuilt body must be sent with its own boundary", prov.gotContentType)
	}
	want := append([]editPart(nil), parts...)
	want[1].body = []byte("asr-q8-gguf")
	requireSameParts(t, readEditParts(t, prov.gotBody, prov.gotContentType), want)
	if ev := lastUsageEvent(t, srv); ev.Model != "agent-asr" || ev.ProviderPath != "/v1/audio/transcriptions" {
		t.Fatalf("usage Model/ProviderPath = %q/%q, want agent-asr and /v1/audio/transcriptions", ev.Model, ev.ProviderPath)
	}
}

// What the payload capture records for a REBUILT form is the CLIENT's request, not
// the body the agent was sent: nativeRelay.raw stays the client's bytes and the
// rebuilt one travels beside it as upstreamBody. It is recorded as the multipart
// summary (text fields and file metadata; the audio is redacted), so the model field
// names what the CLIENT sent -- not the app name the rebuild put in -- and the audio
// bytes are nowhere.
func TestTranscriptionCaptureRecordsTheClientsBodyNotTheRebuiltOne(t *testing.T) {
	prov := &recordingProxyProvider{respBody: transcriptionOKBody}
	srv := newServerAgentTranscriptionTestServer(t, prov, "agent-asr", "asr-q8-gguf", []string{routing.APIFlavorOpenAIAudio})
	declareTranscriptionMultipartProxy(srv)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	srv.Captures, srv.Cipher = cs, cipher
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{
		ID: "tok_cap", UserID: "usr_dev", Name: "Capture Token", Active: true,
		Scopes: []string{"gateway:use"}, LogCommunication: true,
	}, "cap-secret")
	body, ct := buildEditMultipart(t, []editPart{
		{name: "model", body: []byte("agent-asr")},
		{name: "language", body: []byte("de")},
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
	})

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer cap-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(prov.gotBody), "asr-q8-gguf") {
		t.Fatalf("the agent was not sent the rebuilt body: %q", prov.gotBody)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	doc := decodeCapturedMultipart(t, env.ReqBody)
	if len(doc.Parts) != 3 || doc.Parts[0].Text == nil || *doc.Parts[0].Text != "agent-asr" {
		t.Fatalf("captured request = %q, want the CLIENT's model field (agent-asr), not the rebuilt app name", env.ReqBody)
	}
	if p := doc.Parts[2]; p.Name != "file" || p.Filename != "clip.wav" || p.Size != int64(len(transcriptionTestAudio)) || !p.Redacted {
		t.Fatalf("captured file part = %+v, want redacted metadata", p)
	}
	if strings.ContainsRune(env.ReqBody, '�') || strings.Contains(env.ReqBody, "not-a-boundary") {
		t.Fatalf("the capture holds binary audio bytes (or their corruption): %q", env.ReqBody)
	}
}

// The body that is finally sent to an agent-launched target must fit the agent
// router's own request limit (agentRouterMaxBodyBytes), which the cap on what the
// client sends does not guarantee: rebuilding can GROW a body (here, a longer model
// name). The boundary is exact -- a rebuilt body of exactly the limit is relayed, one
// byte more is refused 413 request.body_too_large by the gateway itself, recorded,
// and never sent. The check is the AGENT's: an external target is relayed a body of
// any size the client cap lets through.
func TestTranscriptionRechecksTheAgentBodyLimitAfterTheRewrite(t *testing.T) {
	const gatewayName = "agent-asr"
	appName := "agent-asr-" + strings.Repeat("q", 30) // 30 bytes longer, so the rewrite grows the body by 30
	grow := len(appName) - len(gatewayName)

	t.Run("a rebuilt body exactly at the limit is relayed", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: transcriptionOKBody}
		srv := newServerAgentTranscriptionTestServer(t, prov, gatewayName, appName, []string{routing.APIFlavorOpenAIAudio})
		declareTranscriptionMultipartProxy(srv)
		body, ct := transcriptionBodyOfSize(t, gatewayName, agentRouterMaxBodyBytes-grow)

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for a rebuilt body exactly at the agent's limit; body = %s", rec.Code, rec.Body.String())
		}
		if prov.proxyCalls != 1 || len(prov.gotBody) != agentRouterMaxBodyBytes {
			t.Fatalf("ProxyNative calls = %d, sent %d bytes, want 1 call of exactly %d bytes", prov.proxyCalls, len(prov.gotBody), agentRouterMaxBodyBytes)
		}
	})
	t.Run("a rebuilt body one byte over the limit is refused", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: transcriptionOKBody}
		srv := newServerAgentTranscriptionTestServer(t, prov, gatewayName, appName, []string{routing.APIFlavorOpenAIAudio})
		declareTranscriptionMultipartProxy(srv)
		// The client's own body fits the transcription cap (and the agent's limit).
		body, ct := transcriptionBodyOfSize(t, gatewayName, agentRouterMaxBodyBytes-grow+1)
		if int64(len(body)) > srv.transcriptionBodyLimit() || len(body) > agentRouterMaxBodyBytes {
			t.Fatalf("fixture is %d bytes, want one the client cap and the agent limit both accept", len(body))
		}

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "request.body_too_large")
		if !strings.Contains(rec.Body.String(), "32 MiB") {
			t.Fatalf("body = %s, want a message naming the agent's 32 MiB limit", rec.Body.String())
		}
		if prov.proxyCalls != 0 {
			t.Fatalf("ProxyNative calls = %d, want 0 -- an over-limit body must never be sent to the agent", prov.proxyCalls)
		}
		events := srv.Usage.All()
		if len(events) != 1 {
			t.Fatalf("usage events = %d, want exactly one refusal row", len(events))
		}
		ev := events[0]
		if ev.ErrorCode != "request.body_too_large" || ev.HTTPStatus != http.StatusRequestEntityTooLarge || ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != 0 {
			t.Fatalf("usage row = %+v, want a request.body_too_large 413 billed as audio_second with quantity 0", ev)
		}
		if ev.Host != "" || ev.RouteID != "" || ev.ProviderPath != "" {
			t.Fatalf("usage row = %+v, want Host/RouteID/ProviderPath empty: no upstream call was made", ev)
		}
	})
	t.Run("an external target is not held to the agent's limit", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: transcriptionOKBody}
		srv, _ := newTranscriptionCapableTestServer(t, "http://127.0.0.1:1")
		srv.Provider = prov
		// The client cap is raised above the agent's; the body is over it, and the
		// rewrite (asr-model -> whisper_x) rebuilds it.
		srv.maxAudioTranscriptionBodyBytes = 2 * agentRouterMaxBodyBytes
		body, ct := transcriptionBodyOfSize(t, transcriptionGatewayModel, agentRouterMaxBodyBytes+1)

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/audio/transcriptions", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: the agent's limit does not apply to an external target; body = %s", rec.Code, rec.Body.String())
		}
		if prov.proxyCalls != 1 || len(prov.gotBody) <= agentRouterMaxBodyBytes {
			t.Fatalf("ProxyNative calls = %d, sent %d bytes, want 1 call of more than %d bytes", prov.proxyCalls, len(prov.gotBody), agentRouterMaxBodyBytes)
		}
	})
}

// --- The pure pieces --------------------------------------------------------

// transcriptionTargetServed: an agent-launched target is served only on positive
// evidence (fail-closed, including a nil registry); any other target is served
// whatever the registry says.
func TestTranscriptionTargetServed(t *testing.T) {
	agent := routing.Target{Provider: routing.ProviderServerAgent, ServerID: "srv-1"}
	external := routing.Target{Provider: routing.ProviderAudioCpp, ServerID: "srv-1"}
	declared := &Server{AgentFeatures: NewAgentFeaturesRegistry()}
	declared.AgentFeatures.Set("srv-1", []string{"multipart_proxy"})
	other := &Server{AgentFeatures: NewAgentFeaturesRegistry()}
	other.AgentFeatures.Set("srv-1", []string{"runtime_manager"})
	for _, tc := range []struct {
		name   string
		srv    *Server
		target routing.Target
		want   bool
	}{
		{"an agent that declared it", declared, agent, true},
		{"an agent that reported without it", other, agent, false},
		{"an agent on a server that never reported", &Server{AgentFeatures: NewAgentFeaturesRegistry()}, agent, false},
		{"an agent with no registry at all", &Server{}, agent, false},
		{"an external target, nothing declared", &Server{AgentFeatures: NewAgentFeaturesRegistry()}, external, true},
		{"an external target, no registry", &Server{}, external, true},
		{"an external target on a server that reported without it", other, external, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.srv.transcriptionTargetServed(tc.target); got != tc.want {
				t.Fatalf("transcriptionTargetServed = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rewrite predicate, axis by axis: any target with a mapped name the form does
// not already carry is rewritten (the provider does not matter -- the JSON relay's
// rewriteModelField does not look at it either), and one whose form already says it,
// or which has no mapped name, is not.
func TestTranscriptionNeedsModelRewrite(t *testing.T) {
	agent := func(app string) routing.Target {
		return routing.Target{Provider: routing.ProviderServerAgent, Model: "gw", ProviderModel: app}
	}
	external := func(app string) routing.Target {
		return routing.Target{Provider: routing.ProviderAudioCpp, Model: "gw", ProviderModel: app}
	}
	for _, tc := range []struct {
		name   string
		target routing.Target
		form   string
		want   bool
	}{
		{"agent, form carries the gateway name", agent("app"), "gw", true},
		{"agent, form already carries the app name", agent("app"), "app", false},
		{"agent, form carries neither (override / redirect)", agent("app"), "something-else", true},
		{"agent, form spelled with surrounding space", agent("app"), " app ", true},
		{"agent, no mapped name", agent(""), "gw", false},
		{"external target, names differ", external("app"), "gw", true},
		{"external target, form already carries the mapped name", external("app"), "app", false},
		{"external target, names equal and the form carries them", external("gw"), "gw", false},
		{"external target, names equal but the form carries another (override / redirect)", external("gw"), "something-else", true},
		{"external target, no mapped name", external(""), "gw", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transcriptionNeedsModelRewrite(tc.target, tc.form); got != tc.want {
				t.Fatalf("transcriptionNeedsModelRewrite = %v, want %v", got, tc.want)
			}
		})
	}
}

// rebuildTranscriptionBody replaces the model field's value (when asked to) and
// drops the empty `file` parts, and copies every other part exactly as the client
// sent it: its headers, its bytes and its position. It returns a Content-Type for
// the new body (a fresh boundary).
func TestRebuildTranscriptionBody(t *testing.T) {
	parts := []editPart{
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
		{name: "model", body: []byte("gw")},
		{name: "file", filename: "empty.wav", contentType: "audio/wav", body: nil},
		{name: "prompt", body: []byte("--not-a-boundary\r\n")},
		{name: "reference", filename: "r.bin", contentType: "application/octet-stream", body: []byte("\x00\xff\r\n\x00")},
	}
	body, ct := buildEditMultipart(t, parts)
	form, err := parseTranscriptionForm(ct, body)
	if err != nil {
		t.Fatalf("parseTranscriptionForm: %v", err)
	}

	t.Run("replaces the model and drops the empty file part", func(t *testing.T) {
		out, outCT, err := rebuildTranscriptionBody(body, form.boundary, "mapped-name")
		if err != nil {
			t.Fatalf("rebuildTranscriptionBody: %v", err)
		}
		if outCT == ct || mustBoundary(t, outCT) == form.boundary {
			t.Fatalf("Content-Type = %q, want a fresh boundary (not %q)", outCT, form.boundary)
		}
		requireSameParts(t, readEditParts(t, out, outCT), []editPart{
			parts[0],
			{name: "model", body: []byte("mapped-name")},
			parts[3],
			parts[4],
		})
	})
	t.Run("an empty model keeps the model field and still drops the empty file part", func(t *testing.T) {
		out, outCT, err := rebuildTranscriptionBody(body, form.boundary, "")
		if err != nil {
			t.Fatalf("rebuildTranscriptionBody: %v", err)
		}
		requireSameParts(t, readEditParts(t, out, outCT), []editPart{parts[0], parts[1], parts[3], parts[4]})
	})
	t.Run("the rebuilt form parses to the same single file and the new model", func(t *testing.T) {
		out, outCT, err := rebuildTranscriptionBody(body, form.boundary, "mapped-name")
		if err != nil {
			t.Fatalf("rebuildTranscriptionBody: %v", err)
		}
		got, err := parseTranscriptionForm(outCT, out)
		if err != nil || got.model != "mapped-name" || got.files != 1 || got.emptyFiles != 0 {
			t.Fatalf("parse of the rebuilt form = %+v, %v, want model mapped-name, one file, no empty file", got, err)
		}
	})
	t.Run("a body that is not the form it was parsed from is an error, not a panic", func(t *testing.T) {
		if _, _, err := rebuildTranscriptionBody(body[:len(body)-40], form.boundary, "mapped-name"); err == nil {
			t.Fatal("rebuilding a truncated body succeeded, want an error")
		}
	})
}

// The parse counts the empty `file` parts the relay will drop, apart from the
// non-empty ones validation counts.
func TestParseTranscriptionFormCountsEmptyFileParts(t *testing.T) {
	body, ct := buildEditMultipart(t, []editPart{
		{name: "model", body: []byte("asr-model")},
		{name: "file", filename: "empty.wav", body: nil},
		{name: "file", filename: "clip.wav", body: transcriptionTestAudio},
		{name: "file", filename: "empty2.wav", body: nil},
		{name: "file[]", filename: "also-empty.wav", body: nil},
	})

	form, err := parseTranscriptionForm(ct, body)

	if err != nil || form.files != 1 || form.emptyFiles != 2 {
		t.Fatalf("err = %v, files = %d, emptyFiles = %d, want one file and two empty `file` parts (a file[] part is neither)", err, form.files, form.emptyFiles)
	}
}

// The two refusals the relay writes itself are mapped to a status, a code and a
// message in ONE place (completionHTTPStatus, completionErrorCode,
// completionErrorResponse), so the response and the usage row cannot disagree about
// them. They are distinct values from the image edit's twins even where the code is
// shared: a mapping that matched one sentinel for the other would answer an edit
// with the transcription message.
func TestTranscriptionSentinelsMapToTheirStatusCodeAndMessage(t *testing.T) {
	if codeTranscriptionUnavailable != "transcription.unavailable" {
		t.Fatalf("codeTranscriptionUnavailable = %q, want transcription.unavailable (API surface)", codeTranscriptionUnavailable)
	}
	for _, tc := range []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage []string
	}{
		{"unavailable", errTranscriptionUnavailable, http.StatusNotFound, "transcription.unavailable", []string{"transcription is not available", "multipart_proxy", "older than 0.9.0", "has not reported"}},
		{"unavailable, wrapped", fmt.Errorf("relay: %w", errTranscriptionUnavailable), http.StatusNotFound, "transcription.unavailable", []string{"transcription is not available"}},
		{"too large", errTranscriptionTooLarge, http.StatusRequestEntityTooLarge, "request.body_too_large", []string{"transcription is too large", "32 MiB"}},
		{"too large, wrapped", fmt.Errorf("relay: %w", errTranscriptionTooLarge), http.StatusRequestEntityTooLarge, "request.body_too_large", []string{"transcription is too large"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := completionHTTPStatus(tc.err); got != tc.wantStatus {
				t.Fatalf("completionHTTPStatus = %d, want %d", got, tc.wantStatus)
			}
			if got := completionErrorCode(tc.err); got != tc.wantCode {
				t.Fatalf("completionErrorCode = %q, want %q", got, tc.wantCode)
			}
			resp := completionErrorResponse(tc.err)
			if resp.Error.Code != tc.wantCode {
				t.Fatalf("response code = %q, want %q", resp.Error.Code, tc.wantCode)
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(resp.Error.Message, want) {
					t.Fatalf("message = %q, want it to contain %q", resp.Error.Message, want)
				}
			}
			if strings.Contains(resp.Error.Message, "image edit") {
				t.Fatalf("message = %q reads as the image edit's", resp.Error.Message)
			}
		})
	}
	if errors.Is(errTranscriptionUnavailable, errImageEditUnavailable) || errors.Is(errImageEditUnavailable, errTranscriptionUnavailable) {
		t.Fatal("the transcription and image-edit unavailable sentinels must be distinct values")
	}
	if errors.Is(errTranscriptionTooLarge, errImageEditTooLarge) || errors.Is(errImageEditTooLarge, errTranscriptionTooLarge) {
		t.Fatal("the transcription and image-edit too-large sentinels must be distinct values")
	}
	if got := completionErrorResponse(errImageEditUnavailable).Error.Message; strings.Contains(got, "transcription") {
		t.Fatalf("the image edit's unavailable message = %q reads as the transcription's", got)
	}
}
