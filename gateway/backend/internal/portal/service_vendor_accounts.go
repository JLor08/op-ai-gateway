// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
	"time"
)

// VendorAccountModelDTO is one gateway-model entry a vendor account serves: a
// row of vendor_account_models, seeded from the curated per-vendor catalog
// (VendorCatalog) when the account is created.
type VendorAccountModelDTO struct {
	GatewayModel  string `json:"gateway_model"`
	UpstreamModel string `json:"upstream_model"`
	APIFlavor     string `json:"api_flavor"`
}

// VendorAccountDTO is the portal view of a routing.VendorAccount. It carries NO
// credential material: the sealed api key and OAuth token set are reduced to
// the APIKeySet / SubscriptionConnected booleans (write-only secrets), and the
// owner is implicit (every caller sees only their own accounts).
type VendorAccountDTO struct {
	ID                    string                  `json:"id"`
	Vendor                string                  `json:"vendor"`
	AuthType              string                  `json:"auth_type"`
	Name                  string                  `json:"name"`
	Status                string                  `json:"status"`
	APIKeySet             bool                    `json:"api_key_set"`
	SubscriptionConnected bool                    `json:"subscription_connected"`
	Models                []VendorAccountModelDTO `json:"models"`
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
// System scope does not widen the list: the page is a personal one and the DTO
// carries no owner, so a cross-user list would be unattributed.
func (s *Service) ListVendorAccounts(ctx context.Context, principal auth.Token) (VendorAccountListResponse, error) {
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
// otherwise); system scope may read any account.
func (s *Service) GetVendorAccount(ctx context.Context, principal auth.Token, id string) (VendorAccountDTO, error) {
	acc, err := s.authorizeVendorAccount(ctx, principal, id, false)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	return s.vendorAccountDTO(ctx, acc)
}

// CreateVendorAccount creates an account owned by the calling principal. Any
// authenticated user may create their own (no admin requirement); a principal
// with no user identity cannot own one. The api key is sealed BEFORE the store
// write, so a keyless disk store fails with capture.ErrKeyRequired and persists
// nothing.
func (s *Service) CreateVendorAccount(ctx context.Context, principal auth.Token, req CreateVendorAccountRequest) (VendorAccountDTO, error) {
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
	// seeding failure fails the create and removes the half-made account
	// (best effort: the cleanup's own error is dropped in favour of the cause).
	if err := s.routes.SetVendorAccountModels(ctx, acc.ID, VendorCatalog(vendor)); err != nil {
		_ = s.routes.DeleteVendorAccount(ctx, acc.ID)
		return VendorAccountDTO{}, err
	}
	return s.vendorAccountDTO(ctx, acc)
}

// UpdateVendorAccount renames an account, changes its status, and/or replaces
// or clears its api key. The store keeps id, owner, vendor and created_at
// immutable, so this loads the row, mutates only the requested fields and writes
// it back. The write is OWNER-ONLY (system scope included), and authorization
// runs first, so a stranger gets 404 even for an invalid body.
func (s *Service) UpdateVendorAccount(ctx context.Context, principal auth.Token, id string, req UpdateVendorAccountRequest) (VendorAccountDTO, error) {
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
// scope included); the store cascades its dependent rows (the model catalog). The
// bool reports that a row was removed (always true on a nil error): there is no best-effort side effect to flag, unlike DeleteServer.
func (s *Service) DeleteVendorAccount(ctx context.Context, principal auth.Token, id string) (bool, error) {
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
