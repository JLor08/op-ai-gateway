// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

// vendorAccountColumns is the single column list every vendor_accounts reader
// selects, in the order scanVendorAccount scans them.
const vendorAccountColumns = `id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at`

// CreateVendorAccount inserts a new account. The credential columns are stored
// exactly as given -- the caller seals them first. A duplicate id is
// ErrConflict.
func (s *SQLiteStore) CreateVendorAccount(ctx context.Context, a routing.VendorAccount) error {
	_, err := s.exec(ctx, `insert into vendor_accounts (`+vendorAccountColumns+`)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OwnerUserID, a.Vendor, a.AuthType, a.Name, a.Status, a.APIKey, a.OAuthTokens, a.ModelPrefix, a.BaseURL, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create vendor account: %w", err)
	}
	return nil
}

// UpdateVendorAccount rewrites the mutable columns of an existing account
// (auth_type, name, status, api_key, oauth_tokens, model_prefix, updated_at).
// id, owner_user_id, vendor and created_at are the account's identity, and
// base_url is fixed at create; none of them is ever written, matching the
// memory driver. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateVendorAccount(ctx context.Context, a routing.VendorAccount) error {
	result, err := s.exec(ctx, `
		update vendor_accounts
		set auth_type = ?, name = ?, status = ?, api_key = ?, oauth_tokens = ?, model_prefix = ?, updated_at = ?
		where id = ?`,
		a.AuthType, a.Name, a.Status, a.APIKey, a.OAuthTokens, a.ModelPrefix, a.UpdatedAt, a.ID)
	if err != nil {
		return fmt.Errorf("update vendor account: %w", err)
	}
	return requireAffected(result)
}

// SetVendorAccountOAuthTokens writes ONLY the oauth_tokens + updated_at columns
// of an existing account. It is the narrow dispatch-time refresh writer: it
// leaves auth_type, name, status and api_key untouched so a token refresh cannot
// clobber a concurrent rename or status change (unlike full-row
// UpdateVendorAccount). sealed is the already-sealed envelope. An unknown id is
// ErrNotFound.
func (s *SQLiteStore) SetVendorAccountOAuthTokens(ctx context.Context, accountID, sealed string) error {
	res, err := s.exec(ctx, `
		update vendor_accounts
		set oauth_tokens = ?, updated_at = ?
		where id = ?`,
		sealed, time.Now().UTC(), accountID)
	if err != nil {
		return fmt.Errorf("set vendor account oauth tokens: %w", err)
	}
	return requireAffected(res)
}

// SetVendorAccountStatus writes ONLY the status + updated_at columns of an
// existing account — the narrow needs_reconnect writer, touching no credential
// or identity column. An unknown id is ErrNotFound.
func (s *SQLiteStore) SetVendorAccountStatus(ctx context.Context, accountID, status string) error {
	res, err := s.exec(ctx, `
		update vendor_accounts
		set status = ?, updated_at = ?
		where id = ?`,
		status, time.Now().UTC(), accountID)
	if err != nil {
		return fmt.Errorf("set vendor account status: %w", err)
	}
	return requireAffected(res)
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

// VendorAccountModels returns the models accountID serves, ordered by
// gateway_model. The slice is always non-nil.
func (s *SQLiteStore) VendorAccountModels(ctx context.Context, accountID string) ([]routing.VendorAccountModel, error) {
	rows, err := s.query(ctx, `
		select account_id, gateway_model, upstream_model, api_flavor, display_name
		from vendor_account_models where account_id = ? order by gateway_model`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list vendor account models: %w", err)
	}
	defer rows.Close()
	out := make([]routing.VendorAccountModel, 0)
	for rows.Next() {
		var m routing.VendorAccountModel
		if err := rows.Scan(&m.AccountID, &m.GatewayModel, &m.UpstreamModel, &m.APIFlavor, &m.DisplayName); err != nil {
			return nil, fmt.Errorf("scan vendor account model: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vendor account models: %w", err)
	}
	return out, nil
}

// SetVendorAccountModels atomically REPLACES accountID's whole model set
// (delete-then-insert in one transaction, like SetRuntimeSpecGPUs). The account
// must exist (an empty set on an unknown account is still ErrNotFound). A
// duplicate gateway_model within the set violates the (account_id,
// gateway_model) primary key and surfaces ErrConflict, rolling the delete back
// so the previous set survives. Every row is written under accountID whatever
// its own AccountID says.
func (s *SQLiteStore) SetVendorAccountModels(ctx context.Context, accountID string, models []routing.VendorAccountModel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set vendor account models tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from vendor_accounts where id = ?`), accountID).Scan(&exists); err != nil {
		return fmt.Errorf("check vendor account: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from vendor_account_models where account_id = ?`), accountID); err != nil {
		return fmt.Errorf("clear vendor account models: %w", err)
	}
	for _, m := range models {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into vendor_account_models (account_id, gateway_model, upstream_model, api_flavor, display_name)
			values (?, ?, ?, ?, ?)`),
			accountID, m.GatewayModel, m.UpstreamModel, m.APIFlavor, m.DisplayName); err != nil {
			// accountID was existence-checked above and no other column is an
			// FK, so a failed insert is a duplicate (account_id, gateway_model).
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			if s.dl.isForeignKeyViolation(err) {
				// The account vanished between the check and the insert.
				return ErrNotFound
			}
			return fmt.Errorf("insert vendor account model: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit vendor account models: %w", err)
	}
	return nil
}

// UpsertVendorAccountUsage inserts or replaces accountID's single rate-limit
// usage-snapshot row (vendor_account_usage, keyed by account_id). The whole row
// is overwritten on conflict. An unknown account id is ErrNotFound (the FK). The
// nullable reset columns (five-hour, weekly and spend) take the *time.Time
// pointers directly (nil => NULL).
func (s *SQLiteStore) UpsertVendorAccountUsage(ctx context.Context, u routing.VendorAccountUsage) error {
	_, err := s.exec(ctx, `
		insert into vendor_account_usage (
			account_id, five_hour_pct, five_hour_reset_at, weekly_pct, weekly_reset_at, credit_balance,
			spend_unit, spend_limit, spend_used, spend_remaining, spend_used_pct, spend_reset_at, credit_status,
			updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(account_id) do update set
			five_hour_pct = excluded.five_hour_pct,
			five_hour_reset_at = excluded.five_hour_reset_at,
			weekly_pct = excluded.weekly_pct,
			weekly_reset_at = excluded.weekly_reset_at,
			credit_balance = excluded.credit_balance,
			spend_unit = excluded.spend_unit,
			spend_limit = excluded.spend_limit,
			spend_used = excluded.spend_used,
			spend_remaining = excluded.spend_remaining,
			spend_used_pct = excluded.spend_used_pct,
			spend_reset_at = excluded.spend_reset_at,
			credit_status = excluded.credit_status,
			updated_at = excluded.updated_at`,
		u.AccountID, u.FiveHourPct, u.FiveHourResetAt, u.WeeklyPct, u.WeeklyResetAt, u.CreditBalance,
		u.SpendUnit, u.SpendLimit, u.SpendUsed, u.SpendRemaining, u.SpendUsedPct, u.SpendResetAt, u.CreditStatus,
		u.UpdatedAt)
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("upsert vendor account usage: %w", err)
	}
	return nil
}

// VendorAccountUsageByID returns accountID's usage snapshot; ok is false when no
// snapshot row exists yet (not an error).
func (s *SQLiteStore) VendorAccountUsageByID(ctx context.Context, accountID string) (routing.VendorAccountUsage, bool, error) {
	row := s.queryRow(ctx, `
		select account_id, five_hour_pct, five_hour_reset_at, weekly_pct, weekly_reset_at, credit_balance,
			spend_unit, spend_limit, spend_used, spend_remaining, spend_used_pct, spend_reset_at, credit_status,
			updated_at
		from vendor_account_usage where account_id = ?`, accountID)
	u, err := scanVendorAccountUsage(row)
	if errors.Is(err, ErrNotFound) {
		return routing.VendorAccountUsage{}, false, nil
	}
	if err != nil {
		return routing.VendorAccountUsage{}, false, err
	}
	return u, true, nil
}

func scanVendorAccountUsage(row rowScanner) (routing.VendorAccountUsage, error) {
	var u routing.VendorAccountUsage
	var fiveHourReset, weeklyReset, spendReset sql.NullTime
	err := row.Scan(&u.AccountID, &u.FiveHourPct, &fiveHourReset, &u.WeeklyPct, &weeklyReset, &u.CreditBalance,
		&u.SpendUnit, &u.SpendLimit, &u.SpendUsed, &u.SpendRemaining, &u.SpendUsedPct, &spendReset, &u.CreditStatus,
		&u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.VendorAccountUsage{}, ErrNotFound
	}
	if err != nil {
		return routing.VendorAccountUsage{}, fmt.Errorf("scan vendor account usage: %w", err)
	}
	if fiveHourReset.Valid {
		u.FiveHourResetAt = &fiveHourReset.Time
	}
	if weeklyReset.Valid {
		u.WeeklyResetAt = &weeklyReset.Time
	}
	if spendReset.Valid {
		u.SpendResetAt = &spendReset.Time
	}
	return u, nil
}

func scanVendorAccount(row rowScanner) (routing.VendorAccount, error) {
	var a routing.VendorAccount
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.Vendor, &a.AuthType, &a.Name, &a.Status, &a.APIKey, &a.OAuthTokens, &a.ModelPrefix, &a.BaseURL, &a.CreatedAt, &a.UpdatedAt)
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
