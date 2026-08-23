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

func TestSQLiteAgentTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	s := openMigratedTestSQLite(t)
	defer s.Close()
	if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Provider: routing.ProviderMock, Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	// unknown server → ErrNotFound
	if err := s.UpsertAgentToken(ctx, routing.AgentToken{ID: "agt_x", ServerID: "missing", SecretPrefix: "p", CreatedAt: now, UpdatedAt: now}, "hash-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Upsert unknown server = %v, want ErrNotFound", err)
	}
	// create + read
	if err := s.UpsertAgentToken(ctx, routing.AgentToken{ID: "agt_1", ServerID: "srv_1", SecretPrefix: "opaigw_a", CreatedAt: now, UpdatedAt: now}, "hash-1"); err != nil {
		t.Fatalf("Upsert create: %v", err)
	}
	got, ok, err := s.AgentTokenByServer(ctx, "srv_1")
	if err != nil || !ok || got.ID != "agt_1" || got.SecretPrefix != "opaigw_a" || got.LastUsedAt != nil {
		t.Fatalf("AgentTokenByServer = %#v ok=%v err=%v", got, ok, err)
	}
	// lookup bumps last_used_at
	sid, ok, err := s.LookupAgentToken(ctx, "hash-1")
	if err != nil || !ok || sid != "srv_1" {
		t.Fatalf("LookupAgentToken = %q ok=%v err=%v", sid, ok, err)
	}
	if again, _, _ := s.AgentTokenByServer(ctx, "srv_1"); again.LastUsedAt == nil {
		t.Fatalf("last_used_at not bumped")
	}
	// rotate preserves created_at, replaces hash, resets last_used_at
	later := now.Add(time.Hour)
	if err := s.UpsertAgentToken(ctx, routing.AgentToken{ID: "agt_2", ServerID: "srv_1", SecretPrefix: "opaigw_b", CreatedAt: later, UpdatedAt: later}, "hash-2"); err != nil {
		t.Fatalf("Upsert rotate: %v", err)
	}
	rotated, _, _ := s.AgentTokenByServer(ctx, "srv_1")
	if !rotated.CreatedAt.Equal(now) {
		t.Fatalf("created_at not preserved on rotate: %v", rotated.CreatedAt)
	}
	if !rotated.UpdatedAt.Equal(later) {
		t.Fatalf("updated_at not advanced on rotate: %v", rotated.UpdatedAt)
	}
	if rotated.LastUsedAt != nil {
		t.Fatalf("last_used_at not reset on rotate")
	}
	if _, ok, _ := s.LookupAgentToken(ctx, "hash-1"); ok {
		t.Fatalf("old hash resolves after rotate")
	}
	if sid, ok, _ := s.LookupAgentToken(ctx, "hash-2"); !ok || sid != "srv_1" {
		t.Fatalf("new hash after rotate = %q ok=%v", sid, ok)
	}
	// revoke idempotent
	if err := s.DeleteAgentTokenByServer(ctx, "srv_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteAgentTokenByServer(ctx, "srv_1"); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
	// cross-server hash clash → ErrConflict (global secret_hash UNIQUE)
	if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srv_2", Name: "S2", Domain: "s2.test", Provider: routing.ProviderMock, Status: routing.ServerStatusActive, HealthStatus: routing.HealthUnknown, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer srv_2: %v", err)
	}
	if err := s.UpsertAgentToken(ctx, routing.AgentToken{ID: "agt_s1", ServerID: "srv_1", SecretPrefix: "p", CreatedAt: now, UpdatedAt: now}, "hash-shared"); err != nil {
		t.Fatalf("Upsert srv_1 shared hash: %v", err)
	}
	if err := s.UpsertAgentToken(ctx, routing.AgentToken{ID: "agt_dup", ServerID: "srv_2", SecretPrefix: "p", CreatedAt: now, UpdatedAt: now}, "hash-shared"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reusing another server's hash = %v, want ErrConflict", err)
	}
	// cascade on server delete
	if err := s.UpsertAgentToken(ctx, routing.AgentToken{ID: "agt_3", ServerID: "srv_1", SecretPrefix: "opaigw_c", CreatedAt: now, UpdatedAt: now}, "hash-3"); err != nil {
		t.Fatalf("Upsert before cascade: %v", err)
	}
	if err := s.DeleteAIServer(ctx, "srv_1"); err != nil {
		t.Fatalf("DeleteAIServer: %v", err)
	}
	if _, ok, _ := s.LookupAgentToken(ctx, "hash-3"); ok {
		t.Fatalf("agent token survived server delete (no cascade)")
	}
}
