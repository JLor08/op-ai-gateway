// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/capture"
	"strings"
	"testing"
	"unicode/utf8"
)

// newTranscriptionCaptureServer is the transcription test server with payload
// capture forced on, over a fake store, so a relayed exchange is persisted end to
// end. It is newSpeechCaptureServer's twin.
func newTranscriptionCaptureServer(t *testing.T, upstreamURL string) (*Server, *fakeCaptureStore) {
	t.Helper()
	srv, _ := newTranscriptionCapableTestServer(t, upstreamURL)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	srv.Captures, srv.Cipher = cs, cipher
	srv.CaptureOverride = func() bool { return true }
	return srv, cs
}

// capturedPartByName returns the first summary part called name, failing when the
// summary has none.
func capturedPartByName(t *testing.T, doc capturedMultipart, name string) capturedPart {
	t.Helper()
	for _, p := range doc.Parts {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("the captured multipart summary has no %q part: %+v", name, doc.Parts)
	return capturedPart{}
}

// A transcription is captured as the pair it is: a multipart REQUEST whose text
// fields are stored and whose audio is redacted (it is binary, and it is the user's
// recording), and a JSON RESPONSE whose body -- the transcript -- is stored as the
// client received it. The response shares the openai_audio flavor with speech's
// binary WAV, so this is what proves the capture does not mistake the transcript for
// audio and replace it with a redacted binary summary.
func TestTranscriptionCaptureStoresTheFieldsAndTheTranscript(t *testing.T) {
	up, _ := newTranscriptionUpstream(t, http.StatusOK, transcriptionOKBody)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, []editPart{
		{name: "model", body: []byte(transcriptionGatewayModel)},
		{name: "language", body: []byte("de")},
		{name: "file", filename: "clip.wav", contentType: "audio/wav", body: transcriptionTestAudio},
	})

	if rec.Code != http.StatusOK || rec.Body.String() != transcriptionOKBody {
		t.Fatalf("status = %d, body = %q, want 200 and the upstream's document", rec.Code, rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if saved.APIFlavor != apiFlavorSpeech || saved.HTTPStatus != http.StatusOK {
		t.Fatalf("capture row = %q/%d, want %s/200 (the audio flavor speech shares)", saved.APIFlavor, saved.HTTPStatus, apiFlavorSpeech)
	}
	env := decryptCapture(t, saved.Blob)

	// The response: the transcript, verbatim -- not the redacted binary summary.
	if env.RespBody != transcriptionOKBody {
		t.Fatalf("response body = %q, want the transcript document the client got (%q), not a binary summary", env.RespBody, transcriptionOKBody)
	}
	if decodeCapturedBinaryBodyIsSummary(env.RespBody) {
		t.Fatalf("response body = %q is the redacted binary summary", env.RespBody)
	}
	if env.Truncated {
		t.Fatal("Truncated = true, want false: the transcript is far below the cap")
	}
	if got := env.RespHeaders["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("response headers = %v, want Content-Type application/json", env.RespHeaders)
	}

	// The request: the text fields' values are kept, the audio part is redacted.
	doc := decodeCapturedMultipart(t, env.ReqBody)
	if doc.Multipart != "form-data" || doc.Error != "" || len(doc.Parts) != 3 {
		t.Fatalf("request summary = %q, want a clean form-data summary of 3 parts", env.ReqBody)
	}
	if p := capturedPartByName(t, doc, "model"); p.Text == nil || *p.Text != transcriptionGatewayModel || p.Redacted {
		t.Fatalf("model part = %+v, want its text kept", p)
	}
	if p := capturedPartByName(t, doc, "language"); p.Text == nil || *p.Text != "de" || p.Redacted {
		t.Fatalf("language part = %+v, want its text kept", p)
	}
	if p := capturedPartByName(t, doc, "file"); p.Filename != "clip.wav" || p.ContentType != "audio/wav" || p.Size != int64(len(transcriptionTestAudio)) || !p.Redacted || p.Text != nil {
		t.Fatalf("file part = %+v, want redacted metadata of the %d-byte upload", p, len(transcriptionTestAudio))
	}
	if !utf8.ValidString(env.ReqBody) || strings.ContainsRune(env.ReqBody, utf8.RuneError) || strings.Contains(env.ReqBody, "not-a-boundary") {
		t.Fatalf("the captured request holds the audio bytes (or their corruption): %q", env.ReqBody)
	}
}

// The capture keeps the transcript whatever Content-Type the upstream answered it
// under: the relay buffers and writes the body verbatim, so what the client got is
// what a capture stores, and no header (or lack of one) turns a JSON document into a
// binary one. The upstream here names no type and then a non-JSON one; neither is
// audio.
func TestTranscriptionCaptureStoresTheTranscriptWhateverItsContentType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{"no content type", ""},
		{"text/plain", "text/plain; charset=utf-8"},
		{"octet-stream", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newSpeechErrorUpstream(t, http.StatusOK, tc.contentType, transcriptionOKBody)
			srv, cs := newTranscriptionCaptureServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

			if rec.Code != http.StatusOK || rec.Body.String() != transcriptionOKBody {
				t.Fatalf("status = %d, body = %q, want 200 and the upstream's document", rec.Code, rec.Body.String())
			}
			saved, ok := cs.last()
			if !ok {
				t.Fatal("no capture saved")
			}
			if env := decryptCapture(t, saved.Blob); env.RespBody != transcriptionOKBody {
				t.Fatalf("response body = %q, want the transcript the client got", env.RespBody)
			}
		})
	}
}

// A transcript past the capture cap is clipped and marked truncated like any other
// body, while the CLIENT still gets the whole of it: the cap bounds what a capture
// keeps, never what the relay delivers (a transcript is never clipped on the wire).
func TestTranscriptionCaptureClipsATranscriptPastTheCap(t *testing.T) {
	doc := transcriptionDocOfSize(t, 4096)
	up, _ := newTranscriptionUpstream(t, http.StatusOK, doc)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)
	srv.captureMaxBytes = 1024

	rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusOK || rec.Body.String() != doc {
		t.Fatalf("status = %d, body is %d bytes, want 200 and the whole %d-byte document", rec.Code, rec.Body.Len(), len(doc))
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	if env.RespBody != doc[:1024] || !env.Truncated {
		t.Fatalf("response body is %d bytes, Truncated = %v, want the first 1024 bytes of the transcript and Truncated", len(env.RespBody), env.Truncated)
	}
}

// An upstream error is captured as the client received it, like speech's: its own
// OpenAI object where it sent one, and the gateway's normalised object where the
// upstream sent a plain string.
func TestTranscriptionCaptureStoresAnUpstreamErrorBody(t *testing.T) {
	const upstreamBody = `{"error":{"message":"no audio file in the request","type":"server_error"}}`
	up := newSpeechErrorUpstream(t, http.StatusInternalServerError, "application/json", upstreamBody)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved for the failed relay")
	}
	if saved.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("capture status = %d, want 500", saved.HTTPStatus)
	}
	env := decryptCapture(t, saved.Blob)
	if env.RespBody != upstreamBody || env.RespBody != rec.Body.String() {
		t.Fatalf("response body = %q, want the error body the client got: %q", env.RespBody, upstreamBody)
	}
	if p := capturedPartByName(t, decodeCapturedMultipart(t, env.ReqBody), "file"); !p.Redacted {
		t.Fatalf("file part = %+v, want the audio redacted on a failed relay too", p)
	}
}

func TestTranscriptionCaptureStoresTheNormalisedError(t *testing.T) {
	up := newSpeechErrorUpstream(t, http.StatusBadRequest, "application/json", `{"error":"language not supported"}`)
	srv, cs := newTranscriptionCaptureServer(t, up.URL)

	rec := postTranscriptionParts(t, srv, validTranscriptionParts(transcriptionGatewayModel))

	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved for the failed relay")
	}
	env := decryptCapture(t, saved.Blob)
	if env.RespBody != rec.Body.String() || !strings.Contains(env.RespBody, transcriptionUpstreamErrorCode) {
		t.Fatalf("response body = %q, want the normalised error the client got (%q)", env.RespBody, rec.Body.String())
	}
}

// The transcription fix must not move speech: a 2xx speech response whose upstream
// mislabelled it as JSON is still one audio file, and still stored as the redacted
// summary through the real relay, not as corrupted WAV bytes.
func TestSpeechCaptureStillRedactsAMislabelledWAV(t *testing.T) {
	wav := speechTestWAV()
	up := newSpeechErrorUpstream(t, http.StatusOK, "application/json", string(wav))
	srv, cs := newSpeechCaptureServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	if !utf8.ValidString(env.RespBody) || strings.Contains(env.RespBody, "RIFF") {
		t.Fatalf("the capture holds WAV bytes (or their corruption): %q", env.RespBody)
	}
	if doc := decodeCapturedBinaryBody(t, env.RespBody); !doc.Redacted || doc.Size != int64(len(wav)) {
		t.Fatalf("summary = %+v, want redacted, %d bytes", doc, len(wav))
	}
}

// The predicate itself, over the cases that tell speech from transcription: both
// carry the openai_audio flavor, so only the endpoint separates the binary WAV from
// the JSON transcript. The test is a carve-out of transcription, like
// needsSpeechErrorRelay's, so a speech capture built without an endpoint (the zero
// value is endpointChat) is still a speech capture.
func TestIsBinarySpeechResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flavor   string
		endpoint sessionEndpoint
		status   int
		want     bool
	}{
		{"speech 200", apiFlavorSpeech, endpointSpeech, http.StatusOK, true},
		{"speech 206", apiFlavorSpeech, endpointSpeech, http.StatusPartialContent, true},
		{"speech built without an endpoint", apiFlavorSpeech, endpointChat, http.StatusOK, true},
		{"speech 400", apiFlavorSpeech, endpointSpeech, http.StatusBadRequest, false},
		{"speech 199", apiFlavorSpeech, endpointSpeech, 199, false},
		{"speech 300", apiFlavorSpeech, endpointSpeech, http.StatusMultipleChoices, false},
		{"transcription 200 is JSON", apiFlavorSpeech, endpointTranscription, http.StatusOK, false},
		{"transcription 206", apiFlavorSpeech, endpointTranscription, http.StatusPartialContent, false},
		{"transcription 502", apiFlavorSpeech, endpointTranscription, http.StatusBadGateway, false},
		{"another flavor with a 200", "openai", endpointChat, http.StatusOK, false},
		{"images with a 200", apiFlavorImages, endpointImages, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &captureInput{APIFlavor: tc.flavor, Endpoint: tc.endpoint, HTTPStatus: tc.status}
			if got := isBinarySpeechResponse(in); got != tc.want {
				t.Fatalf("isBinarySpeechResponse(%q, %v, %d) = %v, want %v", tc.flavor, tc.endpoint, tc.status, got, tc.want)
			}
		})
	}
}

// The capture's text-field cap is shared by every multipart endpoint, so it must
// cover each one's own field cap: a field the transcription handler accepted but the
// capture would redact (a long `prompt`) is a hole in the record.
func TestCaptureTextPartCapCoversEveryMultipartEndpointsFieldCap(t *testing.T) {
	captureCap := captureTextPartMaxBytes
	for name, fieldCap := range map[string]int{
		"image edit":    maxImageEditFieldBytes,
		"transcription": maxTranscriptionFieldBytes,
	} {
		if captureCap < fieldCap {
			t.Errorf("captureTextPartMaxBytes = %d, below the %s field cap %d", captureCap, name, fieldCap)
		}
	}
}
