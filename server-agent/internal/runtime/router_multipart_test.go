// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The tests below cover the router's multipart branch: POST /v1/images/edits
// reaches the agent as a multipart/form-data body, and the router must find the
// model to start in the form instead of in a JSON document.
//
// The model it finds MUST be the one the gateway's own parser finds
// (gateway/backend/internal/gateway/images_edit_handler.go, parseImageEditForm):
// mime/multipart.NewReader over the buffered body and a part whose FormName() is
// "model". The gateway decides routing and the operator's allowlist on its
// answer, and the agent starts a process on its own; two answers to "which
// model" would be an allowlist bypass. So the cases below are mostly
// differential-parse cases -- bodies on which a hand-rolled scanner and
// mime/multipart disagree.

// editChild is a stand-in for an image-edit child: it records what the router
// forwarded to it and answers a b64_json document.
type editChild struct {
	srv         *httptest.Server
	mu          sync.Mutex
	hits        int
	path        string
	contentType string
	body        []byte
}

func newEditChild(t *testing.T) *editChild {
	t.Helper()
	return newRecordingChild(t, `{"data":[{"b64_json":"AAAA"}]}`)
}

// newRecordingChild is newEditChild with the child's reply a parameter: the
// transcription tests answer a transcript document instead of an image one. It
// records the request the same way (path, Content-Type, body), so the one
// recorder pins the forwarded request for every multipart flavor.
func newRecordingChild(t *testing.T, reply string) *editChild {
	t.Helper()
	c := &editChild{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.hits++
		c.path = r.URL.Path
		c.contentType = r.Header.Get("Content-Type")
		c.body = b
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *editChild) snapshot() (hits int, path, contentType string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.path, c.contentType, append([]byte(nil), c.body...)
}

// recordingManager is a managerPort fake that records every model
// EnsureRunning was asked for and answers success with one fixed endpoint.
type recordingManager struct {
	endpoint string
	mu       sync.Mutex
	models   []string
}

func (m *recordingManager) EnsureRunning(_ context.Context, model string) (string, func(), error) {
	m.mu.Lock()
	m.models = append(m.models, model)
	m.mu.Unlock()
	return m.endpoint, func() {}, nil
}
func (m *recordingManager) LoadedModels() []string { return nil }
func (m *recordingManager) Status() []Status       { return nil }

func (m *recordingManager) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.models...)
}

// editBody builds a well-formed multipart image-edit body with mime/multipart's
// own writer, so the happy path is exercised against bytes the standard library
// itself produced: a model field, a prompt, a stream field and one image part.
func editBody(t *testing.T, model, stream string) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", model); err != nil {
		t.Fatalf("write model: %v", err)
	}
	if err := mw.WriteField("prompt", "make it blue"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	if stream != "" {
		if err := mw.WriteField("stream", stream); err != nil {
			t.Fatalf("write stream: %v", err)
		}
	}
	fw, err := mw.CreateFormFile("image", "in.png")
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := fw.Write([]byte("\x89PNG\r\n\x1a\n-not-really-a-png-")); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// crlf turns a readable multi-line literal into a multipart body (CRLF line
// ends), so the crafted differential bodies below can be written as the text
// they are.
func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

// postMultipart sends body with contentType to /v1/images/edits through a
// router over mgr, on the test goroutine.
func postMultipart(t *testing.T, mgr managerPort, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return postMultipartTo(t, mgr, "/v1/images/edits", contentType, body)
}

// postMultipartTo is postMultipart for any proxied path: the router's multipart
// branch keys on the Content-Type, never on the path.
func postMultipartTo(t *testing.T, mgr managerPort, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	newRouter(mgr).ServeHTTP(rec, req)
	return rec
}

// assertNotManaged fails unless rec is the router's 404
// runtime.model_not_managed -- the SAME answer, status and code, a JSON body
// that names no managed model gets.
func assertNotManaged(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorCode(t, rec); got != "runtime.model_not_managed" {
		t.Fatalf("error code = %q, want runtime.model_not_managed", got)
	}
}

// TestRouterMultipartRoutesByFormModel is the headline: a multipart edit is
// routed on the model in its form, the child is started for exactly that model,
// and the child is sent the client's bytes and Content-Type untouched (its
// boundary is in that header, so a rewritten one would break the body).
func TestRouterMultipartRoutesByFormModel(t *testing.T) {
	child := newEditChild(t)
	mgr := &recordingManager{endpoint: child.srv.URL}
	body, contentType := editBody(t, "sd-edit", "")

	rec := postMultipart(t, mgr, contentType, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := mgr.asked(); len(got) != 1 || got[0] != "sd-edit" {
		t.Fatalf("EnsureRunning was asked for %q, want exactly [sd-edit]", got)
	}
	hits, path, gotType, gotBody := child.snapshot()
	if hits != 1 {
		t.Fatalf("child hit %d times, want 1", hits)
	}
	if path != "/v1/images/edits" {
		t.Errorf("child path = %q, want /v1/images/edits", path)
	}
	if gotType != contentType {
		t.Errorf("child Content-Type = %q, want the client's %q", gotType, contentType)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("child body differs from the client's (%d bytes vs %d); the relay must be byte-faithful", len(gotBody), len(body))
	}
	if want := `{"data":[{"b64_json":"AAAA"}]}`; rec.Body.String() != want {
		t.Errorf("response body = %q, want the child's %q", rec.Body.String(), want)
	}
}

// TestRouterMultipartAlwaysTakesThePlainProxy: multipart has no stream notion,
// so a form field NAMED stream with the value true must not send the request
// down the SSE path. The discriminator is the response: serveStreamProxy
// overwrites Content-Type with text/event-stream, the plain path relays the
// child's own header.
func TestRouterMultipartAlwaysTakesThePlainProxy(t *testing.T) {
	child := newEditChild(t)
	mgr := &recordingManager{endpoint: child.srv.URL}
	body, contentType := editBody(t, "sd-edit", "true")

	rec := postMultipart(t, mgr, contentType, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("response Content-Type = %q, want the child's application/json -- text/event-stream means the stream proxy took a multipart request", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("response Cache-Control = %q, want none -- the stream proxy sets no-cache", got)
	}
}

// TestRouterMultipartModelIsNotTrimmedOrRewritten: the router keys the child on
// the model exactly as the form carries it, like the JSON path does. (The
// gateway rewrites the form to the child's own name when they differ; the
// router never normalizes on top.)
func TestRouterMultipartModelIsNotTrimmedOrRewritten(t *testing.T) {
	child := newEditChild(t)
	mgr := &recordingManager{endpoint: child.srv.URL}
	body, contentType := editBody(t, " sd-edit ", "")

	rec := postMultipart(t, mgr, contentType, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := mgr.asked(); len(got) != 1 || got[0] != " sd-edit " {
		t.Fatalf("EnsureRunning was asked for %q, want exactly [\" sd-edit \"]", got)
	}
}

// TestRouterMultipartRefusals is the 404 table: every body the gateway's parser
// would not take a model from is, to the router, a body that names no managed
// model -- the same 404 runtime.model_not_managed as for JSON, and EnsureRunning
// is never called, so no child is started.
//
// The crafted bodies are the ones a hand-rolled scanner would read a model out
// of and mime/multipart (so the gateway) does not. Each has the text
// `name="model"` and a value; none has a part whose FormName() is "model".
func TestRouterMultipartRefusals(t *testing.T) {
	const b = "BOUNDARY"
	const ct = "multipart/form-data; boundary=" + b

	// A well-formed edit used as the tail of the crafted bodies, so the
	// only thing wrong with each is the model.
	const rest = "--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"prompt\"\n\nmake it blue\n" +
		"--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\n" +
		"Content-Type: image/png\n\nPNGDATA\n"

	cases := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{
			name:        "no model part",
			contentType: ct,
			body:        crlf(rest + "--BOUNDARY--\n"),
		},
		{
			name:        "empty model value",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\n\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			// FormName() is "" unless the disposition is form-data, so
			// this part is not the model to mime/multipart. A scanner
			// that keys on name="model" starts "ghost-model".
			name:        "model part with an attachment disposition",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: attachment; name=\"model\"\n\nghost-model\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			// The preamble -- everything before the first boundary
			// line -- is ignored by mime/multipart.
			name:        "model-shaped text in the preamble",
			contentType: ct,
			body: crlf("--ignored\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
				"\n" + rest + "--BOUNDARY--\n"),
		},
		{
			// The epilogue -- everything after the closing boundary --
			// is ignored too.
			name:        "model-shaped text in the epilogue",
			contentType: ct,
			body: crlf(rest + "--BOUNDARY--\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
				"--BOUNDARY--\n"),
		},
		{
			// The text sits inside another part's body, not at a part
			// boundary of its own.
			name:        "model-shaped text inside another part's body",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"prompt\"\n\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\n\nPNGDATA\n" +
				"--BOUNDARY--\n"),
		},
		{
			// The model is named twice: the gateway refuses that, and
			// the router must not pick one of the two answers.
			name:        "model named twice",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nother-model\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			// The model part runs into the end of the body with no
			// boundary after it: reading it fails (the gateway reads the
			// same part with the same error and answers malformed
			// multipart body).
			name:        "model part never terminated",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit"),
		},
		{
			name:        "body ends inside the model part's headers",
			contentType: ct,
			body:        crlf("--BOUNDARY\nContent-Disposition: form-data; name=\"mod"),
		},
		{
			// NextPart refuses a part whose header block is not
			// parseable, wherever it sits.
			name:        "unparseable part headers after the model",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				"--BOUNDARY\n" +
				"this is not a header line\n\nx\n" +
				"--BOUNDARY--\n"),
		},
		{
			name:        "boundary parameter does not match the body",
			contentType: "multipart/form-data; boundary=SOMETHINGELSE",
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			name:        "no boundary parameter",
			contentType: "multipart/form-data",
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			name:        "malformed content type parameter",
			contentType: "multipart/form-data; boundary",
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			// A multipart Content-Type commits the request to the
			// multipart branch: the JSON reading is not a fallback.
			name:        "JSON body under a multipart content type",
			contentType: ct,
			body:        []byte(`{"model":"sd-edit"}`),
		},
		{
			name:        "empty body",
			contentType: ct,
			body:        nil,
		},
		{
			name:        "model field over the size bound",
			contentType: ct,
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\n" +
				strings.Repeat("m", maxMultipartModelBytes+1) + "\n" +
				rest + "--BOUNDARY--\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newEditChild(t)
			mgr := &recordingManager{endpoint: child.srv.URL}

			rec := postMultipart(t, mgr, tc.contentType, tc.body)

			assertNotManaged(t, rec)
			if got := mgr.asked(); len(got) != 0 {
				t.Errorf("EnsureRunning was called for %q; a refused body must start no child", got)
			}
			if hits, _, _, _ := child.snapshot(); hits != 0 {
				t.Errorf("child hit %d times, want 0", hits)
			}
		})
	}
}

// TestRouterMultipartPeeksTheSameModelAsMimeMultipart is the positive half of
// the differential cases: where a model-shaped lookalike sits next to a real
// form-data model part, the router must start the real one -- the one
// mime/multipart (so the gateway) reads -- and never the lookalike.
func TestRouterMultipartPeeksTheSameModelAsMimeMultipart(t *testing.T) {
	const ct = "multipart/form-data; boundary=BOUNDARY"
	const tail = "--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"prompt\"\n\nmake it blue\n" +
		"--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\n\nPNGDATA\n"

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "attachment-named model before the real one",
			body: "--BOUNDARY\n" +
				"Content-Disposition: attachment; name=\"model\"\n\nghost-model\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				tail + "--BOUNDARY--\n",
			want: "sd-edit",
		},
		{
			name: "attachment-named model after the real one",
			body: "--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: attachment; name=\"model\"\n\nghost-model\n" +
				tail + "--BOUNDARY--\n",
			want: "sd-edit",
		},
		{
			name: "fake model in the preamble",
			body: "preamble text\n" +
				"--ignored\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				tail + "--BOUNDARY--\n",
			want: "sd-edit",
		},
		{
			name: "fake model in the epilogue",
			body: "--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				tail + "--BOUNDARY--\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
				"--BOUNDARY--\n",
			want: "sd-edit",
		},
		{
			name: "model text inside the prompt part",
			body: "--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"prompt\"\n\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nghost-model\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\n\nPNGDATA\n" +
				"--BOUNDARY--\n",
			want: "sd-edit",
		},
		{
			name: "model after the image part",
			body: "--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\n\nPNGDATA\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nsd-edit\n" +
				"--BOUNDARY--\n",
			want: "sd-edit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newEditChild(t)
			mgr := &recordingManager{endpoint: child.srv.URL}

			rec := postMultipart(t, mgr, ct, crlf(tc.body))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if got := mgr.asked(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("EnsureRunning was asked for %q, want exactly [%s]", got, tc.want)
			}
		})
	}
}

// TestRouterJSONUnchangedByMultipartBranch pins that the JSON default is intact
// next to the new branch: a JSON body is still read for model and stream, and a
// JSON body whose Content-Type is some other type or none at all still routes
// (the router has never looked at the header for JSON).
func TestRouterJSONUnchangedByMultipartBranch(t *testing.T) {
	for _, contentType := range []string{"", "application/json", "text/plain", "application/x-www-form-urlencoded"} {
		t.Run("content-type "+contentType, func(t *testing.T) {
			child := newEditChild(t)
			mgr := &recordingManager{endpoint: child.srv.URL}

			rec := postMultipart(t, mgr, contentType, []byte(`{"model":"sd-edit","prompt":"x"}`))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if got := mgr.asked(); len(got) != 1 || got[0] != "sd-edit" {
				t.Fatalf("EnsureRunning was asked for %q, want exactly [sd-edit]", got)
			}
		})
	}

	t.Run("a JSON body still streams on stream:true", func(t *testing.T) {
		child := newEditChild(t)
		mgr := &recordingManager{endpoint: child.srv.URL}

		rec := postMultipart(t, mgr, "application/json", []byte(`{"model":"sd-edit","stream":true}`))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
			t.Errorf("response Content-Type = %q, want text/event-stream -- the JSON stream path must stay the stream path", got)
		}
	})

	t.Run("a non-JSON body under another content type is still 404", func(t *testing.T) {
		child := newEditChild(t)
		mgr := &recordingManager{endpoint: child.srv.URL}

		rec := postMultipart(t, mgr, "text/plain", []byte("model=sd-edit"))

		assertNotManaged(t, rec)
		if got := mgr.asked(); len(got) != 0 {
			t.Errorf("EnsureRunning was called for %q, want no call", got)
		}
	})
}

// The tests below pin that the same multipart branch serves POST
// /v1/audio/transcriptions. No router code is specific to it -- the branch keys
// on the Content-Type and reads the form's model part, whatever the path and
// whatever the other parts are -- so these tests exist to keep it that way: a
// change that made the branch path- or flavor-aware would break the
// transcription relay without touching a single image-edit test.

const (
	transcriptionPath  = "/v1/audio/transcriptions"
	transcriptionReply = `{"text":"hello world","timing":{"audio_duration_ms":1500}}`
)

// audioPayload is a deterministic, binary-looking stand-in for an audio file,
// large enough (a real recording is far larger than an image's prompt part) that
// the Reader has to skip a part of real size to reach a model that follows it.
func audioPayload() []byte {
	pattern := make([]byte, 256)
	for i := range pattern {
		pattern[i] = byte(i)
	}
	return append([]byte("RIFF....WAVEfmt "), bytes.Repeat(pattern, 1024)...)
}

// transcriptionBody builds a well-formed multipart transcription body with
// mime/multipart's own writer: a model field, the transcription's own scalar
// fields and one audio file part, the file before the model when fileFirst
// (curl -F lists whatever the user wrote first, and the gateway's form does not
// fix an order either).
func transcriptionBody(t *testing.T, model string, fileFirst bool) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	writeFile := func() {
		fw, err := mw.CreateFormFile("file", "speech.wav")
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := fw.Write(audioPayload()); err != nil {
			t.Fatalf("write audio: %v", err)
		}
	}
	writeField := func(name, value string) {
		if err := mw.WriteField(name, value); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if fileFirst {
		writeFile()
	}
	writeField("model", model)
	writeField("language", "en")
	writeField("response_format", "json")
	if !fileFirst {
		writeFile()
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// TestRouterMultipartRoutesATranscriptionByFormModel is the headline for the
// audio path: a multipart transcription is routed on the model in its form, the
// child is started for exactly that model, and the child sees the request the
// client sent -- the same path, the same Content-Type with its boundary, the
// same bytes -- wherever in the form the audio part sits.
func TestRouterMultipartRoutesATranscriptionByFormModel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fileFirst bool
	}{
		{name: "model before the audio part", fileFirst: false},
		{name: "model after the audio part", fileFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := newRecordingChild(t, transcriptionReply)
			mgr := &recordingManager{endpoint: child.srv.URL}
			body, contentType := transcriptionBody(t, "whisper-large", tc.fileFirst)

			rec := postMultipartTo(t, mgr, transcriptionPath, contentType, body)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if got := mgr.asked(); len(got) != 1 || got[0] != "whisper-large" {
				t.Fatalf("EnsureRunning was asked for %q, want exactly [whisper-large]", got)
			}
			hits, path, gotType, gotBody := child.snapshot()
			if hits != 1 {
				t.Fatalf("child hit %d times, want 1", hits)
			}
			if path != transcriptionPath {
				t.Errorf("child path = %q, want %s", path, transcriptionPath)
			}
			if gotType != contentType {
				t.Errorf("child Content-Type = %q, want the client's %q", gotType, contentType)
			}
			if !bytes.Equal(gotBody, body) {
				t.Errorf("child body differs from the client's (%d bytes vs %d); the relay must be byte-faithful", len(gotBody), len(body))
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("response Content-Type = %q, want the child's application/json", got)
			}
			if rec.Body.String() != transcriptionReply {
				t.Errorf("response body = %q, want the child's %q", rec.Body.String(), transcriptionReply)
			}
		})
	}
}

// TestRouterMultipartTranscriptionRefusals mirrors TestRouterMultipartRefusals
// for a transcription body: one with no model, an empty one or two of them is,
// to the router, a body that names no managed model -- the same 404
// runtime.model_not_managed, EnsureRunning never called, no child started and
// nothing forwarded.
func TestRouterMultipartTranscriptionRefusals(t *testing.T) {
	const ct = "multipart/form-data; boundary=BOUNDARY"

	// A well-formed transcription used as the tail of the bodies below, so the
	// only thing wrong with each is the model.
	const rest = "--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"language\"\n\nen\n" +
		"--BOUNDARY\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"speech.wav\"\n" +
		"Content-Type: audio/wav\n\nWAVDATA\n"

	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "no model part",
			body: crlf(rest + "--BOUNDARY--\n"),
		},
		{
			name: "empty model value",
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\n\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			// The gateway refuses a repeated model and the router must not
			// pick one of the two answers.
			name: "model named twice",
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nwhisper-large\n" +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nother-model\n" +
				rest + "--BOUNDARY--\n"),
		},
		{
			// The same repeat with the audio part between the two: the Reader
			// has to walk past the file to see the second one.
			name: "model named twice around the audio part",
			body: crlf("--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nwhisper-large\n" +
				rest +
				"--BOUNDARY\n" +
				"Content-Disposition: form-data; name=\"model\"\n\nother-model\n" +
				"--BOUNDARY--\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := newRecordingChild(t, transcriptionReply)
			mgr := &recordingManager{endpoint: child.srv.URL}

			rec := postMultipartTo(t, mgr, transcriptionPath, ct, tc.body)

			assertNotManaged(t, rec)
			if got := mgr.asked(); len(got) != 0 {
				t.Errorf("EnsureRunning was called for %q; a refused body must start no child", got)
			}
			if hits, _, _, _ := child.snapshot(); hits != 0 {
				t.Errorf("child hit %d times, want 0", hits)
			}
		})
	}
}
