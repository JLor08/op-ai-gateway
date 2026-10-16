// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"
)

// The in-flight record carries the request's billing unit so the running-
// connections panel can tell a non-token request (speech, images) apart from a
// token request that simply has not produced tokens yet. The completed usage row
// already carries the unit (usageMeta.BillingUnit); these tests pin the LIVE
// record to the same billingUnitFor chokepoint at every registration site.

// heldUpstream is a stub upstream that holds each request open until release is
// closed (or the request's own context ends), closing started the first time a
// request arrives. The gateway registers its in-flight record BEFORE it calls
// the upstream, so a request parked here is observable in srv.Active. All
// synchronisation is via channels -- no sleeps.
type heldUpstream struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

// newHeldUpstream starts a stub that, once released, answers with body under
// contentType (plus the extra headers).
func newHeldUpstream(t *testing.T, contentType string, extra map[string]string, body []byte) (*httptest.Server, *heldUpstream) {
	t.Helper()
	h := &heldUpstream{started: make(chan struct{}), release: make(chan struct{})}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		h.once.Do(func() { close(h.started) })
		select {
		case <-h.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", contentType)
		for k, v := range extra {
			w.Header().Set(k, v)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(up.Close)
	return up, h
}

// getActiveWithBearer reads /api/portal/usage/active as the dev bearer token,
// the way TestActiveRegistryTracksLiveRequest does, returning the typed DTOs
// and the raw body (for wire-shape assertions).
func getActiveWithBearer(t *testing.T, srv *Server) ([]activeRequestDTO, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/active", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("active endpoint status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []activeRequestDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	return body.Data, rec.Body.Bytes()
}

// observeHeld runs fire (which must make exactly one request through srv) in a
// goroutine, waits until the upstream is holding it, and returns the in-flight
// registry row plus the /usage/active DTO and wire body for it. The upstream is
// released from a defer so a failing assertion cannot leave fire blocked, and
// the completion path is asserted too: the row must be gone afterwards.
func observeHeld(t *testing.T, srv *Server, h *heldUpstream, fire func()) (ActiveRequest, activeRequestDTO, []byte) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fire()
	}()
	defer func() {
		close(h.release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("request did not finish after release")
			return
		}
		if snap := srv.Active.Snapshot(); len(snap) != 0 {
			t.Errorf("post-completion snapshot = %d rows, want 0 (%#v)", len(snap), snap)
		}
	}()

	select {
	case <-h.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream never received the request")
	}
	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in-flight snapshot = %d rows, want 1 (%#v)", len(snap), snap)
	}
	dtos, raw := getActiveWithBearer(t, srv)
	if len(dtos) != 1 || dtos[0].ID != snap[0].ID {
		t.Fatalf("active endpoint during flight = %#v, want the one row %q", dtos, snap[0].ID)
	}
	return snap[0], dtos[0], raw
}

// A speech request in flight is a non-token request: its live record and its
// DTO both say audio_second.
func TestActiveRequestCarriesAudioSecondForSpeech(t *testing.T) {
	up, held := newHeldUpstream(t, "audio/wav", map[string]string{"X-AudioCPP-Audio-Duration-Ms": "1500"}, speechTestWAV())
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	row, dto, _ := observeHeld(t, srv, held, func() {
		postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","voice":"alba"}`)
	})

	if row.APIFlavor != apiFlavorSpeech {
		t.Fatalf("in-flight APIFlavor = %q, want %q (the test must exercise the speech path)", row.APIFlavor, apiFlavorSpeech)
	}
	if row.BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("in-flight BillingUnit = %q, want %q", row.BillingUnit, usage.BillingUnitAudioSecond)
	}
	if dto.BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("DTO BillingUnit = %q, want %q", dto.BillingUnit, usage.BillingUnitAudioSecond)
	}
}

// An images request in flight says image. This is the EXISTING gap the field
// closes: before it, the live panel showed "0" for an images request too.
func TestActiveRequestCarriesImageForImages(t *testing.T) {
	up, held := newHeldUpstream(t, "application/json", nil, []byte(`{"created":1,"output_format":"png","data":[{"b64_json":"AA=="}]}`))
	srv := newImageCapableTestServer(t, up.URL)

	row, dto, _ := observeHeld(t, srv, held, func() {
		postImages(t, srv, `{"model":"sd-turbo","prompt":"a cat"}`)
	})

	if row.APIFlavor != apiFlavorImages {
		t.Fatalf("in-flight APIFlavor = %q, want %q (the test must exercise the images path)", row.APIFlavor, apiFlavorImages)
	}
	if row.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("in-flight BillingUnit = %q, want %q", row.BillingUnit, usage.BillingUnitImage)
	}
	if dto.BillingUnit != usage.BillingUnitImage {
		t.Fatalf("DTO BillingUnit = %q, want %q", dto.BillingUnit, usage.BillingUnitImage)
	}
}

// A chat request in flight is token-metered: the unit is the empty string, on
// BOTH chat registration sites (non-streaming Complete and the streaming path,
// which build their own ActiveRequest literals). The DTO key is present (not
// omitted) with an empty value, so "token request" is explicit on the wire.
func TestActiveRequestCarriesNoBillingUnitForChat(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"non-streaming", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`},
		{"streaming", `{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := newBlockingProvider()
			srv := newStreamTestServerWithProvider(prov)

			done := make(chan struct{})
			go func() {
				defer close(done)
				req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(tc.body))
				req.Header.Set("Authorization", "Bearer dev-secret")
				srv.ServeHTTP(httptest.NewRecorder(), req)
			}()
			defer func() {
				close(prov.release)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("request did not finish after release")
				}
			}()

			select {
			case <-prov.started:
			case <-time.After(2 * time.Second):
				t.Fatal("provider was never entered")
			}
			snap := srv.Active.Snapshot()
			if len(snap) != 1 {
				t.Fatalf("in-flight snapshot = %d rows, want 1 (%#v)", len(snap), snap)
			}
			if snap[0].BillingUnit != usage.BillingUnitTokens {
				t.Fatalf("in-flight BillingUnit = %q, want %q (token-metered)", snap[0].BillingUnit, usage.BillingUnitTokens)
			}
			dtos, raw := getActiveWithBearer(t, srv)
			if len(dtos) != 1 || dtos[0].BillingUnit != usage.BillingUnitTokens {
				t.Fatalf("DTO = %#v, want one row with an empty BillingUnit", dtos)
			}
			var generic struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(raw, &generic); err != nil || len(generic.Data) != 1 {
				t.Fatalf("unmarshal generic: %v (%s)", err, raw)
			}
			if unit, present := generic.Data[0]["billing_unit"]; !present || unit != "" {
				t.Fatalf("wire billing_unit = %#v (present=%v), want the key present with an empty string: %s", unit, present, raw)
			}
		})
	}
}

// The struct-to-DTO mapping carries the field verbatim, independent of how any
// path registered the row.
func TestPortalUsageActiveMapsBillingUnit(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	srv.Active.Add(ActiveRequest{ID: "req_speech", UserID: "usr_owner", APIFlavor: apiFlavorSpeech, BillingUnit: usage.BillingUnitAudioSecond, StartedAt: base})
	srv.Active.Add(ActiveRequest{ID: "req_chat", UserID: "usr_owner", APIFlavor: "openai", StartedAt: base.Add(time.Second)})

	rows := getActive(t, srv, loginCookie(t, srv, "owner@example.test", "password-1"), "")
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want 2", rows)
	}
	if rows[0].ID != "req_speech" || rows[0].BillingUnit != usage.BillingUnitAudioSecond {
		t.Fatalf("speech row = %#v, want billing_unit %q", rows[0], usage.BillingUnitAudioSecond)
	}
	if rows[1].ID != "req_chat" || rows[1].BillingUnit != "" {
		t.Fatalf("chat row = %#v, want an empty billing_unit", rows[1])
	}
}
