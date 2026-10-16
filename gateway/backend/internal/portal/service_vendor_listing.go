// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
)

// ownVendorAccountModels is the single source every principal-facing listing reads
// the principal's vendor-account models from: each ACTIVE vendor account the
// principal can ROUTE to, together with the models it serves. The listing overlay
// (vendorModelFlavorSets: /v1/models, the Anthropic list, the chat picker,
// /api/v1/models) and the dashboard's route table (vendorDashboardRoutes) both
// go through it, so which accounts and models they surface cannot drift.
//
// The accounts are enumerated by VendorAccountsForRouting, the very enumerator the
// resolver routes by (#183, spec 2026-10-15 section 8): the principal's OWN accounts
// first, then the accounts SHARED with it through a resource group it is provisioned
// into, with the same live share level and the same fail direction. That is what
// makes ADVERTISED == ROUTABLE: a shared account the principal can route to is
// advertised, one it cannot is not, with the master sharing flag off only the own
// accounts are, and when the shared arm cannot be computed the shared accounts are
// left out here exactly as the resolver leaves them out (the own accounts stay). The
// name of this function is the one it had before sharing; the set is no longer
// owner-only.
//
// Only the accounts the TOKEN may use are listed (token.VendorAccess, strict by
// default: none unless the token opts in) and each is named under the
// token-effective prefix (routing.TokenVendorPrefix, the same helper the resolver
// uses, so a listed name is exactly a routable name). Downstream consumers key on
// the relabeled GatewayModel. Reach and opt-in are separate questions: the
// enumerator answers the first, the token's policy the second.
//
// Returns nil (nothing to overlay) unless the vendor_accounts_enabled master flag
// is on, the principal carries an identity the grant can be asked about (a USER, or
// a SERVICE token with its service id: vendorListingPrincipal, the resolver's own
// eligibility), and a routing store is wired. While the vendor_subscriptions_enabled
// switch is off, SUBSCRIPTION accounts are left out too (their api_key accounts are
// not), mirroring the resolver's skip so that no model is advertised that cannot be
// routed. Both switches come from ONE settings read, because the store read is an
// uncached full-table SELECT; the enumerator reads the sharing flags itself, as it
// does for the resolver.
//
// Best-effort and FAIL-OPEN like the group overlay: a store error yields what was
// read so far (nil on the first read) rather than blanking the whole listing, and
// an account whose model rows cannot be read is skipped.
//
// The second result, loaded, says whether that set is COMPLETE, so a consumer
// that must not act on a partial one can tell. It is true when no vendor ACCOUNT
// or MODEL read failed, INCLUDING the no-vendor-access cases above (a
// legitimately empty set is not a failure), and false when the principal's own
// accounts could not be read (VendorAccountsForRouting: nil, false) or an account's
// model rows could not (VendorAccountModels: the partial set, false). A failure of
// the SHARED arm alone is not an incomplete set: it is the routable set, the one
// the resolver uses in that same window. The listing overlays ignore loaded and
// stay fail-open; the generated-alias derivation fails CLOSED on it, because its
// vendor-shadow exclusion needs the whole set.
//
// The vendor_accounts_enabled master-flag read is NOT covered: a missing or
// unreadable settings store counts as the flag being off (it fails closed, like
// Service.VendorAccountsEnabled), which lands in the no-vendor-access case above
// (nil, true), so a failed flag read stays fail-open for the alias derivation too,
// as it does in the resolver.
func (s *Service) ownVendorAccountModels(ctx context.Context, token auth.Token) (owned []vendorAccountModels, loaded bool) {
	if s.routes == nil || !vendorListingPrincipal(token) || s.settings == nil {
		return nil, true
	}
	// One settings read for both switches: the master flag (off when the store
	// cannot be read) and the subscriptions switch (the pure readers, so default-on
	// for an absent or unparseable value).
	values, err := s.settings.SystemSettings(ctx)
	if err != nil || !VendorAccountsEnabled(values) {
		return nil, true
	}
	accounts, err := s.VendorAccountsForRouting(ctx, token)
	if err != nil {
		return nil, false
	}
	return s.advertisedVendorAccountModels(ctx, token, accounts, VendorSubscriptionsEnabled(values))
}

// vendorListingPrincipal reports whether token carries an identity the vendor grant
// can be asked about: a USER (UserID set) or a SERVICE token with a service id. It
// is the resolver's eligibility (routing.vendorPrincipalEligible) and the two must
// move together, or an advertised name stops being a routable one: a token with
// neither identity owns nothing and is provisioned nowhere, and a service token
// whose id is blank must not be asked either.
func vendorListingPrincipal(token auth.Token) bool {
	return token.UserID != "" || (token.IsService() && token.ServiceID != "")
}

// advertisedVendorAccountModels keeps, from the enumerated accounts, the ones the
// resolver would serve for this token (active, the subscriptions switch, token
// access) together with their models under the token-effective prefix. accounts is in
// the enumerator's order and the result keeps it: it is the precedence the resolver
// applies, which decides which account serves a name on a dialect. loaded is false
// when an account's models could not be read; the accounts that could be are still
// returned.
func (s *Service) advertisedVendorAccountModels(ctx context.Context, token auth.Token, accounts []routing.VendorAccount, subscriptionsOn bool) (out []vendorAccountModels, loaded bool) {
	loaded = true
	for _, acc := range accounts {
		if acc.Status != routing.VendorAccountStatusActive {
			continue
		}
		// While vendor_subscriptions_enabled is off a SUBSCRIPTION account is not
		// advertised, exactly as the resolver (resolveVendorAccount) no longer routes it,
		// so the two stay in lock-step. api_key accounts are untouched.
		if acc.AuthType == routing.VendorAuthSubscription && !subscriptionsOn {
			continue
		}
		prefix, allowed := routing.TokenVendorPrefix(token, acc)
		if !allowed {
			continue
		}
		models, err := s.routes.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			// Keep collecting the accounts that can be read (the listing overlay
			// folds in what it has), but the set is no longer complete.
			loaded = false
			continue
		}
		// Token-effective public names (prefix + upstream model). With All or no
		// override the prefix is the account's own, so this equals the stored
		// GatewayModel and the listing is unchanged.
		models, _ = relabelVendorModels(models, prefix)
		out = append(out, vendorAccountModels{Account: acc, Models: models})
	}
	return out, loaded
}

// vendorAccountModels is one active vendor account with the models it serves.
type vendorAccountModels struct {
	Account routing.VendorAccount
	Models  []routing.VendorAccountModel
}

// vendorModelFlavorSets returns, for the PRINCIPAL, each gateway model one of
// the vendor accounts it can route to serves → the coarse inbound flavors dispatch
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
// A name several accounts serve is advertised under the UNION of the dialects they
// serve it on, because the resolver chooses an account per (name, dialect), not per
// name (spec 2026-10-15 section 8, Precedence & parity). The accounts arrive in the
// resolver's order (the principal's own accounts first, then the shared ones by id) and
// the precedence winner decides WHICH account serves each dialect: the first account,
// in that order, that serves the name on it. An account that does not serve the
// requested dialect (an OpenAI subscription for the anthropic one) is skipped and the
// request falls through to the next account, so a name whose first account is
// openai-only is still routable on the anthropic dialect through a later both-dialect
// account, and is advertised on it. A dialect is in a name's set exactly when some
// eligible account serves the name on it, which is exactly when the resolver can
// route it: the union never advertises a dialect that is not routable, and never drops
// one that is. Only active, reachable, token-allowed accounts get here, so an inactive
// account contributes nothing.
//
// Gating, reach, principal scope and the fail-open behaviour are
// ownVendorAccountModels's.
//
// loaded is ownVendorAccountModels's: false exactly when the set is incomplete
// because a vendor account or model read failed (the map is then nil or partial),
// true for a complete set and for the legitimately empty no-vendor-access one
// (an unreadable vendor_accounts_enabled flag counts as that, fail-open like the
// resolver). The listing overlay uses the map either way (fail-open); the alias
// derivation refuses an incomplete one (fail-closed).
func (s *Service) vendorModelFlavorSets(ctx context.Context, token auth.Token) (sets map[string]map[string]struct{}, loaded bool) {
	owned, loaded := s.ownVendorAccountModels(ctx, token)
	if owned == nil {
		return nil, loaded
	}
	out := make(map[string]map[string]struct{})
	for _, am := range owned {
		flavors := vendorAccountServedFlavors(am.Account)
		for _, m := range am.Models {
			set := out[m.GatewayModel]
			if set == nil {
				set = make(map[string]struct{}, len(flavors))
				out[m.GatewayModel] = set
			}
			for _, f := range flavors {
				set[f] = struct{}{}
			}
		}
	}
	return out, loaded
}

// vendorDashboardRoutes is the dashboard half of the vendor overlay: one row per
// model of every active vendor account the principal can route to (its own and the
// ones shared with it), under the model's PREFIXED gateway name (the id a caller
// requests). A vendor model has no server and no mapping, so the row carries the
// vendor as its provider, the account's name as its host and, as its id, the account
// id and the model (the pair is the vendor_account_models key, so two accounts of the
// same name serving the same model still get distinct, stably ordered rows); its
// status is active, because only active accounts are read.
// Like the model listings' overlay it is NOT subject to the hidden/locked
// suppression (that is a gateway-wide setting on self-hosted models, and these
// are the principal's own or shared with it), and it never reaches the admin
// management surface.
func (s *Service) vendorDashboardRoutes(ctx context.Context, token auth.Token) []RouteDTO {
	var out []RouteDTO
	owned, _ := s.ownVendorAccountModels(ctx, token) // best-effort listing: a partial set is shown as is
	for _, am := range owned {
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

// overlayVendorModels unions a principal's vendor-model flavor overlay (the
// vendorModelFlavorSets result the caller has read) into an existing per-name
// flavor set map, in place. Both listing paths use it: the per-flavor path
// (modelFlavorSets → ModelsForFlavor, the ids-only reference the HTTP listings
// are pinned against; no gateway handler reads it) and the DTO path
// (modelsResponse → Models() → the chat picker, plus /v1/models, /api/v1/models
// and /anthropic/v1/models through openAIModelDTOs and anthropicModelDTOs). A
// name a vendor account shares with a self-hosted model simply gets the union of
// their flavors, so there is one entry per name (deduped).
//
// The caller reads vendor once and hands the same set to the generated-alias
// derivation (claudeAliasesFromListing) too, so a listing costs one vendor
// store read, not one per consumer. vendor is only read here, and each flavor is
// copied into sets, so sets never aliases it.
//
// It overlays whatever vendor holds, nil or partial after a vendor store-read
// error included: the listing fails OPEN and never blanks. It is the alias
// derivation, not this overlay, that refuses an incomplete set (vendorLoaded).
func overlayVendorModels(sets, vendor map[string]map[string]struct{}) {
	for name, flavors := range vendor {
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
