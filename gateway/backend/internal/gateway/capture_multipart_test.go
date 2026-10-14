// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// capturedPart mirrors one entry of the summary a multipart capture stores.
type capturedPart struct {
	Name        string  `json:"name"`
	Filename    string  `json:"filename"`
	ContentType string  `json:"content_type"`
	Size        int64   `json:"size"`
	Text        *string `json:"text"`
	Redacted    bool    `json:"redacted"`
	Truncated   bool    `json:"truncated"`
}

type capturedMultipart struct {
	Multipart string         `json:"multipart"`
	Parts     []capturedPart `json:"parts"`
	Error     string         `json:"error"`
}

func multipartCaptureInput(t *testing.T, parts []editPart) (*captureInput, []byte) {
	t.Helper()
	body, ct := buildEditMultipart(t, parts)
	return &captureInput{
		ReqHeaders: http.Header{"Content-Type": {ct}, "Authorization": {"Bearer secret"}},
		RawReq:     body,
		RespBody:   []byte(editsOKBody),
	}, body
}

func decodeCapturedMultipart(t *testing.T, reqBody string) capturedMultipart {
	t.Helper()
	var doc capturedMultipart
	if err := json.Unmarshal([]byte(reqBody), &doc); err != nil {
		t.Fatalf("the captured multipart body is not the JSON summary (%v): %q", err, reqBody)
	}
	return doc
}

// A multipart body is binary, and the envelope stores a body as a STRING that is
// then JSON-marshalled: raw image bytes would be corrupted to U+FFFD, clipped
// mid-file, and bloat every capture. For multipart/form-data the stored body is
// instead a summary: the text fields' values, and for every file part only its
// metadata (name, file name, type, size) -- the image and mask bytes are
// redacted, never stored.
func TestBuildCaptureEnvelopeRedactsMultipartBinary(t *testing.T) {
	big := bytes.Repeat([]byte{0xFF, 0xFE, 0x00, 0x80}, 50_000) // 200 KB, nothing like UTF-8
	in, _ := multipartCaptureInput(t, []editPart{
		{name: "model", body: []byte("sd-turbo")},
		{name: "prompt", body: []byte("a cat in a hat <sd_cpp_extra_args>{\"strength\":0.9}</sd_cpp_extra_args>")},
		{name: "image", filename: "cat.png", contentType: "image/png", body: editTestPNG},
		{name: "image[]", filename: "dog.jpg", contentType: "image/jpeg", body: big},
		{name: "mask", filename: "mask.png", contentType: "image/png", body: []byte("\x89PNG\r\n\x1a\nmask bytes \x00\xff")},
	})

	// A cap far below the body's size: the summary must not be what it clips.
	env := buildCaptureEnvelope(in, 4096)

	if !utf8.ValidString(env.ReqBody) || strings.ContainsRune(env.ReqBody, utf8.RuneError) {
		t.Fatalf("the captured request body carries corrupted bytes (U+FFFD) or is not UTF-8: %q", env.ReqBody)
	}
	for _, leaked := range []string{"not-a-boundary", "mask bytes", "\x89", "\xff\xfe"} {
		if strings.Contains(env.ReqBody, leaked) {
			t.Fatalf("binary content %q leaked into the captured body: %q", leaked, env.ReqBody)
		}
	}
	if env.Truncated {
		t.Fatal("Truncated = true: the summary is small and complete, the redacted bytes are not 'cut off'")
	}
	doc := decodeCapturedMultipart(t, env.ReqBody)
	if doc.Multipart != "form-data" || doc.Error != "" {
		t.Fatalf("summary = %+v, want a clean form-data summary", doc)
	}
	if len(doc.Parts) != 5 {
		t.Fatalf("summary has %d parts, want 5: %q", len(doc.Parts), env.ReqBody)
	}
	// Text fields: the value is kept.
	if p := doc.Parts[0]; p.Name != "model" || p.Text == nil || *p.Text != "sd-turbo" || p.Redacted {
		t.Fatalf("model part = %+v", p)
	}
	if p := doc.Parts[1]; p.Name != "prompt" || p.Text == nil || !strings.Contains(*p.Text, "a cat in a hat") || !strings.Contains(*p.Text, `"strength":0.9`) {
		t.Fatalf("prompt part = %+v, want the full text (the extra-args block included)", p)
	}
	// File parts: metadata only, in wire order, with the real size.
	wantFiles := []struct {
		name, filename, ctype string
		size                  int64
	}{
		{"image", "cat.png", "image/png", int64(len(editTestPNG))},
		{"image[]", "dog.jpg", "image/jpeg", int64(len(big))},
		{"mask", "mask.png", "image/png", int64(len("\x89PNG\r\n\x1a\nmask bytes \x00\xff"))},
	}
	for i, w := range wantFiles {
		p := doc.Parts[2+i]
		if p.Name != w.name || p.Filename != w.filename || p.ContentType != w.ctype || p.Size != w.size || !p.Redacted || p.Text != nil {
			t.Fatalf("file part %d = %+v, want redacted metadata %+v", i, p, w)
		}
	}
	// Headers are redacted exactly as for any capture.
	if v := env.ReqHeaders["Authorization"]; len(v) != 1 || v[0] != redactedMarker {
		t.Fatalf("Authorization = %v, want the redaction marker", v)
	}
}

// What makes a part a file is what it says about itself (a file name, or a
// non-text type) -- and, whatever it says, content that is not valid text is not
// stored as a field either: a part with no file name that is nonetheless binary
// is redacted.
func TestBuildCaptureEnvelopeRedactsABinaryPartThatClaimsToBeAField(t *testing.T) {
	in, _ := multipartCaptureInput(t, []editPart{
		{name: "prompt", body: []byte("a cat")},
		{name: "image", body: editTestPNG}, // no filename, no type, binary
		{name: "note", contentType: "application/octet-stream", body: []byte("plain ascii but declared binary")},
	})

	env := buildCaptureEnvelope(in, 4096)

	doc := decodeCapturedMultipart(t, env.ReqBody)
	if strings.Contains(env.ReqBody, "not-a-boundary") || strings.Contains(env.ReqBody, "plain ascii") {
		t.Fatalf("binary content leaked: %q", env.ReqBody)
	}
	if !doc.Parts[1].Redacted || doc.Parts[1].Text != nil || !doc.Parts[2].Redacted || doc.Parts[2].Text != nil {
		t.Fatalf("parts = %+v, want the binary-looking parts redacted", doc.Parts)
	}
	if doc.Parts[0].Text == nil || *doc.Parts[0].Text != "a cat" {
		t.Fatalf("the prompt part = %+v, want its text kept", doc.Parts[0])
	}
}

// Text values are clipped to the same cap as any captured body, and that -- only
// that -- makes the envelope Truncated.
func TestBuildCaptureEnvelopeClipsMultipartText(t *testing.T) {
	in, _ := multipartCaptureInput(t, []editPart{
		{name: "prompt", body: bytes.Repeat([]byte("é"), 400)}, // 800 bytes, two per rune
		{name: "model", body: []byte("sd-turbo")},
	})

	env := buildCaptureEnvelope(in, 101) // odd: the clip lands inside a rune

	doc := decodeCapturedMultipart(t, env.ReqBody)
	if !env.Truncated {
		t.Fatal("Truncated = false, want true: the prompt was clipped")
	}
	if p := doc.Parts[0]; p.Text == nil || len(*p.Text) > 101 || !p.Truncated || !utf8.ValidString(*p.Text) {
		t.Fatalf("prompt part = %+v, want clipped valid UTF-8 within the cap, flagged truncated", p)
	}
	if strings.ContainsRune(env.ReqBody, utf8.RuneError) {
		t.Fatalf("the clip split a rune: %q", env.ReqBody)
	}
	// Once the budget is spent the later field is still listed, with its size.
	if p := doc.Parts[1]; p.Name != "model" || p.Size != int64(len("sd-turbo")) {
		t.Fatalf("model part = %+v, want it listed with its size even past the budget", p)
	}
}

// A multipart body that cannot be walked is never stored raw: the summary says so
// and keeps what could be read before the break.
func TestBuildCaptureEnvelopeNeverStoresAnUnparseableMultipartBodyRaw(t *testing.T) {
	in, body := multipartCaptureInput(t, []editPart{
		{name: "model", body: []byte("sd-turbo")},
		{name: "image", filename: "cat.png", contentType: "image/png", body: editTestPNG},
	})
	t.Run("cut off inside a part", func(t *testing.T) {
		in := *in
		in.RawReq = body[:len(body)-30]

		env := buildCaptureEnvelope(&in, 4096)

		doc := decodeCapturedMultipart(t, env.ReqBody)
		if doc.Error == "" || len(doc.Parts) == 0 || doc.Parts[0].Name != "model" {
			t.Fatalf("summary = %+v, want the parts read so far and an error", doc)
		}
		if strings.Contains(env.ReqBody, "not-a-boundary") {
			t.Fatalf("binary leaked from a cut-off body: %q", env.ReqBody)
		}
	})
	t.Run("no usable boundary", func(t *testing.T) {
		in := *in
		in.ReqHeaders = http.Header{"Content-Type": {"multipart/form-data"}}

		env := buildCaptureEnvelope(&in, 4096)

		doc := decodeCapturedMultipart(t, env.ReqBody)
		if doc.Error == "" || len(doc.Parts) != 0 {
			t.Fatalf("summary = %+v, want an error and no parts", doc)
		}
		if strings.Contains(env.ReqBody, "sd-turbo") || strings.Contains(env.ReqBody, "not-a-boundary") {
			t.Fatalf("an unparsed body leaked into the capture: %q", env.ReqBody)
		}
	})
}

// Only multipart is summarised. A JSON body is stored exactly as before, even one
// that merely mentions multipart, and so is a body with no Content-Type.
func TestBuildCaptureEnvelopeLeavesOtherBodiesAlone(t *testing.T) {
	for _, ct := range []string{"application/json", "text/plain", "multipart/mixed; boundary=x", ""} {
		in := &captureInput{ReqHeaders: http.Header{"Content-Type": {ct}}, RawReq: []byte(`{"model":"m","note":"multipart/form-data"}`)}
		if ct == "" {
			in.ReqHeaders = nil
		}

		env := buildCaptureEnvelope(in, 4096)

		if env.ReqBody != string(in.RawReq) {
			t.Fatalf("content type %q: ReqBody = %q, want the body unchanged", ct, env.ReqBody)
		}
	}
}
