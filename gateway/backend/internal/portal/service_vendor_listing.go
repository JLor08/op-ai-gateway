// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
)

// ownVendorAccountModels is the single source every owner-facing listing reads
// the principal's vendor-account models from: each of the PRINCIPAL's own ACTIVE
// vendor accounts together with the models it serves. The listing overlay
// (vendorModelFlavorSets: /v1/models, the Anthropic list, the chat picker,
// /api/v0/models) and the dashboard's route table (vendorDashboardRoutes) both
// go through it, so which accounts and models they surface cannot drift.
//
// Returns nil (nothing to overlay) unless the vendor_accounts_enabled master flag
// is on, the principal is a USER (a service token has no UserID and owns no vendor
// accounts), and a routing store is wired. Best-effort and FAIL-OPEN like the
// group overlay: a store error yields what was read so far (nil on the first
// read) rather than blanking the whole listing, and an account whose model rows
// cannot be read is skipped.
func (s *Service) ownVendorAccountModels(ctx context.Context, token auth.Token) []vendorAccountModels {
	if s.routes == nil || token.UserID == "" || !s.VendorAccountsEnabled(ctx) {
		return nil
	}
	accounts, err := s.routes.VendorAccountsByOwner(ctx, token.UserID)
	if err != nil {
		return nil
	}
	var out []vendorAccountModels
	for _, acc := range accounts {
		if acc.Status != routing.VendorAccountStatusActive {
			continue
		}
		models, err := s.routes.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			continue
		}
		out = append(out, vendorAccountModels{Account: acc, Models: models})
	}
	return out
}

// vendorAccountModels is one active vendor account with the models it serves.
type vendorAccountModels struct {
	Account routing.VendorAccount
	Models  []routing.VendorAccountModel
}

// vendorModelFlavorSets returns, for the PRINCIPAL, each gateway model one of
// their OWN active vendor accounts serves → the coarse inbound flavors dispatch
// serves it under. It is the listing half of the resolver's resolveVendorAccount
// branch, and the two are deliberately kept in lock-step (served_flavors parity):
//
//   - Most accounts (api_key OpenAI/Anthropic, Anthropic subscription) serve a
//     model over BOTH the openai and anthropic inbound dialects via translate (their
//     Target carries APIFlavors [openai, anthropic]), so the model maps to
//     {openai, anthropic} here.
//   - An OpenAI SUBSCRIPTION account (the ChatGPT backend, Responses-only) serves a
//     model over the OPENAI dialect only — openai_responses via native passthrough
//     and chat/completions via translate, both coarse "openai" — and NOT the
//     anthropic dialect (its Target carries [openai] only). So its models map to
//     {openai} here. Listing them under anthropic would put them in
//     /anthropic/v1/models where an anthropic call then 404s, breaking parity.
//
// Gating, owner scope and the fail-open behaviour are ownVendorAccountModels's.
func (s *Service) vendorModelFlavorSets(ctx context.Context, token auth.Token) map[string]map[string]struct{} {
	owned := s.ownVendorAccountModels(ctx, token)
	if owned == nil {
		return nil
	}
	out := make(map[string]map[string]struct{})
	for _, am := range owned {
		flavors := vendorAccountServedFlavors(am.Account)
		for _, m := range am.Models {
			set := out[m.GatewayModel]
			if set == nil {
				set = make(map[string]struct{})
				out[m.GatewayModel] = set
			}
			for _, f := range flavors {
				set[f] = struct{}{}
			}
		}
	}
	return out
}

// vendorDashboardRoutes is the dashboard half of the owner overlay: one row per
// model of the principal's own active vendor accounts, under the model's
// PREFIXED gateway name (the id a caller requests). A vendor model has no
// server and no mapping, so the row carries the vendor as its provider, the
// account's name as its host and, as its id, the account id and the model (the
// pair is the vendor_account_models key, so two accounts of the same name
// serving the same model still get distinct, stably ordered rows); its status
// is active, because only active accounts are read.
// Like the model listings' overlay it is NOT subject to the hidden/locked
// suppression (that is a gateway-wide setting on self-hosted models, and these
// are the principal's own), and it never reaches the admin management surface.
func (s *Service) vendorDashboardRoutes(ctx context.Context, token auth.Token) []RouteDTO {
	var out []RouteDTO
	for _, am := range s.ownVendorAccountModels(ctx, token) {
		for _, m := range am.Models {
			out = append(out, RouteDTO{
				ID:       am.Account.ID + ":" + m.GatewayModel,
				Model:    m.GatewayModel,
				Provider: am.Account.Vendor,
				Host:     am.Account.Name,
				Status:   routing.ServerStatusActive,
			})
		}
	}
	return out
}

// vendorAccountServedFlavors is the dialects an account's models are dispatched
// under. An OpenAI subscription account serves the openai dialect only (the ChatGPT
// backend speaks Responses only); every other account serves both via translate.
func vendorAccountServedFlavors(acc routing.VendorAccount) []string {
	if acc.AuthType == routing.VendorAuthSubscription && acc.Vendor == routing.VendorOpenAI {
		return []string{routing.APIFlavorOpenAI}
	}
	return []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}
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
