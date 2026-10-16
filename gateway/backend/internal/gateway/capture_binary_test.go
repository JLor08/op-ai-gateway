// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/capture"
	"strings"
	"testing"
	"unicode/utf8"
)

// capturedBinaryBody mirrors the document a skipped binary response body is
// stored as.
type capturedBinaryBody struct {
	ContentType     string              `json:"content_type"`
	Size            int64               `json:"size"`
	Redacted        bool                `json:"redacted"`
	MeteringHeaders map[string][]string `json:"metering_headers"`
}

func decodeCapturedBinaryBody(t *testing.T, respBody string) capturedBinaryBody {
	t.Helper()
	var doc capturedBinaryBody
	if err := json.Unmarshal([]byte(respBody), &doc); err != nil {
		t.Fatalf("the captured response body is not the JSON summary (%v): %q", err, respBody)
	}
	return doc
}

func audioCaptureInput(contentType string, body []byte) *captureInput {
	return &captureInput{
		ReqHeaders:  http.Header{"Content-Type": {"application/json"}},
		RawReq:      []byte(`{"model":"tts-voice","input":"Hello there."}`),
		RespHeaders: http.Header{"Content-Type": {contentType}},
		RespBody:    body,
	}
}

// A binary audio RESPONSE body is stored as a string that json.Marshal then
// rewrites to valid UTF-8 (every invalid byte becomes U+FFFD) and the cap clips
// mid-file: a corrupted, truncated WAV nobody can play. So the bytes are not
// stored at all -- only what the capture viewer can usefully say about them: the
// content type, the size, and the upstream's metering headers.
func TestBuildCaptureEnvelopeSkipsABinaryAudioResponseBody(t *testing.T) {
	wav := speechTestWAV()
	in := audioCaptureInput("audio/wav", wav)
	in.RespBytes = int64(len(wav))
	in.RespMetering = http.Header{"X-Audiocpp-Audio-Duration-Ms": {"1500.000000"}}

	// A cap below the WAV's size (52 bytes) but above the request's (44): the
	// summary is longer than the cap and must not be what it clips.
	env := buildCaptureEnvelope(in, 48)

	if !utf8.ValidString(env.RespBody) || strings.ContainsRune(env.RespBody, utf8.RuneError) {
		t.Fatalf("the captured response body carries corrupted bytes (U+FFFD) or is not UTF-8: %q", env.RespBody)
	}
	for _, leaked := range []string{string(wav), "RIFF", "WAVE", "\x00", "\xff\xfe"} {
		if strings.Contains(env.RespBody, leaked) {
			t.Fatalf("WAV content %q leaked into the captured response body: %q", leaked, env.RespBody)
		}
	}
	doc := decodeCapturedBinaryBody(t, env.RespBody)
	if doc.ContentType != "audio/wav" || doc.Size != int64(len(wav)) || !doc.Redacted {
		t.Fatalf("summary = %+v, want content type audio/wav, size %d, redacted", doc, len(wav))
	}
	if got := doc.MeteringHeaders["X-Audiocpp-Audio-Duration-Ms"]; len(got) != 1 || got[0] != "1500.000000" {
		t.Fatalf("metering headers = %v, want the duration header kept", doc.MeteringHeaders)
	}
	if env.Truncated {
		t.Fatal("Truncated = true: the summary is complete, a redacted body is not 'cut off'")
	}
	if got := env.RespHeaders["Content-Type"]; len(got) != 1 || got[0] != "audio/wav" {
		t.Fatalf("response headers = %v, want the Content-Type kept", env.RespHeaders)
	}
	// The REQUEST side is untouched: the text to synthesize is exactly what the
	// opt-in payload capture is for.
	if env.ReqBody != string(in.RawReq) {
		t.Fatalf("ReqBody = %q, want the request JSON unchanged", env.ReqBody)
	}
}

// The recorded size is the number of bytes the relay sent, not the length of the
// bounded capture buffer: a clip bigger than the cap would otherwise be recorded
// as the cap, a size that was never the clip's.
func TestBuildCaptureEnvelopeBinaryBodySizeIsTheRelayedSizeNotTheBuffers(t *testing.T) {
	in := audioCaptureInput("audio/wav", []byte("RIFF....")) // the clipped tee
	in.RespBytes = 5_000_000

	doc := decodeCapturedBinaryBody(t, buildCaptureEnvelope(in, 8).RespBody)

	if doc.Size != 5_000_000 {
		t.Fatalf("size = %d, want the relayed 5000000", doc.Size)
	}
}

// With no relayed size recorded, the body in hand is what is sized.
func TestBuildCaptureEnvelopeBinaryBodySizeFallsBackToTheBody(t *testing.T) {
	wav := speechTestWAV()

	doc := decodeCapturedBinaryBody(t, buildCaptureEnvelope(audioCaptureInput("audio/wav", wav), 4096).RespBody)

	if doc.Size != int64(len(wav)) {
		t.Fatalf("size = %d, want %d", doc.Size, len(wav))
	}
}

// No metering headers is not an empty object.
func TestBuildCaptureEnvelopeBinaryBodyOmitsAbsentMeteringHeaders(t *testing.T) {
	env := buildCaptureEnvelope(audioCaptureInput("audio/wav", speechTestWAV()), 4096)

	if strings.Contains(env.RespBody, "metering_headers") {
		t.Fatalf("response body = %q, want no metering_headers key when there are none", env.RespBody)
	}
}

// The skip is keyed on the response's own media type: any audio/* is binary,
// whatever the case or the parameters.
func TestBuildCaptureEnvelopeSkipsEveryAudioMediaType(t *testing.T) {
	for _, ct := range []string{"audio/wav", "audio/x-wav", "Audio/WAV", "audio/wav; codecs=1", " audio/mpeg ", "audio/L16;rate=24000"} {
		in := audioCaptureInput(ct, speechTestWAV())

		env := buildCaptureEnvelope(in, 4096)

		if !decodeCapturedBinaryBodyIsSummary(env.RespBody) {
			t.Errorf("content type %q: response body = %q, want the binary summary", ct, env.RespBody)
		}
	}
}

func decodeCapturedBinaryBodyIsSummary(respBody string) bool {
	var doc capturedBinaryBody
	return json.Unmarshal([]byte(respBody), &doc) == nil && doc.Redacted
}

// Only an audio response is skipped. An error response (application/json) -- the
// shape every failed speech request has -- and any other text body is stored
// exactly as before, byte for byte, as is a response that names no content type.
func TestBuildCaptureEnvelopeStoresEveryNonAudioResponseBody(t *testing.T) {
	const body = `{"error":{"message":"unknown model id: x","type":"server_error"}}`
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "text/plain", "text/event-stream", "text/html", ""} {
		in := audioCaptureInput(ct, []byte(body))
		in.RespBytes = int64(len(body))
		if ct == "" {
			in.RespHeaders = nil
		}

		env := buildCaptureEnvelope(in, 4096)

		if env.RespBody != body {
			t.Errorf("content type %q: RespBody = %q, want the body unchanged", ct, env.RespBody)
		}
	}
}

// The response's own media type is the primary gate, not the request's flavor: a
// speech request whose upstream answered an error (application/json, non-2xx) is
// stored, and an audio response is skipped whatever flavor asked for it.
func TestBuildCaptureEnvelopeGateIsTheResponseContentTypeNotTheFlavor(t *testing.T) {
	const errBody = `{"error":{"message":"boom","type":"server_error"}}`
	speechErr := audioCaptureInput("application/json", []byte(errBody))
	speechErr.APIFlavor = apiFlavorSpeech
	speechErr.HTTPStatus = http.StatusBadGateway
	if env := buildCaptureEnvelope(speechErr, 4096); env.RespBody != errBody {
		t.Errorf("speech flavor with a JSON error response: RespBody = %q, want the error body stored", env.RespBody)
	}

	otherAudio := audioCaptureInput("audio/wav", speechTestWAV())
	otherAudio.APIFlavor = "openai"
	if env := buildCaptureEnvelope(otherAudio, 4096); !decodeCapturedBinaryBodyIsSummary(env.RespBody) {
		t.Errorf("non-speech flavor with an audio response: RespBody = %q, want the binary summary", env.RespBody)
	}
}

// Defense in depth behind the Content-Type gate: a speech request that was
// answered 2xx got an audio body, whatever its Content-Type says. proxyNative
// defaults a missing Content-Type to application/json, so without this fallback an
// upstream that forgot (or mislabelled) the header would send raw WAV bytes
// through json.Marshal and into the capture, corrupted and clipped mid-file. The
// metadata summary is stored instead.
func TestBuildCaptureEnvelopeSkipsA2xxSpeechBodyWhateverItsContentType(t *testing.T) {
	wav := speechTestWAV()
	for _, tc := range []struct {
		name        string
		contentType string // "" = the response names none
		status      int
	}{
		{"no content type, 200", "", http.StatusOK},
		{"json content type, 200", "application/json", http.StatusOK},
		{"octet-stream, 200", "application/octet-stream", http.StatusOK},
		{"no content type, 206", "", http.StatusPartialContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := audioCaptureInput(tc.contentType, wav)
			if tc.contentType == "" {
				in.RespHeaders = nil
			}
			in.APIFlavor = apiFlavorSpeech
			in.HTTPStatus = tc.status
			in.RespBytes = int64(len(wav))

			env := buildCaptureEnvelope(in, 4096)

			if !utf8.ValidString(env.RespBody) || strings.ContainsRune(env.RespBody, utf8.RuneError) {
				t.Fatalf("the captured response body carries corrupted bytes (U+FFFD) or is not UTF-8: %q", env.RespBody)
			}
			for _, leaked := range []string{string(wav), "RIFF", "WAVE", "\x00"} {
				if strings.Contains(env.RespBody, leaked) {
					t.Fatalf("WAV content %q leaked into the captured response body: %q", leaked, env.RespBody)
				}
			}
			doc := decodeCapturedBinaryBody(t, env.RespBody)
			if doc.ContentType != tc.contentType || doc.Size != int64(len(wav)) || !doc.Redacted {
				t.Fatalf("summary = %+v, want content type %q, size %d, redacted", doc, tc.contentType, len(wav))
			}
			if env.Truncated {
				t.Fatal("Truncated = true: the summary is complete, a redacted body is not 'cut off'")
			}
		})
	}
}

// The fallback is exactly "speech AND 2xx". A speech request answered with an
// error, and every other flavor with a 2xx and no content type, keep storing the
// body, so the new rule widens nothing beyond the one binary response it exists for.
func TestBuildCaptureEnvelopeSpeechFallbackIsNarrow(t *testing.T) {
	const body = `{"error":{"message":"boom","type":"server_error"}}`
	for _, tc := range []struct {
		name   string
		flavor string
		status int
	}{
		{"speech with a 400", apiFlavorSpeech, http.StatusBadRequest},
		{"speech with a 502", apiFlavorSpeech, http.StatusBadGateway},
		{"speech with a 199", apiFlavorSpeech, 199},
		{"speech with a 300", apiFlavorSpeech, http.StatusMultipleChoices},
		{"speech with no status recorded", apiFlavorSpeech, 0},
		{"another flavor with a 200", "openai", http.StatusOK},
		{"no flavor with a 200", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := audioCaptureInput("", []byte(body))
			in.RespHeaders = nil
			in.APIFlavor = tc.flavor
			in.HTTPStatus = tc.status

			if env := buildCaptureEnvelope(in, 4096); env.RespBody != body {
				t.Fatalf("RespBody = %q, want the body stored unchanged", env.RespBody)
			}
		})
	}
}

// An ordinary text body above the cap is still clipped and marked truncated: the
// binary skip does not change how everything else is capped.
func TestBuildCaptureEnvelopeStillClipsALargeTextBody(t *testing.T) {
	in := audioCaptureInput("application/json", []byte(strings.Repeat("a", 100)))

	env := buildCaptureEnvelope(in, 10)

	if env.RespBody != strings.Repeat("a", 10) || !env.Truncated {
		t.Fatalf("RespBody = %q, Truncated = %v, want the body clipped to 10 bytes and truncated", env.RespBody, env.Truncated)
	}
}

// The relayed size comes from the copier, which counts every byte it writes --
// including those past the capture cap that its tee buffer no longer keeps.
func TestNativeCopierCountsEveryByteItRelays(t *testing.T) {
	w := &fakeFlushWriter{}
	c := newNativeCopier(w, 5) // the tee stops growing after the first chunk
	body := &scriptedReader{reads: []scriptedRead{
		{chunk: []byte("hello"), err: nil},
		{chunk: []byte("world!"), err: nil},
		{chunk: []byte("and more"), err: nil},
	}}

	if err := c.run(body); err != nil {
		t.Fatalf("run: %v", err)
	}

	if want := int64(len("helloworld!and more")); c.written != want {
		t.Fatalf("written = %d, want %d (every relayed byte, past the tee's cap too)", c.written, want)
	}
	if c.respBuf.Len() >= int(c.written) {
		t.Fatalf("tee holds %d of %d bytes, want it clipped by the cap so the count is the only full size", c.respBuf.Len(), c.written)
	}
}

// A write that fails part-way is counted only for the bytes that were written.
func TestNativeCopierCountsOnlyTheBytesTheClientTook(t *testing.T) {
	w := &partialWriter{limit: 3}
	c := newNativeCopier(w, 100)
	body := &scriptedReader{reads: []scriptedRead{{chunk: []byte("hello"), err: nil}}}

	if err := c.run(body); err == nil {
		t.Fatal("run: want the short-write error")
	}

	if c.written != 3 {
		t.Fatalf("written = %d, want the 3 bytes the client actually took", c.written)
	}
}

// partialWriter accepts at most limit bytes of one Write and then fails.
type partialWriter struct {
	fakeFlushWriter
	limit int
}

func (p *partialWriter) Write(b []byte) (int, error) {
	if len(b) > p.limit {
		n, _ := p.buf.Write(b[:p.limit])
		return n, http.ErrHandlerTimeout
	}
	return p.buf.Write(b)
}

// newSpeechCaptureServer is the speech test server with payload capture forced
// on, over a fake store, so a relayed exchange is persisted end to end.
func newSpeechCaptureServer(t *testing.T, upstreamURL string) (*Server, *fakeCaptureStore) {
	t.Helper()
	srv, _ := newSpeechCapableTestServer(t, upstreamURL)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	srv.Captures, srv.Cipher = cs, cipher
	srv.CaptureOverride = func() bool { return true }
	return srv, cs
}

// A successful speech relay is captured WITHOUT its WAV: the stored response
// body is the metadata summary (the type, the size the client received, the
// duration header the row is billed from), the request's text is captured as
// usual, and the client still got the audio byte for byte.
func TestSpeechCaptureStoresMetadataNotTheWAV(t *testing.T) {
	wav := speechTestWAV()
	up, _ := newSpeechUpstream(t, wav)
	srv, cs := newSpeechCaptureServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","voice":"alba"}`)

	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("status = %d, body = %x, want 200 and the WAV byte for byte", rec.Code, rec.Body.Bytes())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if saved.APIFlavor != apiFlavorSpeech || saved.HTTPStatus != http.StatusOK {
		t.Fatalf("capture row = %q/%d, want %s/200", saved.APIFlavor, saved.HTTPStatus, apiFlavorSpeech)
	}
	env := decryptCapture(t, saved.Blob)
	if !strings.Contains(env.ReqBody, `"input":"Hello there."`) {
		t.Fatalf("request body = %q, want the text to synthesize captured", env.ReqBody)
	}
	if !utf8.ValidString(env.RespBody) || strings.ContainsRune(env.RespBody, utf8.RuneError) || strings.Contains(env.RespBody, "RIFF") {
		t.Fatalf("the capture holds WAV bytes (or their corruption): %q", env.RespBody)
	}
	doc := decodeCapturedBinaryBody(t, env.RespBody)
	if doc.ContentType != "audio/wav" || doc.Size != int64(len(wav)) || !doc.Redacted {
		t.Fatalf("summary = %+v, want audio/wav, %d bytes, redacted", doc, len(wav))
	}
	if got := doc.MeteringHeaders["X-Audiocpp-Audio-Duration-Ms"]; len(got) != 1 || got[0] != "1500" {
		t.Fatalf("metering headers = %v, want the upstream's duration header (1500)", doc.MeteringHeaders)
	}
	if env.Truncated {
		t.Fatal("Truncated = true, want false: nothing was cut off")
	}
	if got := env.RespHeaders["Content-Type"]; len(got) != 1 || got[0] != "audio/wav" {
		t.Fatalf("response headers = %v, want Content-Type audio/wav", env.RespHeaders)
	}
}

// A clip bigger than the capture cap is recorded at its real size: the copier's
// tee stops at the cap, and a size read off that buffer would be the cap.
func TestSpeechCaptureRecordsTheFullSizeOfAClipPastTheCap(t *testing.T) {
	wav := append(speechTestWAV(), bytes.Repeat([]byte{0xff, 0xfe, 0x80}, 70_000)...)
	up, _ := newSpeechUpstream(t, wav)
	srv, cs := newSpeechCaptureServer(t, up.URL)
	srv.captureMaxBytes = 1024

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("status = %d, want 200 and the whole %d-byte WAV relayed", rec.Code, len(wav))
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	doc := decodeCapturedBinaryBody(t, decryptCapture(t, saved.Blob).RespBody)
	if doc.Size != int64(len(wav)) {
		t.Fatalf("size = %d, want the relayed %d (the cap is 1024)", doc.Size, len(wav))
	}
}

// An upstream error is still captured: its Content-Type is application/json, so
// the body is stored as the client received it -- here audio.cpp's own object.
func TestSpeechCaptureStoresAnUpstreamErrorBody(t *testing.T) {
	const upstreamBody = `{"error":{"message":"unknown model id: x","type":"server_error"}}`
	up := newSpeechErrorUpstream(t, http.StatusInternalServerError, "application/json", upstreamBody)
	srv, cs := newSpeechCaptureServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

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
	if !strings.Contains(env.ReqBody, `"input":"Hello there."`) {
		t.Fatalf("request body = %q, want the text to synthesize captured", env.ReqBody)
	}
}

// A normalised upstream error is captured as normalised: the capture is a record
// of what the client was sent.
func TestSpeechCaptureStoresTheNormalisedError(t *testing.T) {
	up := newSpeechErrorUpstream(t, http.StatusBadRequest, "application/json", `{"error":"voice not found"}`)
	srv, cs := newSpeechCaptureServer(t, up.URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","voice":"nobody"}`)

	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved for the failed relay")
	}
	env := decryptCapture(t, saved.Blob)
	if env.RespBody != rec.Body.String() || !strings.Contains(env.RespBody, speechUpstreamErrorCode) {
		t.Fatalf("response body = %q, want the normalised error the client got (%q)", env.RespBody, rec.Body.String())
	}
}
