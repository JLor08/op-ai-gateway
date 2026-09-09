// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mappingCapabilityWire is ModelMappingDTO's capability half as the PORTAL
// actually receives it -- named by JSON tag, so a renamed or forgotten tag
// fails here rather than silently handing the form an undefined field. The
// service-level tests in internal/portal cannot catch that: they read Go
// fields.
type mappingCapabilityWire struct {
	Capability string `json:"capability"`
	Verdict    string `json:"verdict"`
	Source     string `json:"source"`
	CheckedAt  string `json:"checked_at"`
}

type mappingWire struct {
	ID            string                  `json:"id"`
	ContextSize   int                     `json:"context_size"`
	IsMtp         bool                    `json:"is_mtp"`
	VisionCapable bool                    `json:"vision_capable"`
	Capabilities  []mappingCapabilityWire `json:"capabilities"`
}

// createTestMappingWire POSTs one mapping under appID and returns it as the
// portal sees it.
func createTestMappingWire(t *testing.T, srv *Server, appID, body string) mappingWire {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/applications/"+appID+"/mappings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create mapping status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out mappingWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal mapping: %v", err)
	}
	return out
}

// patchMapping PATCHes one mapping and returns the recorder, so a caller can
// assert on either a success body or an error code.
func patchMapping(t *testing.T, srv *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/mappings/"+id, body))
	return rec
}

// TestPortalMappingCapabilityResetRoundTrip walks the operator's whole way
// back to UNKNOWN over real HTTP: set a verdict, see it published as a
// capability ROW (not just as the folded boolean), reset it, and see the row
// gone from the very response the form re-seeds from.
//
// The wire shape is the point. The DTO's `capabilities` array is what lets the
// form tell a verdict of "no" from no row at all, and `reset_capabilities` is
// what carries the third state back -- both by JSON name, on the existing
// PATCH, with no new endpoint and so no new OpenAPI path.
func TestPortalMappingCapabilityResetRoundTrip(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8031,"scheme":"https"}`)
	created := createTestMappingWire(t, srv, appID, `{"gateway_model_name":"cap-reset","app_model_name":"cap-reset-up","context_size":4096,"vision_capable":true}`)
	if !created.VisionCapable {
		t.Fatalf("created vision_capable = false, want true")
	}
	if len(created.Capabilities) != 1 || created.Capabilities[0].Capability != "vision" {
		t.Fatalf("created capabilities = %+v, want exactly the one \"vision\" row", created.Capabilities)
	}
	if got := created.Capabilities[0]; got.Verdict != "yes" || got.Source != "manual" {
		t.Fatalf("created vision row = %+v, want yes/manual", got)
	}

	// The reset, on the SAME request that carries the booleans.
	rec := patchMapping(t, srv, created.ID, `{"context_size":8192,"reset_capabilities":["vision"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var reset mappingWire
	if err := json.Unmarshal(rec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("unmarshal reset: %v", err)
	}
	if len(reset.Capabilities) != 0 {
		t.Fatalf("capabilities after the reset = %+v, want NONE -- absence is unknown", reset.Capabilities)
	}
	if reset.VisionCapable {
		t.Fatalf("vision_capable after the reset = true, want false")
	}
	if reset.ContextSize != 8192 {
		t.Fatalf("context_size = %d, want 8192 -- the rest of the update must still apply", reset.ContextSize)
	}
	// `[]`, never `null`: the frontend types `capabilities` as a required
	// array and must not need a nil branch for "nothing determined".
	if body := rec.Body.String(); !strings.Contains(body, `"capabilities":[]`) {
		t.Fatalf("reset body = %s, want \"capabilities\":[] (an empty ARRAY, never null)", body)
	}
}

// TestPortalMappingCapabilityResetRejectionsReturn400 guards that the two new
// sentinels reach the HTTP layer as 400s with their own codes rather than a
// default 500 -- the same guard TestPortalMappingCreateNegativeMetricReturns400
// provides for ErrMappingMetricInvalid, and equally easy to lose: a sentinel
// missing from portalMappingErrRows compiles and passes every service test.
func TestPortalMappingCapabilityResetRejectionsReturn400(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8032,"scheme":"https"}`)
	created := createTestMappingWire(t, srv, appID, `{"gateway_model_name":"cap-reject","app_model_name":"cap-reject-up"}`)

	for _, tc := range []struct {
		name, body, wantCode string
	}{
		{
			name:     "an empty capability name",
			body:     `{"reset_capabilities":["  "]}`,
			wantCode: "mapping.capability_name_required",
		},
		{
			name:     "reset and set the same capability",
			body:     `{"vision_capable":true,"reset_capabilities":["vision"]}`,
			wantCode: "mapping.capability_conflict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchMapping(t, srv, created.ID, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal error body: %v", err)
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q (body = %s)", body.Error.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}
