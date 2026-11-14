// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// The SQL store is an auth.BearerRechecker (the realtime session's periodic re-check
// depends on it): a compile-time guard beside the behavior tests below.
var _ auth.BearerRechecker = (*SQLStore)(nil)

// RecheckBearer refuses (auth.ErrBearerRefused) on exactly the conditions
// LookupBearer refuses on, resolves a valid token with its service's allowlist, and
// never records the use -- on both dialects.
func TestConformanceRecheckBearer(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateUser(ctx, newTestUser("u1", "recheck@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateService(ctx, testServiceRecord(now)); err != nil {
			t.Fatalf("create service: %v", err)
		}
		if err := s.CreateService(ctx, routing.Service{ID: "svc_off", Name: "Off", Status: routing.ServerStatusDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create disabled service: %v", err)
		}
		if err := s.SetServiceAllowedModels(ctx, "svc_1", []string{"llama3", "gpt-4o"}); err != nil {
			t.Fatalf("set allowlist: %v", err)
		}
		expired := now.Add(-time.Hour)
		for _, tc := range []struct {
			rec    TokenRecord
			secret string
		}{
			{TokenRecord{ID: "tok_ok", UserID: "u1", Name: "ok", Scopes: `["gateway:use"]`}, "recheck-ok-secret"},
			{TokenRecord{ID: "tok_off", UserID: "u1", Name: "off", Status: TokenStatusDisabled, Scopes: `["gateway:use"]`}, "recheck-off-secret"},
			{TokenRecord{ID: "tok_exp", UserID: "u1", Name: "exp", Scopes: `["gateway:use"]`, ExpiresAt: &expired}, "recheck-exp-secret"},
			{TokenRecord{ID: "tok_svc", ServiceID: "svc_1", Kind: TokenKindService, Name: "svc", Scopes: `["llm:invoke"]`}, "recheck-svc-secret"},
			{TokenRecord{ID: "tok_svc_off", ServiceID: "svc_off", Kind: TokenKindService, Name: "svc-off", Scopes: `["llm:invoke"]`}, "recheck-svc-off-secret"},
			{TokenRecord{ID: "tok_orphan", Kind: TokenKindService, Name: "orphan", Scopes: `["llm:invoke"]`}, "recheck-orphan-secret"},
		} {
			tc.rec.CreatedAt, tc.rec.UpdatedAt = now, now
			if err := s.CreatePlainToken(ctx, tc.rec, tc.secret); err != nil {
				t.Fatalf("create token %s: %v", tc.rec.ID, err)
			}
		}

		tok, err := s.RecheckBearer(ctx, "Bearer recheck-ok-secret")
		if err != nil || tok.ID != "tok_ok" || tok.UserID != "u1" || !tok.HasScope("gateway:use") {
			t.Fatalf("RecheckBearer(valid) = %+v, %v; want tok_ok with gateway:use", tok, err)
		}
		svc, err := s.RecheckBearer(ctx, "Bearer recheck-svc-secret")
		if err != nil || svc.ID != "tok_svc" || svc.ServiceName != "Nightly Batch" || !slices.Equal(svc.AllowedModels, []string{"gpt-4o", "llama3"}) {
			t.Fatalf("RecheckBearer(service) = %+v, %v; want tok_svc with its allowlist", svc, err)
		}

		for _, header := range []string{
			"",                              // no bearer
			"Basic recheck-ok-secret",       // not a bearer
			"Bearer unknown-secret",         // no token with that hash
			"Bearer recheck-off-secret",     // inactive
			"Bearer recheck-exp-secret",     // expired
			"Bearer recheck-svc-off-secret", // service disabled
			"Bearer recheck-orphan-secret",  // service token without a service id
		} {
			if _, err := s.RecheckBearer(ctx, header); !errors.Is(err, auth.ErrBearerRefused) {
				t.Fatalf("RecheckBearer(%q) err = %v, want auth.ErrBearerRefused", header, err)
			}
			if _, ok := s.LookupBearer(header); ok {
				t.Fatalf("LookupBearer(%q) ok, want refused like RecheckBearer", header)
			}
		}

		// No record of the use: last_used_at and updated_at stay as created.
		for _, id := range []string{"tok_ok", "tok_svc"} {
			rec, err := s.TokenByID(ctx, id)
			if err != nil {
				t.Fatalf("TokenByID(%s): %v", id, err)
			}
			if rec.LastUsedAt != nil || !rec.UpdatedAt.Equal(now) {
				t.Fatalf("%s after RecheckBearer: last_used_at %v updated_at %v, want nil and %v", id, rec.LastUsedAt, rec.UpdatedAt, now)
			}
		}
	})
}

// A RecheckBearer whose context has ended fails with that context's error, which is
// not a refusal: a read that could not run says nothing about the credential.
func TestConformanceRecheckBearerCanceledIsNotARefusal(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateUser(ctx, newTestUser("u1", "recheck@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok_ok", UserID: "u1", Name: "ok", Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "recheck-ok-secret"); err != nil {
			t.Fatalf("create token: %v", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := s.RecheckBearer(canceled, "Bearer recheck-ok-secret")
		if err == nil || errors.Is(err, auth.ErrBearerRefused) {
			t.Fatalf("RecheckBearer(canceled) err = %v, want a non-refusal error", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RecheckBearer(canceled) err = %v, want it to wrap context.Canceled", err)
		}
	})
}
