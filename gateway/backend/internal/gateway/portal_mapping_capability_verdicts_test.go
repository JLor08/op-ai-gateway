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

// patchMappingWire PATCHes one mapping, requires a 200 and returns the mapping
// as the portal sees it.
func patchMappingWire(t *testing.T, srv *Server, id, body string) mappingWire {
	t.Helper()
	rec := patchMapping(t, srv, id, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out mappingWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal mapping: %v", err)
	}
	return out
}

// TestPortalMappingCapabilityVerdictRoundTrip walks all three states over real
// HTTP, by JSON name: a mapping with nothing determined, an operator stating
// NO (the transition that used to answer 200 having written nothing), the same
// row moved to yes, and then back to unknown -- each read back out of the very
// response the form re-seeds from.
//
// The wire shape is the point. The DTO's `capabilities` array is what lets the
// form tell a verdict of "no" from no row at all, and `capability_verdicts` is
// what carries all three states back -- both on the existing PATCH, with no
// new endpoint and so no new OpenAPI path.
func TestPortalMappingCapabilityVerdictRoundTrip(t *testing.T) {
	// system-scope: see TestPortalServerAgentTokenGenerateStatusRevoke.
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8031,"scheme":"https"}`)
	created := createTestMappingWire(t, srv, appID, `{"gateway_model_name":"cap-verdict","app_model_name":"cap-verdict-up","context_size":4096}`)
	if len(created.Capabilities) != 0 {
		t.Fatalf("created capabilities = %+v, want none -- this mapping starts UNKNOWN", created.Capabilities)
	}

	// UNKNOWN -> "no", the defect: an ordinary 200 that wrote nothing at all.
	stated := patchMappingWire(t, srv, created.ID, `{"context_size":8192,"capability_verdicts":{"vision":"no"}}`)
	if len(stated.Capabilities) != 1 || stated.Capabilities[0].Capability != "vision" {
		t.Fatalf("capabilities after a stated no = %+v, want exactly the one \"vision\" row", stated.Capabilities)
	}
	if got := stated.Capabilities[0]; got.Verdict != "no" || got.Source != "manual" {
		t.Fatalf("vision row = %+v, want no/manual", got)
	}
	if stated.VisionCapable {
		t.Fatalf("vision_capable for a stated no = true, want false (the FOLD of a \"no\" row is false)")
	}
	if stated.ContextSize != 8192 {
		t.Fatalf("context_size = %d, want 8192 -- the rest of the update must still apply", stated.ContextSize)
	}

	// "no" -> "yes": still a verdict, still a row.
	moved := patchMappingWire(t, srv, created.ID, `{"capability_verdicts":{"vision":"yes"}}`)
	if len(moved.Capabilities) != 1 || moved.Capabilities[0].Verdict != "yes" {
		t.Fatalf("capabilities after no -> yes = %+v, want one vision/yes row", moved.Capabilities)
	}
	if !moved.VisionCapable {
		t.Fatalf("vision_capable after no -> yes = false, want true")
	}

	// "yes" -> unknown: an EMPTY verdict deletes the row.
	rec := patchMapping(t, srv, created.ID, `{"capability_verdicts":{"vision":""}}`)
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
	// `[]`, never `null`: the frontend types `capabilities` as a required
	// array and must not need a nil branch for "nothing determined".
	if body := rec.Body.String(); !strings.Contains(body, `"capabilities":[]`) {
		t.Fatalf("reset body = %s, want \"capabilities\":[] (an empty ARRAY, never null)", body)
	}
}

// TestPortalMappingCreateStatesACapabilityVerdict: the create path carries the
// same field, and the negative verdict it can express is the one
// `vision_capable`/`is_mtp` cannot (a plain bool's unset `false` is
// indistinguishable from a control nobody touched, so only `true` writes).
func TestPortalMappingCreateStatesACapabilityVerdict(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8033,"scheme":"https"}`)
	created := createTestMappingWire(t, srv, appID, `{"gateway_model_name":"cap-create","app_model_name":"cap-create-up","capability_verdicts":{"vision":"no"}}`)
	if len(created.Capabilities) != 1 {
		t.Fatalf("created capabilities = %+v, want exactly the one stated row", created.Capabilities)
	}
	if got := created.Capabilities[0]; got.Capability != "vision" || got.Verdict != "no" || got.Source != "manual" {
		t.Fatalf("created vision row = %+v, want vision/no/manual", got)
	}
	if created.VisionCapable {
		t.Fatalf("created vision_capable = true, want false")
	}
}

// TestPortalMappingCapabilityVerdictRejectionsReturn400 guards that the four
// sentinels reach the HTTP layer as 400s with their own codes rather than a
// default 500 -- the same guard TestPortalMappingCreateNegativeMetricReturns400
// provides for ErrMappingMetricInvalid, and equally easy to lose: a sentinel
// missing from portalMappingErrRows compiles and passes every service test.
func TestPortalMappingCapabilityVerdictRejectionsReturn400(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use", "admin", "system"})
	appID := createTestApplication(t, srv, "mock-host-qwen", `{"type":"vllm","port":8032,"scheme":"https"}`)
	created := createTestMappingWire(t, srv, appID, `{"gateway_model_name":"cap-reject","app_model_name":"cap-reject-up"}`)

	for _, tc := range []struct {
		name, body, wantCode string
	}{
		{
			name:     "an empty capability name",
			body:     `{"capability_verdicts":{"  ":""}}`,
			wantCode: "mapping.capability_name_required",
		},
		{
			name:     "a verdict outside yes/no/empty",
			body:     `{"capability_verdicts":{"vision":"maybe"}}`,
			wantCode: "mapping.capability_verdict_invalid",
		},
		{
			name:     "state a capability and send its legacy boolean",
			body:     `{"vision_capable":true,"capability_verdicts":{"vision":"no"}}`,
			wantCode: "mapping.capability_conflict",
		},
		{
			name:     "two keys that trim to the same capability",
			body:     `{"capability_verdicts":{"vision":"no"," vision":"yes"}}`,
			wantCode: "mapping.capability_duplicate",
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
