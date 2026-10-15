// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// vendorAccountInsertColumns is the column list CreateVendorAccount writes. It
// deliberately omits share_level and system_group_id (migration 91): a new account
// is always private, so both take their column DEFAULTs ('none' and the empty
// string) whatever the routing.VendorAccount carries -- a zero-value struct must
// not write an empty string into share_level and bypass the DEFAULT. The two
// columns are changed only by their narrow writers (UpdateVendorAccountShareLevel,
// UpdateVendorAccountSystemGroup, ClearVendorAccountSharing).
const vendorAccountInsertColumns = `id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at`

// vendorAccountColumns is the single column list every vendor_accounts reader
// selects, in the order scanVendorAccount scans them: the insert columns, then
// the two sharing columns the narrow writers own.
const vendorAccountColumns = vendorAccountInsertColumns + `, share_level, system_group_id`

// CreateVendorAccount inserts a new account. The credential columns are stored
// exactly as given -- the caller seals them first. ShareLevel and SystemGroupID
// are ignored: the account starts at the column DEFAULTs (private). A duplicate id
// is ErrConflict.
func (s *SQLiteStore) CreateVendorAccount(ctx context.Context, a routing.VendorAccount) error {
	_, err := s.exec(ctx, `insert into vendor_accounts (`+vendorAccountInsertColumns+`)
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
// rows, its vendor_account_admin_groups links and its
// resource_group_vendor_accounts links go with it through their ON DELETE
// CASCADE FKs. An unknown id is ErrNotFound.
func (s *SQLiteStore) DeleteVendorAccount(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from vendor_accounts where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete vendor account: %w", err)
	}
	return requireAffected(res)
}

// UpdateVendorAccountShareLevel writes only share_level (migration 91) with a
// targeted UPDATE of that one column, so it cannot race a concurrent full-row
// write; updated_at is left alone, like UpdateServerSystemGroup. The level is
// stored verbatim -- the service validates it. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateVendorAccountShareLevel(ctx context.Context, id, level string) error {
	result, err := s.exec(ctx, `update vendor_accounts set share_level = ? where id = ?`, level, id)
	if err != nil {
		return fmt.Errorf("update vendor account share level: %w", err)
	}
	return requireAffected(result)
}

// UpdateVendorAccountSystemGroup writes only system_group_id -- the containment
// root (migration 91) -- with a targeted UPDATE of that one column, mirroring
// UpdateServerSystemGroup. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateVendorAccountSystemGroup(ctx context.Context, id, systemGroupID string) error {
	result, err := s.exec(ctx, `update vendor_accounts set system_group_id = ? where id = ?`, systemGroupID, id)
	if err != nil {
		return fmt.Errorf("update vendor account system group: %w", err)
	}
	return requireAffected(result)
}

// SetVendorAccountAdminGroup links accountID to groupID
// (vendor_account_admin_groups), idempotent on the (vendor_account_id,
// group_id) unique pair -- mirrors SetServerAdminGroup. A missing accountID or
// groupID surfaces ErrNotFound (FK violation).
func (s *SQLiteStore) SetVendorAccountAdminGroup(ctx context.Context, accountID, groupID string) error {
	_, err := s.exec(ctx, `insert into vendor_account_admin_groups (vendor_account_id, group_id, created_at)
		values (?, ?, ?) on conflict (vendor_account_id, group_id) do nothing`,
		accountID, groupID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set vendor account admin group: %w", err)
	}
	return nil
}

// RemoveVendorAccountAdminGroup unlinks accountID from groupID. A no-op
// (non-error) when the link does not exist -- mirrors RemoveServerAdminGroup.
func (s *SQLiteStore) RemoveVendorAccountAdminGroup(ctx context.Context, accountID, groupID string) error {
	_, err := s.exec(ctx, `delete from vendor_account_admin_groups where vendor_account_id = ? and group_id = ?`, accountID, groupID)
	if err != nil {
		return fmt.Errorf("remove vendor account admin group: %w", err)
	}
	return nil
}

// VendorAccountAdminGroups lists every admin-group id linked to accountID,
// ordered by created_at then group_id -- mirrors ServerAdminGroups. Always
// non-nil, empty when none.
func (s *SQLiteStore) VendorAccountAdminGroups(ctx context.Context, accountID string) ([]string, error) {
	rows, err := s.query(ctx, `select group_id from vendor_account_admin_groups where vendor_account_id = ? order by created_at, group_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list vendor account admin groups: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan vendor account admin group: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// VendorAccountsByAdminGroups returns every vendor account linked to ANY of
// groupIDs, deduped by account id (an account linked to more than one of the
// given groups is returned once), ordered by id -- mirrors ServersByAdminGroups.
// An empty groupIDs returns an empty slice without issuing a query.
//
// The link table is consulted through an IN subquery rather than a join so the
// select list is vendorAccountColumns itself, unprefixed: both tables carry a
// created_at, which a join would make ambiguous, and a hand-copied aliased
// column list is exactly the kind of reader that drifts from scanVendorAccount.
func (s *SQLiteStore) VendorAccountsByAdminGroups(ctx context.Context, groupIDs []string) ([]routing.VendorAccount, error) {
	if len(groupIDs) == 0 {
		return make([]routing.VendorAccount, 0), nil
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.query(ctx, `select `+vendorAccountColumns+` from vendor_accounts
		where id in (select vendor_account_id from vendor_account_admin_groups where group_id in (`+strings.Join(placeholders, ",")+`))
		order by id`, args...)
	if err != nil {
		return nil, fmt.Errorf("vendor accounts by admin groups: %w", err)
	}
	defer rows.Close()
	return scanVendorAccounts(rows)
}

// ClearVendorAccountSharing returns accountID to a private account in ONE
// transaction, in this order: share_level = 'none' first, then every
// vendor_account_admin_groups link and every resource_group_vendor_accounts link
// of the account is deleted, then system_group_id is cleared. Writing the level
// first is the load-bearing part: the account is already owner-only before any
// link row disappears, so even a torn write leaves it owner-only rather than
// live-shared over half-deleted links. The first UPDATE also takes the
// account's row lock, serializing a concurrent Clear or delete of the same
// account. An unknown id is ErrNotFound; an already-private account is a
// successful no-op.
func (s *SQLiteStore) ClearVendorAccountSharing(ctx context.Context, accountID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("clear vendor account sharing tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.dl.rebind(`update vendor_accounts set share_level = ? where id = ?`), routing.VendorShareLevelNone, accountID)
	if err != nil {
		return fmt.Errorf("reset vendor account share level: %w", err)
	}
	if err := requireAffected(res); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from vendor_account_admin_groups where vendor_account_id = ?`), accountID); err != nil {
		return fmt.Errorf("clear vendor account admin groups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from resource_group_vendor_accounts where vendor_account_id = ?`), accountID); err != nil {
		return fmt.Errorf("clear vendor account resource group links: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`update vendor_accounts set system_group_id = '' where id = ?`), accountID); err != nil {
		return fmt.Errorf("clear vendor account system group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clear vendor account sharing: %w", err)
	}
	return nil
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
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.Vendor, &a.AuthType, &a.Name, &a.Status, &a.APIKey, &a.OAuthTokens, &a.ModelPrefix, &a.BaseURL, &a.CreatedAt, &a.UpdatedAt, &a.ShareLevel, &a.SystemGroupID)
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
