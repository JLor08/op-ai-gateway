// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// TestEnsureFreshPassthroughWhenNotStale proves a token with a comfortable
// expiry is returned unchanged and no token endpoint is called.
func TestEnsureFreshPassthroughWhenNotStale(t *testing.T) {
	ep, calls := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) {
		writeJSON(w, 200, `{"access_token":"should-not-be-used","expires_in":3600}`)
	})
	in := TokenSet{
		AccessToken: "live-access", RefreshToken: "live-refresh",
		ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-1", PlanType: "max",
	}
	got, changed, err := EnsureFresh(context.Background(), http.DefaultClient, ep, in, time.Minute)
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want false for a non-stale token")
	}
	if got != in {
		t.Fatalf("token mutated: got %+v, want %+v", got, in)
	}
	if *calls != 0 {
		t.Fatalf("token endpoint called %d times, want 0 (no refresh)", *calls)
	}
}

// TestEnsureFreshUnknownExpiryNeverRefreshes proves a zero (unknown) expiry is
// treated as not-stale: refreshing blindly would burn the refresh token.
func TestEnsureFreshUnknownExpiryNeverRefreshes(t *testing.T) {
	ep, calls := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) {
		writeJSON(w, 200, `{"access_token":"x","expires_in":3600}`)
	})
	in := TokenSet{AccessToken: "a", RefreshToken: "r"} // zero ExpiresAt
	got, changed, err := EnsureFresh(context.Background(), http.DefaultClient, ep, in, time.Hour)
	if err != nil || changed || got != in || *calls != 0 {
		t.Fatalf("EnsureFresh(unknown expiry) = (%+v, %v, %v), calls=%d; want passthrough, no call", got, changed, err, *calls)
	}
}

// TestEnsureFreshRefreshesAndCarriesIdentityForward proves a near-expiry token is
// refreshed, the new access/refresh/expiry are adopted, and AccountID/PlanType
// are carried forward when the refresh response omits them.
func TestEnsureFreshRefreshesAndCarriesIdentityForward(t *testing.T) {
	var sentRefresh string
	ep, calls := anthropicStub(t, func(w http.ResponseWriter, body map[string]string) {
		sentRefresh = body["refresh_token"]
		// A rotating refresh response that carries NO account_id / plan_type.
		writeJSON(w, 200, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	})
	in := TokenSet{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(10 * time.Second), // within the buffer => stale
		AccountID: "acct-7", PlanType: "pro", Scope: "user:inference",
	}
	got, changed, err := EnsureFresh(context.Background(), http.DefaultClient, ep, in, time.Minute)
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true after a refresh")
	}
	if *calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", *calls)
	}
	if sentRefresh != "stale-refresh" {
		t.Fatalf("refresh_token sent = %q, want the stored stale-refresh", sentRefresh)
	}
	if got.AccessToken != "fresh-access" || got.RefreshToken != "fresh-refresh" {
		t.Fatalf("refreshed tokens not adopted: %+v", got)
	}
	if got.AccountID != "acct-7" || got.PlanType != "pro" {
		t.Fatalf("identity not carried forward: AccountID=%q PlanType=%q, want acct-7/pro", got.AccountID, got.PlanType)
	}
	if !got.ExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("ExpiresAt not refreshed forward: %v", got.ExpiresAt)
	}
}

// TestEnsureFreshRejectionSurfacesAuthRejected proves a dead refresh token (401 /
// invalid_grant) returns an error matching ErrAuthRejected and no token.
func TestEnsureFreshRejectionSurfacesAuthRejected(t *testing.T) {
	ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) {
		writeJSON(w, 401, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	})
	in := TokenSet{
		AccessToken: "stale", RefreshToken: "dead",
		ExpiresAt: time.Now().Add(-time.Second), // already expired
	}
	got, changed, err := EnsureFresh(context.Background(), http.DefaultClient, ep, in, time.Minute)
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err = %v, want ErrAuthRejected", err)
	}
	if changed || got != (TokenSet{}) {
		t.Fatalf("on rejection want (zero, false); got (%+v, %v)", got, changed)
	}
}

// TestEnsureFreshOpenAIPassthroughWhenNotStale proves the OpenAI variant also
// hands back a comfortable-expiry token untouched and calls no token endpoint.
func TestEnsureFreshOpenAIPassthroughWhenNotStale(t *testing.T) {
	var calls int
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		calls++
		writeJSON(w, 200, `{"access_token":"should-not-be-used","expires_in":3600}`)
	})
	in := TokenSet{
		AccessToken: "live-access", RefreshToken: "live-refresh",
		ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-1", PlanType: "plus",
	}
	got, changed, err := EnsureFreshOpenAI(context.Background(), http.DefaultClient, ep, in, time.Minute)
	if err != nil {
		t.Fatalf("EnsureFreshOpenAI: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want false for a non-stale token")
	}
	if got != in {
		t.Fatalf("token mutated: got %+v, want %+v", got, in)
	}
	if calls != 0 {
		t.Fatalf("token endpoint called %d times, want 0 (no refresh)", calls)
	}
}

// TestEnsureFreshOpenAIRefreshesAndCarriesIdentityForward proves a near-expiry
// OpenAI token is refreshed via a FORM-ENCODED exchange (not JSON — the Anthropic
// shape), the new tokens adopted, and AccountID/PlanType carried forward when the
// refresh response omits an id_token.
func TestEnsureFreshOpenAIRefreshesAndCarriesIdentityForward(t *testing.T) {
	var sentRefresh, sentGrant string
	var calls int
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, form url.Values) {
		calls++
		sentRefresh = form.Get("refresh_token")
		sentGrant = form.Get("grant_type")
		// A rotating response that carries NO id_token / access-JWT claims.
		writeJSON(w, 200, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	})
	in := TokenSet{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(10 * time.Second), // within the buffer => stale
		AccountID: "acct-9", PlanType: "pro", Scope: "openid profile",
	}
	got, changed, err := EnsureFreshOpenAI(context.Background(), http.DefaultClient, ep, in, time.Minute)
	if err != nil {
		t.Fatalf("EnsureFreshOpenAI: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true after a refresh")
	}
	if calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", calls)
	}
	if sentGrant != "refresh_token" || sentRefresh != "stale-refresh" {
		t.Fatalf("form = grant_type=%q refresh_token=%q, want refresh_token/stale-refresh", sentGrant, sentRefresh)
	}
	if got.AccessToken != "fresh-access" || got.RefreshToken != "fresh-refresh" {
		t.Fatalf("refreshed tokens not adopted: %+v", got)
	}
	if got.AccountID != "acct-9" || got.PlanType != "pro" {
		t.Fatalf("identity not carried forward: AccountID=%q PlanType=%q, want acct-9/pro", got.AccountID, got.PlanType)
	}
}

// TestEnsureFreshOpenAIRejectionSurfacesAuthRejected proves a dead OpenAI refresh
// token (401 / invalid_grant) returns an error matching ErrAuthRejected and no
// token, so the dispatch caller marks the account needs_reconnect.
func TestEnsureFreshOpenAIRejectionSurfacesAuthRejected(t *testing.T) {
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		writeJSON(w, 401, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	})
	in := TokenSet{
		AccessToken: "stale", RefreshToken: "dead",
		ExpiresAt: time.Now().Add(-time.Second), // already expired
	}
	got, changed, err := EnsureFreshOpenAI(context.Background(), http.DefaultClient, ep, in, time.Minute)
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err = %v, want ErrAuthRejected", err)
	}
	if changed || got != (TokenSet{}) {
		t.Fatalf("on rejection want (zero, false); got (%+v, %v)", got, changed)
	}
}
