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

// CreateResourceGroup persists a new resource group (Resource Groups Phase 1,
// migration v54). It mirrors CreateService/CreateAIServer: the caller (portal
// layer) generates the id and stamps CreatedAt/UpdatedAt; the store does not
// default them. Status defaults to ServerStatusActive when empty, mirroring
// the column's own DB default.
func (s *SQLiteStore) CreateResourceGroup(ctx context.Context, rg routing.ResourceGroup) error {
	status := rg.Status
	if status == "" {
		status = routing.ServerStatusActive
	}
	_, err := s.exec(ctx, `
		insert into resource_groups (id, name, system_group_id, status, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?)`,
		rg.ID, rg.Name, rg.SystemGroupID, status, rg.CreatedAt, rg.UpdatedAt)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create resource group: %w", err)
	}
	return nil
}

// UpdateResourceGroup updates name/status/updated_at by id (created_at and
// system_group_id are never touched — system_group_id is written solely via
// UpdateResourceGroupSystemGroup, mirroring UpdateService/UpdateAIServer). An
// unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateResourceGroup(ctx context.Context, rg routing.ResourceGroup) error {
	status := rg.Status
	if status == "" {
		status = routing.ServerStatusActive
	}
	res, err := s.exec(ctx, `
		update resource_groups set name = ?, status = ?, updated_at = ?
		where id = ?`,
		rg.Name, status, rg.UpdatedAt, rg.ID)
	if err != nil {
		return fmt.Errorf("update resource group: %w", err)
	}
	return requireAffected(res)
}

// DeleteResourceGroup removes a resource group; the FK cascade (on delete
// cascade) removes its resource_group_admin_groups and resource_group_servers
// rows. An unknown id is ErrNotFound.
func (s *SQLiteStore) DeleteResourceGroup(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from resource_groups where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete resource group: %w", err)
	}
	return requireAffected(res)
}

func (s *SQLiteStore) ResourceGroupByID(ctx context.Context, id string) (routing.ResourceGroup, error) {
	row := s.queryRow(ctx, `
		select id, name, system_group_id, status, created_at, updated_at
		from resource_groups where id = ?`, id)
	return scanResourceGroup(row)
}

func (s *SQLiteStore) ResourceGroups(ctx context.Context) ([]routing.ResourceGroup, error) {
	rows, err := s.query(ctx, `
		select id, name, system_group_id, status, created_at, updated_at
		from resource_groups order by id`)
	if err != nil {
		return nil, fmt.Errorf("list resource groups: %w", err)
	}
	defer rows.Close()
	return scanResourceGroups(rows)
}

// UpdateResourceGroupSystemGroup writes only system_group_id — the
// containment root — with a targeted UPDATE (only that one column), so it
// cannot race a concurrent full-row write. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateResourceGroupSystemGroup(ctx context.Context, rgID, systemGroupID string) error {
	result, err := s.exec(ctx, `
		update resource_groups
		set system_group_id = ?
		where id = ?`,
		systemGroupID, rgID,
	)
	if err != nil {
		return fmt.Errorf("update resource group system group: %w", err)
	}
	return requireAffected(result)
}

// SetResourceGroupAdminGroup links rgID to groupID (resource_group_admin_groups),
// idempotent on the (resource_group_id, group_id) unique pair — mirrors
// SetServiceAdminGroup. A missing rgID or groupID surfaces ErrNotFound (FK
// violation).
func (s *SQLiteStore) SetResourceGroupAdminGroup(ctx context.Context, rgID, groupID string) error {
	_, err := s.exec(ctx, `insert into resource_group_admin_groups (resource_group_id, group_id, created_at)
		values (?, ?, ?) on conflict (resource_group_id, group_id) do nothing`,
		rgID, groupID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set resource group admin group: %w", err)
	}
	return nil
}

// RemoveResourceGroupAdminGroup unlinks rgID from groupID. A no-op
// (non-error) when the link does not exist — mirrors RemoveServiceAdminGroup.
func (s *SQLiteStore) RemoveResourceGroupAdminGroup(ctx context.Context, rgID, groupID string) error {
	_, err := s.exec(ctx, `delete from resource_group_admin_groups where resource_group_id = ? and group_id = ?`, rgID, groupID)
	if err != nil {
		return fmt.Errorf("remove resource group admin group: %w", err)
	}
	return nil
}

// ResourceGroupAdminGroups lists every admin-group id linked to rgID, ordered
// by created_at then group_id — mirrors ServiceAdminGroups. Always non-nil,
// empty when none.
func (s *SQLiteStore) ResourceGroupAdminGroups(ctx context.Context, rgID string) ([]string, error) {
	rows, err := s.query(ctx, `select group_id from resource_group_admin_groups where resource_group_id = ? order by created_at, group_id`, rgID)
	if err != nil {
		return nil, fmt.Errorf("list resource group admin groups: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resource group admin group: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResourceGroupsByAdminGroups returns every resource group linked to ANY of
// groupIDs, deduped by resource-group id (a resource group linked to more
// than one of the given groups is returned once) — mirrors
// ServicesByAdminGroups. An empty groupIDs returns an empty slice without
// issuing a query.
func (s *SQLiteStore) ResourceGroupsByAdminGroups(ctx context.Context, groupIDs []string) ([]routing.ResourceGroup, error) {
	if len(groupIDs) == 0 {
		return make([]routing.ResourceGroup, 0), nil
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.query(ctx, `
		select distinct rg.id, rg.name, rg.system_group_id, rg.status, rg.created_at, rg.updated_at
		from resource_groups rg
		join resource_group_admin_groups g on g.resource_group_id = rg.id
		where g.group_id in (`+strings.Join(placeholders, ",")+`)
		order by rg.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("resource groups by admin groups: %w", err)
	}
	defer rows.Close()
	return scanResourceGroups(rows)
}

// SetResourceGroupServer links rgID to serverID (resource_group_servers):
// serverID becomes a MEMBER of the resource group. Idempotent on the
// (resource_group_id, server_id) unique pair. A missing rgID or serverID
// surfaces ErrNotFound (FK violation).
func (s *SQLiteStore) SetResourceGroupServer(ctx context.Context, rgID, serverID string) error {
	_, err := s.exec(ctx, `insert into resource_group_servers (resource_group_id, server_id, created_at)
		values (?, ?, ?) on conflict (resource_group_id, server_id) do nothing`,
		rgID, serverID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set resource group server: %w", err)
	}
	return nil
}

// RemoveResourceGroupServer unlinks rgID from serverID. A no-op (non-error)
// when the link does not exist.
func (s *SQLiteStore) RemoveResourceGroupServer(ctx context.Context, rgID, serverID string) error {
	_, err := s.exec(ctx, `delete from resource_group_servers where resource_group_id = ? and server_id = ?`, rgID, serverID)
	if err != nil {
		return fmt.Errorf("remove resource group server: %w", err)
	}
	return nil
}

// ResourceGroupServers lists every server id that is a member of rgID,
// ordered by created_at then server_id. Always non-nil, empty when none.
func (s *SQLiteStore) ResourceGroupServers(ctx context.Context, rgID string) ([]string, error) {
	rows, err := s.query(ctx, `select server_id from resource_group_servers where resource_group_id = ? order by created_at, server_id`, rgID)
	if err != nil {
		return nil, fmt.Errorf("list resource group servers: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resource group server: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResourceGroupsByServer returns every resource group serverID is a member of
// (deduped by resource-group id), mirroring ResourceGroupsByAdminGroups.
func (s *SQLiteStore) ResourceGroupsByServer(ctx context.Context, serverID string) ([]routing.ResourceGroup, error) {
	rows, err := s.query(ctx, `
		select distinct rg.id, rg.name, rg.system_group_id, rg.status, rg.created_at, rg.updated_at
		from resource_groups rg
		join resource_group_servers s on s.resource_group_id = rg.id
		where s.server_id = ?
		order by rg.id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("resource groups by server: %w", err)
	}
	defer rows.Close()
	return scanResourceGroups(rows)
}

// SetResourceGroupProvision links rgID to one (kind, targetID) pair
// (resource_group_provisions), idempotent on the (resource_group_id,
// target_kind, target_id) unique triple — mirrors SetResourceGroupServer.
// target_id carries no foreign key (polymorphic); a missing rgID surfaces
// ErrNotFound (FK violation on resource_group_id).
func (s *SQLiteStore) SetResourceGroupProvision(ctx context.Context, rgID, kind, targetID string) error {
	_, err := s.exec(ctx, `insert into resource_group_provisions (resource_group_id, target_kind, target_id, created_at)
		values (?, ?, ?, ?) on conflict (resource_group_id, target_kind, target_id) do nothing`,
		rgID, kind, targetID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set resource group provision: %w", err)
	}
	return nil
}

// RemoveResourceGroupProvision unlinks rgID from one (kind, targetID) pair. A
// no-op (non-error) when the link does not exist — mirrors
// RemoveResourceGroupServer.
func (s *SQLiteStore) RemoveResourceGroupProvision(ctx context.Context, rgID, kind, targetID string) error {
	_, err := s.exec(ctx, `delete from resource_group_provisions where resource_group_id = ? and target_kind = ? and target_id = ?`,
		rgID, kind, targetID)
	if err != nil {
		return fmt.Errorf("remove resource group provision: %w", err)
	}
	return nil
}

// SetResourceGroupProvisions atomically REPLACES the whole provisioned-for set
// of rgID (delete-then-insert in one transaction) — mirrors SetGroupMembers/
// SetServerOwners. The resource group must exist (an empty provisions on an
// unknown rgID is still ErrNotFound). A duplicate (kind, targetID) pair
// WITHIN provisions is silently deduplicated (on-conflict-do-nothing), so the
// replace is idempotent even against a caller-supplied duplicate.
func (s *SQLiteStore) SetResourceGroupProvisions(ctx context.Context, rgID string, provisions []routing.ResourceGroupProvision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set resource group provisions tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx, s.dl.rebind(`select id from resource_groups where id = ?`), rgID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check resource group: %w", err)
	}

	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from resource_group_provisions where resource_group_id = ?`), rgID); err != nil {
		return fmt.Errorf("clear resource group provisions: %w", err)
	}
	now := time.Now().UTC()
	for _, p := range provisions {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into resource_group_provisions (resource_group_id, target_kind, target_id, created_at)
			values (?, ?, ?, ?) on conflict (resource_group_id, target_kind, target_id) do nothing`),
			rgID, p.Kind, p.TargetID, now); err != nil {
			return fmt.Errorf("insert resource group provision: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit resource group provisions: %w", err)
	}
	return nil
}

// ResourceGroupProvisions lists every (kind, target) pair rgID is provisioned
// for, ordered by target_kind then target_id. Always non-nil, empty when
// none.
func (s *SQLiteStore) ResourceGroupProvisions(ctx context.Context, rgID string) ([]routing.ResourceGroupProvision, error) {
	rows, err := s.query(ctx, `select target_kind, target_id from resource_group_provisions
		where resource_group_id = ? order by target_kind, target_id`, rgID)
	if err != nil {
		return nil, fmt.Errorf("list resource group provisions: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ResourceGroupProvision, 0)
	for rows.Next() {
		var p routing.ResourceGroupProvision
		if err := rows.Scan(&p.Kind, &p.TargetID); err != nil {
			return nil, fmt.Errorf("scan resource group provision: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResourceGroupIDsByProvisionTargets returns the ids of every resource group
// provisioned for ANY of targetIDs under kind (deduped via SELECT DISTINCT).
// An empty targetIDs returns (nil, nil) without issuing a query.
func (s *SQLiteStore) ResourceGroupIDsByProvisionTargets(ctx context.Context, kind string, targetIDs []string) ([]string, error) {
	if len(targetIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(targetIDs))
	args := make([]any, 0, len(targetIDs)+1)
	args = append(args, kind)
	for i, id := range targetIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := s.query(ctx, `
		select distinct resource_group_id from resource_group_provisions
		where target_kind = ? and target_id in (`+strings.Join(placeholders, ",")+`)
		order by resource_group_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("resource group ids by provision targets: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resource group id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ProvisionedResourceGroupIDs returns the set of every resource group id that
// carries at least one provision (of any kind).
func (s *SQLiteStore) ProvisionedResourceGroupIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.query(ctx, `select distinct resource_group_id from resource_group_provisions`)
	if err != nil {
		return nil, fmt.Errorf("provisioned resource group ids: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resource group id: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

func scanResourceGroup(row rowScanner) (routing.ResourceGroup, error) {
	var rg routing.ResourceGroup
	err := row.Scan(&rg.ID, &rg.Name, &rg.SystemGroupID, &rg.Status, &rg.CreatedAt, &rg.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.ResourceGroup{}, ErrNotFound
	}
	if err != nil {
		return routing.ResourceGroup{}, fmt.Errorf("scan resource group: %w", err)
	}
	return rg, nil
}

func scanResourceGroups(rows *sql.Rows) ([]routing.ResourceGroup, error) {
	out := make([]routing.ResourceGroup, 0)
	for rows.Next() {
		rg, err := scanResourceGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resource groups: %w", err)
	}
	return out, nil
}
