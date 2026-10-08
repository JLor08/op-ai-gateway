// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
	"time"
)

// vendorAccountCleanupTimeout bounds the removal of a vendor account whose model
// catalog could not be seeded (see CreateVendorAccount). The cleanup runs on a
// context detached from the request, so this is its only deadline.
const vendorAccountCleanupTimeout = 5 * time.Second

// VendorAccountModelDTO is one gateway-model entry a vendor account serves: a
// row of vendor_account_models, seeded from the curated per-vendor catalog
// (VendorCatalog) when the account is created.
type VendorAccountModelDTO struct {
	GatewayModel  string `json:"gateway_model"`
	UpstreamModel string `json:"upstream_model"`
	APIFlavor     string `json:"api_flavor"`
}

// VendorAccountUsageDTO is the portal view of a routing.VendorAccountUsage: the
// latest rate-limit snapshot the gateway scraped off the account's upstream
// responses. It carries percentages and reset times only -- neither vendor
// exposes an absolute cap, so there is deliberately no "N of M". A percentage is
// 0..100, or -1 when that window is UNKNOWN (never observed): the portal must
// not read -1 as a real 0 % used. A reset time is nil when the vendor sent none;
// CreditBalance is the vendor's raw credit string ("" = none).
type VendorAccountUsageDTO struct {
	FiveHourPct     float64    `json:"five_hour_pct"`
	FiveHourResetAt *time.Time `json:"five_hour_reset_at"`
	WeeklyPct       float64    `json:"weekly_pct"`
	WeeklyResetAt   *time.Time `json:"weekly_reset_at"`
	CreditBalance   string     `json:"credit_balance"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// VendorAccountDTO is the portal view of a routing.VendorAccount. It carries NO
// credential material: the sealed api key and OAuth token set are reduced to
// the APIKeySet / SubscriptionConnected booleans (write-only secrets), and the
// owner is implicit (every caller sees only their own accounts).
//
// Usage is the rate-limit snapshot and is filled by GetVendorAccount ONLY (the
// detail view's Usage & Limits panel): the list and the write endpoints leave it
// nil -- one snapshot read per row would be an N+1 on the list -- and so does a
// detail read of an account the gateway has not yet seen a rate-limit header
// for. A nil Usage is omitted from the JSON.
type VendorAccountDTO struct {
	ID                    string                  `json:"id"`
	Vendor                string                  `json:"vendor"`
	AuthType              string                  `json:"auth_type"`
	Name                  string                  `json:"name"`
	Status                string                  `json:"status"`
	APIKeySet             bool                    `json:"api_key_set"`
	SubscriptionConnected bool                    `json:"subscription_connected"`
	Models                []VendorAccountModelDTO `json:"models"`
	Usage                 *VendorAccountUsageDTO  `json:"usage,omitempty"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

// VendorAccountListResponse is the {data:[...]} envelope of the list endpoint.
type VendorAccountListResponse struct {
	Data []VendorAccountDTO `json:"data"`
}

// CreateVendorAccountRequest creates an account owned by the calling principal.
// APIKey is write-only and only meaningful for AuthType == api_key; a
// subscription account is created unconnected (the OAuth connect flow fills its
// token set later). An empty Status defaults to active.
type CreateVendorAccountRequest struct {
	Vendor   string `json:"vendor"`
	AuthType string `json:"auth_type"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	APIKey   string `json:"api_key"`
}

// UpdateVendorAccountRequest is a partial update: a nil field keeps the stored
// value. APIKey is the write-only secret sentinel: nil keeps the sealed key,
// "" clears it, any other value replaces it. Vendor and auth type are immutable.
type UpdateVendorAccountRequest struct {
	Name   *string `json:"name"`
	Status *string `json:"status"`
	APIKey *string `json:"api_key"`
}

// vendorAccountDTO maps a stored account to its credential-free view, reading
// the model rows the account serves from the store. Models is always a non-nil
// slice so it serializes as [] (never null).
func (s *Service) vendorAccountDTO(ctx context.Context, acc routing.VendorAccount) (VendorAccountDTO, error) {
	rows, err := s.routes.VendorAccountModels(ctx, acc.ID)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	models := make([]VendorAccountModelDTO, 0, len(rows))
	for _, row := range rows {
		models = append(models, VendorAccountModelDTO{
			GatewayModel:  row.GatewayModel,
			UpstreamModel: row.UpstreamModel,
			APIFlavor:     row.APIFlavor,
		})
	}
	return VendorAccountDTO{
		ID:                    acc.ID,
		Vendor:                acc.Vendor,
		AuthType:              acc.AuthType,
		Name:                  acc.Name,
		Status:                acc.Status,
		APIKeySet:             acc.APIKey != "",
		SubscriptionConnected: acc.OAuthTokens != "",
		Models:                models,
		CreatedAt:             acc.CreatedAt,
		UpdatedAt:             acc.UpdatedAt,
	}, nil
}

// vendorAccountUsageDTO reads accountID's rate-limit snapshot and maps it to its
// portal view. A nil result (and a nil error) means no snapshot has been scraped
// for the account yet. The store hands out its own copy of the snapshot, so the
// reset pointers are not shared with stored state.
func (s *Service) vendorAccountUsageDTO(ctx context.Context, accountID string) (*VendorAccountUsageDTO, error) {
	usage, ok, err := s.routes.VendorAccountUsageByID(ctx, accountID)
	if err != nil || !ok {
		return nil, err
	}
	return &VendorAccountUsageDTO{
		FiveHourPct:     usage.FiveHourPct,
		FiveHourResetAt: usage.FiveHourResetAt,
		WeeklyPct:       usage.WeeklyPct,
		WeeklyResetAt:   usage.WeeklyResetAt,
		CreditBalance:   usage.CreditBalance,
		UpdatedAt:       usage.UpdatedAt,
	}, nil
}

func normalizeVendorAccountVendor(raw string) (string, error) {
	switch vendor := strings.TrimSpace(raw); vendor {
	case routing.VendorOpenAI, routing.VendorAnthropic:
		return vendor, nil
	default:
		return "", ErrVendorAccountVendorInvalid
	}
}

func normalizeVendorAccountAuthType(raw string) (string, error) {
	switch authType := strings.TrimSpace(raw); authType {
	case routing.VendorAuthAPIKey, routing.VendorAuthSubscription:
		return authType, nil
	default:
		return "", ErrVendorAccountAuthTypeInvalid
	}
}

// normalizeVendorAccountStatus validates a status a USER may set: active or
// disabled. needs_reconnect is set by the system when a subscription's token
// refresh fails, so a user cannot assign it (a PATCH that merely echoes an
// account's current needs_reconnect status is let through by the caller, not here).
func normalizeVendorAccountStatus(raw string) (string, error) {
	switch status := strings.TrimSpace(raw); status {
	case routing.VendorAccountStatusActive, routing.VendorAccountStatusDisabled:
		return status, nil
	default:
		return "", ErrVendorAccountStatusInvalid
	}
}

// sealVendorAPIKey trims surrounding whitespace (a pasted key often carries a
// trailing newline) and seals the key for storage: "enc:" with a cipher,
// "plain:" on a volatile store, capture.ErrKeyRequired on a keyless disk store.
// An empty key seals to "".
func (s *Service) sealVendorAPIKey(raw string) (string, error) {
	return capture.SealSecret(s.cipher, s.settingsVolatile, strings.TrimSpace(raw))
}

// authorizeVendorAccount loads the account and returns ErrVendorAccountNotFound
// unless the principal OWNS it. With write=false (a READ) system scope is also
// let through, so an operator can inspect any account's credential-free
// metadata; with write=true the check is strictly owner-only. A vendor account
// is a personal credential, not shared infrastructure like a server, so the
// authorizeServer system bypass deliberately does not extend to writes: a
// system-scope non-owner may not rotate or clear another user's key, rename or
// disable the account, or delete it. As with authorizeServer, an unknown id and
// a stranger's account are indistinguishable (404-no-leak), and a plain admin
// who is not the owner is a stranger too.
func (s *Service) authorizeVendorAccount(ctx context.Context, principal auth.Token, id string, write bool) (routing.VendorAccount, error) {
	acc, err := s.routes.VendorAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return routing.VendorAccount{}, ErrVendorAccountNotFound
		}
		return routing.VendorAccount{}, err
	}
	if principal.UserID != "" && acc.OwnerUserID == principal.UserID {
		return acc, nil
	}
	if !write && isSystem(principal) {
		return acc, nil
	}
	return routing.VendorAccount{}, ErrVendorAccountNotFound
}

// ListVendorAccounts returns the calling principal's OWN accounts, oldest first.
// Like every vendor-account method it is refused with ErrVendorAccountsDisabled
// (before anything else) while the vendor_accounts_enabled master flag is off.
// System scope does not widen the list: the page is a personal one and the DTO
// carries no owner, so a cross-user list would be unattributed.
func (s *Service) ListVendorAccounts(ctx context.Context, principal auth.Token) (VendorAccountListResponse, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountListResponse{}, err
	}
	out := make([]VendorAccountDTO, 0)
	if principal.UserID == "" {
		return VendorAccountListResponse{Data: out}, nil
	}
	accounts, err := s.routes.VendorAccountsByOwner(ctx, principal.UserID)
	if err != nil {
		return VendorAccountListResponse{}, err
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if !accounts[i].CreatedAt.Equal(accounts[j].CreatedAt) {
			return accounts[i].CreatedAt.Before(accounts[j].CreatedAt)
		}
		return accounts[i].ID < accounts[j].ID
	})
	for _, acc := range accounts {
		dto, err := s.vendorAccountDTO(ctx, acc)
		if err != nil {
			return VendorAccountListResponse{}, err
		}
		out = append(out, dto)
	}
	return VendorAccountListResponse{Data: out}, nil
}

// GetVendorAccount returns one account the principal owns (404-no-leak
// otherwise); system scope may read any account. It is the only read that also
// carries the account's rate-limit usage snapshot (Usage; nil until one has been
// scraped). ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) GetVendorAccount(ctx context.Context, principal auth.Token, id string) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.authorizeVendorAccount(ctx, principal, id, false)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	dto, err := s.vendorAccountDTO(ctx, acc)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if dto.Usage, err = s.vendorAccountUsageDTO(ctx, acc.ID); err != nil {
		return VendorAccountDTO{}, err
	}
	return dto, nil
}

// CreateVendorAccount creates an account owned by the calling principal
// (ErrVendorAccountsDisabled while the master flag is off). Any authenticated user may create their own (no admin requirement); a principal
// with no user identity cannot own one. The api key is sealed BEFORE the store
// write, so a keyless disk store fails with capture.ErrKeyRequired and persists
// nothing.
func (s *Service) CreateVendorAccount(ctx context.Context, principal auth.Token, req CreateVendorAccountRequest) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	if principal.UserID == "" {
		return VendorAccountDTO{}, ErrVendorAccountForbidden
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return VendorAccountDTO{}, ErrVendorAccountNameRequired
	}
	vendor, err := normalizeVendorAccountVendor(req.Vendor)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	authType, err := normalizeVendorAccountAuthType(req.AuthType)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = routing.VendorAccountStatusActive
	}
	if status, err = normalizeVendorAccountStatus(status); err != nil {
		return VendorAccountDTO{}, err
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey != "" && authType != routing.VendorAuthAPIKey {
		return VendorAccountDTO{}, ErrVendorAccountAPIKeyNotAllowed
	}
	sealedKey, err := s.sealVendorAPIKey(apiKey)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	now := s.clock().UTC()
	acc := routing.VendorAccount{
		ID:          "va_" + compactRandomHex(16),
		OwnerUserID: principal.UserID,
		Vendor:      vendor,
		AuthType:    authType,
		Name:        name,
		Status:      status,
		APIKey:      sealedKey,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.routes.CreateVendorAccount(ctx, acc); err != nil {
		return VendorAccountDTO{}, err
	}
	// Seed the vendor's curated model catalog so the account is routable from
	// the start. An account without its rows would silently serve nothing, so a
	// seeding failure fails the create and removes the half-made account.
	//
	// The most plausible reason a seed fails on a SQL driver is that ctx died
	// (the client went away, the request timed out); a cleanup on that same ctx
	// would fail identically and strand an unroutable account no one backfills.
	// So the cleanup runs on a context of its own: severed from ctx's
	// cancellation and bounded by vendorAccountCleanupTimeout. If it fails too,
	// the account is left behind unseeded and BOTH faults are returned, so the
	// double fault is visible rather than dropped.
	if err := s.routes.SetVendorAccountModels(ctx, acc.ID, VendorCatalog(vendor)); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vendorAccountCleanupTimeout)
		defer cancel()
		if delErr := s.routes.DeleteVendorAccount(cctx, acc.ID); delErr != nil {
			err = errors.Join(err, fmt.Errorf("remove unseeded vendor account %s: %w", acc.ID, delErr))
		}
		return VendorAccountDTO{}, err
	}
	return s.vendorAccountDTO(ctx, acc)
}

// UpdateVendorAccount renames an account, changes its status, and/or replaces
// or clears its api key. The store keeps id, owner, vendor and created_at
// immutable, so this loads the row, mutates only the requested fields and writes
// it back. The write is OWNER-ONLY (system scope included), and authorization
// runs first, so a stranger gets 404 even for an invalid body.
// ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) UpdateVendorAccount(ctx context.Context, principal auth.Token, id string, req UpdateVendorAccountRequest) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.authorizeVendorAccount(ctx, principal, id, true)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return VendorAccountDTO{}, ErrVendorAccountNameRequired
		}
		acc.Name = name
	}
	// A status equal to the stored one is a no-op, which lets a form echo back a
	// system-managed needs_reconnect without tripping the user-settable check.
	if req.Status != nil && strings.TrimSpace(*req.Status) != acc.Status {
		status, err := normalizeVendorAccountStatus(*req.Status)
		if err != nil {
			return VendorAccountDTO{}, err
		}
		acc.Status = status
	}
	if req.APIKey != nil {
		apiKey := strings.TrimSpace(*req.APIKey)
		// Only the exact empty string clears the key; a value that is blank
		// after trimming is a paste slip and must not silently wipe the key.
		if *req.APIKey != "" && apiKey == "" {
			return VendorAccountDTO{}, ErrVendorAccountAPIKeyInvalid
		}
		if apiKey != "" && acc.AuthType != routing.VendorAuthAPIKey {
			return VendorAccountDTO{}, ErrVendorAccountAPIKeyNotAllowed
		}
		sealedKey, err := s.sealVendorAPIKey(apiKey)
		if err != nil {
			return VendorAccountDTO{}, err
		}
		acc.APIKey = sealedKey
	}
	acc.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateVendorAccount(ctx, acc); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return VendorAccountDTO{}, ErrVendorAccountNotFound
		}
		return VendorAccountDTO{}, err
	}
	return s.vendorAccountDTO(ctx, acc)
}

// DeleteVendorAccount removes an account the principal owns (owner-only, system
// scope included); the store cascades its dependent rows (the model catalog).
// The bool reports that a row was removed (always true on a nil error): there is
// no best-effort side effect to flag, unlike DeleteServer.
// ErrVendorAccountsDisabled while the master flag is off.
func (s *Service) DeleteVendorAccount(ctx context.Context, principal auth.Token, id string) (bool, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return false, err
	}
	acc, err := s.authorizeVendorAccount(ctx, principal, id, true)
	if err != nil {
		return false, err
	}
	if err := s.routes.DeleteVendorAccount(ctx, acc.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, ErrVendorAccountNotFound
		}
		return false, err
	}
	return true, nil
}
