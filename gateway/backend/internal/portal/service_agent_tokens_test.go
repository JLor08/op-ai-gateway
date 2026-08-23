// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerateRotateRevokeAgentTokenAdminOrOwner(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	status, err := svc.AgentTokenStatus(context.Background(), ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("AgentTokenStatus: %v", err)
	}
	if status.Exists {
		t.Fatalf("expected no token initially")
	}
	gen, err := svc.GenerateAgentToken(context.Background(), ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GenerateAgentToken: %v", err)
	}
	if gen.Secret == "" || !gen.Token.Exists || gen.Token.SecretPrefix == "" {
		t.Fatalf("gen = %#v", gen)
	}
	firstSecret := gen.Secret
	status, _ = svc.AgentTokenStatus(context.Background(), ownerToken(), server.ID)
	if !status.Exists || status.SecretPrefix != gen.Token.SecretPrefix {
		t.Fatalf("status = %#v", status)
	}
	gen2, err := svc.GenerateAgentToken(context.Background(), systemAdminToken(), server.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if gen2.Secret == firstSecret {
		t.Fatalf("rotate returned same secret")
	}
	if err := svc.RevokeAgentToken(context.Background(), ownerToken(), server.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if status, _ := svc.AgentTokenStatus(context.Background(), ownerToken(), server.ID); status.Exists {
		t.Fatalf("token still exists after revoke")
	}
}

func TestAgentTokenNonOwnerNonAdminNotFound(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if _, err := svc.GenerateAgentToken(context.Background(), otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner generate = %v, want ErrServerNotFound", err)
	}
	if _, err := svc.AgentTokenStatus(context.Background(), otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner status = %v, want ErrServerNotFound", err)
	}
	if err := svc.RevokeAgentToken(context.Background(), otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner revoke = %v, want ErrServerNotFound", err)
	}
}

func TestGenerateAgentTokenUnknownServerNotFound(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	if _, err := svc.GenerateAgentToken(context.Background(), systemAdminToken(), "missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("unknown server = %v, want ErrServerNotFound", err)
	}
}

func TestGenerateAgentTokenRotatePreservesCreatedAt(t *testing.T) {
	first := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	svc, store := newServerTestService(t, first)
	server := createTestServer(t, svc, "S", "s.example.test")
	// A service over the SAME store with an advancing clock (the system-scope
	// path in authorizeServer needs only the server row, not Users/Groups).
	current := first
	svc2 := NewService(ServiceDeps{Routes: store, Clock: func() time.Time { return current }})
	gen1, err := svc2.GenerateAgentToken(context.Background(), systemAdminToken(), server.ID)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	current = first.Add(time.Hour)
	gen2, err := svc2.GenerateAgentToken(context.Background(), systemAdminToken(), server.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if gen1.Token.CreatedAt == nil || gen2.Token.CreatedAt == nil || !gen2.Token.CreatedAt.Equal(*gen1.Token.CreatedAt) {
		t.Fatalf("created_at not preserved on rotate: %v vs %v", gen1.Token.CreatedAt, gen2.Token.CreatedAt)
	}
	if gen1.Token.UpdatedAt == nil || gen2.Token.UpdatedAt == nil || !gen2.Token.UpdatedAt.After(*gen1.Token.UpdatedAt) {
		t.Fatalf("updated_at not advanced on rotate: %v vs %v", gen1.Token.UpdatedAt, gen2.Token.UpdatedAt)
	}
}
