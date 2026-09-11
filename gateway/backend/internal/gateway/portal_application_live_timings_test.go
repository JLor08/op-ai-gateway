// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPortalApplicationLiveTimingsRefusalReachesTheWire pins the WIRE
// contract (status + code + the offending type in the MESSAGE) of BOTH
// live-timings refusal sentinels, across the three request shapes that can
// produce one, plus a fourth subtest pinning the two PATCH shapes apart.
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
// The last subtest compares the two PATCH statuses directly, because the
// likely implementation slip is ONE sentinel returned for both shapes: that
// version passes every service-level errors.Is check written loosely, and it
// passes two of the three shape subtests here as well.
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

	// The anti-slip pin. One sentinel returned for both PATCH shapes gives
	// them the SAME status, and the two subtests above would then disagree
	// about which one is wrong; this one says plainly what the invariant is.
	t.Run("the two PATCH shapes do not share a status", func(t *testing.T) {
		retypeID := createTestApplication(t, srv, serverID, `{"type":"llama_cpp","port":8103,"scheme":"http"}`)
		retypeRec := httptest.NewRecorder()
		srv.ServeHTTP(retypeRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+retypeID,
			`{"type":"litellm","responses_live_timings_enabled":true}`))

		storedID := createTestApplication(t, srv, serverID, `{"type":"ollama","port":8104,"scheme":"http"}`)
		storedRec := httptest.NewRecorder()
		srv.ServeHTTP(storedRec, newJSONRequest(http.MethodPatch, "/api/portal/applications/"+storedID,
			`{"responses_live_timings_enabled":true}`))

		if retypeRec.Code == storedRec.Code {
			t.Fatalf("both refusal shapes answered %d: the request-supplied type (400) and the stored type (409) must not collapse into one status -- check that the service returns TWO sentinels and that both have their own errRow",
				retypeRec.Code)
		}
	})
}
