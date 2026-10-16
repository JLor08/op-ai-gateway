// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"strings"
	"testing"
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
