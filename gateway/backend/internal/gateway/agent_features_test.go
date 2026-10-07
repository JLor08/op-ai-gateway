// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func agentFeaturesRequest(secret, ifNoneMatch string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/features", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return req
}

// TestAgentFeaturesEndpoint pins the static-list contract (spec §9): an
// authed agent gets back {"features":["runtime_manager"]} plus a stable
// ETag, and a repeat request with that ETag answers 304 with no body.
func TestAgentFeaturesEndpoint(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentFeaturesRequest("features-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// Derived from the registry rather than repeating it: a second hand-kept
	// copy of the feature list is exactly the shape that rots (this
	// repository has the scar -- see agent.capabilitiesTemplate's doc). What
	// this pins is the SHAPE -- the declared list verbatim and nothing else,
	// in particular no in-body etag field -- not which names are in it.
	wantBody, err := json.Marshal(agentFeaturesDTO{Features: gatewayAgentFeatures})
	if err != nil {
		t.Fatalf("marshal expected body: %v", err)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != string(wantBody) {
		t.Fatalf("body = %s, want the static feature list %s, no etag field in-body", body, wantBody)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag header = %q, want a quoted value", etag)
	}

	// A repeat request carrying that ETag is unchanged -> 304, no body, same
	// ETag header.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, agentFeaturesRequest("features-secret", etag))
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Fatalf("304 status = %d, body_bytes = %d", rec2.Code, rec2.Body.Len())
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Fatalf("304 must still carry the ETag; got %q, want %q", got, etag)
	}
}

// TestAgentFeaturesDeclaresTheSdcppCapabilitySource pins the one name the
// agent's sdcpp_capabilities report hangs on. The agent sends that source
// only when this endpoint lists "capability_source_sdcpp"
// (gatewayFeatureCapabilitySourceSdcpp on its side), so the name is compared
// as the literal the agent matches, read off the served body: a renamed
// constant or a dropped list entry both fail here, where the agent would
// otherwise just go quiet.
func TestAgentFeaturesDeclaresTheSdcppCapabilitySource(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentFeaturesRequest("features-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got agentFeaturesDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %s: %v", rec.Body.String(), err)
	}
	if !slices.Contains(got.Features, "capability_source_sdcpp") {
		t.Fatalf("features = %v, want it to declare capability_source_sdcpp", got.Features)
	}
}

// TestAgentFeaturesEndpointAuthAndMethod pins the shared agent-endpoint
// skeleton: Cache-Control:no-store even on a failure, 401 with no bearer
// (never reaching the feature list), and 405 on a non-GET.
func TestAgentFeaturesEndpointAuthAndMethod(t *testing.T) {
	t.Run("no bearer", func(t *testing.T) {
		srv := NewTestServer()
		seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentFeaturesRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store even on an auth failure", got)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		srv := NewTestServer()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentFeaturesRequest("not-a-token", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		srv := NewTestServer()
		seedTestAgentToken(t, srv, "agt_features", "mock-host-qwen", "features-secret")
		req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/features", nil)
		req.Header.Set("Authorization", "Bearer features-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

// TestRuntimeUpstreamPropsFeatureIsAgentDeclaredOnly pins the literal the
// agent declares for its router's props passthrough (its features.go) and
// that the name stays off the gateway's own list: it states a fact about the
// agent alone, and the gateway only reads it off the agent's declared set.
func TestRuntimeUpstreamPropsFeatureIsAgentDeclaredOnly(t *testing.T) {
	if RuntimeUpstreamPropsFeature != "runtime_upstream_props" {
		t.Fatalf("RuntimeUpstreamPropsFeature = %q, want the agent's literal %q", RuntimeUpstreamPropsFeature, "runtime_upstream_props")
	}
	if slices.Contains(gatewayAgentFeatures, RuntimeUpstreamPropsFeature) {
		t.Fatalf("gatewayAgentFeatures = %v, want it without the agent-declared %q", gatewayAgentFeatures, RuntimeUpstreamPropsFeature)
	}
}
