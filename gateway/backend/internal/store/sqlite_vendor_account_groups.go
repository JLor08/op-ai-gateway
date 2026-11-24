// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
	"op-ai-gateway/internal/routing"
)

const vendorAccountGroupColumns = `id, owner_user_id, vendor, name, model_prefix, status,
	threshold_five_hour_pct, threshold_weekly_pct, threshold_credits_pct, created_at, updated_at`

// CreateVendorAccountGroup inserts a new group. A duplicate id is ErrConflict; an
// owner that does not exist is ErrNotFound (the FK).
func (s *SQLiteStore) CreateVendorAccountGroup(ctx context.Context, g routing.VendorAccountGroup) error {
	_, err := s.exec(ctx, `insert into vendor_account_groups (`+vendorAccountGroupColumns+`)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.OwnerUserID, g.Vendor, g.Name, g.ModelPrefix, g.Status,
		g.ThresholdFiveHourPct, g.ThresholdWeeklyPct, g.ThresholdCreditsPct, g.CreatedAt, g.UpdatedAt)
	if err != nil {
		// FK first: the sqlite classifier for "unique" also matches any
		// "constraint failed" message, FK violations included.
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create vendor account group: %w", err)
	}
	return nil
}

// UpdateVendorAccountGroup writes name, model_prefix, status, the three
// thresholds and updated_at; owner_user_id, vendor and created_at are never
// written, matching the memory driver. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateVendorAccountGroup(ctx context.Context, g routing.VendorAccountGroup) error {
	res, err := s.exec(ctx, `update vendor_account_groups
		set name = ?, model_prefix = ?, status = ?,
			threshold_five_hour_pct = ?, threshold_weekly_pct = ?, threshold_credits_pct = ?, updated_at = ?
		where id = ?`,
		g.Name, g.ModelPrefix, g.Status, g.ThresholdFiveHourPct, g.ThresholdWeeklyPct, g.ThresholdCreditsPct, g.UpdatedAt, g.ID)
	if err != nil {
		return fmt.Errorf("update vendor account group: %w", err)
	}
	return requireAffected(res)
}

// DeleteVendorAccountGroup removes the group; its member rows go with it through
// the ON DELETE CASCADE FK. An unknown id is ErrNotFound.
func (s *SQLiteStore) DeleteVendorAccountGroup(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from vendor_account_groups where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete vendor account group: %w", err)
	}
	return requireAffected(res)
}

func scanVendorAccountGroup(sc interface{ Scan(...any) error }) (routing.VendorAccountGroup, error) {
	var g routing.VendorAccountGroup
	err := sc.Scan(&g.ID, &g.OwnerUserID, &g.Vendor, &g.Name, &g.ModelPrefix, &g.Status,
		&g.ThresholdFiveHourPct, &g.ThresholdWeeklyPct, &g.ThresholdCreditsPct, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// VendorAccountGroupByID returns the group or ErrNotFound.
func (s *SQLiteStore) VendorAccountGroupByID(ctx context.Context, id string) (routing.VendorAccountGroup, error) {
	g, err := scanVendorAccountGroup(s.queryRow(ctx, `select `+vendorAccountGroupColumns+` from vendor_account_groups where id = ?`, id))
	if err != nil {
		if err == sql.ErrNoRows {
			return routing.VendorAccountGroup{}, ErrNotFound
		}
		return routing.VendorAccountGroup{}, fmt.Errorf("vendor account group by id: %w", err)
	}
	return g, nil
}

// VendorAccountGroupsByOwner lists userID's groups ordered by id; never nil.
func (s *SQLiteStore) VendorAccountGroupsByOwner(ctx context.Context, userID string) ([]routing.VendorAccountGroup, error) {
	rows, err := s.query(ctx, `select `+vendorAccountGroupColumns+` from vendor_account_groups where owner_user_id = ? order by id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list vendor account groups: %w", err)
	}
	defer rows.Close()
	out := make([]routing.VendorAccountGroup, 0)
	for rows.Next() {
		g, err := scanVendorAccountGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan vendor account group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// VendorAccountGroupMembers lists the group's members in position order; never nil.
func (s *SQLiteStore) VendorAccountGroupMembers(ctx context.Context, groupID string) ([]routing.VendorAccountGroupMember, error) {
	rows, err := s.query(ctx, `select group_id, vendor_account_id, position from vendor_account_group_members
		where group_id = ? order by position`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list vendor account group members: %w", err)
	}
	defer rows.Close()
	out := make([]routing.VendorAccountGroupMember, 0)
	for rows.Next() {
		var m routing.VendorAccountGroupMember
		if err := rows.Scan(&m.GroupID, &m.AccountID, &m.Position); err != nil {
			return nil, fmt.Errorf("scan vendor account group member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetVendorAccountGroupMembers atomically REPLACES the group's member set
// (delete-then-insert in one transaction, like SetVendorAccountModels), writing
// positions 1..n in slice order. An unknown group or account is ErrNotFound; a
// duplicate account id violates unique(group_id, vendor_account_id) and is
// ErrConflict. Either rolls the delete back so the previous set survives.
func (s *SQLiteStore) SetVendorAccountGroupMembers(ctx context.Context, groupID string, accountIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set vendor account group members tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select count(*) from vendor_account_groups where id = ?`), groupID).Scan(&exists); err != nil {
		return fmt.Errorf("check vendor account group: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from vendor_account_group_members where group_id = ?`), groupID); err != nil {
		return fmt.Errorf("clear vendor account group members: %w", err)
	}
	for i, accountID := range accountIDs {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`insert into vendor_account_group_members (group_id, vendor_account_id, position)
			values (?, ?, ?)`), groupID, accountID, i+1); err != nil {
			// FK first: the sqlite classifier for "unique" also matches any
			// "constraint failed" message, FK violations included.
			if s.dl.isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert vendor account group member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit vendor account group members: %w", err)
	}
	return nil
}
