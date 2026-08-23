// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// nullStr converts an empty string to a genuine SQL NULL and a non-empty
// string to itself — used for user_groups.parent_group_id/.owner_user_id,
// whose FK constraints only exempt a genuine NULL from the
// reference-existence check (an empty string would be checked against the
// referenced table and rejected).
func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

const userGroupCols = `id, tier, name, parent_group_id, owner_user_id, created_at, updated_at`

func scanUserGroup(row rowScanner) (UserGroup, error) {
	var g UserGroup
	var parent, owner sql.NullString
	err := row.Scan(&g.ID, &g.Tier, &g.Name, &parent, &owner, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return UserGroup{}, ErrNotFound
	}
	if err != nil {
		return UserGroup{}, fmt.Errorf("scan user group: %w", err)
	}
	g.ParentGroupID = parent.String
	g.OwnerUserID = owner.String
	return g, nil
}

func (s *SQLiteStore) CreateUserGroup(ctx context.Context, g UserGroup) error {
	_, err := s.exec(ctx, `insert into user_groups
		(id, tier, name, parent_group_id, owner_user_id, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.Tier, g.Name, nullStr(g.ParentGroupID), nullStr(g.OwnerUserID), g.CreatedAt, g.UpdatedAt)
	if err != nil {
		// isForeignKeyViolation MUST be checked before isUniqueViolation:
		// sqlite's FK-violation error text ("FOREIGN KEY constraint failed")
		// also matches isUniqueViolation's "constraint failed" substring
		// check, so checking unique first would misclassify a missing
		// parent_group_id/owner_user_id as ErrConflict instead of
		// ErrNotFound (the ordering every other Create* in this package
		// uses — see sqlite_routes.go/sqlite_token.go).
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create user group: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UserGroupByID(ctx context.Context, id string) (UserGroup, error) {
	row := s.queryRow(ctx, `select `+userGroupCols+` from user_groups where id = ?`, id)
	return scanUserGroup(row)
}

func (s *SQLiteStore) UpdateUserGroup(ctx context.Context, g UserGroup) error {
	res, err := s.exec(ctx, `update user_groups set name = ?, owner_user_id = ?, updated_at = ? where id = ?`,
		g.Name, nullStr(g.OwnerUserID), g.UpdatedAt, g.ID)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update user group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user group rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteUserGroup(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from user_groups where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user group rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) listUserGroups(ctx context.Context, where string, args ...any) ([]UserGroup, error) {
	rows, err := s.query(ctx, `select `+userGroupCols+` from user_groups `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list user groups: %w", err)
	}
	defer rows.Close()
	out := make([]UserGroup, 0)
	for rows.Next() {
		g, err := scanUserGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListUserGroupsByTier(ctx context.Context, tier string) ([]UserGroup, error) {
	return s.listUserGroups(ctx, `where tier = ? order by name, id`, tier)
}

func (s *SQLiteStore) ChildUserGroups(ctx context.Context, parentID string) ([]UserGroup, error) {
	return s.listUserGroups(ctx, `where parent_group_id = ? order by name, id`, parentID)
}

// UserGroupsForUser returns every group where userID has a membership row,
// optionally narrowed to a specific tier and/or membership state (either or
// both may be "" to mean "any").
func (s *SQLiteStore) UserGroupsForUser(ctx context.Context, userID, tier, state string) ([]UserGroup, error) {
	query := `select g.id, g.tier, g.name, g.parent_group_id, g.owner_user_id, g.created_at, g.updated_at
		from user_groups g join user_group_members m on m.group_id = g.id
		where m.user_id = ?`
	args := []any{userID}
	if tier != "" {
		query += ` and g.tier = ?`
		args = append(args, tier)
	}
	if state != "" {
		query += ` and m.state = ?`
		args = append(args, state)
	}
	query += ` order by g.name, g.id`
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("user groups for user: %w", err)
	}
	defer rows.Close()
	out := make([]UserGroup, 0)
	for rows.Next() {
		g, err := scanUserGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetUserGroupMember(ctx context.Context, groupID, userID, state, invitedBy string) error {
	_, err := s.exec(ctx, `insert into user_group_members (group_id, user_id, state, invited_by, created_at)
		values (?, ?, ?, ?, ?)
		on conflict (group_id, user_id) do update set state = excluded.state, invited_by = excluded.invited_by`,
		groupID, userID, state, invitedBy, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set group member: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RemoveUserGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := s.exec(ctx, `delete from user_group_members where group_id = ? and user_id = ?`, groupID, userID)
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UserGroupMembers(ctx context.Context, groupID string) ([]UserGroupMembership, error) {
	rows, err := s.query(ctx, `select group_id, user_id, state, invited_by, created_at
		from user_group_members where group_id = ? order by created_at, user_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()
	out := make([]UserGroupMembership, 0)
	for rows.Next() {
		var m UserGroupMembership
		if err := rows.Scan(&m.GroupID, &m.UserID, &m.State, &m.InvitedBy, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetUserGroupManager(ctx context.Context, groupID, userID string) error {
	_, err := s.exec(ctx, `insert into user_group_managers (group_id, user_id, created_at)
		values (?, ?, ?) on conflict (group_id, user_id) do nothing`, groupID, userID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set group manager: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RemoveUserGroupManager(ctx context.Context, groupID, userID string) error {
	_, err := s.exec(ctx, `delete from user_group_managers where group_id = ? and user_id = ?`, groupID, userID)
	if err != nil {
		return fmt.Errorf("remove group manager: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UserGroupManagers(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.query(ctx, `select user_id from user_group_managers where group_id = ? order by created_at, user_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group managers: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UserGroupManagerPerms returns every co-manager row of groupID with its
// per-permission flags (per-Admin-Group co-manager permissions, spec
// 2026-08-10 + Phase B 2026-08-10 + Phase C 2026-08-10 + Resource Groups
// Phase 1 2026-08-11). A group with no co-managers returns an empty
// (non-nil) slice.
func (s *SQLiteStore) UserGroupManagerPerms(ctx context.Context, groupID string) ([]UserGroupManagerPerm, error) {
	rows, err := s.query(ctx, `select user_id, can_manage_users, can_manage_group, can_manage_servers, can_manage_services, can_manage_resources
		from user_group_managers where group_id = ? order by created_at, user_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group manager perms: %w", err)
	}
	defer rows.Close()
	out := make([]UserGroupManagerPerm, 0)
	for rows.Next() {
		var p UserGroupManagerPerm
		var canUsers, canGroup, canServers, canServices, canResources int64
		if err := rows.Scan(&p.UserID, &canUsers, &canGroup, &canServers, &canServices, &canResources); err != nil {
			return nil, err
		}
		p.CanManageUsers = canUsers != 0
		p.CanManageGroup = canGroup != 0
		p.CanManageServers = canServers != 0
		p.CanManageServices = canServices != 0
		p.CanManageResources = canResources != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetUserGroupManagerPermissions updates an EXISTING co-manager row's
// permission flags (per-Admin-Group co-manager permissions, spec 2026-08-10 +
// Phase B 2026-08-10 + Phase C 2026-08-10 + Resource Groups Phase 1
// 2026-08-11). It does not create a row — a group/user pair with no existing
// manager row returns ErrNotFound (mirrors UpdateUserGroup's zero-rows-
// affected check). perm.UserID identifies the row (groupID is passed
// separately since a UserGroupManagerPerm is otherwise group-agnostic);
// perm's five Can* flags are the new values.
func (s *SQLiteStore) SetUserGroupManagerPermissions(ctx context.Context, groupID string, perm UserGroupManagerPerm) error {
	res, err := s.exec(ctx, `update user_group_managers set can_manage_users = ?, can_manage_group = ?, can_manage_servers = ?, can_manage_services = ?, can_manage_resources = ?
		where group_id = ? and user_id = ?`, perm.CanManageUsers, perm.CanManageGroup, perm.CanManageServers, perm.CanManageServices, perm.CanManageResources, groupID, perm.UserID)
	if err != nil {
		return fmt.Errorf("set group manager permissions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set group manager permissions rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
