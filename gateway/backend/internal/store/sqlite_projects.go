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

const projectCols = `id, name, description, owner_user_id, coupled_group_id, created_at, updated_at`

// projectColsPrefixed is projectCols qualified with the `p` alias, for the two
// join queries below (ProjectsByOwnerOrMember/ProjectsByGroup) where an
// unqualified column list would be ambiguous against the joined table.
const projectColsPrefixed = `p.id, p.name, p.description, p.owner_user_id, p.coupled_group_id, p.created_at, p.updated_at`

func scanProject(row rowScanner) (Project, error) {
	var p Project
	var owner, coupled sql.NullString
	err := row.Scan(&p.ID, &p.Name, &p.Description, &owner, &coupled, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("scan project: %w", err)
	}
	p.OwnerUserID = owner.String
	p.CoupledGroupID = coupled.String
	return p, nil
}

func (s *SQLiteStore) CreateProject(ctx context.Context, p Project) error {
	_, err := s.exec(ctx, `insert into projects
		(id, name, description, owner_user_id, coupled_group_id, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Description, nullStr(p.OwnerUserID), nullStr(p.CoupledGroupID), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		// isForeignKeyViolation MUST be checked before isUniqueViolation — see
		// the identical ordering note in sqlite_user_groups.go CreateUserGroup:
		// sqlite's FK-violation error text also matches the unique-violation
		// substring check, so checking unique first would misclassify a
		// missing owner_user_id as ErrConflict instead of ErrNotFound.
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create project: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ProjectByID(ctx context.Context, id string) (Project, error) {
	row := s.queryRow(ctx, `select `+projectCols+` from projects where id = ?`, id)
	return scanProject(row)
}

// UpdateProject replaces name/description/owner/updated_at only (mirrors
// UpdateUserGroup in sqlite_user_groups.go, which likewise never touches a
// non-writable column — here there is none, but the SET clause is
// deliberately explicit rather than a blanket column list).
func (s *SQLiteStore) UpdateProject(ctx context.Context, p Project) error {
	res, err := s.exec(ctx, `update projects set name = ?, description = ?, owner_user_id = ?, updated_at = ? where id = ?`,
		p.Name, p.Description, nullStr(p.OwnerUserID), p.UpdatedAt, p.ID)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("update project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update project rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProject relies on the schema's ON DELETE CASCADE from
// project_members.project_id/project_groups.project_id to remove every
// member/group row for this project (see migration45Up).
func (s *SQLiteStore) DeleteProject(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `delete from projects where id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete project rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) queryProjects(ctx context.Context, query string, args ...any) ([]Project, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	out := make([]Project, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListProjects(ctx context.Context) ([]Project, error) {
	return s.queryProjects(ctx, `select `+projectCols+` from projects order by name, id`)
}

// ProjectsByOwnerOrMember returns every project where userID is the owner OR
// has a direct project_members row (the group part is composed in the
// service via user_group_members — see Task 3).
func (s *SQLiteStore) ProjectsByOwnerOrMember(ctx context.Context, userID string) ([]Project, error) {
	return s.queryProjects(ctx, `select `+projectColsPrefixed+`
		from projects p
		where p.owner_user_id = ?
		   or exists (select 1 from project_members m where m.project_id = p.id and m.user_id = ?)
		order by p.name, p.id`, userID, userID)
}

// ProjectsByGroup returns every project that has groupID assigned (used by
// the service to avoid an N+1 when composing group-membership access).
func (s *SQLiteStore) ProjectsByGroup(ctx context.Context, groupID string) ([]Project, error) {
	return s.queryProjects(ctx, `select `+projectColsPrefixed+`
		from projects p join project_groups g on g.project_id = p.id
		where g.group_id = ?
		order by p.name, p.id`, groupID)
}

// CoupledProjectsByGroup returns every project coupled to groupID (coupled_group_id
// = groupID). Used to warn on group deletion (spec §7) and for coupled-name
// uniqueness. Distinct from ProjectsByGroup, which reads project_groups assignments.
func (s *SQLiteStore) CoupledProjectsByGroup(ctx context.Context, groupID string) ([]Project, error) {
	return s.queryProjects(ctx, `select `+projectCols+` from projects where coupled_group_id = ? order by name, id`, groupID)
}

func (s *SQLiteStore) SetProjectMember(ctx context.Context, projectID, userID string) error {
	_, err := s.exec(ctx, `insert into project_members (project_id, user_id, created_at)
		values (?, ?, ?) on conflict (project_id, user_id) do nothing`,
		projectID, userID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set project member: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RemoveProjectMember(ctx context.Context, projectID, userID string) error {
	_, err := s.exec(ctx, `delete from project_members where project_id = ? and user_id = ?`, projectID, userID)
	if err != nil {
		return fmt.Errorf("remove project member: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ProjectMembers(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.query(ctx, `select user_id from project_members where project_id = ? order by created_at, user_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project members: %w", err)
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

func (s *SQLiteStore) SetProjectGroup(ctx context.Context, projectID, groupID string) error {
	_, err := s.exec(ctx, `insert into project_groups (project_id, group_id, created_at)
		values (?, ?, ?) on conflict (project_id, group_id) do nothing`,
		projectID, groupID, time.Now().UTC())
	if err != nil {
		if s.dl.isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("set project group: %w", err)
	}
	return nil
}

func (s *SQLiteStore) RemoveProjectGroup(ctx context.Context, projectID, groupID string) error {
	_, err := s.exec(ctx, `delete from project_groups where project_id = ? and group_id = ?`, projectID, groupID)
	if err != nil {
		return fmt.Errorf("remove project group: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ProjectGroups(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.query(ctx, `select group_id from project_groups where project_id = ? order by created_at, group_id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project groups: %w", err)
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
