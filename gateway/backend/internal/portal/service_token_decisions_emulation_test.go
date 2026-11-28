// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"testing"
	"time"
)

// TestTokenDecisionsEmulationRoundTrip: create, keep on an unrelated PATCH, clear
// explicitly, and reach the bearer (#380). Ungated like the normalize flag.
func TestTokenDecisionsEmulationRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, _, tokens := usageSwitchesFixture(t)
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	created, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "de", Scopes: []string{"gateway:use"}, DecisionsEmulation: true})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !created.Token.DecisionsEmulation {
		t.Fatalf("create DTO = %+v", created.Token)
	}
	if tok, ok := tokens.LookupBearer("Bearer " + created.Secret); !ok || !tok.DecisionsEmulation {
		t.Fatalf("bearer ok=%v emulation=%v", ok, tok.DecisionsEmulation)
	}
	name := "renamed"
	dto, err := svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{Name: &name})
	if err != nil || !dto.DecisionsEmulation {
		t.Fatalf("unrelated PATCH: %+v %v", dto, err)
	}
	dto, err = svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{DecisionsEmulation: boolPtr(false)})
	if err != nil || dto.DecisionsEmulation {
		t.Fatalf("explicit false: %+v %v", dto, err)
	}
	if tok, ok := tokens.LookupBearer("Bearer " + created.Secret); !ok || tok.DecisionsEmulation {
		t.Fatalf("bearer after clear ok=%v emulation=%v", ok, tok.DecisionsEmulation)
	}
}

// TestCreateServiceTokenDecisionsEmulation: the flag is accepted on service-token
// create and listed back (#380).
func TestCreateServiceTokenDecisionsEmulation(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "T1", DecisionsEmulation: true})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	if !resp.Token.DecisionsEmulation {
		t.Fatalf("create DTO = %+v", resp.Token)
	}
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil || len(list) != 1 || !list[0].DecisionsEmulation {
		t.Fatalf("listed = %+v err %v", list, err)
	}
}
