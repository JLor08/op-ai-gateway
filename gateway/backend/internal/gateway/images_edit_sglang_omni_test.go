// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// newSGLangOmniEditTestServer is the agent edit fixture (multipart_proxy declared,
// gateway and app model names equal) whose spec is typed specType.
func newSGLangOmniEditTestServer(t *testing.T, prov *recordingProxyProvider, specType routing.RuntimeSpecType) *Server {
	t.Helper()
	srv := newAgentEditTestServer(t, prov, "flux1-dev", "flux1-dev")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.UpsertRuntimeSpec(context.Background(), routing.RuntimeSpec{ID: "spec-agent-images", MappingID: "route-agent-images", Type: string(specType), APIFlavors: []string{routing.APIFlavorOpenAIImages}, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	return srv
}

// rawEditBody joins raw part blocks (each its header lines, a blank line and its
// body) into a multipart body under boundary, so a test can send header shapes
// multipart.Writer would never write.
func rawEditBody(boundary string, parts ...string) []byte {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\n" + p + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

const (
	omniEditBoundary = "omni-edit-boundary"
	omniModelPart    = "Content-Disposition: form-data; name=\"model\"\r\n\r\nflux1-dev"
	omniPromptPart   = "Content-Disposition: form-data; name=\"prompt\"\r\n\r\na cat"
	omniImagePart    = "Content-Disposition: form-data; name=\"image\"; filename=\"cat.png\"\r\nContent-Type: image/png\r\n\r\nPNGDATA"
)

// An edit bound for an sglang_omni target may name its source only as an uploaded
// file. sglang-omni's /v1/images/edits takes the source from image, image[], url or
// url[], and resolves a TEXT value as a path on the model host or a URL; Starlette
// keeps the LAST Content-Disposition of a part while Go's FormName and FileName read
// the FIRST, so a part whose disposition the two could read differently is refused too.
func TestImagesEditToSGLangOmniRefusesNonFileSources(t *testing.T) {
	cases := map[string]struct {
		part  string
		field string
	}{
		"double content-disposition": {
			"Content-Disposition: form-data; name=\"image\"; filename=\"cat.png\"\r\nContent-Disposition: form-data; name=\"url\"\r\nContent-Type: image/png\r\n\r\n/etc/passwd",
			"Content-Disposition",
		},
		"duplicate parameters": {
			"Content-Disposition: form-data; name=\"image\"; filename=\"cat.png\"; name=\"url\"\r\n\r\n/etc/passwd",
			"Content-Disposition",
		},
		"not form-data": {
			"Content-Disposition: attachment; name=\"image\"; filename=\"cat.png\"\r\n\r\nPNGDATA",
			"Content-Disposition",
		},
		"no content-disposition": {
			"Content-Type: image/png\r\n\r\n/etc/passwd",
			"Content-Disposition",
		},
		"text url":           {"Content-Disposition: form-data; name=\"url\"\r\n\r\nhttps://169.254.169.254/x.png", "url"},
		"text url[]":         {"Content-Disposition: form-data; name=\"url[]\"\r\n\r\n/etc/passwd", "url[]"},
		"file url":           {"Content-Disposition: form-data; name=\"url\"; filename=\"x.png\"\r\n\r\n/etc/passwd", "url"},
		"image without name": {"Content-Disposition: form-data; name=\"image\"\r\n\r\n/etc/passwd", "image"},
		"image[] text":       {"Content-Disposition: form-data; name=\"image[]\"\r\n\r\nfile:///etc/passwd", "image[]"},
		"image empty name":   {"Content-Disposition: form-data; name=\"image\"; filename=\"\"\r\n\r\n/etc/passwd", "image"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newSGLangOmniEditTestServer(t, prov, routing.RuntimeSpecTypeSGLangOmni)
			body := rawEditBody(omniEditBoundary, omniModelPart, omniPromptPart, omniImagePart, tc.part)
			rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, "multipart/form-data; boundary="+omniEditBoundary, map[string]string{"Authorization": "Bearer dev-secret"})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "images.source_unsupported")
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			if !strings.Contains(rec.Body.String(), tc.field) {
				t.Fatalf("refusal does not name %q: %s", tc.field, rec.Body.String())
			}
			for _, leak := range []string{"passwd", "169.254", "cat.png", "x.png"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("refusal echoes the value (%q): %s", leak, rec.Body.String())
				}
			}
			ev := lastUsageEvent(t, srv)
			if ev.ErrorCode != "images.source_unsupported" || ev.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("usage row = %+v, want a 400 images.source_unsupported row", ev)
			}
		})
	}
}

// A valid edit to an sglang_omni target is relayed with every part's headers built by
// the gateway: one form-data Content-Disposition with the part's name (and filename),
// and its Content-Type, and nothing else of what the client sent.
func TestImagesEditToSGLangOmniRelaysCanonicalParts(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newSGLangOmniEditTestServer(t, prov, routing.RuntimeSpecTypeSGLangOmni)
	body := rawEditBody(omniEditBoundary,
		"Content-Disposition: form-data; NAME=\"model\"\r\nX-Extra: 1\r\n\r\nflux1-dev",
		omniPromptPart,
		"Content-Disposition: form-data; name=image; filename=cat.png\r\nContent-Type: image/png\r\nX-Smuggle: url\r\n\r\nPNGDATA",
		"Content-Disposition: form-data; name=\"n\"\r\n\r\n1",
	)
	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, "multipart/form-data; boundary="+omniEditBoundary, map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusOK || prov.proxyCalls != 1 {
		t.Fatalf("status = %d, calls = %d, want a relayed 200; body = %s", rec.Code, prov.proxyCalls, rec.Body.String())
	}
	_, params, err := mime.ParseMediaType(prov.gotContentType)
	if err != nil || params["boundary"] == "" || params["boundary"] == omniEditBoundary {
		t.Fatalf("Content-Type = %q, want the rebuilt body's own boundary", prov.gotContentType)
	}
	b := params["boundary"]
	want := "--" + b + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nflux1-dev\r\n" +
		"--" + b + "\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\na cat\r\n" +
		"--" + b + "\r\nContent-Disposition: form-data; name=\"image\"; filename=\"cat.png\"\r\nContent-Type: image/png\r\n\r\nPNGDATA\r\n" +
		"--" + b + "\r\nContent-Disposition: form-data; name=\"n\"\r\n\r\n1\r\n" +
		"--" + b + "--\r\n"
	if string(prov.gotBody) != want {
		t.Fatalf("relayed body =\n%q\nwant\n%q", prov.gotBody, want)
	}
}

// The source rules belong to sglang_omni: an sglang target (or any other) is relayed as
// before, the client's bytes verbatim when nothing needs changing.
func TestImagesEditToOtherTargetsKeepsTheClientsParts(t *testing.T) {
	for _, specType := range []routing.RuntimeSpecType{routing.RuntimeSpecTypeSGLang, ""} {
		t.Run("type "+string(specType), func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newSGLangOmniEditTestServer(t, prov, specType)
			body := rawEditBody(omniEditBoundary, omniModelPart, omniPromptPart, omniImagePart, "Content-Disposition: form-data; name=\"url\"\r\n\r\nhttps://example.com/x.png")
			ct := "multipart/form-data; boundary=" + omniEditBoundary
			rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})
			if rec.Code != http.StatusOK || prov.proxyCalls != 1 {
				t.Fatalf("status = %d, calls = %d; body = %s", rec.Code, prov.proxyCalls, rec.Body.String())
			}
			if prov.gotContentType != ct || !bytes.Equal(prov.gotBody, body) {
				t.Fatalf("body was rebuilt (%q), want the client's bytes verbatim", prov.gotContentType)
			}
		})
	}
}

// python-multipart splits a Content-Disposition with a copy of CPython's
// email.message._parseparam, which counts quotes as count('"') - count('\\"'), so a
// quoted value ending in a backslash misparses and a smuggled parameter becomes a
// field the gateway never saw. The relayed names and filenames therefore stay inside a
// charset that has no such shape: a refused part is a 400 naming the header, never the
// value, and a plain filename with a space still passes.
func TestImagesEditToSGLangOmniRefusesUnsafePartNames(t *testing.T) {
	cases := map[string]string{
		"backslash name smuggling a url field":    "Content-Disposition: form-data; name=\"a\\\\\"; filename=\";name=url;\"\r\n\r\n/etc/passwd",
		"filename with quote semicolon backslash": "Content-Disposition: form-data; name=\"image\"; filename=\"x\\\"; name=\\\"url;\\\\\"\r\n\r\n/etc/passwd",
		"filename with a plain quote":             "Content-Disposition: form-data; name=\"image\"; filename=\"a\\\"b.png\"\r\n\r\nPNGDATA",
		"filename with a semicolon":               "Content-Disposition: form-data; name=\"image\"; filename=\"a;b.png\"\r\n\r\nPNGDATA",
		"filename with a backslash":               "Content-Disposition: form-data; name=\"image\"; filename=\"a\\\\b.png\"\r\n\r\nPNGDATA",
		"name with a space":                       "Content-Disposition: form-data; name=\"zz yy\"\r\n\r\nx",
		"name with a semicolon":                   "Content-Disposition: form-data; name=\"a;b\"\r\n\r\nx",
		"empty name":                              "Content-Disposition: form-data; name=\"\"\r\n\r\nx",
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newSGLangOmniEditTestServer(t, prov, routing.RuntimeSpecTypeSGLangOmni)
			body := rawEditBody(omniEditBoundary, omniModelPart, omniPromptPart, omniImagePart, part)
			rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, "multipart/form-data; boundary="+omniEditBoundary, map[string]string{"Authorization": "Bearer dev-secret"})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "images.source_unsupported")
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			if !strings.Contains(rec.Body.String(), "Content-Disposition") {
				t.Fatalf("refusal does not name the Content-Disposition header: %s", rec.Body.String())
			}
			for _, leak := range []string{"passwd", "name=url", "zz yy", "b.png"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("refusal echoes the value (%q): %s", leak, rec.Body.String())
				}
			}
		})
	}
}

// The charset rule leaves ordinary uploads alone: a filename with a space, and the
// bracketed, dotted and dashed names OpenAI clients send, are relayed.
func TestImagesEditToSGLangOmniRelaysSafePartNames(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newSGLangOmniEditTestServer(t, prov, routing.RuntimeSpecTypeSGLangOmni)
	body := rawEditBody(omniEditBoundary, omniModelPart, omniPromptPart,
		"Content-Disposition: form-data; name=\"image\"; filename=\"photo 1.png\"\r\nContent-Type: image/png\r\n\r\nPNGDATA",
		"Content-Disposition: form-data; name=\"image[]\"; filename=\"photo-2_b.v1.png\"\r\nContent-Type: image/png\r\n\r\nPNGDATA",
	)
	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, "multipart/form-data; boundary="+omniEditBoundary, map[string]string{"Authorization": "Bearer dev-secret"})
	if rec.Code != http.StatusOK || prov.proxyCalls != 1 {
		t.Fatalf("status = %d, calls = %d, want a relayed 200; body = %s", rec.Code, prov.proxyCalls, rec.Body.String())
	}
	if !strings.Contains(string(prov.gotBody), `filename="photo 1.png"`) {
		t.Fatalf("relayed body lost the filename: %q", prov.gotBody)
	}
}
