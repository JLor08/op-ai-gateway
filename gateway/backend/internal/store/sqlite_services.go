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

// CreateService persists a new Service Account (Phase 1 service accounts,
// migration v40). It mirrors CreateAIServer/CreateModelGroup: the caller
// (portal layer) generates the id and stamps CreatedAt/UpdatedAt; the store
// does not default them. Status defaults to ServerStatusActive when empty,
// mirroring the column's own DB default.
func (s *SQLiteStore) CreateService(ctx context.Context, svc routing.Service) error {
	status := svc.Status
	if status == "" {
		status = routing.ServerStatusActive
	}
	_, err := s.exec(ctx, `
		insert into services (id, name, description, status, created_at, updated_at, system_group_id)
		values (?, ?, ?, ?, ?, ?, ?)`,
		svc.ID, svc.Name, svc.Description, status, svc.CreatedAt, svc.UpdatedAt, svc.SystemGroupID)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

// UpdateService updates name/description/status/updated_at/system_group_id by
// id (created_at is never touched, mirroring UpdateModelGroup / UpdateAIServer).
// An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateService(ctx context.Context, svc routing.Service) error {
	status := svc.Status
	if status == "" {
		status = routing.ServerStatusActive
	}
	res, err := s.exec(ctx, `
		update services set name = ?, description = ?, status = ?, updated_at = ?, system_group_id = ?
		where id = ?`,
		svc.Name, svc.Description, status, svc.UpdatedAt, svc.SystemGroupID, svc.ID)
	if err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	return requireAffected(res)
}

func (s *SQLiteStore) ServiceByID(ctx context.Context, id string) (routing.Service, error) {
	row := s.queryRow(ctx, `
		select id, name, description, status, created_at, updated_at, system_group_id
		from services where id = ?`, id)
	return scanService(row)
}

func (s *SQLiteStore) Services(ctx context.Context) ([]routing.Service, error) {
	rows, err := s.query(ctx, `
		select id, name, description, status, created_at, updated_at, system_group_id
		from services order by id`)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()
	return scanServices(rows)
}

// ServicesByDelegate lists the services where userID is a delegate at EITHER
// stage (Token- or Full-Delegate) — matching "beide Stufen" from the design.
func (s *SQLiteStore) ServicesByDelegate(ctx context.Context, userID string) ([]routing.Service, error) {
	rows, err := s.query(ctx, `
		select sv.id, sv.name, sv.description, sv.status, sv.created_at, sv.updated_at, sv.system_group_id
		from services sv
		join service_delegates sd on sd.service_id = sv.id
		where sd.user_id = ?
		order by sv.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list services by delegate: %w", err)
	}
	defer rows.Close()
	return scanServices(rows)
}

// DeleteService removes a service; the FK cascade (on delete cascade) removes
// its service_delegates, service_allowed_models, and api_tokens rows. An
// unknown id is ErrNotFound.
func (s *SQLiteStore) DeleteService(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from services where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return requireAffected(res)
}

// SetServiceDelegates atomically REPLACES a service's delegate list (delete-
// then-insert, transactional — mirrors SetGroupMembers). An unknown service id
// is ErrNotFound (even for an empty set, so a caller cannot silently no-op
// against a typo'd id); a duplicate UserID within the set is ErrConflict
// (mirrors the service_delegates primary key).
func (s *SQLiteStore) SetServiceDelegates(ctx context.Context, serviceID string, delegates []routing.ServiceDelegate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set service delegates tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select id from services where id = ?`), serviceID).Scan(&existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("check service: %w", err)
	}

	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from service_delegates where service_id = ?`), serviceID); err != nil {
		return fmt.Errorf("clear service delegates: %w", err)
	}
	for _, d := range delegates {
		// can_manage_settings is an integer column; a raw Go bool passed straight
		// to tx.ExecContext (bypassing s.exec's sanitizeArgs) fails to encode on
		// postgres ("unable to encode true into binary format for int4") — sqlite
		// is lenient here, which is why this needs an explicit int conversion.
		canManage := 0
		if d.CanManageSettings {
			canManage = 1
		}
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into service_delegates (service_id, user_id, can_manage_settings)
			values (?, ?, ?)`),
			serviceID, d.UserID, canManage); err != nil {
			if s.dl.isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert service delegate: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit service delegates: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ServiceDelegates(ctx context.Context, serviceID string) ([]routing.ServiceDelegate, error) {
	rows, err := s.query(ctx, `
		select user_id, can_manage_settings from service_delegates where service_id = ? order by user_id`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("list service delegates: %w", err)
	}
	defer rows.Close()
	out := make([]routing.ServiceDelegate, 0)
	for rows.Next() {
		var d routing.ServiceDelegate
		var canManage int64
		if err := rows.Scan(&d.UserID, &canManage); err != nil {
			return nil, fmt.Errorf("scan service delegate: %w", err)
		}
		d.CanManageSettings = canManage != 0
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate service delegates: %w", err)
	}
	return out, nil
}

// SetServiceAllowedModels atomically REPLACES a service's model allowlist
// (delete-then-insert, transactional — mirrors SetServiceDelegates). An
// unknown service id is ErrNotFound (even for an empty set); a duplicate model
// name within the set is ErrConflict (mirrors the composite primary key). An
// empty allowlist means "every model is allowed" — enforced by the caller
// (the admission gate), not by the store.
func (s *SQLiteStore) SetServiceAllowedModels(ctx context.Context, serviceID string, models []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set service allowed models tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	if err := tx.QueryRowContext(ctx, s.dl.rebind(`select id from services where id = ?`), serviceID).Scan(&existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("check service: %w", err)
	}

	if _, err := tx.ExecContext(ctx, s.dl.rebind(`delete from service_allowed_models where service_id = ?`), serviceID); err != nil {
		return fmt.Errorf("clear service allowed models: %w", err)
	}
	for _, model := range models {
		if _, err := tx.ExecContext(ctx, s.dl.rebind(`
			insert into service_allowed_models (service_id, gateway_model_name)
			values (?, ?)`),
			serviceID, model); err != nil {
			if s.dl.isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if s.dl.isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("insert service allowed model: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit service allowed models: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ServiceAllowedModels(ctx context.Context, serviceID string) ([]string, error) {
	rows, err := s.query(ctx, `
		select gateway_model_name from service_allowed_models where service_id = ? order by gateway_model_name`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("list service allowed models: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan service allowed model: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate service allowed models: %w", err)
	}
	return out, nil
}

// UpdateServiceSystemGroup writes only system_group_id — the admin-group
// permissions Phase C containment root (migration v52) — with a targeted
// UPDATE (only that one column), so it cannot race a concurrent full-row or
// state write on the same row. An unknown id is ErrNotFound.
func (s *SQLiteStore) UpdateServiceSystemGroup(ctx context.Context, serviceID, systemGroupID string) error {
	result, err := s.exec(ctx, `
		update services
		set system_group_id = ?
		where id = ?`,
		systemGroupID, serviceID,
	)
	if err != nil {
		return fmt.Errorf("update service system group: %w", err)
	}
	return requireAffected(result)
}

// SetServiceAdminGroup links serviceID to groupID (service_admin_groups),
// idempotent on the (service_id, group_id) unique pair — mirrors
// SetServerAdminGroup. A missing serviceID or groupID surfaces ErrNotFound
// (FK violation).
func (s *SQLiteStore) SetServiceAdminGroup(ctx context.Context, serviceID, groupID string) error {
	_, err := s.exec(ctx, `insert into service_admin_groups (service_id, group_id, created_at)
		values (?, ?, ?) on conflict (service_id, group_id) do nothing`,
		serviceID, groupID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set service admin group: %w", err)
	}
	return nil
}

// RemoveServiceAdminGroup unlinks serviceID from groupID. A no-op (non-error)
// when the link does not exist — mirrors RemoveServerAdminGroup.
func (s *SQLiteStore) RemoveServiceAdminGroup(ctx context.Context, serviceID, groupID string) error {
	_, err := s.exec(ctx, `delete from service_admin_groups where service_id = ? and group_id = ?`, serviceID, groupID)
	if err != nil {
		return fmt.Errorf("remove service admin group: %w", err)
	}
	return nil
}

// ServiceAdminGroups lists every admin-group id linked to serviceID, ordered
// by created_at then group_id — mirrors ServerAdminGroups. Always non-nil,
// empty when none.
func (s *SQLiteStore) ServiceAdminGroups(ctx context.Context, serviceID string) ([]string, error) {
	rows, err := s.query(ctx, `select group_id from service_admin_groups where service_id = ? order by created_at, group_id`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("list service admin groups: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan service admin group: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ServicesByAdminGroups returns every service linked to ANY of groupIDs,
// deduped by service id (a service linked to more than one of the given
// groups is returned once) — mirrors ServersByAdminGroups. An empty groupIDs
// returns an empty slice without issuing a query.
func (s *SQLiteStore) ServicesByAdminGroups(ctx context.Context, groupIDs []string) ([]routing.Service, error) {
	if len(groupIDs) == 0 {
		return make([]routing.Service, 0), nil
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.query(ctx, `
		select distinct sv.id, sv.name, sv.description, sv.status, sv.created_at, sv.updated_at, sv.system_group_id
		from services sv
		join service_admin_groups g on g.service_id = sv.id
		where g.group_id in (`+strings.Join(placeholders, ",")+`)
		order by sv.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("services by admin groups: %w", err)
	}
	defer rows.Close()
	return scanServices(rows)
}

func scanService(row rowScanner) (routing.Service, error) {
	var svc routing.Service
	err := row.Scan(&svc.ID, &svc.Name, &svc.Description, &svc.Status, &svc.CreatedAt, &svc.UpdatedAt, &svc.SystemGroupID)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.Service{}, ErrNotFound
	}
	if err != nil {
		return routing.Service{}, fmt.Errorf("scan service: %w", err)
	}
	return svc, nil
}

func scanServices(rows *sql.Rows) ([]routing.Service, error) {
	out := make([]routing.Service, 0)
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate services: %w", err)
	}
	return out, nil
}
