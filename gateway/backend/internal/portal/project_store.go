// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/store"
)

// ProjectStore is the persistence contract for projects. Satisfied by
// *store.SQLStore (sqlite/postgres) and *MemoryDirectory (memory mode).
type ProjectStore interface {
	CreateProject(ctx context.Context, p store.Project) error
	ProjectByID(ctx context.Context, id string) (store.Project, error)
	UpdateProject(ctx context.Context, p store.Project) error
	DeleteProject(ctx context.Context, id string) error
	ListProjects(ctx context.Context) ([]store.Project, error)
	SetProjectMember(ctx context.Context, projectID, userID string) error
	RemoveProjectMember(ctx context.Context, projectID, userID string) error
	ProjectMembers(ctx context.Context, projectID string) ([]string, error)
	SetProjectGroup(ctx context.Context, projectID, groupID string) error
	RemoveProjectGroup(ctx context.Context, projectID, groupID string) error
	ProjectGroups(ctx context.Context, projectID string) ([]string, error)
	// ProjectsByOwnerOrMember returns every project where userID is the owner
	// OR has a direct project_members row. The GROUP part (projects reachable
	// via a user_group_members row) is composed in the service via
	// ProjectsByGroup — see Task 3.
	ProjectsByOwnerOrMember(ctx context.Context, userID string) ([]store.Project, error)
	// ProjectsByGroup returns every project with groupID assigned, avoiding an
	// N+1 when the service composes group-based access (Task 3).
	ProjectsByGroup(ctx context.Context, groupID string) ([]store.Project, error)
	// CoupledProjectsByGroup returns every project coupled to groupID.
	CoupledProjectsByGroup(ctx context.Context, groupID string) ([]store.Project, error)
}

var (
	_ ProjectStore = (*store.SQLStore)(nil)
	_ ProjectStore = (*MemoryDirectory)(nil)
)
