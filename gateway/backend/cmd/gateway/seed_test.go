// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func TestSeedDefaultServerResolvesSeededModels(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, store, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer: %v", err)
	}
	resolver := routing.NewResolver(store, func() time.Time { return now }, nil)
	for _, model := range []string{"qwen-coder", "gpt-oss-20b"} {
		target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "usr", Active: true}, inference.Request{Model: model, APIFlavor: "openai_chat"})
		if err != nil {
			t.Fatalf("Resolve(%s): %v", model, err)
		}
		if target.Provider != routing.ProviderMock {
			t.Fatalf("Resolve(%s) provider = %q, want mock", model, target.Provider)
		}
	}
	if err := seedDefaultServer(ctx, store, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer (second run): %v", err)
	}
}

func TestSeedDefaultServerSeedsAgentToken(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, store, now, "agent-secret", ""); err != nil {
		t.Fatalf("seedDefaultServer: %v", err)
	}
	serverID, ok, err := store.LookupAgentToken(ctx, auth.HashSecret("agent-secret"))
	if err != nil || !ok || serverID != "mock-server" {
		t.Fatalf("LookupAgentToken = %q ok=%v err=%v", serverID, ok, err)
	}
	if err := seedDefaultServer(ctx, store, now, "agent-secret", ""); err != nil {
		t.Fatalf("seedDefaultServer (reseed): %v", err)
	}
	serverID, ok, err = store.LookupAgentToken(ctx, auth.HashSecret("agent-secret"))
	if err != nil || !ok || serverID != "mock-server" {
		t.Fatalf("LookupAgentToken after reseed = %q ok=%v err=%v", serverID, ok, err)
	}
	store2 := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, store2, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer empty secret: %v", err)
	}
	if _, ok, _ := store2.AgentTokenByServer(ctx, "mock-server"); ok {
		t.Fatalf("empty secret should not seed an agent token")
	}
}
