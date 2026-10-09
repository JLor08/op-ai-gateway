// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// tokenWithAccess returns the owner token carrying the given per-token vendor
// access policy, overriding whatever default ownerToken() ships.
func tokenWithAccess(a auth.VendorAccess) auth.Token {
	tok := ownerToken()
	tok.VendorAccess = a
	return tok
}

// TestResolveHonorsTokenVendorAccess proves the routing half of per-token vendor
// access: the resolver filters accounts by the token's policy and reverse-maps the
// token-effective prefix back to the raw upstream slug, using the same
// prefix + UpstreamModel formula the listing advertises.
func TestResolveHonorsTokenVendorAccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	// Account stored with native prefix "native/" serving slug gpt-4o.
	seedPrefixedVendorAccount(t, store, now, "acc_1", VendorOpenAI, VendorAuthAPIKey, "native/", "gpt-4o", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	// (a) strict default (no access): even the native name must NOT resolve.
	if _, err := resolver.Resolve(ctx, tokenWithAccess(auth.VendorAccess{}), inference.Request{Model: "native/gpt-4o", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("no-access token resolved a vendor model; want ErrNoModelRoute, got %v", err)
	}

	// (b) All: native name resolves, raw slug sent upstream.
	target, err := resolver.Resolve(ctx, tokenWithAccess(auth.VendorAccess{All: true}), inference.Request{Model: "native/gpt-4o", APIFlavor: "openai_chat"})
	if err != nil || target.ProviderModel != "gpt-4o" || target.Model != "native/gpt-4o" {
		t.Fatalf("all: target=%+v err=%v", target, err)
	}

	// (c) Override to a new prefix: the NEW name resolves (reverse map), native name does not.
	acc := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "x/"}}}
	target, err = resolver.Resolve(ctx, tokenWithAccess(acc), inference.Request{Model: "x/gpt-4o", APIFlavor: "openai_chat"})
	if err != nil || target.ProviderModel != "gpt-4o" || target.Model != "x/gpt-4o" {
		t.Fatalf("override: target=%+v err=%v", target, err)
	}
	if _, err := resolver.Resolve(ctx, tokenWithAccess(acc), inference.Request{Model: "native/gpt-4o", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("override: native name should no longer resolve, got %v", err)
	}

	// (d) Override to empty: the bare slug resolves.
	accEmpty := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}
	target, err = resolver.Resolve(ctx, tokenWithAccess(accEmpty), inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat"})
	if err != nil || target.ProviderModel != "gpt-4o" || target.Model != "gpt-4o" {
		t.Fatalf("empty-override: target=%+v err=%v", target, err)
	}
}
