// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func TestSQLiteTelemetryAndAffinityRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := st.CreateUser(ctx, User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser returned %v", err)
	}
	if err := st.CreatePlainToken(ctx, TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}
	if err := st.CreateAIServer(ctx, routing.AIServer{ID: "host_1", Name: "Host 1", Provider: routing.ProviderMock, Endpoint: "mock://host", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer returned %v", err)
	}
	if err := st.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "host_1", Type: routing.ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	if err := st.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "host_1", ReportedAt: now, ActiveRequests: 3, QueueDepth: 2, LatencyMS: 200, ErrorRate: 0.02, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry returned %v", err)
	}
	if err := st.UpsertAffinity(ctx, routing.RouteAffinity{ID: "aff_1", APITokenID: "tok_dev", UserID: "usr_dev", Model: "qwen-coder", APIFlavor: routing.APIFlavorOpenAI, ApplicationID: "app_1", ServerID: "host_1", ExpiresAt: now.Add(30 * time.Minute), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertAffinity returned %v", err)
	}

	telemetry, ok, err := st.TelemetryByServer(ctx, "host_1")
	if err != nil {
		t.Fatalf("TelemetryByServer returned %v", err)
	}
	if !ok || telemetry.QueueDepth != 2 {
		t.Fatalf("telemetry = %#v ok=%v", telemetry, ok)
	}
	affinity, ok, err := st.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_dev", Model: "qwen-coder", APIFlavor: routing.APIFlavorOpenAI})
	if err != nil {
		t.Fatalf("Affinity returned %v", err)
	}
	if !ok || affinity.ApplicationID != "app_1" || affinity.ServerID != "host_1" {
		t.Fatalf("affinity = %#v ok=%v", affinity, ok)
	}
}

func TestSQLiteStoreImplementsRoutingStore(t *testing.T) {
	var _ routing.Store = (*SQLiteStore)(nil)
}

func TestSQLiteSetServerHealth(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	if err := st.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "GPU 1", Domain: "gpu1.example.test", Provider: routing.ProviderVLLM, Endpoint: "https://gpu1:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer returned %v", err)
	}
	if err := st.SetServerHealth(ctx, "srv_1", routing.HealthDegraded); err != nil {
		t.Fatalf("SetServerHealth returned %v", err)
	}
	got, err := st.AIServerByID(ctx, "srv_1")
	if err != nil {
		t.Fatalf("AIServerByID returned %v", err)
	}
	if got.HealthStatus != routing.HealthDegraded {
		t.Fatalf("HealthStatus = %q, want %q", got.HealthStatus, routing.HealthDegraded)
	}
	// The targeted update touches only health_status + updated_at, preserving
	// every other column.
	if got.Name != "GPU 1" || got.Domain != "gpu1.example.test" || got.Provider != routing.ProviderVLLM || got.Endpoint != "https://gpu1:8000" || got.Status != routing.ServerStatusActive {
		t.Fatalf("SetServerHealth clobbered fields: %#v", got)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("updated_at %v not advanced past created_at %v", got.UpdatedAt, got.CreatedAt)
	}
	if err := st.SetServerHealth(ctx, "missing", routing.HealthHealthy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetServerHealth(unknown) = %v, want ErrNotFound", err)
	}
}

func TestSQLiteAIServerCarriesDomain(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if err := st.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "GPU 1", Domain: "gpu1.example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer returned %v", err)
	}
	got, err := st.AIServerByID(ctx, "srv_1")
	if err != nil {
		t.Fatalf("AIServerByID returned %v", err)
	}
	if got.Domain != "gpu1.example.test" || got.Name != "GPU 1" {
		t.Fatalf("server = %#v", got)
	}
}

func TestSQLiteServerOwnersSetGetAndByOwner(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	for _, u := range []string{"usr_a", "usr_b"} {
		if err := st.CreateUser(ctx, User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	if err := st.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := st.SetServerOwners(ctx, "srv_1", []string{"usr_a", "usr_b"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	owners, err := st.ServerOwners(ctx, "srv_1")
	if err != nil || len(owners) != 2 {
		t.Fatalf("owners = %#v err=%v", owners, err)
	}
	if err := st.SetServerOwners(ctx, "srv_1", []string{"usr_a"}); err != nil {
		t.Fatalf("SetServerOwners replace: %v", err)
	}
	byOwnerA, err := st.ServersByOwner(ctx, "usr_a")
	if err != nil || len(byOwnerA) != 1 || byOwnerA[0].ID != "srv_1" {
		t.Fatalf("servers by owner a = %#v err=%v", byOwnerA, err)
	}
	byOwnerB, err := st.ServersByOwner(ctx, "usr_b")
	if err != nil || len(byOwnerB) != 0 {
		t.Fatalf("owner b should own nothing after replace, got %#v", byOwnerB)
	}
}

func TestSQLiteDeleteAIServerCascadesOwners(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if err := st.CreateUser(ctx, User{ID: "usr_a", Email: "a@example.test", DisplayName: "a", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.example.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := st.SetServerOwners(ctx, "srv_1", []string{"usr_a"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	if err := st.DeleteAIServer(ctx, "srv_1"); err != nil {
		t.Fatalf("DeleteAIServer: %v", err)
	}
	byOwner, err := st.ServersByOwner(ctx, "usr_a")
	if err != nil || len(byOwner) != 0 {
		t.Fatalf("owner rows should have cascaded, got %#v err=%v", byOwner, err)
	}
}
