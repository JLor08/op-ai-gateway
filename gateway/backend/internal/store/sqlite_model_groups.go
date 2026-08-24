// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"time"
)

// CreateModelGroup inserts a new model group. A duplicate id classifies as
// ErrConflict. An empty FailoverMode defaults to "sticky" (mirrors the column
// default so a caller that omits it reads back the same value). MemberOrder,
// ClimbSpeedMarginPercent and MinSpeedFallback are defaulted the same way
// (their zero Go values do not all match the column defaults).
func (s *SQLiteStore) CreateModelGroup(ctx context.Context, group routing.ModelGroup) error {
	failover := group.FailoverMode
	if failover == "" {
		failover = "sticky"
	}
	traversal := group.Traversal
	if traversal == "" {
		traversal = "round_robin"
	}
	memberOrder := group.MemberOrder
	if memberOrder == "" {
		memberOrder = routing.MemberOrderPriority
	}
	climbMargin := group.ClimbSpeedMarginPercent
	if climbMargin == 0 {
		climbMargin = routing.DefaultClimbSpeedMarginPercent
	}
	minSpeedFallback := group.MinSpeedFallback
	if minSpeedFallback == "" {
		minSpeedFallback = routing.MinSpeedFallbackError
	}
	_, err := s.exec(ctx, `
		insert into model_groups (
			id, gateway_model_name, display_name, status, failover_mode, created_at, updated_at, traversal,
			loaded_only, member_order, climb_speed_margin_percent, min_tokens_per_second, min_speed_fallback
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		group.ID,
		group.GatewayModelName,
		group.DisplayName,
		group.Status,
		failover,
		group.CreatedAt,
		group.UpdatedAt,
		traversal,
		group.LoadedOnly,
		memberOrder,
		climbMargin,
		group.MinTokensPerSecond,
		minSpeedFallback,
	)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create model group: %w", err)
	}
	return nil
}

// UpdateModelGroup updates a group's mutable fields (not id/created_at). A missing
// id is ErrNotFound.
func (s *SQLiteStore) UpdateModelGroup(ctx context.Context, group routing.ModelGroup) error {
	failover := group.FailoverMode
	if failover == "" {
		failover = "sticky"
	}
	traversal := group.Traversal
	if traversal == "" {
		traversal = "round_robin"
	}
	memberOrder := group.MemberOrder
	if memberOrder == "" {
		memberOrder = routing.MemberOrderPriority
	}
	climbMargin := group.ClimbSpeedMarginPercent
	if climbMargin == 0 {
		climbMargin = routing.DefaultClimbSpeedMarginPercent
	}
	minSpeedFallback := group.MinSpeedFallback
	if minSpeedFallback == "" {
		minSpeedFallback = routing.MinSpeedFallbackError
	}
	result, err := s.exec(ctx, `
		update model_groups set
			gateway_model_name = ?, display_name = ?, status = ?, failover_mode = ?, updated_at = ?, traversal = ?,
			loaded_only = ?, member_order = ?, climb_speed_margin_percent = ?, min_tokens_per_second = ?, min_speed_fallback = ?
		where id = ?`,
		group.GatewayModelName,
		group.DisplayName,
		group.Status,
		failover,
		group.UpdatedAt,
		traversal,
		group.LoadedOnly,
		memberOrder,
		climbMargin,
		group.MinTokensPerSecond,
		minSpeedFallback,
		group.ID,
	)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update model group: %w", err)
	}
	return requireAffected(result)
}

// ModelGroupByID returns one group. A missing id is ErrNotFound.
func (s *SQLiteStore) ModelGroupByID(ctx context.Context, id string) (routing.ModelGroup, error) {
	row := s.queryRow(ctx, `
		select id, gateway_model_name, display_name, status, failover_mode, created_at, updated_at, traversal,
			loaded_only, member_order, climb_speed_margin_percent, min_tokens_per_second, min_speed_fallback
		from model_groups where id = ?`, id)
	return scanModelGroup(row)
}

// ModelGroups returns all groups, ordered by gateway_model_name then id.
func (s *SQLiteStore) ModelGroups(ctx context.Context) ([]routing.ModelGroup, error) {
	rows, err := s.query(ctx, `
		select id, gateway_model_name, display_name, status, failover_mode, created_at, updated_at, traversal,
			loaded_only, member_order, climb_speed_margin_percent, min_tokens_per_second, min_speed_fallback
		from model_groups order by gateway_model_name, id`)
	if err != nil {
		return nil, fmt.Errorf("list model groups: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ModelGroup, 0)
	for rows.Next() {
		group, err := scanModelGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model groups: %w", err)
	}
	return out, nil
}

// DeleteModelGroup removes a group; its members cascade (FK on delete cascade). A
// missing id is ErrNotFound.
func (s *SQLiteStore) DeleteModelGroup(ctx context.Context, id string) error {
	result, err := s.exec(ctx, `delete from model_groups where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete model group: %w", err)
	}
	return requireAffected(result)
}

// SetGroupMembers atomically REPLACES the members of a group (delete-then-insert
// in one transaction). The group must exist (an empty set on an unknown group is
// still ErrNotFound). A duplicate member_gateway_name within the set violates the
// unique(group_id, member_gateway_name) constraint and surfaces ErrConflict. The
// passed Priority is honored verbatim (the caller sets it from the array index);
// an empty member ID / zero CreatedAt is filled in.
func (s *SQLiteStore) SetGroupMembers(ctx context.Context, groupID string, members []routing.GroupMember) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set group members tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx, s.dl.rebind(`select id from model_groups where id = ?`), groupID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check model group: %w", err)
	}

	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from model_group_members where group_id = ?`), groupID); err != nil {
		return fmt.Errorf("clear group members: %w", err)
	}
	for _, m := range members {
		id := m.ID
		if id == "" {
			id = newGroupMemberID()
		}
		createdAt := m.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into model_group_members (id, group_id, member_gateway_name, priority, created_at)
			values (?, ?, ?, ?, ?)`),
			id, groupID, m.MemberGatewayName, m.Priority, createdAt); err != nil {
			if s.dl.isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert group member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit group members: %w", err)
	}
	return nil
}

// GroupMembersByGroup returns a group's members ordered by priority then id.
func (s *SQLiteStore) GroupMembersByGroup(ctx context.Context, groupID string) ([]routing.GroupMember, error) {
	rows, err := s.query(ctx, `
		select id, group_id, member_gateway_name, priority, created_at
		from model_group_members where group_id = ? order by priority, id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()
	out := make([]routing.GroupMember, 0)
	for rows.Next() {
		var m routing.GroupMember
		if err := rows.Scan(&m.ID, &m.GroupID, &m.MemberGatewayName, &m.Priority, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan group member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group members: %w", err)
	}
	return out, nil
}

// ModelSettings returns all per-model settings, ordered by gateway_model_name.
func (s *SQLiteStore) ModelSettings(ctx context.Context) ([]routing.ModelSetting, error) {
	rows, err := s.query(ctx, `
		select gateway_model_name, visibility, created_at, updated_at
		from model_settings order by gateway_model_name`)
	if err != nil {
		return nil, fmt.Errorf("list model settings: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ModelSetting, 0)
	for rows.Next() {
		setting, err := scanModelSetting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, setting)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model settings: %w", err)
	}
	return out, nil
}

// ModelSettingByName returns one model's setting. A missing row is (zero, false, nil).
func (s *SQLiteStore) ModelSettingByName(ctx context.Context, name string) (routing.ModelSetting, bool, error) {
	row := s.queryRow(ctx, `
		select gateway_model_name, visibility, created_at, updated_at
		from model_settings where gateway_model_name = ?`, name)
	setting, err := scanModelSetting(row)
	if errors.Is(err, ErrNotFound) {
		return routing.ModelSetting{}, false, nil
	}
	if err != nil {
		return routing.ModelSetting{}, false, err
	}
	return setting, true, nil
}

// UpsertModelSetting inserts or updates a model's setting by name. An empty
// Visibility defaults to "shown" (mirrors the column default). The on-conflict
// upsert works in both sqlite and postgres.
func (s *SQLiteStore) UpsertModelSetting(ctx context.Context, setting routing.ModelSetting) error {
	visibility := setting.Visibility
	if visibility == "" {
		visibility = "shown"
	}
	_, err := s.exec(ctx, `
		insert into model_settings (gateway_model_name, visibility, created_at, updated_at)
		values (?, ?, ?, ?)
		on conflict(gateway_model_name) do update set
			visibility = excluded.visibility,
			updated_at = excluded.updated_at`,
		setting.GatewayModelName,
		visibility,
		setting.CreatedAt,
		setting.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert model setting: %w", err)
	}
	return nil
}

func scanModelGroup(row rowScanner) (routing.ModelGroup, error) {
	var group routing.ModelGroup
	var loadedOnly int64
	err := row.Scan(
		&group.ID,
		&group.GatewayModelName,
		&group.DisplayName,
		&group.Status,
		&group.FailoverMode,
		&group.CreatedAt,
		&group.UpdatedAt,
		&group.Traversal,
		&loadedOnly,
		&group.MemberOrder,
		&group.ClimbSpeedMarginPercent,
		&group.MinTokensPerSecond,
		&group.MinSpeedFallback,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ModelGroup{}, ErrNotFound
	}
	if err != nil {
		return routing.ModelGroup{}, fmt.Errorf("scan model group: %w", err)
	}
	group.LoadedOnly = loadedOnly != 0
	return group, nil
}

func scanModelSetting(row rowScanner) (routing.ModelSetting, error) {
	var setting routing.ModelSetting
	err := row.Scan(
		&setting.GatewayModelName,
		&setting.Visibility,
		&setting.CreatedAt,
		&setting.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ModelSetting{}, ErrNotFound
	}
	if err != nil {
		return routing.ModelSetting{}, fmt.Errorf("scan model setting: %w", err)
	}
	return setting, nil
}

func newGroupMemberID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "gmem_" + hex.EncodeToString(b[:])
}
