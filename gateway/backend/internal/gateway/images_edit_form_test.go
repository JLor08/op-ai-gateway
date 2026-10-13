// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

func mustBoundary(t *testing.T, contentType string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		t.Fatalf("content type %q has no boundary: %v", contentType, err)
	}
	return params["boundary"]
}

// The rebuild changes exactly one thing: the value of the model field. Every
// other part keeps its headers (including ones this code knows nothing about),
// its exact bytes -- binary, CRLF and NUL included -- and its position, and the
// new Content-Type carries a boundary of its own that the new body parses with.
func TestRebuildImageEditBodyChangesOnlyTheModelField(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	write := func(h textproto.MIMEHeader, body []byte) {
		t.Helper()
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := pw.Write(body); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="image[]"; filename="a b.png"`}, "Content-Type": {"image/png"}, "X-Custom": {"kept"}}, editTestPNG)
	write(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="model"`}}, []byte("client-name"))
	write(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="prompt"`}}, []byte("line one\r\nline two -- with dashes"))
	write(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="mask"; filename="m.png"`}, "Content-Type": {"image/png"}}, []byte{0, 1, 2, 0xff, '\r', '\n'})
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	original, originalCT := buf.Bytes(), mw.FormDataContentType()

	rebuilt, rebuiltCT, err := rebuildImageEditBody(original, mustBoundary(t, originalCT), "app-name")
	if err != nil {
		t.Fatalf("rebuildImageEditBody: %v", err)
	}

	if rebuiltCT == originalCT {
		t.Fatalf("rebuilt Content-Type %q reuses the original boundary", rebuiltCT)
	}
	was := readEditParts(t, original, originalCT)
	got := readEditParts(t, rebuilt, rebuiltCT)
	if len(got) != len(was) {
		t.Fatalf("rebuilt body has %d parts, want %d", len(got), len(was))
	}
	for i := range was {
		wantBody := was[i].body
		if was[i].name == "model" {
			wantBody = []byte("app-name")
		}
		if got[i].name != was[i].name || got[i].filename != was[i].filename || got[i].contentType != was[i].contentType || !bytes.Equal(got[i].body, wantBody) {
			t.Fatalf("part %d = %+v, want name/filename/type of %+v and body %q", i, got[i], was[i], wantBody)
		}
	}
	// A header the code has no special knowledge of survives too.
	first, err := multipart.NewReader(bytes.NewReader(rebuilt), mustBoundary(t, rebuiltCT)).NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}
	if first.Header.Get("X-Custom") != "kept" {
		t.Fatalf("first part's X-Custom header = %q, want kept", first.Header.Get("X-Custom"))
	}
}

// A rebuild of a body that is cut off reports the error instead of emitting a
// form that looks complete.
func TestRebuildImageEditBodyReportsAMalformedBody(t *testing.T) {
	body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

	if _, _, err := rebuildImageEditBody(body[:len(body)-20], mustBoundary(t, ct), "app"); err == nil {
		t.Fatal("rebuild of a truncated body reported no error")
	}
}

// parseImageEditForm reads what routing needs from anywhere in the form, bounds
// every field it reads, and keeps the model as sent (untrimmed) for the rewrite
// decision while routing on the trimmed one.
func TestParseImageEditForm(t *testing.T) {
	t.Run("fields in any order, boundary kept", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{
			{name: "image[]", filename: "a.png", body: editTestPNG},
			{name: "n", body: []byte("2")},
			{name: "model", body: []byte("  sd-turbo \n")},
			{name: "prompt", body: []byte("a cat")},
			{name: "image", filename: "b.png", body: []byte("x")},
			{name: "response_format", body: []byte("b64_json")},
			{name: "stream", body: []byte("false")},
		})

		form, err := parseImageEditForm(ct, body)
		if err != nil {
			t.Fatalf("parseImageEditForm: %v", err)
		}
		if form.model != "  sd-turbo \n" || form.routingModel() != "sd-turbo" {
			t.Fatalf("model = %q / routing %q, want the untrimmed field and its trimmed routing value", form.model, form.routingModel())
		}
		if form.prompt != "a cat" || form.responseFormat != "b64_json" || form.stream != "false" {
			t.Fatalf("form = %+v", form)
		}
		if form.images != 2 {
			t.Fatalf("images = %d, want 2 (one under each spelling)", form.images)
		}
		if form.boundary != mustBoundary(t, ct) {
			t.Fatalf("boundary = %q, want the request's own %q", form.boundary, mustBoundary(t, ct))
		}
	})
	t.Run("a field past its bound is refused", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{{name: "prompt", body: bytes.Repeat([]byte("a"), maxImageEditFieldBytes+1)}})

		_, err := parseImageEditForm(ct, body)

		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err = %v, want a too-large refusal", err)
		}
	})
	t.Run("a field at its bound is read", func(t *testing.T) {
		body, ct := buildEditMultipart(t, []editPart{{name: "prompt", body: bytes.Repeat([]byte("a"), maxImageEditFieldBytes)}})

		form, err := parseImageEditForm(ct, body)
		if err != nil || len(form.prompt) != maxImageEditFieldBytes {
			t.Fatalf("err = %v, prompt length = %d, want %d", err, len(form.prompt), maxImageEditFieldBytes)
		}
	})
}

// proxyNative relays what its caller built: a distinct upstream body and content
// type reach the provider untouched (the multipart rebuild), a non-JSON content
// type is never run through the JSON model rewrite, and a caller that sets
// neither gets exactly what every JSON relay always got.
func TestProxyNativeRelaysTheCallersBodyAndContentType(t *testing.T) {
	target := routing.Target{Provider: routing.ProviderVLLM, ServerID: "srv-sd-turbo-fake", Model: "sd-turbo", ProviderModel: "mapped-name"}
	cases := []struct {
		name         string
		contentType  string
		raw          string
		upstreamBody []byte
		wantType     string
		wantBody     string
	}{
		{"JSON default rewrites the model to the mapped name", "", `{"model":"sd-turbo","prompt":"a"}`, nil, jsonContentType, `{"model":"mapped-name","prompt":"a"}`},
		{"a distinct upstream body and content type are relayed as given", "multipart/form-data; boundary=NEW", `{"client":"bytes"}`, []byte("rebuilt body"), "multipart/form-data; boundary=NEW", "rebuilt body"},
		{"a non-JSON content type is never run through the JSON rewrite", "multipart/form-data; boundary=X", `{"model":"sd-turbo"}`, nil, "multipart/form-data; boundary=X", `{"model":"sd-turbo"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)
			req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(tc.raw))
			rec := httptest.NewRecorder()

			srv.proxyNative(rec, req, nativeRelay{
				token:        auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true},
				target:       target,
				path:         upstreamPath(target, apiFlavorImages, endpointImagesEdit),
				raw:          []byte(tc.raw),
				pfReq:        inference.Request{Model: "sd-turbo", RequestedModel: "sd-turbo", APIFlavor: apiFlavorImages},
				endpoint:     endpointImagesEdit,
				contentType:  tc.contentType,
				upstreamBody: tc.upstreamBody,
			})

			if prov.gotContentType != tc.wantType {
				t.Fatalf("Content-Type = %q, want %q", prov.gotContentType, tc.wantType)
			}
			if string(prov.gotBody) != tc.wantBody {
				t.Fatalf("body = %q, want %q", prov.gotBody, tc.wantBody)
			}
		})
	}
}
