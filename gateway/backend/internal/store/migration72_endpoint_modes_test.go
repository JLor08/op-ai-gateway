// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// reinvokeMigration72 re-runs migration72Up in its own tx (idempotent: the
// ADD COLUMNs are duplicate-tolerant and the backfill UPDATEs are guarded on
// the column being blank, so already-set rows are untouched) after seeding
// legacy rows, mirroring TestMigration20BackfillPeerManaged.
func reinvokeMigration72(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration72Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration72Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// mustExec runs a raw SQL statement directly against s.db, rebound for the
// dialect under test. Used to force the pre-72 legacy shape (blank the mode
// columns migration72Up itself would have already set) so the migration's
// backfill has something to do.
func mustExec(ctx context.Context, t *testing.T, s *SQLStore, q string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(q), args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestMigration72BackfillApplicationModes proves the bool→mode backfill:
// native_responses=1 -> passthrough, 0 -> translate (same for messages).
func TestMigration72BackfillApplicationModes(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m72", Name: "M72", Provider: routing.ProviderVLLM, Endpoint: "http://m:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		for _, id := range []string{"app_pt", "app_tp"} {
			if err := s.CreateApplication(ctx, routing.Application{
				ID: id, ServerID: "srv_m72", Type: routing.ProviderVLLM,
				Port: map[string]int{"app_pt": 9001, "app_tp": 9002}[id], Scheme: "http",
				APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
				Status:     routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create app %s: %v", id, err)
			}
		}
		// Force the pre-72 legacy shape: set the inert bools, blank the modes.
		mustExec(ctx, t, s, `update applications set native_responses = 1, native_messages = 0,
			responses_mode = '', messages_mode = '' where id = ?`, "app_pt")
		mustExec(ctx, t, s, `update applications set native_responses = 0, native_messages = 1,
			responses_mode = '', messages_mode = '' where id = ?`, "app_tp")

		reinvokeMigration72(ctx, t, s)

		pt, _ := s.ApplicationByID(ctx, "app_pt")
		if pt.ResponsesMode != routing.EndpointModePassthrough || pt.MessagesMode != routing.EndpointModeTranslate {
			t.Fatalf("app_pt: got %q/%q want passthrough/translate", pt.ResponsesMode, pt.MessagesMode)
		}
		tp, _ := s.ApplicationByID(ctx, "app_tp")
		if tp.ResponsesMode != routing.EndpointModeTranslate || tp.MessagesMode != routing.EndpointModePassthrough {
			t.Fatalf("app_tp: got %q/%q want translate/passthrough", tp.ResponsesMode, tp.MessagesMode)
		}
	})
}

// TestMigration72SnapshotRuntimeSpec proves each spec is snapshotted from its
// parent app via spec.mapping_id -> model_mappings -> applications.
func TestMigration72SnapshotRuntimeSpec(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_sp", Name: "SP", Provider: routing.ProviderMock, Endpoint: "mock://sp",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_sp", ServerID: "srv_sp", Type: routing.ProviderServerAgent, Port: 8090, Scheme: "http",
			APIFlavors:    []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModeTranslate,
			Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create app: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_sp", ApplicationID: "app_sp", GatewayModelName: "sp-model", AppModelName: "up-sp",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: "spec_sp", MappingID: "map_sp", Enabled: true, Binary: "/usr/bin/llama-server",
			Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert spec: %v", err)
		}
		// Force the pre-72 legacy shape on the spec.
		mustExec(ctx, t, s, `update agent_runtime_specs set api_flavors = '[]',
			responses_mode = '', messages_mode = '' where id = ?`, "spec_sp")

		reinvokeMigration72(ctx, t, s)

		got, ok, err := s.RuntimeSpecByMapping(ctx, "map_sp")
		if err != nil || !ok {
			t.Fatalf("read back spec: ok=%v err=%v", ok, err)
		}
		if got.ResponsesMode != routing.EndpointModePassthrough || got.MessagesMode != routing.EndpointModeTranslate {
			t.Fatalf("spec modes: got %q/%q want passthrough/translate", got.ResponsesMode, got.MessagesMode)
		}
		if !reflect.DeepEqual(got.APIFlavors, []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}) {
			t.Fatalf("spec flavors: got %#v want [openai anthropic]", got.APIFlavors)
		}
	})
}
