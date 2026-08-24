// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import "time"

const (
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
	UserStatusInvited  = "invited"

	TokenStatusActive   = "active"
	TokenStatusDisabled = "disabled"
	TokenStatusExpired  = "expired"

	// TokenKindUser / TokenKindService distinguish a normal user-owned token
	// (UserID set, ServiceID empty) from a service token (ServiceID set,
	// UserID empty) — see routing.Service (Phase 1 service accounts).
	TokenKindUser    = "user"
	TokenKindService = "service"

	// User-group tiers and membership states.
	GroupTierSystem = "system"
	GroupTierAdmin  = "admin"
	GroupTierUser   = "user"

	GroupStateMember  = "member"
	GroupStateInvited = "invited"

	// Fixed, well-known ids for the migration-seeded default groups so the
	// seed is idempotent and the two groups are referenceable.
	DefaultSystemGroupID = "ugrp_default_system"
	DefaultAdminGroupID  = "ugrp_default_admin"
)

// UserGroup is one node in the system→admin→user group tree.
type UserGroup struct {
	ID            string
	Tier          string
	Name          string
	ParentGroupID string // "" for system tier (NULL in DB)
	OwnerUserID   string // "" for system tier + migration defaults (NULL in DB)
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UserGroupMembership is a content/peer membership row.
type UserGroupMembership struct {
	GroupID   string
	UserID    string
	State     string // GroupStateMember | GroupStateInvited
	InvitedBy string // "" unless an invitation (user tier)
	CreatedAt time.Time
}

// UserGroupManagerPerm is a co-manager row's per-permission flags
// (per-Admin-Group co-manager permissions, spec 2026-08-10 + Phase B
// 2026-08-10 + Phase C 2026-08-10 + Resource Groups Phase 1 2026-08-11):
// CanManageUsers gates managing the group's users, CanManageGroup gates
// managing the group itself (rename/members/etc), CanManageServers gates
// managing the AI-servers linked to the group's admin tier (Phase B),
// CanManageServices gates managing the services linked to the group's admin
// tier (Phase C), CanManageResources gates managing the resources linked to
// the group's admin tier (Resource Groups Phase 1). A row inserted by
// SetUserGroupManager defaults all five to true via the column default
// (migration v48 for the first two, v49 for CanManageServers, v51 for
// CanManageServices, v53 for CanManageResources) — today's "a co-manager can
// do everything" behavior, preserved byte-for-byte until a caller narrows
// them via SetUserGroupManagerPermissions.
type UserGroupManagerPerm struct {
	UserID             string
	CanManageUsers     bool
	CanManageGroup     bool
	CanManageServers   bool
	CanManageServices  bool
	CanManageResources bool
}

// Project groups work across people; users + user-groups are assigned to it,
// and a member may attach their own API tokens to it (usage attribution).
type Project struct {
	ID          string
	Name        string
	Description string
	OwnerUserID string // "" only via ON DELETE SET NULL of a removed user, OR always "" for a coupled project (owner derived from the coupled group)
	// CoupledGroupID, when non-empty, couples the project to exactly one
	// user-group: no individual members, owner = the group's owner (derived on
	// read). "" for a normal project. FK -> user_groups(id) ON DELETE SET NULL.
	CoupledGroupID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type User struct {
	ID                string
	Email             string
	DisplayName       string
	Role              string
	Status            string
	PreferredLanguage string
	// ChatLogCommunication / ChatSecret are the token-less session-chat capture
	// flags carried on the user profile (Feature 5). They mirror the api_tokens
	// log_communication / secret flags and feed the session principal.
	ChatLogCommunication bool
	ChatSecret           bool
	TOTPSecret           string
	TOTPPendingSecret    string
	TOTPEnabled          bool
	TOTPConfirmedAt      *time.Time
	PasswordHash         string
	PasswordSetAt        *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type TokenRecord struct {
	ID            string
	UserID        string
	Name          string
	SecretHash    string
	SecretPrefix  string
	Status        string
	Scopes        string
	ExpiresAt     *time.Time
	LastUsedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ModelOverride string
	// ModelOverrideMap is the per-requested-model override map (requested -> gateway
	// model), stored as a JSON object string in the api_tokens.model_override_map
	// column. Empty string = no per-model entries. ModelOverride stays the catch-all.
	ModelOverrideMap string
	LogCommunication bool
	Secret           bool
	// ServiceID / Kind identify a SERVICE token (Kind==TokenKindService): it
	// belongs to a routing.Service, not a user — UserID is then empty and
	// ServiceID is the owning service's id. Kind==""/TokenKindUser is a normal
	// user token (ServiceID empty). A row's kind/service association is set
	// once at creation and never changes.
	ServiceID string
	Kind      string
	// ProjectID is the optional project this token is attached to for usage
	// attribution ("" = no project; nullable FK in DB, ON DELETE SET NULL).
	ProjectID string
	// ServerOverride is the id of an AI-server this token forces every request
	// onto, bypassing provisioning/affinity/maintenance-status ("" = no
	// override; see the server-override design). ServerOverrideForceUnreachable,
	// when true, allows the override to route even to an unhealthy/unreachable
	// server (else an unhealthy override server is refused).
	ServerOverride                 string
	ServerOverrideForceUnreachable bool
	// LastUsedModel is the gateway model or group name of this token's last
	// SUCCESSFULLY ROUTED request ("" = none yet). Kept for every token, not
	// only those using the redirect below, because the token list shows it.
	LastUsedModel string
	// UnknownModelRedirect turns on the unknown-model redirect: a requested
	// model that does not apply falls back to LastUsedModel, then to
	// UnknownModelFallback. An exact override row and the catch-all both win
	// over it.
	UnknownModelRedirect bool
	// UnknownModelRedirectBlocked widens what counts as "does not apply": with
	// it, a model that exists but this token may not use (allowlist, resource
	// group visibility) is redirected too, instead of being refused.
	UnknownModelRedirectBlocked bool
	// UnknownModelFallback is the model or group used when LastUsedModel is
	// empty or no longer offered ("" = none, the request then fails as before).
	UnknownModelFallback string
}

// UserUIPreference is one per-user UI preference: an opaque JSON value stored
// under a string key at the user profile. It mirrors the system_settings
// key/value store but is scoped to a single user.
type UserUIPreference struct {
	UserID    string
	Key       string
	ValueJSON string
	UpdatedAt time.Time
}

type Session struct {
	ID            string
	UserID        string
	SecretHash    string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	LastSeenAt    time.Time
	ElevatedUntil time.Time
}

type SetPasswordToken struct {
	ID         string
	UserID     string
	SecretHash string
	ExpiresAt  time.Time
	UsedAt     *time.Time
	CreatedAt  time.Time
}
