// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// fakeAgentPresenceReader is a test AgentPresenceReader. Two modes per server:
//   - reportedAge[id] set -> window-SENSITIVE: reports iff the last report's age
//     is <= the window serverDTO computed (proves the effective per-server window
//     is actually applied).
//   - else reporting[id] -> a plain window-insensitive bool (for the 3-state test).
type fakeAgentPresenceReader struct {
	reporting   map[string]bool
	reportedAge map[string]time.Duration
}

func (f fakeAgentPresenceReader) ReportingWithin(serverID string, window time.Duration) bool {
	if age, ok := f.reportedAge[serverID]; ok {
		return age <= window
	}
	return f.reporting[serverID]
}

// newAgentStatusTestService mirrors newServerTestService but additionally
// wires an AgentPresenceReader, letting tests flip the "server X is
// reporting" bit after the service is built.
func newAgentStatusTestService(t *testing.T, now time.Time, reader fakeAgentPresenceReader) (*Service, *routing.MemoryStore) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner"} {
		if err := dir.CreateUser(context.Background(), store.User{
			ID: u, Email: u + "@example.test", DisplayName: u, Role: "user",
			Status: store.UserStatusActive, PreferredLanguage: "de",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, AgentPresence: reader, Clock: func() time.Time { return now }})
	return svc, routeStore
}

// TestServerDTOAgentStatusThreeStates proves the three agent_status values:
// unconfigured (no agent token at all), inactive (a token exists but the
// registry says it's not reporting within the effective window), and active
// (reporting within the window).
func TestServerDTOAgentStatusThreeStates(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	reader := fakeAgentPresenceReader{reporting: map[string]bool{}}
	svc, _ := newAgentStatusTestService(t, now, reader)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S1", Domain: "s1.example.test", AdminGroupIDs: []string{testAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.AgentStatus != "unconfigured" {
		t.Fatalf("agent_status = %q, want unconfigured (no token, not reporting)", dto.AgentStatus)
	}

	if _, err := svc.GenerateAgentToken(context.Background(), systemAdminToken(), dto.ID); err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	got, err := svc.GetServer(context.Background(), systemAdminToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AgentStatus != "inactive" {
		t.Fatalf("agent_status = %q, want inactive (token configured, not reporting)", got.AgentStatus)
	}

	reader.reporting[dto.ID] = true // map is shared by reference; the reader value in svc sees this too
	got2, err := svc.GetServer(context.Background(), systemAdminToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got2.AgentStatus != "active" {
		t.Fatalf("agent_status = %q, want active (reporting within the effective window)", got2.AgentStatus)
	}
}

// TestServerDTOAgentStatusPerServerWindowTightens proves the per-server override
// actually tightens the window serverDTO hands to ReportingWithin: the last report
// is 10s old, the SYSTEM default window (100s) would say "active", but the server's
// own 5s override must win → "inactive". Guards against serverDTO discarding the
// per-server value and always using the system default.
func TestServerDTOAgentStatusPerServerWindowTightens(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner"} {
		if err := dir.CreateUser(context.Background(), store.User{
			ID: u, Email: u + "@example.test", DisplayName: u, Role: "user",
			Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	reader := fakeAgentPresenceReader{reportedAge: map[string]time.Duration{}}
	// System default window 100s; a per-server override of 5s must tighten it.
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, AgentPresence: reader, Clock: func() time.Time { return now }, AgentPresenceTimeoutDefault: 100})

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S1", Domain: "s1.example.test", AgentPresenceTimeoutSeconds: intPtr(5), AdminGroupIDs: []string{testAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if _, err := svc.GenerateAgentToken(context.Background(), systemAdminToken(), dto.ID); err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	reader.reportedAge[dto.ID] = 10 * time.Second // 10s old: within 100s system default, OUTSIDE the 5s per-server override

	got, err := svc.GetServer(context.Background(), systemAdminToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AgentStatus != "inactive" {
		t.Fatalf("agent_status = %q, want inactive (the per-server 5s window must tighten the 100s system default)", got.AgentStatus)
	}
}

// TestServerDTOAgentStatusUsesSystemDefaultWindow proves the column reads the
// (env-aware) SYSTEM default window when the server has NO per-server override —
// not a hardcoded constant. System default 100s, no override, a 50s-old report →
// "active"; a regression to a hardcoded 15s default would read "inactive".
func TestServerDTOAgentStatusUsesSystemDefaultWindow(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner"} {
		if err := dir.CreateUser(context.Background(), store.User{
			ID: u, Email: u + "@example.test", DisplayName: u, Role: "user",
			Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	reader := fakeAgentPresenceReader{reportedAge: map[string]time.Duration{}}
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, AgentPresence: reader, Clock: func() time.Time { return now }, AgentPresenceTimeoutDefault: 100})

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S1", Domain: "s1.example.test", AdminGroupIDs: []string{testAdminGroupID}}) // no per-server override (0 → follow system)
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if _, err := svc.GenerateAgentToken(context.Background(), systemAdminToken(), dto.ID); err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	reader.reportedAge[dto.ID] = 50 * time.Second // within the 100s system default, but > a hardcoded-15s regression

	got, err := svc.GetServer(context.Background(), systemAdminToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AgentStatus != "active" {
		t.Fatalf("agent_status = %q, want active (a 50s report is within the 100s SYSTEM default window)", got.AgentStatus)
	}
}

// TestServerDTOAgentStatusReportingWinsOverMissingToken pins the derivation
// precedence: reporting → "active" wins even without an agent token (the
// reporting check must NOT be nested inside the has-token branch). In practice a
// report only arrives with a valid token, but the structure must stay flat.
func TestServerDTOAgentStatusReportingWinsOverMissingToken(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	reader := fakeAgentPresenceReader{reporting: map[string]bool{}}
	svc, _ := newAgentStatusTestService(t, now, reader)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S1", Domain: "s1.example.test", AdminGroupIDs: []string{testAdminGroupID}}) // NO agent token generated
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	reader.reporting[dto.ID] = true // reporting, but no token configured
	got, err := svc.GetServer(context.Background(), systemAdminToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AgentStatus != "active" {
		t.Fatalf("agent_status = %q, want active (reporting wins even without a token)", got.AgentStatus)
	}
}

// TestServerDTOAgentStatusNilReaderIsUnconfigured confirms a Service built
// without an AgentPresence reader (nil-safe) never reports "active" and falls
// back to "unconfigured" for a server with no agent token.
func TestServerDTOAgentStatusNilReaderIsUnconfigured(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now) // no AgentPresence wired
	server := createTestServer(t, svc, "S", "s.example.test")
	got, err := svc.GetServer(context.Background(), systemAdminToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AgentStatus != "unconfigured" {
		t.Fatalf("agent_status = %q, want unconfigured (nil reader, no token)", got.AgentStatus)
	}
}

// TestCreateUpdateServerAgentPresenceTimeoutSeconds proves the per-server
// override rides create+update and rejects a negative value.
func TestCreateUpdateServerAgentPresenceTimeoutSeconds(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)

	custom := 42
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", AgentPresenceTimeoutSeconds: &custom,
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.AgentPresenceTimeoutSeconds != 42 {
		t.Fatalf("AgentPresenceTimeoutSeconds = %d, want 42", dto.AgentPresenceTimeoutSeconds)
	}

	zero := 0
	got, err := svc.UpdateServer(context.Background(), systemAdminToken(), dto.ID, UpdateServerRequest{AgentPresenceTimeoutSeconds: &zero})
	if err != nil {
		t.Fatalf("UpdateServer(0): %v", err)
	}
	if got.AgentPresenceTimeoutSeconds != 0 {
		t.Fatalf("AgentPresenceTimeoutSeconds after reset-to-follow-system = %d, want 0", got.AgentPresenceTimeoutSeconds)
	}

	neg := -1
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), dto.ID, UpdateServerRequest{AgentPresenceTimeoutSeconds: &neg}); err == nil {
		t.Fatal("UpdateServer(-1) should reject a negative agent_presence_timeout_seconds")
	}
}

func TestCreateServerRejectsNegativeAgentPresenceTimeoutSeconds(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	neg := -5
	if _, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", AgentPresenceTimeoutSeconds: &neg,
		AdminGroupIDs: []string{testAdminGroupID},
	}); err == nil {
		t.Fatal("CreateServer(-5) should reject a negative agent_presence_timeout_seconds")
	}
}
