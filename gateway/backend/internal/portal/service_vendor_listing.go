// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
)

// vendorModelFlavorSets returns, for the PRINCIPAL, each gateway model one of
// their OWN active vendor accounts serves → the coarse inbound flavors dispatch
// serves it under. It is the listing half of the resolver's resolveVendorAccount
// branch, and the two are deliberately kept in lock-step (served_flavors parity):
// dispatch serves a vendor model over BOTH the openai and anthropic inbound
// dialects (the vendor Target carries APIFlavors [openai, anthropic] with
// translate endpoint modes) and never over images, so every served model maps to
// {openai, anthropic} here.
//
// Returns nil (no overlay) unless the vendor_accounts_enabled master flag is on,
// the principal is a USER (a service token has no UserID and owns no vendor
// accounts), and a routing store is wired. Best-effort and FAIL-OPEN like the
// group overlay: a store error yields the overlay built so far (nil on the first
// read) rather than blanking the whole listing.
func (s *Service) vendorModelFlavorSets(ctx context.Context, token auth.Token) map[string]map[string]struct{} {
	if s.routes == nil || token.UserID == "" || !s.VendorAccountsEnabled(ctx) {
		return nil
	}
	accounts, err := s.routes.VendorAccountsByOwner(ctx, token.UserID)
	if err != nil {
		return nil
	}
	out := make(map[string]map[string]struct{})
	for _, acc := range accounts {
		if acc.Status != routing.VendorAccountStatusActive {
			continue
		}
		models, err := s.routes.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			continue
		}
		for _, m := range models {
			set := out[m.GatewayModel]
			if set == nil {
				set = make(map[string]struct{})
				out[m.GatewayModel] = set
			}
			set[routing.APIFlavorOpenAI] = struct{}{}
			set[routing.APIFlavorAnthropic] = struct{}{}
		}
	}
	return out
}

// overlayVendorModels unions the principal's vendor-model flavor overlay into an
// existing per-name flavor set map, in place. Both listing paths use it: the
// per-flavor path (modelFlavorSets → ModelsForFlavor → /v1/models, the Anthropic
// list) and the DTO path (modelsResponse → Models() → the chat picker,
// /api/v0/models). A name a vendor account shares with a self-hosted model simply
// gets the union of their flavors, so there is one entry per name (deduped).
func (s *Service) overlayVendorModels(ctx context.Context, token auth.Token, sets map[string]map[string]struct{}) {
	for name, flavors := range s.vendorModelFlavorSets(ctx, token) {
		set := sets[name]
		if set == nil {
			set = make(map[string]struct{})
			sets[name] = set
		}
		for f := range flavors {
			set[f] = struct{}{}
		}
	}
}
