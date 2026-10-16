// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestNewWiresTheSharedVendorAccountGrantIntoTheResolver proves the gateway hands the
// portal's routing grant to the resolver it builds: the server's OWN resolver, over the
// real portal.Service, routes a shared account to a principal provisioned into its
// resource group, denies a principal who is not, and follows the master sharing flag
// live. Without the wiring the resolver would silently fall back to owner-only and the
// first assertion would fail; the grant itself is pinned in internal/portal.
func TestNewWiresTheSharedVendorAccountGrantIntoTheResolver(t *testing.T) {
	ctx := context.Background()
	srv, routes := newShareLevelTestServer(t)
	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":true}`)

	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	const (
		accountID = "va_wired_shared"
		rgID      = "rgrp_wired"
		model     = "wired-model"
	)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: accountID, OwnerUserID: "usr_va_a", Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
		Name: "wired", Status: routing.VendorAccountStatusActive, APIKey: "enc:wired-key", CreatedAt: now, UpdatedAt: now,
	}))
	must(routes.SetVendorAccountModels(ctx, accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: model, UpstreamModel: model, APIFlavor: routing.APIFlavorOpenAI},
	}))
	must(routes.UpdateVendorAccountShareLevel(ctx, accountID, routing.VendorShareLevelShared))
	must(routes.CreateResourceGroup(ctx, routing.ResourceGroup{ID: rgID, Name: rgID, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(routes.SetResourceGroupVendorAccount(ctx, rgID, accountID))
	must(routes.SetResourceGroupProvision(ctx, rgID, routing.ProvisionKindUser, "usr_va_b"))

	resolve := func(userID string) (string, error) {
		target, err := srv.Resolver.Resolve(ctx, auth.Token{ID: "tok_" + userID, UserID: userID, Active: true, VendorAccess: auth.VendorAccess{All: true}}, inference.Request{Model: model, APIFlavor: "openai_chat"})
		return target.VendorAccountID, err
	}

	if got, err := resolve("usr_va_b"); err != nil || got != accountID {
		t.Fatalf("provisioned borrower resolved (%q, %v), want the shared account %s: the grant is not wired into the resolver", got, err, accountID)
	}
	if got, err := resolve("usr_va_a"); err != nil || got != accountID {
		t.Fatalf("owner resolved (%q, %v), want their own account %s", got, err, accountID)
	}
	if got, err := resolve("usr_va_stranger"); !errors.Is(err, routing.ErrNoModelRoute) {
		t.Fatalf("unprovisioned user resolved (%q, %v), want ErrNoModelRoute", got, err)
	}

	putVendorSystemSetting(t, srv, `{"vendor_account_sharing_enabled":false}`)
	if got, err := resolve("usr_va_b"); !errors.Is(err, routing.ErrNoModelRoute) {
		t.Fatalf("borrower with sharing switched off resolved (%q, %v), want ErrNoModelRoute (the pause is live)", got, err)
	}
	if got, err := resolve("usr_va_a"); err != nil || got != accountID {
		t.Fatalf("owner with sharing switched off resolved (%q, %v), want their own account %s", got, err, accountID)
	}
}
