// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"
)

// editPart describes one part of a multipart edit request: a plain form field
// when filename is empty, a file part otherwise.
type editPart struct {
	name        string
	filename    string
	contentType string
	body        []byte
}

// editTestPNG is deliberately NOT valid UTF-8 and carries CRLF, NUL and a "--"
// run: the bytes a string round trip or a line-oriented rewrite would damage,
// so a test that compares it survives only a byte-faithful relay.
var editTestPNG = []byte("\x89PNG\r\n\x1a\n\x00\x01\xfe\xff\r\n--not-a-boundary\r\n\x00 tail")

// editsOKBody is a minimal successful sd-server answer for an edit; the shape is
// the generations one (the edits endpoint answers the same document).
const editsOKBody = `{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`

// buildEditMultipart assembles a multipart/form-data body from parts, in order,
// and returns it with the Content-Type (boundary included) a client would send.
func buildEditMultipart(t *testing.T, parts []editPart) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name=%q`, p.name)
		if p.filename != "" {
			disposition += fmt.Sprintf(`; filename=%q`, p.filename)
		}
		h.Set("Content-Disposition", disposition)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart(%s): %v", p.name, err)
		}
		if _, err := pw.Write(p.body); err != nil {
			t.Fatalf("write part %s: %v", p.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// validEditParts is a well-formed edit request for model: the model and prompt
// fields and one source image.
func validEditParts(model string) []editPart {
	return []editPart{
		{name: "model", body: []byte(model)},
		{name: "prompt", body: []byte("a cat in a hat")},
		{name: "image", filename: "cat.png", contentType: "image/png", body: editTestPNG},
	}
}

// replaceEditPart returns parts with the first part named name replaced by
// repl, or with that part dropped when repl is nil.
func replaceEditPart(parts []editPart, name string, repl *editPart) []editPart {
	out := make([]editPart, 0, len(parts))
	for _, p := range parts {
		if p.name != name {
			out = append(out, p)
			continue
		}
		if repl != nil {
			out = append(out, *repl)
			repl = nil
		}
	}
	return out
}

// postEditRaw sends an arbitrary request at the edits endpoint through the real
// mux. The caller owns every header: nothing is added beyond Content-Type, so a
// loopback test is not also carrying a bearer.
func postEditRaw(t *testing.T, srv *Server, method, path string, body []byte, contentType string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// postImageEdit is the common case: a bearer-authenticated multipart edit built
// from parts, posted to /v1/images/edits.
func postImageEdit(t *testing.T, srv *Server, parts []editPart) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildEditMultipart(t, parts)
	return postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})
}

// readEditParts decodes a multipart body into its parts, in wire order.
func readEditParts(t *testing.T, body []byte, contentType string) []editPart {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type %q is not multipart/form-data: %v", contentType, err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out []editPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		b, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part %s: %v", p.FormName(), err)
		}
		out = append(out, editPart{name: p.FormName(), filename: p.FileName(), contentType: p.Header.Get("Content-Type"), body: b})
	}
}

// editUpstream records what a stub sd-server received on its edits endpoint.
type editUpstream struct {
	mu          sync.Mutex
	calls       int
	method      string
	path        string
	contentType string
	body        []byte
}

func (u *editUpstream) snapshot() (calls int, method, path, contentType string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls, u.method, u.path, u.contentType, append([]byte(nil), u.body...)
}

// newEditUpstream starts a stub upstream that answers every request with status
// and respBody, and records the request it was sent.
func newEditUpstream(t *testing.T, status int, respBody string) (*httptest.Server, *editUpstream) {
	t.Helper()
	rec := &editUpstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls++
		rec.method, rec.path, rec.contentType, rec.body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), b
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(up.Close)
	return up, rec
}

// An edit is relayed as the multipart request the client sent, to the upstream's
// EDITS endpoint, and the generations-shaped answer comes back unchanged. Every
// claim of this test is also a regression a mutation would produce: a relay that
// reached /v1/images/generations, one that re-encoded the body, one that sent
// application/json, and one whose usage row recorded the generations path (the
// flavor is shared, so only the endpoint threaded through usageMeta tells them
// apart -- A2's carry).
func TestImagesEditRelaysMultipartToTheEditsPath(t *testing.T) {
	up, got := newEditUpstream(t, http.StatusOK, editsOKBody)
	srv := newImageCapableTestServer(t, up.URL)
	body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"b64_json":"AA=="`) {
		t.Fatalf("response body = %s, want the upstream's b64_json document", rec.Body.String())
	}
	calls, method, path, upstreamCT, upstreamBody := got.snapshot()
	if calls != 1 || method != http.MethodPost {
		t.Fatalf("upstream calls = %d, method = %q, want exactly one POST", calls, method)
	}
	if path != "/v1/images/edits" {
		t.Fatalf("upstream path = %q, want /v1/images/edits", path)
	}
	if upstreamCT != ct {
		t.Fatalf("upstream Content-Type = %q, want the client's %q (verbatim, boundary included)", upstreamCT, ct)
	}
	if !bytes.Equal(upstreamBody, body) {
		t.Fatalf("upstream body differs from the client's (%d vs %d bytes): an external target with no mask must be relayed verbatim", len(upstreamBody), len(body))
	}

	ev := lastUsageEvent(t, srv)
	if ev.ProviderPath != "/v1/images/edits" {
		t.Fatalf("usage ProviderPath = %q, want /v1/images/edits (the endpoint must reach recordUsage, else an edit records the generations path)", ev.ProviderPath)
	}
	if ev.ReqPath != "/v1/images/edits" {
		t.Fatalf("usage ReqPath = %q, want /v1/images/edits", ev.ReqPath)
	}
	if ev.Status != "success" || ev.BillingUnit != usage.BillingUnitImage || ev.BillingQuantity != 1 {
		t.Fatalf("usage row = status %q unit %q quantity %v, want a successful 1-image row", ev.Status, ev.BillingUnit, ev.BillingQuantity)
	}
}

// The usage row's path must also be the edits path when the upstream refuses:
// the non-2xx branch goes through relayImagesUpstreamError, a separate recordUsage
// site from the success one above.
func TestImagesEditUpstreamErrorRecordsTheEditsPath(t *testing.T) {
	up, _ := newEditUpstream(t, http.StatusInternalServerError, `{"error":"out of memory"}`)
	srv := newImageCapableTestServer(t, up.URL)

	rec := postImageEdit(t, srv, validEditParts("sd-turbo"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream's 500; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), imagesUpstreamErrorCode)
	ev := lastUsageEvent(t, srv)
	if ev.ProviderPath != "/v1/images/edits" || ev.Status != "error" {
		t.Fatalf("usage row = path %q status %q, want /v1/images/edits and error", ev.ProviderPath, ev.Status)
	}
}

// Both image field spellings the OpenAI clients use are accepted, in any order
// relative to the other fields -- including an image part that arrives BEFORE
// the model field (the model is peeked from the whole form, not the first part).
func TestImagesEditAcceptsBothImageFieldSpellingsInAnyOrder(t *testing.T) {
	cases := []struct {
		name  string
		parts []editPart
	}{
		{"image", validEditParts("sd-turbo")},
		{"image[]", []editPart{
			{name: "model", body: []byte("sd-turbo")},
			{name: "prompt", body: []byte("a cat")},
			{name: "image[]", filename: "a.png", contentType: "image/png", body: editTestPNG},
			{name: "image[]", filename: "b.png", contentType: "image/png", body: editTestPNG},
		}},
		{"image before model and prompt", []editPart{
			{name: "image", filename: "cat.png", contentType: "image/png", body: editTestPNG},
			{name: "prompt", body: []byte("a cat")},
			{name: "model", body: []byte("sd-turbo")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)

			rec := postImageEdit(t, srv, tc.parts)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if prov.proxyCalls != 1 {
				t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
			}
		})
	}
}

// stream=false (and an empty value) describe what the endpoint already does and
// are accepted, exactly as validateImagesRequest accepts "stream": false.
func TestImagesEditAcceptsAFalseStream(t *testing.T) {
	for _, v := range []string{"false", ""} {
		t.Run("stream="+v, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)
			parts := append(validEditParts("sd-turbo"), editPart{name: "stream", body: []byte(v)})

			rec := postImageEdit(t, srv, parts)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// invalidEditCase is one request the handler must refuse before preflight.
type invalidEditCase struct {
	name  string
	parts []editPart
	code  string
}

func invalidEditCases() []invalidEditCase {
	valid := validEditParts("sd-turbo")
	return []invalidEditCase{
		{"missing model", replaceEditPart(valid, "model", nil), "images.model_required"},
		{"blank model", replaceEditPart(valid, "model", &editPart{name: "model", body: []byte("   ")}), "images.model_required"},
		{"missing prompt", replaceEditPart(valid, "prompt", nil), "images.prompt_required"},
		{"blank prompt", replaceEditPart(valid, "prompt", &editPart{name: "prompt", body: []byte(" \n")}), "images.prompt_required"},
		{"no image part", replaceEditPart(valid, "image", nil), "images.image_required"},
		{"image under another field name", replaceEditPart(valid, "image", &editPart{name: "file", filename: "cat.png", contentType: "image/png", body: editTestPNG}), "images.image_required"},
		{"empty image part", replaceEditPart(valid, "image", &editPart{name: "image", filename: "cat.png", contentType: "image/png"}), "images.image_required"},
		{"stream true", append(append([]editPart(nil), valid...), editPart{name: "stream", body: []byte("true")}), "images.stream_unsupported"},
		{"stream not a boolean", append(append([]editPart(nil), valid...), editPart{name: "stream", body: []byte("yes please")}), "images.stream_unsupported"},
		{"response_format url", append(append([]editPart(nil), valid...), editPart{name: "response_format", body: []byte("url")}), "images.response_format_unsupported"},
		// The model decides routing AND the allowlist, and an agent may peek it
		// again: two of them is two answers to "which model", so it is refused
		// instead of one being picked.
		{"duplicate model", append(append([]editPart(nil), valid...), editPart{name: "model", body: []byte("qwen-coder")}), "request.invalid"},
		{"duplicate prompt", append(append([]editPart(nil), valid...), editPart{name: "prompt", body: []byte("another")}), "request.invalid"},
	}
}

// Every refusal the validation twin owns is a 400 with its own code, answered
// BEFORE the gate: no upstream call and no usage row (a request that never
// reached routing records nothing, as the generations twin's refusals).
func TestImagesEditRefusesInvalidRequests(t *testing.T) {
	for _, tc := range invalidEditCases() {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)

			rec := postImageEdit(t, srv, tc.parts)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.code)
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			if n := len(srv.Usage.All()); n != 0 {
				t.Fatalf("usage rows = %d, want 0 for a request refused before routing", n)
			}
		})
	}
}

// THE POINT of peeking the model from the multipart form: the refusals above
// happen before inferencePreflight, so none of them spends an admission. The
// limiter here allows ONE request per minute; seven invalid requests go first,
// the valid one that follows is still admitted, and a second valid one is then
// denied -- the control showing the limiter was live the whole time.
func TestImagesEditValidationRunsBeforeAdmission(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newImageCapableTestServerWithProvider(t, prov)
	fakeStore := newFakePrincipalStore()
	fakeStore.setConfig(Principal{Type: routing.PrincipalTypeUser, ID: "usr_dev"}, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	for _, tc := range invalidEditCases() {
		if rec := postImageEdit(t, srv, tc.parts); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body = %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if rec := postImageEdit(t, srv, validEditParts("sd-turbo")); rec.Code != http.StatusOK {
		t.Fatalf("the first valid edit after the refusals: status = %d, want 200 (a refusal spent the admission); body = %s", rec.Code, rec.Body.String())
	}
	rec := postImageEdit(t, srv, validEditParts("sd-turbo"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the second valid edit: status = %d, want 429 (the limiter must have been live)", rec.Code)
	}
	requireErrorCode(t, rec.Body.String(), "limit.rate_limited")
}

// A body that is not a multipart form is refused 400 before anything else: a
// JSON body posted by an client that took this for the generations endpoint, a
// multipart type with no boundary, and a body cut off before its closing
// boundary.
func TestImagesEditRefusesAMalformedBody(t *testing.T) {
	validBody, validCT := buildEditMultipart(t, validEditParts("sd-turbo"))
	cases := []struct {
		name string
		body []byte
		ct   string
	}{
		{"json body", []byte(`{"model":"sd-turbo","prompt":"a cat"}`), "application/json"},
		{"no content type", validBody, ""},
		{"multipart without a boundary", validBody, "multipart/form-data"},
		{"truncated before the closing boundary", validBody[:len(validBody)-16], validCT},
		{"empty body", nil, validCT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)

			rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", tc.body, tc.ct, map[string]string{"Authorization": "Bearer dev-secret"})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
		})
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The multipart read is bounded (the endpoint's body is held in memory, so an
// unbounded read is a memory exposure). A body past the cap is a 413 whether the
// size was declared up front -- refused without reading a byte of it -- or only
// discovered while reading (chunked), when the read stops at the cap.
func TestImagesEditRefusesABodyPastTheCap(t *testing.T) {
	big := validEditParts("sd-turbo")
	big = append(big, editPart{name: "mask", filename: "m.png", contentType: "image/png", body: bytes.Repeat([]byte{0xAB}, maxImageEditBodyBytes)})
	body, ct := buildEditMultipart(t, big)
	if len(body) <= maxImageEditBodyBytes {
		t.Fatalf("fixture is %d bytes, want more than the %d cap", len(body), maxImageEditBodyBytes)
	}
	for _, declared := range []bool{true, false} {
		name := "chunked"
		if declared {
			name = "declared length"
		}
		t.Run(name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)
			counted := &countingReader{r: bytes.NewReader(body)}
			req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", io.NopCloser(counted))
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
				t.Fatalf("status = %d, want 413", rec.Code)
			}
			requireErrorCode(t, rec.Body.String(), "request.body_too_large")
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
			}
			switch {
			case declared && counted.n != 0:
				t.Fatalf("read %d bytes of a body whose declared length was already past the cap", counted.n)
			case !declared && counted.n > maxImageEditBodyBytes+(1<<20):
				t.Fatalf("read %d bytes of a chunked body, want the read to stop at the %d cap", counted.n, maxImageEditBodyBytes)
			}
		})
	}
}

// Only POST reaches the handler; the 405 is answered before auth, like the
// generations endpoint.
func TestImagesEditRejectsOtherMethods(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			srv := NewTestServer()

			rec := postEditRaw(t, srv, m, "/v1/images/edits", nil, "", nil)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Fatalf("Allow = %q, want POST", got)
			}
			requireErrorCode(t, rec.Body.String(), codeRequestMethodNotAllowed)
		})
	}
}

// Auth is exactly the generations endpoint's: no credential is a 401, a bearer
// and the internal loopback pair are accepted on BOTH routes, and a browser
// session cookie is refused (a logged-in browser must never reach this endpoint
// directly -- the portal calls it server-side).
func TestImagesEditAuth(t *testing.T) {
	t.Run("no credential", func(t *testing.T) {
		srv := newImageCapableTestServerWithProvider(t, &recordingProxyProvider{respBody: editsOKBody})
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, nil)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("loopback pair on the alias route", func(t *testing.T) {
		up, _ := newEditUpstream(t, http.StatusOK, editsOKBody)
		srv, _ := newImageCapableLoopbackTestServer(t, up.URL)
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

		rec := postEditRaw(t, srv, http.MethodPost, "/openai/v1/images/edits", body, ct,
			map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 on the /openai alias; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("bearer on the alias route", func(t *testing.T) {
		up, _ := newEditUpstream(t, http.StatusOK, editsOKBody)
		srv := newImageCapableTestServer(t, up.URL)
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

		rec := postEditRaw(t, srv, http.MethodPost, "/openai/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("session cookie", func(t *testing.T) {
		srv, dir := newChatTestServer(t)
		seedLoginUser(t, dir, "usr_img", "img@example.test", "password-1", "user")
		cookie := loginCookie(t, srv, "img@example.test", "password-1")
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct,
			map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, csrfHeaderName: "1"})

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 for a session cookie", rec.Code)
		}
	})
}

// The run-as block is copied from the generations handler: a loopback request
// naming a token the loopback user owns is attributed to that token, one naming
// somebody else's is refused, and one with no Portal to authorize it fails
// closed.
func TestImagesEditRunAs(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	loopback := map[string]string{internalAuthHeaderName: "s3cret", internalUserHeaderName: "usr_dev"}
	withRunAs := func(id string) map[string]string {
		h := map[string]string{runAsHeaderName: id}
		for k, v := range loopback {
			h[k] = v
		}
		return h
	}

	t.Run("owned token attributes usage", func(t *testing.T) {
		up, _ := newEditUpstream(t, http.StatusOK, editsOKBody)
		srv, dir := newImageCapableLoopbackTestServer(t, up.URL)
		if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_ra_edit", UserID: "usr_dev", Name: "RA Edit", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "ra-secret"); err != nil {
			t.Fatalf("seed run-as token: %v", err)
		}
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, withRunAs("tok_ra_edit"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if got := lastUsageEvent(t, srv).TokenID; got != "tok_ra_edit" {
			t.Fatalf("usage TokenID = %q, want the run-as token tok_ra_edit", got)
		}
	})
	t.Run("someone else's token is refused", func(t *testing.T) {
		up, upstream := newEditUpstream(t, http.StatusOK, editsOKBody)
		srv, dir := newImageCapableLoopbackTestServer(t, up.URL)
		dir.AddUser(store.User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
		if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_other", UserID: "usr_other", Name: "Other", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "other-secret"); err != nil {
			t.Fatalf("seed foreign token: %v", err)
		}
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))

		rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, withRunAs("tok_other"))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "portal.token_forbidden")
		if calls, _, _, _, _ := upstream.snapshot(); calls != 0 {
			t.Fatalf("upstream calls = %d, want 0", calls)
		}
		if events := srv.Usage.All(); len(events) != 0 {
			t.Fatalf("usage events = %#v, want none for a forbidden run-as", events)
		}
	})
	t.Run("no portal fails closed", func(t *testing.T) {
		s := &Server{internalAuthSecret: "s3cret", users: fakeUserLookup{
			"usr_1": {ID: "usr_1", Role: "user"},
		}} // Portal, Routes, Tokens all nil
		body, ct := buildEditMultipart(t, validEditParts("sd-turbo"))
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
		r.Header.Set("Content-Type", ct)
		r.Header.Set(internalAuthHeaderName, "s3cret")
		r.Header.Set(internalUserHeaderName, "usr_1")
		r.Header.Set(runAsHeaderName, "tok_whatever")
		w := httptest.NewRecorder()

		s.handleOpenAIImagesEdit(w, r)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d body = %s, want 403", w.Code, w.Body.String())
		}
		requireErrorCode(t, w.Body.String(), "portal.token_forbidden")
	})
}

// RequiredCapabilities = [image_edit] comes from the handler's identity. A model
// with no image verdict at all is refused with the capability code, and a model
// the operator opted OUT of editing (image_edit:no) is refused for an edit while
// still generating -- the policy is on the edit, not on the model.
func TestImagesEditCapabilityGate(t *testing.T) {
	t.Run("a text model is not capable", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newImageCapableTestServerWithProvider(t, prov)

		rec := postImageEdit(t, srv, validEditParts("qwen-coder"))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")
		if prov.proxyCalls != 0 {
			t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
		}
		if got := lastUsageEvent(t, srv); got.BillingUnit != usage.BillingUnitImage {
			t.Fatalf("usage BillingUnit = %q, want the image unit on the refusal row", got.BillingUnit)
		}
	})
	t.Run("an operator opt-out refuses the edit and not the generation", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newImageCapableTestServerWithProvider(t, prov)
		if err := srv.Routes.UpsertMappingCapabilities(context.Background(), "route-sd-turbo-fake", []routing.CapabilityRow{{Capability: routing.CapabilityImageEdit, Verdict: routing.CapabilityNo, Source: "manual", CheckedAt: time.Now()}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities: %v", err)
		}

		rec := postImageEdit(t, srv, validEditParts("sd-turbo"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("edit status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "routing.model_not_capable")

		gen := postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)
		if gen.Code != http.StatusOK {
			t.Fatalf("generation status = %d, want 200 (the opt-out is on the edit only); body = %s", gen.Code, gen.Body.String())
		}
	})
}

// An UNKNOWN edit model is redirected -- not 404'd -- through preflight when the
// token opted in: the offering the redirect asks must carry image_edit, which
// capableNames derives (A4). The served request records the client's wish as
// RequestedModel and goes to the edits path. The negative control: a fallback
// the operator opted out of editing is not a usable candidate, so the redirect
// declines and the client gets the ordinary unknown-model answer.
func TestImagesEditRedirectsAnUnknownModel(t *testing.T) {
	t.Run("redirects to an edit-capable fallback", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newRedirectingTestServer(t, prov, "", "sd-turbo")

		rec := postImageEdit(t, srv, validEditParts("dall-e-2"))

		requireRedirectedImagesRequest(t, srv, prov, rec, "dall-e-2", "sd-turbo")
		if prov.gotPath != "/v1/images/edits" {
			t.Fatalf("upstream path = %q, want /v1/images/edits", prov.gotPath)
		}
	})
	t.Run("does not redirect to a fallback opted out of editing", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newRedirectingTestServer(t, prov, "", "sd-turbo")
		if err := srv.Routes.UpsertMappingCapabilities(context.Background(), "route-sd-turbo", []routing.CapabilityRow{{Capability: routing.CapabilityImageEdit, Verdict: routing.CapabilityNo, Source: "manual", CheckedAt: time.Now()}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities: %v", err)
		}

		rec := postImageEdit(t, srv, validEditParts("dall-e-2"))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
		if prov.proxyCalls != 0 {
			t.Fatalf("ProxyNative calls = %d, want 0", prov.proxyCalls)
		}
	})
}

// The effective-served conjunct (ADR-044 b): an agent-launched child whose
// runtime spec lists only text cannot be picked to serve an edit, and the
// refusal looks like candidacy's own no-route answer. The positive control is
// the same fixture with a spec that lists openai_images.
func TestImagesEditRefusesServerAgentChildWhoseSpecExcludesImages(t *testing.T) {
	t.Run("text-only spec is refused", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAI})

		rec := postImageEdit(t, srv, validEditParts("flux1-dev"))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), "routing.no_model_route")
		if prov.proxyCalls != 0 {
			t.Fatalf("ProxyNative calls = %d, want 0 -- a text-only spec must never be sent an edit", prov.proxyCalls)
		}
		events := srv.Usage.All()
		if len(events) != 1 || events[0].ErrorCode != "routing.no_model_route" || events[0].BillingUnit != usage.BillingUnitImage {
			t.Fatalf("usage events = %+v, want one routing.no_model_route error billed as image", events)
		}
		if ev := events[0]; ev.Host != "" || ev.RouteID != "" || ev.ProviderPath != "" {
			t.Fatalf("usage row = %+v, want Host/RouteID/ProviderPath empty: no upstream call was made", ev)
		}
	})
	t.Run("images spec is served", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAIImages})
		declareMultipartProxy(srv)

		rec := postImageEdit(t, srv, validEditParts("flux1-dev"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if prov.proxyCalls != 1 {
			t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
		}
	})
}

// agentEditServerID is the server the server_agent images fixture
// (newServerAgentImagesSpecTestServer) places its one agent application on.
const agentEditServerID = "srv-agent-images"

// declareMultipartProxy records, the way a telemetry sample does, that the
// fixture's agent serves multipart edits. The feature name is spelled as a
// LITERAL on purpose: it is what an agent puts on the wire (Task B), so a typo in
// the gateway's constant must fail here rather than agree with itself.
func declareMultipartProxy(srv *Server) {
	srv.AgentFeatures.Set(agentEditServerID, []string{"runtime_manager", "multipart_proxy"})
}

// newAgentEditTestServer is the server_agent images fixture with its single
// mapping renamed so the client-facing name (GatewayModelName) and the name the
// agent's router keys the child on (AppModelName) differ. Its agent declares
// multipart_proxy: every test built on it is about HOW an edit is relayed to an
// agent that can take one (the gate itself is
// TestImagesEditRefusesAnAgentTargetWithoutMultipartProxy).
func newAgentEditTestServer(t *testing.T, prov *recordingProxyProvider, gatewayName, appName string) *Server {
	t.Helper()
	srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAIImages})
	declareMultipartProxy(srv)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := srv.Routes.UpdateMapping(context.Background(), routing.ModelMapping{ID: "route-agent-images", ApplicationID: "app-agent-images", GatewayModelName: gatewayName, AppModelName: appName, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	return srv
}

// For an agent-launched target the agent's router keys the child on the
// mapping's AppModelName, which is not the name the client sent. The relayed
// form must therefore carry the app name in its model field -- and nothing else
// may change: every other field, the image's bytes and its headers, and the
// part order survive the rebuild. The request is sent as multipart with a NEW
// boundary (the Content-Type handed to ProxyNative is the rebuilt body's).
func TestImagesEditRewritesTheModelForAnAgentTarget(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux-gateway-name", "flux1-dev-q8")
	parts := []editPart{
		{name: "image", filename: "src.png", contentType: "image/png", body: editTestPNG},
		{name: "model", body: []byte("flux-gateway-name")},
		{name: "prompt", body: []byte("a cat in a hat")},
		{name: "n", body: []byte("1")},
		{name: "mask", filename: "mask.png", contentType: "image/png", body: []byte("\x00\xff\r\n\x00mask")},
	}
	body, ct := buildEditMultipart(t, parts)

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
	}
	if prov.gotPath != "/v1/images/edits" {
		t.Fatalf("upstream path = %q, want /v1/images/edits", prov.gotPath)
	}
	if prov.gotContentType == ct {
		t.Fatalf("Content-Type = %q is the client's: the rebuilt body must be sent with its own boundary", prov.gotContentType)
	}
	got := readEditParts(t, prov.gotBody, prov.gotContentType)
	want := append([]editPart(nil), parts...)
	want[1].body = []byte("flux1-dev-q8")
	if len(got) != len(want) {
		t.Fatalf("rebuilt body has %d parts, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].filename != want[i].filename || got[i].contentType != want[i].contentType || !bytes.Equal(got[i].body, want[i].body) {
			t.Fatalf("part %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// What capture and the usage row record is still the CLIENT's request.
	if ev := lastUsageEvent(t, srv); ev.Model != "flux-gateway-name" || ev.ProviderPath != "/v1/images/edits" {
		t.Fatalf("usage Model/ProviderPath = %q/%q, want flux-gateway-name and /v1/images/edits", ev.Model, ev.ProviderPath)
	}
}

// The rewrite is needed only when the form's model is not already the name the
// agent keys the child on: an agent target whose names are equal is relayed
// verbatim, byte for byte, with the client's Content-Type.
func TestImagesEditRelaysAnAgentTargetVerbatimWhenTheNamesAreEqual(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux1-dev", "flux1-dev")
	body, ct := buildEditMultipart(t, validEditParts("flux1-dev"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.gotContentType != ct || !bytes.Equal(prov.gotBody, body) {
		t.Fatalf("body/Content-Type were rebuilt (%q); an agent target whose names are equal must be relayed verbatim", prov.gotContentType)
	}
}

// ...and the decision is made against what the CLIENT sent, not against the
// mapping's own name pair: a token whose model override sends every request to
// "flux1-dev" resolves a mapping whose two names are equal, yet the form still
// says whatever the client wrote, which is not a name the agent's router knows.
func TestImagesEditRewritesTheModelAnOverrideReplaced(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux1-dev", "flux1-dev")
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{
		ID: "tok_ovr", UserID: "usr_dev", Name: "Override Token", Active: true,
		Scopes: []string{"gateway:use"}, ModelOverride: "flux1-dev",
	}, "ovr-secret")
	body, ct := buildEditMultipart(t, validEditParts("dall-e-2"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer ovr-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var model string
	for _, p := range readEditParts(t, prov.gotBody, prov.gotContentType) {
		if p.name == "model" {
			model = string(p.body)
		}
	}
	if model != "flux1-dev" {
		t.Fatalf("relayed model = %q, want the app name flux1-dev (the client's dall-e-2 is not a name the agent knows)", model)
	}
}

// The rewrite predicate, axis by axis: only an agent-launched target with a
// mapped name the form does not already carry is rewritten.
func TestImageEditNeedsModelRewrite(t *testing.T) {
	agent := func(app string) routing.Target {
		return routing.Target{Provider: routing.ProviderServerAgent, Model: "gw", ProviderModel: app}
	}
	cases := []struct {
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
		{"external target, names differ", routing.Target{Provider: routing.ProviderVLLM, Model: "gw", ProviderModel: "app"}, "gw", false},
		{"external target, names equal", routing.Target{Provider: routing.ProviderVLLM, Model: "gw", ProviderModel: "gw"}, "gw", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageEditNeedsModelRewrite(tc.target, tc.form); got != tc.want {
				t.Fatalf("imageEditNeedsModelRewrite = %v, want %v", got, tc.want)
			}
		})
	}
}

// An EXTERNAL sd-server target is relayed verbatim even when the mapping's two
// names differ: sd-server ignores the model field (one process serves one
// model), so rewriting buys nothing and every rebuild is a chance to damage the
// body.
func TestImagesEditRelaysAnExternalTargetVerbatimEvenWhenTheNamesDiffer(t *testing.T) {
	up, got := newEditUpstream(t, http.StatusOK, editsOKBody)
	srv := newImagesHTTPTestServer(t, up.URL) // gw-image-model -> upstream-image-model
	body, ct := buildEditMultipart(t, validEditParts("gw-image-model"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	calls, _, path, upstreamCT, upstreamBody := got.snapshot()
	if calls != 1 || path != "/v1/images/edits" {
		t.Fatalf("upstream calls = %d, path = %q, want one call to /v1/images/edits", calls, path)
	}
	if upstreamCT != ct || !bytes.Equal(upstreamBody, body) {
		t.Fatalf("the external target's body was rebuilt; it must be relayed verbatim")
	}
}

// The generations relay is unchanged by sharing the resolve + served-flavor
// step with the edits relay: it still reaches the generations path with a JSON
// content type, and the JSON model rewrite still applies to it.
func TestImagesGenerationsRelayIsUnchangedBySharingTheResolveStep(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newImageCapableTestServerWithProvider(t, prov)

	rec := postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.gotPath != "/v1/images/generations" || prov.gotContentType != jsonContentType {
		t.Fatalf("path/content type = %q/%q, want /v1/images/generations and application/json", prov.gotPath, prov.gotContentType)
	}
	if got := lastUsageEvent(t, srv).ProviderPath; got != "/v1/images/generations" {
		t.Fatalf("usage ProviderPath = %q, want /v1/images/generations", got)
	}
}

// What the payload capture records for a REBUILT edit is the CLIENT's request,
// not the body the agent was sent: nativeRelay.raw stays the client's bytes and
// the rebuilt one travels beside it as upstreamBody. (The fixture is text-only on
// purpose: the capture envelope stores a body as a string, so how it stores
// BINARY multipart is a separate question from which body it stores.)
func TestImagesEditCaptureRecordsTheClientsBodyNotTheRebuiltOne(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux-gateway-name", "flux1-dev-q8")
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
		{name: "model", body: []byte("flux-gateway-name")},
		{name: "prompt", body: []byte("a cat in a hat")},
		{name: "image", filename: "cat.png", contentType: "image/png", body: []byte("plain-text-image")},
	})

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer cap-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(prov.gotBody), "flux1-dev-q8") {
		t.Fatalf("the agent was not sent the rebuilt body: %q", prov.gotBody)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	if env.ReqBody != string(body) {
		t.Fatalf("captured request body = %q, want the client's own bytes %q", env.ReqBody, body)
	}
}

// An edit relays as MULTIPART, and an agent's router that predates the
// multipart branch peeks the model out of a JSON body: it cannot read one out of
// a form, so it answers 404 runtime.model_not_managed -- the same status and
// code it answers an unmanaged model with, which is why trying the route cannot
// tell the two apart. The gateway therefore refuses an edit to an agent target
// unless that agent has DECLARED multipart_proxy, and refuses it as itself
// (images.edit_unavailable) instead of relaying a request that is known to 404.
//
// Every case is an agent the gateway has no positive evidence about, so each is
// refused: fail-closed is the registry's own default (AgentFeatures.Has). Each is
// falsifiable against a gate that is too loose -- "any features reported", "the
// feature on some server", a prefix match -- and against one that is not a gate
// at all (ProxyNative must not be reached, and no upstream call may be recorded).
func TestImagesEditRefusesAnAgentTargetWithoutMultipartProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared func(*Server)
	}{
		{"the agent has never reported", func(*Server) {}},
		{"the agent reports other features but not multipart_proxy", func(srv *Server) {
			srv.AgentFeatures.Set(agentEditServerID, []string{"runtime_manager", "runtime_ensure", "runtime_model_probe"})
		}},
		{"only another server declares multipart_proxy", func(srv *Server) {
			srv.AgentFeatures.Set("srv-elsewhere", []string{"multipart_proxy"})
		}},
		{"a name that merely resembles it", func(srv *Server) {
			srv.AgentFeatures.Set(agentEditServerID, []string{"multipart", "multipart-proxy", "multipart_proxy_v2"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAIImages})
			tc.declared(srv)

			rec := postImageEdit(t, srv, validEditParts("flux1-dev"))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "images.edit_unavailable")
			if body := rec.Body.String(); !strings.Contains(body, "not available") || !strings.Contains(body, "multipart_proxy") {
				t.Fatalf("body = %s, want a message saying the edit is not available and naming the missing feature", body)
			}
			if prov.proxyCalls != 0 {
				t.Fatalf("ProxyNative calls = %d, want 0 -- an edit must never be sent multipart to an agent that did not declare multipart_proxy", prov.proxyCalls)
			}
			events := srv.Usage.All()
			if len(events) != 1 {
				t.Fatalf("usage events = %d, want exactly one error row", len(events))
			}
			ev := events[0]
			if ev.ErrorCode != "images.edit_unavailable" || ev.HTTPStatus != http.StatusNotFound || ev.BillingUnit != usage.BillingUnitImage {
				t.Fatalf("usage row = %+v, want an images.edit_unavailable 404 billed as image", ev)
			}
			if ev.Host != "" || ev.RouteID != "" || ev.ProviderPath != "" {
				t.Fatalf("usage row = %+v, want Host/RouteID/ProviderPath empty: no upstream call was made", ev)
			}
		})
	}
}

// The positive control of the gate, and its edge: an agent that declares
// multipart_proxy is relayed to exactly as before (the edits path, the client's
// multipart Content-Type), and the registry is a FULL snapshot per sample, so an
// agent that stops declaring it -- a downgrade, an older binary reconnecting --
// is refused again on its very next edit rather than keeping a stale grant.
func TestImagesEditRelaysToAnAgentTargetThatDeclaresMultipartProxyAndStopsWhenItDoesNot(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newAgentEditTestServer(t, prov, "flux1-dev", "flux1-dev") // declares multipart_proxy
	body, ct := buildEditMultipart(t, validEditParts("flux1-dev"))

	rec := postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/images/edits" || prov.gotContentType != ct {
		t.Fatalf("ProxyNative calls/path/Content-Type = %d/%q/%q, want 1, /v1/images/edits and the client's multipart type", prov.proxyCalls, prov.gotPath, prov.gotContentType)
	}

	// The agent reconnects as a build that no longer declares it.
	srv.AgentFeatures.Set(agentEditServerID, []string{"runtime_manager"})

	rec = postEditRaw(t, srv, http.MethodPost, "/v1/images/edits", body, ct, map[string]string{"Authorization": "Bearer dev-secret"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status after the agent stopped declaring the feature = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "images.edit_unavailable")
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want still 1 -- the refused edit must not be relayed", prov.proxyCalls)
	}
}

// The gate is the EDIT's: a generation is JSON, which every agent router serves,
// so an agent without multipart_proxy keeps serving images. A gate that sat on
// "images to an agent" rather than on "edits to an agent" would silently take
// image generation away from every agent until Task B ships.
func TestImagesGenerationToAnAgentTargetIsNotGatedOnMultipartProxy(t *testing.T) {
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newServerAgentImagesSpecTestServer(t, prov, []string{routing.APIFlavorOpenAIImages}) // no feature declared

	rec := postImages(t, srv, `{"model":"flux1-dev","prompt":"a cat"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 || prov.gotPath != "/v1/images/generations" {
		t.Fatalf("ProxyNative calls/path = %d/%q, want 1 and /v1/images/generations", prov.proxyCalls, prov.gotPath)
	}
}

// An EXTERNAL target (an sd-server the operator runs and registered as an
// application, not one an agent launches) has no agent and no router in between,
// so there is nothing to gate: it relays an edit whatever the feature registry
// says. The second case is the one that tells "the target is an agent" apart
// from "the server has an agent": the server DID report features, none of them
// multipart_proxy, and the edit still goes through because the TARGET is not a
// server_agent application.
func TestImagesEditToAnExternalTargetIsNotGatedOnMultipartProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared func(*Server)
	}{
		{"the server never reported features", func(*Server) {}},
		{"the server reports features without multipart_proxy", func(srv *Server) {
			srv.AgentFeatures.Set("srv-sd-turbo-fake", []string{"runtime_manager"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: editsOKBody}
			srv := newImageCapableTestServerWithProvider(t, prov)
			tc.declared(srv)

			rec := postImageEdit(t, srv, validEditParts("sd-turbo"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if prov.proxyCalls != 1 || prov.gotPath != "/v1/images/edits" {
				t.Fatalf("ProxyNative calls/path = %d/%q, want 1 and /v1/images/edits", prov.proxyCalls, prov.gotPath)
			}
		})
	}
}
