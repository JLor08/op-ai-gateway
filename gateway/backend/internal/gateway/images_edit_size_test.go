// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/usage"
	"testing"
)

// bigEditParts is a well-formed edit whose extra `reference` part carries size
// bytes. (An extra part, not a mask: nothing in it is decoded.)
func bigEditParts(model string, size int) []editPart {
	return append(validEditParts(model), editPart{name: "reference", filename: "ref.png", contentType: "image/png", body: bytes.Repeat([]byte{0xAB}, size)})
}

// The multipart read cap is the server's setting, not a constant: ServerDeps
// carries it (config OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES), a value <= 0 falls
// back to the default, and the default is the agent router's own 32 MiB.
func TestImageEditBodyCapIsASetting(t *testing.T) {
	if defaultMaxImageEditBodyBytes != 32<<20 {
		t.Fatalf("defaultMaxImageEditBodyBytes = %d, want 32 MiB (the agent router's own cap)", defaultMaxImageEditBodyBytes)
	}
	cases := []struct {
		name string
		deps int
		want int64
	}{
		{"unset uses the default", 0, defaultMaxImageEditBodyBytes},
		{"negative uses the default", -5, defaultMaxImageEditBodyBytes},
		{"configured value is honoured", 4096, 4096},
		{"a value above the default is honoured", 64 << 20, 64 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(ServerDeps{MaxImageEditBodyBytes: tc.deps})
			if srv.maxImageEditBodyBytes != tc.want {
				t.Fatalf("maxImageEditBodyBytes = %d, want %d", srv.maxImageEditBodyBytes, tc.want)
			}
		})
	}
}

// The handler enforces whatever the setting says: a body under a small limit is
// served, one over it is a 413, and the refusal comes before any upstream call.
func TestImagesEditHonoursTheConfiguredBodyCap(t *testing.T) {
	const limit = 64 << 10
	prov := &recordingProxyProvider{respBody: editsOKBody}
	srv := newImageCapableTestServerWithProvider(t, prov)
	srv.maxImageEditBodyBytes = limit

	small := postImageEdit(t, srv, bigEditParts("sd-turbo", limit/4))
	if small.Code != http.StatusOK {
		t.Fatalf("a body under the limit: status = %d, want 200; body = %s", small.Code, small.Body.String())
	}

	large := postImageEdit(t, srv, bigEditParts("sd-turbo", limit))
	if large.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the limit: status = %d, want 413; body = %s", large.Code, large.Body.String())
	}
	requireErrorCode(t, large.Body.String(), "request.body_too_large")
	if prov.proxyCalls != 1 {
		t.Fatalf("ProxyNative calls = %d, want 1 (only the body under the limit)", prov.proxyCalls)
	}
}

// The cap on what the client SENDS is not the cap on what the upstream will
// ACCEPT: an agent's router refuses a request past its own 32 MiB, and a rebuild
// (the mask conversion, the model rewrite) can make a body larger than the
// client's. So a body handed to an agent-launched target is checked against the
// agent's limit, and refused here as a 413 of the gateway's own -- recorded, and
// never sent -- instead of being relayed to come back as the agent's error. It
// holds for a verbatim relay and a rebuilt one alike, and an EXTERNAL target,
// which has no such router, is not subject to it.
func TestImagesEditRefusesABodyTheAgentRouterWouldRefuse(t *testing.T) {
	over := agentRouterMaxBodyBytes + (1 << 20)
	t.Run("verbatim to an agent", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newAgentEditTestServer(t, prov, "flux1-dev", "flux1-dev") // names equal: nothing to rewrite
		srv.maxImageEditBodyBytes = 64 << 20

		rec := postImageEdit(t, srv, bigEditParts("flux1-dev", over))

		requireEditTooLargeForTheAgent(t, srv, prov, rec)
	})
	t.Run("rebuilt for an agent", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		// The mapped name is longer than the one the client sends, so the rebuilt
		// body is a little LARGER than the client's.
		srv := newAgentEditTestServer(t, prov, "flux", "flux1-dev-q8-long-name")
		srv.maxImageEditBodyBytes = 64 << 20
		// Four bytes under the agent's limit as the client sends it (the boundary
		// length is constant, so the body is exactly its fixed part plus the
		// reference's size) -- fine verbatim, over once the model is rewritten.
		fixed, _ := buildEditMultipart(t, bigEditParts("flux", 0))

		rec := postImageEdit(t, srv, bigEditParts("flux", agentRouterMaxBodyBytes-len(fixed)-4))

		requireEditTooLargeForTheAgent(t, srv, prov, rec)
	})
	t.Run("an external target is not subject to the agent's limit", func(t *testing.T) {
		prov := &recordingProxyProvider{respBody: editsOKBody}
		srv := newImageCapableTestServerWithProvider(t, prov)
		srv.maxImageEditBodyBytes = 64 << 20

		rec := postImageEdit(t, srv, bigEditParts("sd-turbo", over))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if prov.proxyCalls != 1 {
			t.Fatalf("ProxyNative calls = %d, want 1", prov.proxyCalls)
		}
	})
}

func requireEditTooLargeForTheAgent(t *testing.T, srv *Server, prov *recordingProxyProvider, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %.200s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "request.body_too_large")
	if prov.proxyCalls != 0 {
		t.Fatalf("ProxyNative calls = %d, want 0: a body the agent's router would refuse must not be sent", prov.proxyCalls)
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want one error row", len(events))
	}
	ev := events[0]
	if ev.ErrorCode != "request.body_too_large" || ev.HTTPStatus != http.StatusRequestEntityTooLarge || ev.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("usage row = %+v, want a request.body_too_large 413 billed as image", ev)
	}
	if ev.Host != "" || ev.ProviderPath != "" {
		t.Fatalf("usage row = %+v, want Host/ProviderPath empty: no upstream call was made", ev)
	}
}
