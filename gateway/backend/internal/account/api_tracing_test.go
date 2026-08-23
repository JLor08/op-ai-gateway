// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package account_test

import (
	"context"
	"io"
	"log/slog"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/tracing"
	"testing"
	"time"
)

// TestAccountAPIDecoratorDelegatesAndTraces proves the gowrap-generated
// account.APIWithTracing decorator (a) delegates to the wrapped *account.Service
// with the return value unchanged and (b) opens an OTel span named
// "account.Service.<Method>" via the OTel global tracer that lands in the
// trace-level logbuffer. It is same-package generated and must NOT depend on
// internal/tracing at compile time (that dependency would form an import cycle).
func TestAccountAPIDecoratorDelegatesAndTraces(t *testing.T) {
	logs := logbuffer.NewBuffer(20, logbuffer.LevelTrace)
	prev := slog.Default()
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	defer slog.SetDefault(prev)
	p, err := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	if err != nil {
		t.Fatalf("tracing setup: %v", err)
	}
	defer p.Shutdown(context.Background())

	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	now := time.Now().UTC()
	if err := dir.CreateUser(context.Background(), store.User{
		ID:                "usr_1",
		Email:             "a@example.test",
		DisplayName:       "a",
		Role:              "admin",
		Status:            store.UserStatusActive,
		PreferredLanguage: "de",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	svc := account.NewService(account.Deps{
		Users:             dir,
		Sessions:          dir,
		SetPasswordTokens: dir,
		SettingsVolatile:  true,
	}, account.Config{})

	wrapped := account.NewAPIWithTracing(svc)

	// Delegation: the wrapped call must return exactly what the Service returns.
	user, err := wrapped.UserByID(context.Background(), "usr_1")
	if err != nil {
		t.Fatalf("delegate UserByID: %v", err)
	}
	if user.ID != "usr_1" || user.Email != "a@example.test" {
		t.Fatalf("delegation altered the result: %+v", user)
	}

	// Tracing: the span must have been mirrored into the logbuffer.
	var found bool
	for _, r := range logs.Snapshot() {
		if r.Msg == "span" && r.Attrs["span"] == "account.Service.UserByID" {
			found = true
		}
	}
	if !found {
		t.Fatalf("account.Service.UserByID span not emitted: %+v", logs.Snapshot())
	}
}
