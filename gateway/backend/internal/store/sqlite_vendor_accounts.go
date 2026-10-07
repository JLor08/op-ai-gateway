// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
)

// vendorAccountColumns is the single column list every vendor_accounts reader
// selects, in the order scanVendorAccount scans them.
const vendorAccountColumns = `id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, created_at, updated_at`

// CreateVendorAccount inserts a new account. The credential columns are stored
// exactly as given -- the caller seals them first. A duplicate id is
// ErrConflict.
func (s *SQLiteStore) CreateVendorAccount(ctx context.Context, a routing.VendorAccount) error {
	_, err := s.exec(ctx, `insert into vendor_accounts (`+vendorAccountColumns+`)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OwnerUserID, a.Vendor, a.AuthType, a.Name, a.Status, a.APIKey, a.OAuthTokens, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create vendor account: %w", err)
	}
	return nil
}

// UpdateVendorAccount rewrites the mutable columns of an existing account.
// id, owner_user_id, vendor and created_at are the account's identity and are
// never written, matching the memory driver. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateVendorAccount(ctx context.Context, a routing.VendorAccount) error {
	result, err := s.exec(ctx, `
		update vendor_accounts
		set auth_type = ?, name = ?, status = ?, api_key = ?, oauth_tokens = ?, updated_at = ?
		where id = ?`,
		a.AuthType, a.Name, a.Status, a.APIKey, a.OAuthTokens, a.UpdatedAt, a.ID)
	if err != nil {
		return fmt.Errorf("update vendor account: %w", err)
	}
	return requireAffected(result)
}

// VendorAccountByID returns the account or ErrNotFound.
func (s *SQLiteStore) VendorAccountByID(ctx context.Context, id string) (routing.VendorAccount, error) {
	row := s.queryRow(ctx, `select `+vendorAccountColumns+` from vendor_accounts where id = ?`, id)
	return scanVendorAccount(row)
}

// VendorAccounts lists every account, ordered by id.
func (s *SQLiteStore) VendorAccounts(ctx context.Context) ([]routing.VendorAccount, error) {
	rows, err := s.query(ctx, `select `+vendorAccountColumns+` from vendor_accounts order by id`)
	if err != nil {
		return nil, fmt.Errorf("list vendor accounts: %w", err)
	}
	defer rows.Close()
	return scanVendorAccounts(rows)
}

// VendorAccountsByOwner lists the accounts owned by userID, ordered by id.
func (s *SQLiteStore) VendorAccountsByOwner(ctx context.Context, userID string) ([]routing.VendorAccount, error) {
	rows, err := s.query(ctx, `select `+vendorAccountColumns+` from vendor_accounts where owner_user_id = ? order by id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list vendor accounts by owner: %w", err)
	}
	defer rows.Close()
	return scanVendorAccounts(rows)
}

// DeleteVendorAccount removes the account; its model-catalog and usage-snapshot
// rows go with it through their ON DELETE CASCADE FKs. An unknown id is
// ErrNotFound.
func (s *SQLiteStore) DeleteVendorAccount(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from vendor_accounts where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete vendor account: %w", err)
	}
	return requireAffected(res)
}

func scanVendorAccount(row rowScanner) (routing.VendorAccount, error) {
	var a routing.VendorAccount
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.Vendor, &a.AuthType, &a.Name, &a.Status, &a.APIKey, &a.OAuthTokens, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.VendorAccount{}, ErrNotFound
	}
	if err != nil {
		return routing.VendorAccount{}, fmt.Errorf("scan vendor account: %w", err)
	}
	return a, nil
}

func scanVendorAccounts(rows *sql.Rows) ([]routing.VendorAccount, error) {
	accounts := make([]routing.VendorAccount, 0)
	for rows.Next() {
		a, err := scanVendorAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vendor accounts: %w", err)
	}
	return accounts, nil
}
