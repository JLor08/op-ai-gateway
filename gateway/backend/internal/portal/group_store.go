// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/store"
)

// GroupStore is the persistence contract for user groups. Satisfied by
// *store.SQLStore (sqlite/postgres) and *MemoryDirectory (memory mode).
type GroupStore interface {
	CreateUserGroup(ctx context.Context, g store.UserGroup) error
	UserGroupByID(ctx context.Context, id string) (store.UserGroup, error)
	UpdateUserGroup(ctx context.Context, g store.UserGroup) error
	DeleteUserGroup(ctx context.Context, id string) error
	ListUserGroupsByTier(ctx context.Context, tier string) ([]store.UserGroup, error)
	ChildUserGroups(ctx context.Context, parentID string) ([]store.UserGroup, error)
	SetUserGroupMember(ctx context.Context, groupID, userID, state, invitedBy string) error
	RemoveUserGroupMember(ctx context.Context, groupID, userID string) error
	UserGroupMembers(ctx context.Context, groupID string) ([]store.UserGroupMembership, error)
	UserGroupsForUser(ctx context.Context, userID, tier, state string) ([]store.UserGroup, error)
	SetUserGroupManager(ctx context.Context, groupID, userID string) error
	RemoveUserGroupManager(ctx context.Context, groupID, userID string) error
	UserGroupManagers(ctx context.Context, groupID string) ([]string, error)
	UserGroupManagerPerms(ctx context.Context, groupID string) ([]store.UserGroupManagerPerm, error)
	SetUserGroupManagerPermissions(ctx context.Context, groupID string, perm store.UserGroupManagerPerm) error
}

var (
	_ GroupStore = (*store.SQLStore)(nil)
	_ GroupStore = (*MemoryDirectory)(nil)
)
