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

// TestPortalApplicationLiveTimingsRefusalReachesTheWire pins the WIRE
// contract (status + code + the offending type in the MESSAGE) of BOTH
// live-timings refusal sentinels, across the four request shapes that can
// produce one.
//
// The two statuses are the point of the test, not an incidental detail:
//
//   - 400 application.responses_live_timings_unsupported when the REQUEST
//     supplied the incapable type -- a create (whose "type" is always the
//     request's own) and a PATCH that sends "type" alongside the flag. Such a
//     body is self-contradictory and needs no stored row to judge it.
//   - 409 application.responses_live_timings_conflict when the PATCH asserts
//     the flag and does NOT send "type" -- the request is well-formed and
//     collides with the application's own stored state. That is the
//     distinction ErrServerManagedRuntimeOnly already records in the same
//     table ("the request shape is fine, it is simply refused given the
//     server's current state", portal_application_endpoints.go:203-206).
//
// The likely implementation slip is ONE sentinel returned for both PATCH
// shapes -- a version that passes every service-level errors.Is check written
// loosely. Each exact-status assertion below is what catches it: collapsing to
// the 400 breaks the 409 subtest, collapsing to the 409 breaks all three 400
// ones. A separate subtest asserting only that the two statuses DIFFER was
// tried here and removed: it could not fail unless one of those subtests
// already had, and "differ" is also satisfied by a 500, so it restated the
// invariant without testing it.
//
// The message assertion is not decoration either: the whole reason this
// request is refused instead of quietly stored as false is that the caller
// asked for something this row cannot do, and a refusal that does not say
// WHICH kind cannot do it leaves them no better off than the silent rewrite
// would have.
func TestPortalApplicationLiveTimingsRefusalReachesTheWire(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	serverID := newProxyExclusionTestServer(t, srv, "live-timings.example.test")

	t.Run("create true on an incapable kind is 400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		body := `{"type":"litellm","port":8100,"scheme":"http","responses_live_timings_enabled":true}`
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_unsupported" {
			t.Fatalf("error code = %q, want application.responses_live_timings_unsupported, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "litellm") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})

	t.Run("retype plus true on the new incapable kind is 400", func(t *testing.T) {
		appID := createTestApplication(t, srv, serverID, `{"type":"llama_cpp","port":8101,"scheme":"http"}`)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+appID,
			`{"type":"litellm","responses_live_timings_enabled":true}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_unsupported" {
			t.Fatalf("error code = %q, want application.responses_live_timings_unsupported, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "litellm") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})

	// The shape the portal itself sends. ApplicationSection.tsx's buildBody()
	// restates "type" on EVERY save, so from part 2 onward this -- not the
	// retype above -- is the body an operator actually produces by ticking the
	// box on an incapable application. It is an ordinary HTTP request, and it
	// is the only wire shape that separates "the request supplied the type"
	// from "the request CHANGED the type": a service reading the branch off
	// *req.Type != app.Type answers 409 here while every other subtest in this
	// file stays green.
	t.Run("restating the stored incapable type alongside true is 400", func(t *testing.T) {
		appID := createTestApplication(t, srv, serverID, `{"type":"ollama","port":8105,"scheme":"http"}`)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+appID,
			`{"type":"ollama","responses_live_timings_enabled":true}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (the body SUPPLIED the type it is judged against, unchanged or not), body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_unsupported" {
			t.Fatalf("error code = %q, want application.responses_live_timings_unsupported, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "ollama") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})

	t.Run("true against a stored incapable kind, no type sent, is 409", func(t *testing.T) {
		appID := createTestApplication(t, srv, serverID, `{"type":"ollama","port":8102,"scheme":"http"}`)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+appID,
			`{"responses_live_timings_enabled":true}`))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
		}
		if code := errorBodyOf(t, rec); code != "application.responses_live_timings_conflict" {
			t.Fatalf("error code = %q, want application.responses_live_timings_conflict, body = %s", code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "ollama") {
			t.Fatalf("the refusal does not name the offending kind: %s", rec.Body.String())
		}
	})
}

// TestPortalApplicationLiveTimingsJSONKeyReachesTheWire pins the RESPONSE-side
// JSON tag of ApplicationDTO.ResponsesLiveTimingsEnabled by decoding it out of
// real success bodies, by name.
//
// The service-level DTO test reads the Go FIELD through applicationDTO, so it
// cannot see the tag at all: a typo in `json:"responses_live_timings_enabled"`
// leaves every Go test green and every status 2xx, and surfaces only when the
// frontend reads the key and finds undefined. Both write responses are checked
// -- the 201 from a create and the 200 from a PATCH -- because each marshals
// the DTO at its own call site, and both values are checked, because a tag
// typo and a dropped mapper line look identical if you only ever assert true.
func TestPortalApplicationLiveTimingsJSONKeyReachesTheWire(t *testing.T) {
	srv := NewTestServerWithGroups([]string{"gateway:use", "admin"})
	serverID := newProxyExclusionTestServer(t, srv, "live-timings-json.example.test")

	decode := func(t *testing.T, rec *httptest.ResponseRecorder) bool {
		t.Helper()
		var body struct {
			Value *bool `json:"responses_live_timings_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal application: %v, body = %s", err, rec.Body.String())
		}
		if body.Value == nil {
			t.Fatalf("the success body carries no \"responses_live_timings_enabled\" key (check the json tag on ApplicationDTO): %s", rec.Body.String())
		}
		return *body.Value
	}

	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, newJSONRequest(http.MethodPost, "/api/portal/servers/"+serverID+"/applications",
		`{"type":"llama_cpp","port":8110,"scheme":"http"}`))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	if !decode(t, createRec) {
		t.Fatalf("created llama_cpp application reports the key as false, want the kind-dependent true: %s", createRec.Body.String())
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal id: %v", err)
	}
	patchRec := httptest.NewRecorder()
	srv.ServeHTTP(patchRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+created.ID,
		`{"responses_live_timings_enabled":false}`))
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchRec.Code, patchRec.Body.String())
	}
	if decode(t, patchRec) {
		t.Fatalf("the PATCH response still reports true after an explicit false: %s", patchRec.Body.String())
	}
}
