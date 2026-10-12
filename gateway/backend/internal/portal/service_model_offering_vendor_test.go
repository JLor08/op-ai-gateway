// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestModelOfferingIncludesTokenVendorModels pins that the unknown-model
// redirect's reachability set (ModelOffering.Callable and .Existing) includes a
// token's own vendor-account models, under the token-effective prefix — the same
// names /v1/models advertises and the resolver routes. Without it, a routable
// vendor model is absent from the offering, so redirectUnknownModel treats it as
// unknown and reroutes it to the fallback, even though the token can serve it.
//
// The fixture seeds no server mappings, so a vendor model can reach Callable or
// Existing only through the vendor overlay — the exact gap this test guards.
func TestModelOfferingIncludesTokenVendorModels(t *testing.T) {
	fx := newTokenVendorAccessFixture(t) // acc_a ("a/"), acc_b ("b/") owned by usr_1, each serving gpt-4o
	setVendorAccountsEnabled(t, fx.svc, true)
	ctx := context.Background()

	offerFor := func(access auth.VendorAccess) ModelOffering {
		token := fx.owner
		token.VendorAccess = access
		return fx.svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI, nil)
	}
	has := func(set map[string]struct{}, name string) bool { _, ok := set[name]; return ok }

	// Native prefix (no override): the account's own "a/gpt-4o" is reachable, so
	// the redirect must see it as callable and existing.
	nativeAccess := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a"}}}
	off := offerFor(nativeAccess)
	if !has(off.Callable, "a/gpt-4o") {
		t.Errorf("Callable missing native vendor model a/gpt-4o; got %v", off.Callable)
	}
	if !has(off.Existing, "a/gpt-4o") {
		t.Errorf("Existing missing native vendor model a/gpt-4o; got %v", off.Existing)
	}

	// Empty override prefix (the reported bug): the bare upstream name "gpt-4o"
	// is what the token requests, so it must be reachable in the offering too.
	emptyOverride := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a", OverrideEnabled: true, OverridePrefix: ""}}}
	off = offerFor(emptyOverride)
	if !has(off.Callable, "gpt-4o") {
		t.Errorf("Callable missing bare vendor model gpt-4o under empty override; got %v", off.Callable)
	}
	if !has(off.Existing, "gpt-4o") {
		t.Errorf("Existing missing bare vendor model gpt-4o under empty override; got %v", off.Existing)
	}
	// A non-granted account's model must not leak in (strict opt-in).
	if has(off.Callable, "b/gpt-4o") {
		t.Errorf("Callable leaked non-granted account model b/gpt-4o; got %v", off.Callable)
	}

	// Flavor isolation: the vendor model must not appear under a flavor it does
	// not serve. An OpenAI api-key account serves openai and anthropic, not
	// openai_images, so the overlay's flavor filter must drop it here.
	imgToken := fx.owner
	imgToken.VendorAccess = emptyOverride
	imgOff := fx.svc.ModelOfferingFor(ctx, imgToken, routing.APIFlavorOpenAIImages, nil)
	if has(imgOff.Callable, "gpt-4o") || has(imgOff.Existing, "gpt-4o") {
		t.Errorf("vendor model gpt-4o leaked into openai_images, a flavor it does not serve; callable=%v existing=%v", imgOff.Callable, imgOff.Existing)
	}

	// With no vendor access granted, no vendor model is reachable.
	off = offerFor(auth.VendorAccess{})
	if has(off.Callable, "a/gpt-4o") || has(off.Callable, "gpt-4o") {
		t.Errorf("Callable exposed a vendor model without access; got %v", off.Callable)
	}
}
