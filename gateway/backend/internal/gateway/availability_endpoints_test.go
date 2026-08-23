// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func TestResolveAvailabilityWindow(t *testing.T) {
	cases := []struct {
		token string
		want  time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"1h", time.Hour},
		{"1d", 24 * time.Hour},
		{"1y", 31536000 * time.Second},
		{"", defaultAvailabilityWindow},
		{"zzz", defaultAvailabilityWindow},
	}
	for _, c := range cases {
		if got := resolveAvailabilityWindow(c.token); got != c.want {
			t.Fatalf("resolveAvailabilityWindow(%q) = %v, want %v", c.token, got, c.want)
		}
	}
}

// seedAvailability inserts distinct-state availability samples anchored near now
// (the perf test server uses the default real clock) so the reduction keeps them.
func seedAvailability(t *testing.T, routeStore *routing.MemoryStore) {
	t.Helper()
	base := time.Now().UTC().Add(-3 * time.Minute)
	samples := []routing.ServerAvailabilitySample{
		{ServerID: perfServerID, ReportedAt: base, Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: true},
		{ServerID: perfServerID, ReportedAt: base.Add(time.Minute), Health: routing.HealthUnhealthy, ReachableCount: 0, ActiveCount: 1, AgentReporting: true},
		{ServerID: perfServerID, ReportedAt: base.Add(2 * time.Minute), Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: false},
	}
	for i, s := range samples {
		if err := routeStore.InsertServerAvailabilitySample(context.Background(), s); err != nil {
			t.Fatalf("InsertServerAvailabilitySample[%d]: %v", i, err)
		}
	}
}

func TestServerAvailabilityEndpoint(t *testing.T) {
	srv, routeStore := newPerfTestServer(t)
	seedAvailability(t, routeStore)

	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/availability?window=15m", nil)
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto availabilityHistoryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if len(dto.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(dto.Points))
	}
	if dto.From == "" || dto.To == "" {
		t.Fatalf("from/to = %q/%q, want both present", dto.From, dto.To)
	}
	if dto.Points[0].Health != routing.HealthHealthy || !dto.Points[0].AgentReporting || dto.Points[0].ReachableCount != 1 {
		t.Fatalf("points[0] = %#v, want healthy/agent=true/reachable=1", dto.Points[0])
	}
	if dto.Points[2].AgentReporting {
		t.Fatalf("points[2].agent_reporting = true, want false")
	}
}

// TestServerAvailabilityEndpointCarriesNetbirdConnected proves the DTO maps + wires
// the netbird_connected field (json tag) through to the availability response.
func TestServerAvailabilityEndpointCarriesNetbirdConnected(t *testing.T) {
	srv, routeStore := newPerfTestServer(t)
	base := time.Now().UTC().Add(-3 * time.Minute)
	samples := []routing.ServerAvailabilitySample{
		{ServerID: perfServerID, ReportedAt: base, Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: true, NetbirdConnected: true},
		{ServerID: perfServerID, ReportedAt: base.Add(time.Minute), Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: true, NetbirdConnected: false},
	}
	for i, s := range samples {
		if err := routeStore.InsertServerAvailabilitySample(context.Background(), s); err != nil {
			t.Fatalf("insert[%d]: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/availability?window=15m", nil)
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto availabilityHistoryDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if len(dto.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(dto.Points))
	}
	if !dto.Points[0].NetbirdConnected {
		t.Fatalf("points[0].netbird_connected = false, want true")
	}
	if dto.Points[1].NetbirdConnected {
		t.Fatalf("points[1].netbird_connected = true, want false")
	}
}

func TestServerAvailabilityEndpointForbidden(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+perfServerID+"/availability?window=15m", nil)
	req.Header.Set("Authorization", "Bearer "+perfOtherSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "server.not_found" {
		t.Fatalf("error code = %q, want server.not_found", code)
	}
}

func TestServerAvailabilityEndpointMethodNotAllowed(t *testing.T) {
	srv, _ := newPerfTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/"+perfServerID+"/availability", nil)
	req.Header.Set("Authorization", "Bearer "+perfOwnerSecret)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
}
