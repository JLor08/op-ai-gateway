// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
)

// VisibleUserIDs returns the set of user ids the given principal may see,
// per the user-groups visibility model: system_admin sees everyone; an admin
// sees the members of every system group they belong to (plus themselves);
// a regular user sees the members of every admin group they belong to (plus
// themselves). Only GroupStateMember rows count — an invited-but-not-accepted
// membership does not grant visibility.
func (s *Service) VisibleUserIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	visible := map[string]bool{principal.UserID: true}
	if isSystem(principal) {
		users, err := s.users.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(users))
		for _, u := range users {
			out[u.ID] = true
		}
		return out, nil
	}
	// admin -> members of the system groups the admin is a member of;
	// user  -> members of the admin groups the user is a member of.
	tier := store.GroupTierAdmin
	if isAdmin(principal) {
		tier = store.GroupTierSystem
	}
	myGroups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, tier, store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	for _, g := range myGroups {
		members, err := s.groups.UserGroupMembers(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if m.State == store.GroupStateMember {
				visible[m.UserID] = true
			}
		}
	}
	return visible, nil
}

// ManageableUserIDs returns the set of user ids the given principal may
// MANAGE via the admin user-management surface (list/update/disable,
// password-reset via invite-reissue, TOTP-reset, token listing) — narrower
// than VisibleUserIDs, per the per-Admin-Group co-manager permissions model
// (spec 2026-08-10). A system_admin manages every user (same "all users"
// mechanism as VisibleUserIDs' system branch). Anyone else manages
// themselves PLUS, for each ADMIN-tier group where they are either the
// OWNER or a co-manager whose stored CanManageUsers flag is true, that
// group's member-state (state=member, not invited) roster. A co-manager
// WITHOUT CanManageUsers — even one with CanManageGroup — contributes
// nothing beyond self; neither does a plain member.
func (s *Service) ManageableUserIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	if isSystem(principal) {
		users, err := s.users.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(users))
		for _, u := range users {
			out[u.ID] = true
		}
		return out, nil
	}
	manageable := map[string]bool{principal.UserID: true}
	myGroups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierAdmin, store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	for _, g := range myGroups {
		canManageUsers := g.OwnerUserID != "" && g.OwnerUserID == principal.UserID
		if !canManageUsers {
			perms, err := s.groups.UserGroupManagerPerms(ctx, g.ID)
			if err != nil {
				return nil, err
			}
			for _, p := range perms {
				if p.UserID == principal.UserID && p.CanManageUsers {
					canManageUsers = true
					break
				}
			}
		}
		if !canManageUsers {
			continue
		}
		members, err := s.memberIDSet(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for uid := range members {
			manageable[uid] = true
		}
	}
	// A system_admin account that is a member of a managed admin group STAYS in
	// the set (spec 2026-08-10 follow-up, revised): a non-system caller with
	// user-management rights over that group may perform the SUPPORT operations
	// on a system_admin -- set limits, re-invite (password-reset), reset TOTP --
	// all of which are gated ONLY on this set and carry no actor-scope guard. The
	// two DESTRUCTIVE operations, edit and deactivate, both flow through
	// account.UpdateUser, which independently rejects a non-system actor against
	// a system_admin target with ErrForbiddenRole (403) -- so they stay blocked
	// without needing to hide the account here. (An earlier revision dropped every
	// system_admin from the set entirely; that also blocked the allowed support
	// operations, which is not the intended policy.)
	return manageable, nil
}

// serverManageGroupIDs returns the set of ADMIN-tier group ids the given
// principal may manage SERVERS through: every admin group where the
// principal is either the OWNER or a co-manager whose stored
// CanManageServers flag is true (mirrors ManageableUserIDs's enumeration
// exactly, keyed on CanManageServers instead of CanManageUsers -- Phase B,
// spec 2026-08-10). A co-manager with CanManageGroup and/or CanManageUsers
// but WITHOUT CanManageServers contributes nothing; neither does a plain
// member. Never called for a system-scope principal -- system bypasses
// group-scoping entirely (see authorizeServer/ListServers).
//
// Unlike ManageableUserIDs, this is nil-safe on s.groups: authorizeServer
// calls it for every non-owner/non-system caller (and ListServers calls it
// for every non-system caller, unconditionally), which is a FAR wider blast
// radius than ManageableUserIDs' narrow admin-user-management call sites --
// a Service built without a Groups store (a test double, or a future driver
// that omits it) must degrade to "no group-based reach" (ownership-only)
// rather than panic on every server list/read. Every REAL driver still
// wires Groups (see ServiceDeps.Groups), so this is a defensive fallback,
// not the expected path.
func (s *Service) serverManageGroupIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	return s.manageGroupIDs(ctx, principal, func(p store.UserGroupManagerPerm) bool { return p.CanManageServers })
}

// serviceManageGroupIDs returns the set of ADMIN-tier group ids the given
// principal may manage SERVICES through: every admin group where the
// principal is either the OWNER or a co-manager whose stored
// CanManageServices flag is true (mirrors serverManageGroupIDs exactly,
// keyed on CanManageServices instead of CanManageServers -- Phase C, spec
// 2026-08-10). A co-manager with CanManageGroup and/or CanManageUsers and/or
// CanManageServers but WITHOUT CanManageServices contributes nothing;
// neither does a plain member. Never called for a system-scope principal --
// system bypasses group-scoping entirely (see authorizeServiceRead/
// authorizeServiceSettings/ListServices).
//
// Nil-safe on s.groups, mirroring serverManageGroupIDs: a Service built
// without a Groups store degrades to "no group-based reach" (delegate-only)
// rather than panicking on every service read/list.
func (s *Service) serviceManageGroupIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	return s.manageGroupIDs(ctx, principal, func(p store.UserGroupManagerPerm) bool { return p.CanManageServices })
}

// resourceManageGroupIDs returns the set of ADMIN-tier group ids the given
// principal may manage RESOURCE GROUPS through: every admin group where the
// principal is either the OWNER or a co-manager whose stored
// CanManageResources flag is true (mirrors serverManageGroupIDs/
// serviceManageGroupIDs exactly, keyed on CanManageResources instead of
// CanManageServers/CanManageServices -- Resource Groups Phase 1, spec
// 2026-08-11). A co-manager with CanManageGroup and/or CanManageUsers and/or
// CanManageServers and/or CanManageServices but WITHOUT CanManageResources
// contributes nothing; neither does a plain member. Never called for a
// system-scope principal -- system bypasses group-scoping entirely (see
// authorizeResourceGroup/ListResourceGroups).
//
// Nil-safe on s.groups, mirroring serverManageGroupIDs/serviceManageGroupIDs:
// a Service built without a Groups store degrades to "no group-based reach"
// rather than panicking on every resource-group read/list.
func (s *Service) resourceManageGroupIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	return s.manageGroupIDs(ctx, principal, func(p store.UserGroupManagerPerm) bool { return p.CanManageResources })
}

// manageGroupIDs is the shared enumeration behind serverManageGroupIDs,
// serviceManageGroupIDs, and resourceManageGroupIDs: every ADMIN-tier group
// where principal is either the OWNER or a co-manager for whom can(perm) is
// true, keyed on that group's id. can selects the single Can* flag that
// distinguishes the three callers (CanManageServers/CanManageServices/
// CanManageResources) — see their doc comments for the shared policy this
// implements. Nil-safe on s.groups: a Service built without a Groups store
// degrades to "no group-based reach" (map is empty, err is nil) rather than
// panicking, since callers here have a far wider blast radius than
// ManageableUserIDs' narrow admin-user-management call sites. Deliberately
// NOT shared with ManageableUserIDs, which carries an extra
// system_admin-stays-in-the-set policy nuance this loop does not have.
func (s *Service) manageGroupIDs(ctx context.Context, principal auth.Token, can func(store.UserGroupManagerPerm) bool) (map[string]bool, error) {
	out := map[string]bool{}
	if s.groups == nil {
		return out, nil
	}
	myGroups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierAdmin, store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	for _, g := range myGroups {
		canManage := g.OwnerUserID != "" && g.OwnerUserID == principal.UserID
		if !canManage {
			perms, err := s.groups.UserGroupManagerPerms(ctx, g.ID)
			if err != nil {
				return nil, err
			}
			for _, p := range perms {
				if p.UserID == principal.UserID && can(p) {
					canManage = true
					break
				}
			}
		}
		if !canManage {
			continue
		}
		out[g.ID] = true
	}
	return out, nil
}

// Sentinels returned by ResolveInviteAdminGroup when the actor's invite
// cannot be resolved to an admin group they may assign a user to.
var (
	ErrInviteAdminGroupRequired = errors.New("portal: invite requires an admin group")
	ErrInviteAdminGroupInvalid  = errors.New("portal: invite admin group invalid")
)

// ResolveInviteAdminGroup validates that adminGroupID is an Admin-tier group
// the actor may assign a new user to (owner/co-manager, or a system_admin) and
// returns it together with its parent system-group id. A new user MUST be
// assigned to an admin group (mandatory for every actor). Authorization +
// 404-no-leak are reused from authorizeGroupManage (which admits owner,
// co-manager, or the system scope, and returns ErrGroupNotFound otherwise).
func (s *Service) ResolveInviteAdminGroup(ctx context.Context, actor auth.Token, adminGroupID string) (string, string, error) {
	if adminGroupID == "" {
		return "", "", ErrInviteAdminGroupRequired
	}
	g, err := s.authorizeGroupManage(ctx, actor, adminGroupID, needUsers)
	if err != nil {
		return "", "", ErrInviteAdminGroupInvalid
	}
	if g.Tier != store.GroupTierAdmin {
		return "", "", ErrInviteAdminGroupInvalid
	}
	return g.ID, g.ParentGroupID, nil
}

// AddUserToAdminGroup enrolls userID as a member of the parent system group
// first (admin-tier containment requires it), then of the admin group. Both
// are direct member rows (state=member, invited_by=""). Admin-only: enforced
// at the HTTP layer (requireWebScope(..., "admin")) at its one call site (the
// invite flow in handleAdminUsers) and, as of PT-2 Part 2, replicated here via
// isAdmin(principal) as defense-in-depth against a future internal caller
// that bypasses the HTTP gate.
func (s *Service) AddUserToAdminGroup(ctx context.Context, principal auth.Token, userID, adminGroupID, parentSystemGroupID string) error {
	if !isAdmin(principal) {
		return ErrPrincipalForbidden
	}
	if parentSystemGroupID != "" {
		if err := s.groups.SetUserGroupMember(ctx, parentSystemGroupID, userID, store.GroupStateMember, ""); err != nil {
			return err
		}
	}
	return s.groups.SetUserGroupMember(ctx, adminGroupID, userID, store.GroupStateMember, "")
}

// AdminOwnerCandidateDTO is an eligible owner for a system-admin-created
// admin group: an active admin (or system_admin) other than the caller,
// with the system groups they belong to (the candidate's admin group can
// only be parented under one of these).
type AdminOwnerCandidateDTO struct {
	UserID       string        `json:"user_id"`
	DisplayName  string        `json:"display_name"`
	Email        string        `json:"email"`
	SystemGroups []GroupRefDTO `json:"system_groups"`
}

// AdminOwnerCandidates lists the admins a system-admin may hand a new admin
// group to. System-scope only.
func (s *Service) AdminOwnerCandidates(ctx context.Context, principal auth.Token) ([]AdminOwnerCandidateDTO, error) {
	if !isSystem(principal) {
		return nil, ErrGroupForbidden
	}
	users, err := s.users.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AdminOwnerCandidateDTO, 0, len(users))
	for _, u := range users {
		if u.ID == principal.UserID || u.Status != store.UserStatusActive || !isAdminRole(u.Role) {
			continue
		}
		sys, err := s.groups.UserGroupsForUser(ctx, u.ID, store.GroupTierSystem, store.GroupStateMember)
		if err != nil {
			return nil, err
		}
		refs := make([]GroupRefDTO, 0, len(sys))
		for _, g := range sys {
			refs = append(refs, GroupRefDTO{ID: g.ID, Name: g.Name})
		}
		out = append(out, AdminOwnerCandidateDTO{UserID: u.ID, DisplayName: u.DisplayName, Email: u.Email, SystemGroups: refs})
	}
	return out, nil
}

// --- Group CRUD (Task 6) ---------------------------------------------------

// Sentinel errors for the group CRUD/membership/manager service surface.
// HTTP mapping (stable error codes, group.*) is Task 9's concern.
var (
	ErrGroupNotFound      = errors.New("portal: group not found")
	ErrGroupNameConflict  = errors.New("portal: group name conflict")
	ErrGroupNameInvalid   = errors.New("portal: group name invalid")
	ErrGroupParentInvalid = errors.New("portal: group parent invalid")
	ErrGroupTierInvalid   = errors.New("portal: group tier invalid")
	ErrGroupForbidden     = errors.New("portal: group action forbidden")
	// ErrGroupMemberNotVisible is returned when a member/candidate id fails
	// the containment check (admin tier: not a member of the parent system
	// group; system tier: not in the actor's VisibleUserIDs).
	ErrGroupMemberNotVisible = errors.New("portal: group member not visible")
	// ErrGroupCandidateInvalid is returned when a promote/demote/transfer
	// target fails its role/membership eligibility constraint (Task 8 §8).
	ErrGroupCandidateInvalid = errors.New("portal: group candidate invalid")
	// ErrGroupNotParentMember is returned when a user-tier group invite
	// target fails containment: they are not a MEMBER of the group's parent
	// ADMIN group (spec §5.2/§9) — the sole eligibility gate for a user-tier
	// invite. Deliberately distinct from ErrGroupMemberNotVisible: a
	// user-tier invite never falls back to the generic VisibleUserIDs check
	// (see inviteGroupMembers).
	ErrGroupNotParentMember = errors.New("portal: invitee not a member of the parent admin group")
	// ErrGroupOwnerInvalid is returned by createAdminGroup when a caller with
	// the system scope requests an admin group FOR another owner (§ create-
	// for-another) but that owner is not an active admin/system_admin user.
	ErrGroupOwnerInvalid = errors.New("portal: admin group owner invalid")
)

// CreateGroupInput is the create request for CreateGroup.
type CreateGroupInput struct {
	Tier          string
	Name          string
	ParentGroupID string
	// OwnerUserID is admin-tier-only: when set to a user id OTHER than the
	// caller, a system_admin creates the group FOR that user (who becomes its
	// sole owner + member; the creator does NOT join). Blank, or equal to the
	// caller's own id, means "self-owned" -- today's unchanged behavior.
	OwnerUserID string
}

// UserGroupDTO is the read-model for a single group, from the perspective of
// the principal that requested it (MyRole/CanManage are principal-relative).
type UserGroupDTO struct {
	ID            string `json:"id"`
	Tier          string `json:"tier"`
	Name          string `json:"name"`
	ParentGroupID string `json:"parent_group_id"`
	OwnerUserID   string `json:"owner_user_id"`
	// OwnerName is the owner's display name, resolved best-effort (empty for a
	// system-tier / owner-less group, or if the user lookup fails) so the UI
	// can show a name instead of the opaque owner id.
	OwnerName string `json:"owner_name"`
	// MyRole is "owner" | "manager" | "member" | "" (system tier, or a
	// system_admin viewing a group they neither own/manage/belong to).
	MyRole string `json:"my_role"`
	// CanManage is principal-relative: may the caller manage the group's
	// STRUCTURE (rename/delete/membership) -- owner, OR a co-manager whose
	// CanManageGroup flag is set, OR a system_admin (per-Admin-Group
	// co-manager permissions, spec 2026-08-10; previously "can this caller do
	// anything" -- now narrowed to the structure facet specifically).
	CanManage bool `json:"can_manage"`
	// CanManageUsers is principal-relative: may the caller assign a NEW user
	// into the group (e.g. as an admin-group invite target) -- owner, OR a
	// co-manager whose CanManageUsers flag is set, OR a system_admin.
	CanManageUsers bool `json:"can_manage_users"`
	// CanManageServers is principal-relative: may the caller manage the
	// AI-servers linked to this group's admin tier (admin-group permissions
	// Phase B, spec 2026-08-10) -- owner, OR a co-manager whose
	// CanManageServers flag is set, OR a system_admin. Independent of
	// CanManage/CanManageUsers, same pattern.
	CanManageServers bool `json:"can_manage_servers"`
	// CanManageServices is principal-relative: may the caller manage the
	// services linked to this group's admin tier (admin-group permissions
	// Phase C, spec 2026-08-10) -- owner, OR a co-manager whose
	// CanManageServices flag is set, OR a system_admin. Independent of
	// CanManage/CanManageUsers/CanManageServers, same pattern.
	CanManageServices bool `json:"can_manage_services"`
	// CanManageResources is principal-relative: may the caller manage the
	// resources linked to this group's admin tier (Resource Groups Phase 1,
	// spec 2026-08-11) -- owner, OR a co-manager whose CanManageResources
	// flag is set, OR a system_admin. Independent of
	// CanManage/CanManageUsers/CanManageServers/CanManageServices, same
	// pattern.
	CanManageResources bool `json:"can_manage_resources"`
	MemberCount        int  `json:"member_count"`
	ManagerCount       int  `json:"manager_count"`
	// CoupledProjects lists the projects coupled to this group (spec 2026-08-09):
	// non-empty only for user-tier groups. Drives the group-delete warning.
	CoupledProjects []ProjectRefDTO `json:"coupled_projects"`
}

// GroupLandscapeDTO is the full per-principal group tree returned by
// ListGroups, per spec §11: system groups (system_admin only), admin groups
// (owned/managed/member-of), user groups (owned/managed/member-of).
type GroupLandscapeDTO struct {
	System []UserGroupDTO `json:"system"`
	Admin  []UserGroupDTO `json:"admin"`
	User   []UserGroupDTO `json:"user"`
}

// UserRefDTO is a minimal, non-secret user reference used by member/candidate
// lists (never exposes role/status/tokens).
type UserRefDTO struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// UserGroupMemberDTO is a single row in a group's current roster, as returned by
// GroupMembers -- unlike UserRefDTO (used by GroupMemberCandidates' ADD-able
// list), this also carries the row's state and role flags so the manage UI
// can render a real member/manager picker instead of a raw user-id field.
// No secrets.
type UserGroupMemberDTO struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	// State is "member" | "invited" (store.GroupStateMember/Invited).
	State     string `json:"state"`
	IsManager bool   `json:"is_manager"`
	IsOwner   bool   `json:"is_owner"`
	// CanManageUsers/CanManageGroup/CanManageServers/CanManageServices/
	// CanManageResources are this row's stored co-manager per-permission
	// flags (per-Admin-Group co-manager permissions, spec 2026-08-10 +
	// Phase B 2026-08-10 + Phase C 2026-08-10 + Resource Groups Phase 1
	// 2026-08-11) -- all false for a non-manager row (IsManager false).
	CanManageUsers     bool `json:"can_manage_users"`
	CanManageGroup     bool `json:"can_manage_group"`
	CanManageServers   bool `json:"can_manage_servers"`
	CanManageServices  bool `json:"can_manage_services"`
	CanManageResources bool `json:"can_manage_resources"`
}

// isAdminRole reports whether role can own/manage/be-promoted-into an admin
// group (admin or system_admin; never a plain user).
func isAdminRole(role string) bool {
	return role == "admin" || role == "system_admin"
}

// memberIDSet returns the set of userIDs with an ACTIVE (state=member) row in
// groupID — an "invited" row does not count.
func (s *Service) memberIDSet(ctx context.Context, groupID string) (map[string]bool, error) {
	members, err := s.groups.UserGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(members))
	for _, m := range members {
		if m.State == store.GroupStateMember {
			out[m.UserID] = true
		}
	}
	return out, nil
}

func intersectIDSets(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a))
	for id := range a {
		if b[id] {
			out[id] = true
		}
	}
	return out
}

// groupManageNeed selects which per-Admin-Group co-manager permission facet
// authorizeGroupManage checks (spec 2026-08-10). The group's owner and a
// system-scope caller always pass regardless of need -- this only narrows
// what a plain CO-MANAGER may reach.
type groupManageNeed int

const (
	// needRead is satisfied by ANY co-manager, regardless of their
	// CanManageUsers/CanManageGroup flags -- "is this caller a manager (or
	// owner/system) of this group at all" (e.g. GroupMembers' roster read,
	// and the shared owner-only gate authorizeOwnerAction sits on top of --
	// a co-manager lacking a specific facet still legitimately sees the
	// group exists and gets ErrGroupForbidden, not a no-leak 404, when they
	// then attempt an owner-only action).
	needRead groupManageNeed = iota
	// needGroup requires a co-manager's CanManageGroup flag: managing the
	// group's STRUCTURE (rename/delete/membership add-or-remove/candidates).
	needGroup
	// needUsers requires a co-manager's CanManageUsers flag: assigning a new
	// USER into the group (e.g. ResolveInviteAdminGroup's invite path).
	needUsers
)

// authorizeGroupManage loads the group and enforces the per-tier "may manage
// this group" gate, 404-no-leak on failure (ErrGroupNotFound is returned both
// when the group truly does not exist AND when it exists but is invisible to
// the principal — never a distinguishable forbidden, per spec §6.3):
//
//   - system tier: only a system_admin (HasScope("system")); everyone else,
//     including a plain admin, gets ErrGroupNotFound (no existence leak).
//   - admin/user tier: the owner, OR a system_admin, OR a co-manager whose
//     stored per-permission flags satisfy need (per-Admin-Group co-manager
//     permissions, spec 2026-08-10) -- a co-manager who fails the specific
//     need gets the SAME no-leak ErrGroupNotFound as a non-member (never a
//     distinguishable forbidden here either; the group is simply not
//     manageable-by-them-for-this-purpose).
func (s *Service) authorizeGroupManage(ctx context.Context, principal auth.Token, id string, need groupManageNeed) (store.UserGroup, error) {
	g, err := s.groups.UserGroupByID(ctx, id)
	if err != nil {
		return store.UserGroup{}, ErrGroupNotFound
	}
	if g.Tier == store.GroupTierSystem {
		if !isSystem(principal) {
			return store.UserGroup{}, ErrGroupNotFound
		}
		return g, nil
	}
	if isSystem(principal) {
		return g, nil
	}
	if g.OwnerUserID != "" && g.OwnerUserID == principal.UserID {
		return g, nil
	}
	perms, err := s.groups.UserGroupManagerPerms(ctx, id)
	if err != nil {
		return store.UserGroup{}, err
	}
	for _, p := range perms {
		if p.UserID != principal.UserID {
			continue
		}
		switch need {
		case needGroup:
			if !p.CanManageGroup {
				return store.UserGroup{}, ErrGroupNotFound
			}
		case needUsers:
			if !p.CanManageUsers {
				return store.UserGroup{}, ErrGroupNotFound
			}
		}
		return g, nil
	}
	return store.UserGroup{}, ErrGroupNotFound
}

// groupNameConflict reports whether name (case-insensitive) already exists
// among the given siblings, excluding excludeID (used by rename to allow a
// no-op / case-only rename of the group itself).
func groupNameConflict(siblings []store.UserGroup, name, excludeID string) bool {
	for _, sib := range siblings {
		if sib.ID == excludeID {
			continue
		}
		if strings.EqualFold(sib.Name, name) {
			return true
		}
	}
	return false
}

// CreateGroup creates a group per spec §4.1 (uniqueness)/§7 (authorization +
// parent resolution). Owner is set to the creating principal for admin/user
// tiers, and left empty ("") for the ownerless system tier.
func (s *Service) CreateGroup(ctx context.Context, principal auth.Token, in CreateGroupInput) (UserGroupDTO, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return UserGroupDTO{}, ErrGroupNameInvalid
	}
	switch in.Tier {
	case store.GroupTierSystem:
		return s.createSystemGroup(ctx, principal, name)
	case store.GroupTierAdmin:
		return s.createAdminGroup(ctx, principal, name, in.ParentGroupID, in.OwnerUserID)
	case store.GroupTierUser:
		return s.createUserGroup(ctx, principal, name, in.ParentGroupID)
	default:
		return UserGroupDTO{}, ErrGroupTierInvalid
	}
}

func (s *Service) createSystemGroup(ctx context.Context, principal auth.Token, name string) (UserGroupDTO, error) {
	if !isSystem(principal) {
		return UserGroupDTO{}, ErrGroupForbidden
	}
	all, err := s.groups.ListUserGroupsByTier(ctx, store.GroupTierSystem)
	if err != nil {
		return UserGroupDTO{}, err
	}
	if groupNameConflict(all, name, "") {
		return UserGroupDTO{}, ErrGroupNameConflict
	}
	now := s.clock().UTC()
	g := store.UserGroup{
		ID:        "ugrp_" + s.idGenerator(),
		Tier:      store.GroupTierSystem,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.groups.CreateUserGroup(ctx, g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return UserGroupDTO{}, ErrGroupNameConflict
		}
		return UserGroupDTO{}, err
	}
	return s.groupDTO(ctx, principal, g), nil
}

// createAdminGroup implements §7.2 (self-owned; unchanged) plus a
// create-for-another extension (system_admin only): any admin (or
// system_admin) may create an admin group whose parent is a system group the
// CREATOR is a MEMBER of — auto-selected when the creator has exactly one,
// else the caller-provided parent must be one of the creator's own (a
// system_admin may pick ANY system group as parent). When a system_admin
// caller instead supplies ownerID for a DIFFERENT user, the new group is
// created FOR that user: they alone become its owner + sole enrolled member
// (the creating system_admin does NOT join), and the parent resolves from the
// OWNER's own system-group memberships (not the creator's).
func (s *Service) createAdminGroup(ctx context.Context, principal auth.Token, name, parentID, ownerID string) (UserGroupDTO, error) {
	if !isAdmin(principal) && !isSystem(principal) {
		return UserGroupDTO{}, ErrGroupForbidden
	}
	ownerID = strings.TrimSpace(ownerID)
	forAnother := ownerID != "" && ownerID != principal.UserID

	// ownerUserID owns the new group AND is the sole auto-enrolled member.
	ownerUserID := principal.UserID
	var parent string

	if !forAnother {
		// --- self-owned (behavior unchanged from before this feature) ---
		own, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierSystem, store.GroupStateMember)
		if err != nil {
			return UserGroupDTO{}, err
		}
		ownIDs := make(map[string]bool, len(own))
		for _, g := range own {
			ownIDs[g.ID] = true
		}
		parent = parentID
		if parent == "" {
			switch len(own) {
			case 1:
				parent = own[0].ID
			default:
				return UserGroupDTO{}, ErrGroupParentInvalid
			}
		} else {
			pg, err := s.groups.UserGroupByID(ctx, parent)
			if err != nil || pg.Tier != store.GroupTierSystem {
				return UserGroupDTO{}, ErrGroupParentInvalid
			}
			if !isSystem(principal) && !ownIDs[parent] {
				return UserGroupDTO{}, ErrGroupParentInvalid
			}
		}
	} else {
		// --- create-for-another: system_admin only; parent from the OWNER's
		// own system groups (no auto-add; containment holds by construction) ---
		if !isSystem(principal) {
			return UserGroupDTO{}, ErrGroupForbidden
		}
		owner, err := s.users.UserByID(ctx, ownerID)
		if err != nil || owner.Status != store.UserStatusActive || !isAdminRole(owner.Role) {
			return UserGroupDTO{}, ErrGroupOwnerInvalid
		}
		ownerSys, err := s.groups.UserGroupsForUser(ctx, ownerID, store.GroupTierSystem, store.GroupStateMember)
		if err != nil {
			return UserGroupDTO{}, err
		}
		ownerSysIDs := make(map[string]bool, len(ownerSys))
		for _, g := range ownerSys {
			ownerSysIDs[g.ID] = true
		}
		parent = parentID
		if parent == "" {
			switch len(ownerSys) {
			case 1:
				parent = ownerSys[0].ID
			default:
				// 0 -> owner in no system group; >1 -> caller must pick one of theirs.
				return UserGroupDTO{}, ErrGroupParentInvalid
			}
		} else if !ownerSysIDs[parent] {
			return UserGroupDTO{}, ErrGroupParentInvalid
		}
		ownerUserID = ownerID
	}

	siblings, err := s.groups.ChildUserGroups(ctx, parent)
	if err != nil {
		return UserGroupDTO{}, err
	}
	if groupNameConflict(siblings, name, "") {
		return UserGroupDTO{}, ErrGroupNameConflict
	}
	now := s.clock().UTC()
	g := store.UserGroup{
		ID:            "ugrp_" + s.idGenerator(),
		Tier:          store.GroupTierAdmin,
		Name:          name,
		ParentGroupID: parent,
		OwnerUserID:   ownerUserID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.groups.CreateUserGroup(ctx, g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return UserGroupDTO{}, ErrGroupNameConflict
		}
		return UserGroupDTO{}, err
	}
	// Enroll ONLY the owner as a member (self-owned: the creator, as before;
	// create-for-another: the target admin — never the system_admin creator).
	if err := s.groups.SetUserGroupMember(ctx, g.ID, ownerUserID, store.GroupStateMember, ""); err != nil {
		return UserGroupDTO{}, err
	}
	return s.groupDTO(ctx, principal, g), nil
}

// createUserGroup implements §7.3: any authenticated principal may create a
// user group whose parent is an admin group the creator is a MEMBER of —
// auto-selected when the creator has exactly one, else the caller-provided
// parent must be one of the creator's own (no system_admin override here —
// unlike admin-group creation, §7.3 grants no "pick any parent" carve-out).
func (s *Service) createUserGroup(ctx context.Context, principal auth.Token, name, parentID string) (UserGroupDTO, error) {
	own, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierAdmin, store.GroupStateMember)
	if err != nil {
		return UserGroupDTO{}, err
	}
	ownIDs := make(map[string]bool, len(own))
	for _, g := range own {
		ownIDs[g.ID] = true
	}
	parent := parentID
	if parent == "" {
		switch len(own) {
		case 1:
			parent = own[0].ID
		default:
			return UserGroupDTO{}, ErrGroupParentInvalid
		}
	} else {
		pg, err := s.groups.UserGroupByID(ctx, parent)
		if err != nil || pg.Tier != store.GroupTierAdmin {
			return UserGroupDTO{}, ErrGroupParentInvalid
		}
		if !ownIDs[parent] {
			return UserGroupDTO{}, ErrGroupParentInvalid
		}
	}
	// Uniqueness is PER OWNER (not per parent) — scan every user-tier group
	// this principal owns, regardless of which admin group it hangs under.
	mine, err := s.groups.ListUserGroupsByTier(ctx, store.GroupTierUser)
	if err != nil {
		return UserGroupDTO{}, err
	}
	owned := mine[:0:0]
	for _, g := range mine {
		if g.OwnerUserID == principal.UserID {
			owned = append(owned, g)
		}
	}
	if groupNameConflict(owned, name, "") {
		return UserGroupDTO{}, ErrGroupNameConflict
	}
	now := s.clock().UTC()
	g := store.UserGroup{
		ID:            "ugrp_" + s.idGenerator(),
		Tier:          store.GroupTierUser,
		Name:          name,
		ParentGroupID: parent,
		OwnerUserID:   principal.UserID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.groups.CreateUserGroup(ctx, g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return UserGroupDTO{}, ErrGroupNameConflict
		}
		return UserGroupDTO{}, err
	}
	// See createAdminGroup's comment: the creator is also enrolled as a full
	// member of the group they now own.
	if err := s.groups.SetUserGroupMember(ctx, g.ID, principal.UserID, store.GroupStateMember, ""); err != nil {
		return UserGroupDTO{}, err
	}
	return s.groupDTO(ctx, principal, g), nil
}

// groupDTO computes the principal-relative view (MyRole/CanManage) of g,
// plus its member/manager counts. Called once per group so N+1 store reads
// are the cost of correctness over a raw list (acceptable at this scale —
// see the Task 6 report for a documented performance note).
func (s *Service) groupDTO(ctx context.Context, principal auth.Token, g store.UserGroup) UserGroupDTO {
	dto := UserGroupDTO{
		ID:            g.ID,
		Tier:          g.Tier,
		Name:          g.Name,
		ParentGroupID: g.ParentGroupID,
		OwnerUserID:   g.OwnerUserID,
	}
	// Resolve the owner's display name best-effort (empty owner or a failed
	// lookup leaves it "" — the UI falls back to the id), mirroring projectDTO.
	if g.OwnerUserID != "" && s.users != nil {
		if u, err := s.users.UserByID(ctx, g.OwnerUserID); err == nil {
			dto.OwnerName = u.DisplayName
		}
	}
	members, _ := s.groups.UserGroupMembers(ctx, g.ID)
	isMember := false
	for _, m := range members {
		if m.State != store.GroupStateMember {
			continue
		}
		dto.MemberCount++
		if m.UserID == principal.UserID {
			isMember = true
		}
	}
	var perms []store.UserGroupManagerPerm
	if g.Tier != store.GroupTierSystem {
		perms, _ = s.groups.UserGroupManagerPerms(ctx, g.ID)
	}
	dto.ManagerCount = len(perms)
	isManager := false
	var myPerm store.UserGroupManagerPerm
	for _, p := range perms {
		if p.UserID == principal.UserID {
			isManager = true
			myPerm = p
		}
	}
	switch {
	case g.Tier == store.GroupTierSystem:
		// System groups have no owner/manager concept (§8 preamble) — every
		// system_admin manages every system group equally, every facet.
		dto.CanManage = isSystem(principal)
		dto.CanManageUsers = isSystem(principal)
		dto.CanManageServers = isSystem(principal)
		dto.CanManageServices = isSystem(principal)
		dto.CanManageResources = isSystem(principal)
	case g.OwnerUserID != "" && g.OwnerUserID == principal.UserID:
		dto.MyRole = "owner"
		dto.CanManage = true
		dto.CanManageUsers = true
		dto.CanManageServers = true
		dto.CanManageServices = true
		dto.CanManageResources = true
	case isManager:
		// A co-manager's facets are independent -- CanManage (structure),
		// CanManageUsers, CanManageServers, CanManageServices, and
		// CanManageResources each follow ONLY their own stored flag, per spec
		// 2026-08-10 (a manager narrowed to one facet must not silently gain
		// another here).
		dto.MyRole = "manager"
		dto.CanManage = myPerm.CanManageGroup
		dto.CanManageUsers = myPerm.CanManageUsers
		dto.CanManageServers = myPerm.CanManageServers
		dto.CanManageServices = myPerm.CanManageServices
		dto.CanManageResources = myPerm.CanManageResources
	case isSystem(principal):
		if isMember {
			dto.MyRole = "member"
		}
		dto.CanManage = true
		dto.CanManageUsers = true
		dto.CanManageServers = true
		dto.CanManageServices = true
		dto.CanManageResources = true
	case isMember:
		dto.MyRole = "member"
	}
	// CoupledProjects: only user-tier groups can be coupled (spec 2026-08-09);
	// gate the query to avoid a needless lookup for system/admin groups. Init
	// to a non-nil empty slice so the JSON is `[]`, never `null` (a nil Go
	// slice marshals to `null`, which crashes a naive TS `.length`/`.map`
	// consumer) — best-effort: a lookup error leaves it empty rather than
	// failing the whole group listing.
	dto.CoupledProjects = []ProjectRefDTO{}
	if g.Tier == store.GroupTierUser && s.projects != nil {
		if coupled, err := s.projects.CoupledProjectsByGroup(ctx, g.ID); err == nil {
			for _, p := range coupled {
				dto.CoupledProjects = append(dto.CoupledProjects, ProjectRefDTO{ID: p.ID, Name: p.Name})
			}
		}
	}
	return dto
}

// myGroupsRaw returns the store-level user groups of the given tier that the
// principal owns, co-manages, or is a member of (deduped + sorted). It is the
// shared basis for myGroups (DTO wrapper) and ListGroups' admin/default
// branches, which need the raw groups to compute the parent-linked expansion.
func (s *Service) myGroupsRaw(ctx context.Context, principal auth.Token, tier string) ([]store.UserGroup, error) {
	all, err := s.groups.ListUserGroupsByTier(ctx, tier)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]store.UserGroup, len(all))
	for _, g := range all {
		if g.OwnerUserID != "" && g.OwnerUserID == principal.UserID {
			seen[g.ID] = g
		}
	}
	memberOf, err := s.groups.UserGroupsForUser(ctx, principal.UserID, tier, store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	for _, g := range memberOf {
		seen[g.ID] = g
	}
	for _, g := range all {
		if _, ok := seen[g.ID]; ok {
			continue
		}
		managers, mErr := s.groups.UserGroupManagers(ctx, g.ID)
		if mErr != nil {
			return nil, mErr
		}
		for _, m := range managers {
			if m == principal.UserID {
				seen[g.ID] = g
				break
			}
		}
	}
	list := make([]store.UserGroup, 0, len(seen))
	for _, g := range seen {
		list = append(list, g)
	}
	sortUserGroups(list)
	return list, nil
}

// ListGroups assembles the principal's full group landscape, tier-aware per
// spec 2026-08-09 (extends the original §11/§7.2 behavior):
//   - system_admin: System/Admin/User each = every group of that tier.
//   - a plain admin: System = the admin's own member-of system groups
//     (unchanged); Admin = the admin's own admin groups (unchanged,
//     myGroupsRaw); User = the admin's own user groups UNION every user
//     group that is a child of one of the admin's own admin groups (read-only
//     for the latter — parent-linked visibility, not membership).
//   - a plain user (default): unchanged (own admin groups + own user groups,
//     System empty).
//
// System/Admin/User MUST default to a non-nil empty slice: encoding/json
// marshals a nil Go slice as `null` rather than `[]`, and the frontend
// (GroupsView.tsx) unconditionally maps over every section — a null field
// crashes it. Caught live by the sqlite-backed e2e:groups suite driving a
// real "user" login into the Groups view (task 16).
func (s *Service) ListGroups(ctx context.Context, principal auth.Token) (GroupLandscapeDTO, error) {
	out := GroupLandscapeDTO{System: []UserGroupDTO{}, Admin: []UserGroupDTO{}, User: []UserGroupDTO{}}
	dtosByTier := func(tier string) ([]UserGroupDTO, error) {
		all, err := s.groups.ListUserGroupsByTier(ctx, tier)
		if err != nil {
			return nil, err
		}
		dtos := make([]UserGroupDTO, 0, len(all))
		for _, g := range all {
			dtos = append(dtos, s.groupDTO(ctx, principal, g))
		}
		return dtos, nil
	}
	rawToDTO := func(raw []store.UserGroup) []UserGroupDTO {
		dtos := make([]UserGroupDTO, 0, len(raw))
		for _, g := range raw {
			dtos = append(dtos, s.groupDTO(ctx, principal, g))
		}
		return dtos
	}

	switch {
	case isSystem(principal):
		// system_admin sees ALL groups at every tier.
		var err error
		if out.System, err = dtosByTier(store.GroupTierSystem); err != nil {
			return GroupLandscapeDTO{}, err
		}
		if out.Admin, err = dtosByTier(store.GroupTierAdmin); err != nil {
			return GroupLandscapeDTO{}, err
		}
		if out.User, err = dtosByTier(store.GroupTierUser); err != nil {
			return GroupLandscapeDTO{}, err
		}
	case isAdmin(principal):
		// System: the admin's own member-of system groups (unchanged).
		sys, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierSystem, store.GroupStateMember)
		if err != nil {
			return GroupLandscapeDTO{}, err
		}
		out.System = make([]UserGroupDTO, 0, len(sys))
		for _, g := range sys {
			out.System = append(out.System, s.groupDTO(ctx, principal, g))
		}
		// Admin: the admin's own admin groups (unchanged).
		adminRaw, err := s.myGroupsRaw(ctx, principal, store.GroupTierAdmin)
		if err != nil {
			return GroupLandscapeDTO{}, err
		}
		out.Admin = rawToDTO(adminRaw)
		// User: own user groups UNION every user group whose parent admin
		// group the admin belongs to (read-only for the latter).
		userRaw, err := s.myGroupsRaw(ctx, principal, store.GroupTierUser)
		if err != nil {
			return GroupLandscapeDTO{}, err
		}
		seen := make(map[string]bool, len(userRaw))
		for _, g := range userRaw {
			seen[g.ID] = true
		}
		for _, ag := range adminRaw {
			children, err := s.groups.ChildUserGroups(ctx, ag.ID)
			if err != nil {
				return GroupLandscapeDTO{}, err
			}
			for _, c := range children {
				if c.Tier != store.GroupTierUser || seen[c.ID] {
					continue
				}
				seen[c.ID] = true
				userRaw = append(userRaw, c)
			}
		}
		sortUserGroups(userRaw)
		out.User = rawToDTO(userRaw)
	default:
		// regular user: unchanged.
		adminRaw, err := s.myGroupsRaw(ctx, principal, store.GroupTierAdmin)
		if err != nil {
			return GroupLandscapeDTO{}, err
		}
		out.Admin = rawToDTO(adminRaw)
		userRaw, err := s.myGroupsRaw(ctx, principal, store.GroupTierUser)
		if err != nil {
			return GroupLandscapeDTO{}, err
		}
		out.User = rawToDTO(userRaw)
	}
	return out, nil
}

// RenameGroup renames a group (owner, co-manager, or system_admin — the same
// gate as authorizeGroupManage), re-checking name uniqueness within the
// group's own scope (excluding itself, so a no-op/case-only rename is fine).
func (s *Service) RenameGroup(ctx context.Context, principal auth.Token, id, name string) (UserGroupDTO, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return UserGroupDTO{}, ErrGroupNameInvalid
	}
	g, err := s.authorizeGroupManage(ctx, principal, id, needGroup)
	if err != nil {
		return UserGroupDTO{}, err
	}
	var siblings []store.UserGroup
	switch g.Tier {
	case store.GroupTierSystem:
		siblings, err = s.groups.ListUserGroupsByTier(ctx, store.GroupTierSystem)
	case store.GroupTierAdmin:
		siblings, err = s.groups.ChildUserGroups(ctx, g.ParentGroupID)
	case store.GroupTierUser:
		var all []store.UserGroup
		all, err = s.groups.ListUserGroupsByTier(ctx, store.GroupTierUser)
		if err == nil {
			for _, sib := range all {
				if sib.OwnerUserID == g.OwnerUserID {
					siblings = append(siblings, sib)
				}
			}
		}
	}
	if err != nil {
		return UserGroupDTO{}, err
	}
	if groupNameConflict(siblings, name, g.ID) {
		return UserGroupDTO{}, ErrGroupNameConflict
	}
	g.Name = name
	g.UpdatedAt = s.clock().UTC()
	if err := s.groups.UpdateUserGroup(ctx, g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return UserGroupDTO{}, ErrGroupNameConflict
		}
		if errors.Is(err, store.ErrNotFound) {
			return UserGroupDTO{}, ErrGroupNotFound
		}
		return UserGroupDTO{}, err
	}
	return s.groupDTO(ctx, principal, g), nil
}

// DeleteGroup deletes a group (FK-cascade removes children + member/manager
// rows). System tier: system_admin only. Admin/user tier: OWNER or
// system_admin only — a co-manager is visible (authorizeGroupManage succeeds)
// but gets ErrGroupForbidden, per spec §8 ("NICHT löschen (nur Eigentümer)").
func (s *Service) DeleteGroup(ctx context.Context, principal auth.Token, id string) error {
	g, err := s.authorizeGroupManage(ctx, principal, id, needGroup)
	if err != nil {
		return err
	}
	if g.Tier != store.GroupTierSystem && !isSystem(principal) && g.OwnerUserID != principal.UserID {
		return ErrGroupForbidden
	}
	if err := s.groups.DeleteUserGroup(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrGroupNotFound
		}
		return err
	}
	return nil
}

// --- Membership (Task 7) ----------------------------------------------------

// AddGroupMembers adds the given userIDs to groupID. For a system/admin tier
// group this is the DIRECT path (state=member, invited_by=""); for a
// user-tier group it is routed to inviteGroupMembers (Task 10), which adds
// them as a PENDING invitation instead (state=invited) — a user-tier
// membership is never granted directly, only accepted via RespondInvitation.
//
// Containment (spec §5.2), enforced per-user so a partial add can never
// smuggle in one invisible id alongside valid ones:
//   - admin tier: each userID must be a MEMBER of the group's parent system
//     group.
//   - system tier: each userID must be in the actor's VisibleUserIDs (in
//     practice "everyone", since only a system_admin — who sees all users —
//     can reach this branch via authorizeGroupManage).
//   - user tier: see inviteGroupMembers — membership of the group's parent
//     ADMIN group, and ONLY that; the generic VisibleUserIDs fallback below
//     is deliberately never consulted for this tier (it would admit a
//     sibling admin group's members, who are merely "visible" to the actor
//     through their OWN admin-group membership but not eligible for THIS
//     group — the containment hazard this branch closes).
func (s *Service) AddGroupMembers(ctx context.Context, principal auth.Token, groupID string, userIDs []string) error {
	g, err := s.authorizeGroupManage(ctx, principal, groupID, needGroup)
	if err != nil {
		return err
	}
	if g.Tier == store.GroupTierUser {
		return s.inviteGroupMembers(ctx, principal, g, userIDs)
	}
	visible, err := s.VisibleUserIDs(ctx, principal)
	if err != nil {
		return err
	}
	var parentMemberIDs map[string]bool
	if g.Tier == store.GroupTierAdmin {
		parentMemberIDs, err = s.memberIDSet(ctx, g.ParentGroupID)
		if err != nil {
			return err
		}
	}
	for _, uid := range userIDs {
		if !visible[uid] {
			return ErrGroupMemberNotVisible
		}
		if parentMemberIDs != nil && !parentMemberIDs[uid] {
			return ErrGroupMemberNotVisible
		}
	}
	for _, uid := range userIDs {
		if err := s.groups.SetUserGroupMember(ctx, groupID, uid, store.GroupStateMember, ""); err != nil {
			return err
		}
	}
	return nil
}

// inviteGroupMembers is AddGroupMembers' user-tier branch (spec §9): each
// userID is added to g as a PENDING invitation (state=invited,
// invited_by=principal.UserID), gated on membership of g's parent ADMIN
// group — checked DIRECTLY via memberIDSet, never through the generic
// VisibleUserIDs fallback the admin/system branch above uses (that fallback
// is keyed off every admin group the ACTOR belongs to, not specifically g's
// parent — too broad, and exactly the containment hazard this branch
// closes). This diverges from GroupMemberCandidates, which narrows the SAME
// parent-member set further by intersecting it with the actor's own
// VisibleUserIDs (relevant when listing candidates for THIS actor to pick
// from); an invite's eligibility rule is parent-admin-group membership
// alone, so the two checks are related but not identical — do not describe
// this as "mirroring" candidates. Whole-batch validated before any write,
// mirroring the direct path's all-or-nothing containment check.
//
// A userID that ALREADY holds state=member is left untouched (skipped, never
// re-upserted): SetUserGroupMember unconditionally overwrites the stored
// state, so inviting an EXISTING member — a raw API retry, or the group's own
// owner (auto-enrolled as a member at creation) — would otherwise silently
// DEMOTE them member→invited, dropping them out of memberIDSet/visibility
// until they happen to re-accept. Re-inviting an already-invited userID is
// harmless (same state; invited_by refreshed to the current inviter).
func (s *Service) inviteGroupMembers(ctx context.Context, principal auth.Token, g store.UserGroup, userIDs []string) error {
	parentMemberIDs, err := s.memberIDSet(ctx, g.ParentGroupID)
	if err != nil {
		return err
	}
	for _, uid := range userIDs {
		if !parentMemberIDs[uid] {
			return ErrGroupNotParentMember
		}
	}
	currentMembers, err := s.memberIDSet(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, uid := range userIDs {
		if currentMembers[uid] {
			continue
		}
		if err := s.groups.SetUserGroupMember(ctx, g.ID, uid, store.GroupStateInvited, principal.UserID); err != nil {
			return err
		}
	}
	return nil
}

// InvitationDTO is a principal's own pending (state=invited) user-tier group
// membership, as returned by ListInvitations.
type InvitationDTO struct {
	GroupID       string `json:"group_id"`
	GroupName     string `json:"group_name"`
	ParentGroupID string `json:"parent_group_id"`
	InvitedBy     string `json:"invited_by"`
}

// RespondInvitation lets the invitee accept or decline their OWN pending
// invitation to groupID — never an owner/manager action, so this does NOT go
// through authorizeGroupManage. The principal must hold an invited-state row
// in groupID themselves; a missing group, a group the caller was never
// invited to, and someone else's invitation all read identically as
// ErrGroupNotFound (no-leak, per spec §6.3's existing convention). Accepting
// flips the row to state=member (invited_by cleared, mirroring the direct-add
// path); declining removes it outright.
func (s *Service) RespondInvitation(ctx context.Context, principal auth.Token, groupID string, accept bool) error {
	members, err := s.groups.UserGroupMembers(ctx, groupID)
	if err != nil {
		return err
	}
	invited := false
	for _, m := range members {
		if m.UserID == principal.UserID && m.State == store.GroupStateInvited {
			invited = true
			break
		}
	}
	if !invited {
		return ErrGroupNotFound
	}
	if accept {
		return s.groups.SetUserGroupMember(ctx, groupID, principal.UserID, store.GroupStateMember, "")
	}
	return s.groups.RemoveUserGroupMember(ctx, groupID, principal.UserID)
}

// ListInvitations returns the principal's own pending user-tier group
// invitations (state=invited), each annotated with the group's name/parent
// and who invited them.
func (s *Service) ListInvitations(ctx context.Context, principal auth.Token) ([]InvitationDTO, error) {
	groups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierUser, store.GroupStateInvited)
	if err != nil {
		return nil, err
	}
	out := make([]InvitationDTO, 0, len(groups))
	for _, g := range groups {
		members, err := s.groups.UserGroupMembers(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		invitedBy := ""
		for _, m := range members {
			if m.UserID == principal.UserID && m.State == store.GroupStateInvited {
				invitedBy = m.InvitedBy
				break
			}
		}
		out = append(out, InvitationDTO{
			GroupID:       g.ID,
			GroupName:     g.Name,
			ParentGroupID: g.ParentGroupID,
			InvitedBy:     invitedBy,
		})
	}
	return out, nil
}

// removeMemberCascade removes userID's member AND manager rows from groupID
// and recursively from every descendant group (spec §5.3 — containment must
// never be left violated: a user removed from a system group can no longer
// legitimately sit in a child admin group's member/manager rows either).
// removeMemberCascade walks the descendant tree BOTTOM-UP (every child fully
// processed, including its own succession, before this group's own removal +
// succession runs) — this matters because succession for THIS level can
// DELETE the group; processing children first guarantees they are never
// dangling by the time that happens. It also triggers owner succession at
// EVERY level the removed user happens to own, not just the top-level group
// RemoveGroupMember was called on: userID being cascade-removed from a
// descendant group's membership row genuinely means they "left" that group
// too (spec §8.1's succession trigger), so if userID owns groupID itself
// (system tier has no owner concept and is skipped), succeedOwner runs for
// groupID once its own member/manager rows are gone.
func (s *Service) removeMemberCascade(ctx context.Context, groupID, userID string) error {
	children, err := s.groups.ChildUserGroups(ctx, groupID)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := s.removeMemberCascade(ctx, c.ID, userID); err != nil {
			return err
		}
	}
	g, err := s.groups.UserGroupByID(ctx, groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Already gone (defensive — shouldn't happen given the
			// bottom-up ordering, but a vanished group has nothing left to
			// remove userID from).
			return nil
		}
		return err
	}
	if err := s.groups.RemoveUserGroupMember(ctx, groupID, userID); err != nil {
		return err
	}
	if err := s.groups.RemoveUserGroupManager(ctx, groupID, userID); err != nil {
		return err
	}
	if g.Tier == store.GroupTierSystem {
		return nil
	}
	if g.OwnerUserID != "" && g.OwnerUserID == userID {
		return s.succeedOwner(ctx, g)
	}
	if g.OwnerUserID == "" {
		// The group is ALREADY ownerless (a prior succession found nobody
		// eligible — admin tier only, in practice). userID departing was
		// necessarily a plain member (not "the owner", since there is
		// none) — but they may have been its LAST member, in which case
		// spec §8.1's "kept while it has members, deleted only when truly
		// empty" now applies retroactively. Nothing hooks this case other
		// than re-checking here on every membership change.
		return s.deleteIfOwnerlessAndEmpty(ctx, g)
	}
	return nil
}

// deleteIfOwnerlessAndEmpty deletes g (already ownerless) iff it has zero
// remaining state=member rows. Called after every membership removal from an
// ownerless group, since there is no other trigger for "the last member of a
// keep-while-non-empty group just left".
func (s *Service) deleteIfOwnerlessAndEmpty(ctx context.Context, g store.UserGroup) error {
	members, err := s.groups.UserGroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.State == store.GroupStateMember {
			return nil
		}
	}
	if err := s.groups.DeleteUserGroup(ctx, g.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// RemoveGroupMember removes userID from groupID (with the full descendant
// cascade). Authorized either by authorizeGroupManage (owner/manager/
// system_admin) OR by self-leave (principal.UserID == userID — a plain
// member may always remove themselves, even though they could never pass
// authorizeGroupManage). Owner succession (Task 8 §8.1) is handled INSIDE
// removeMemberCascade itself (at every level of the cascade whose owner is
// userID, not just groupID) — see its doc comment.
func (s *Service) RemoveGroupMember(ctx context.Context, principal auth.Token, groupID, userID string) error {
	if principal.UserID == userID {
		if _, err := s.groups.UserGroupByID(ctx, groupID); err != nil {
			return ErrGroupNotFound
		}
	} else if _, err := s.authorizeGroupManage(ctx, principal, groupID, needGroup); err != nil {
		return err
	}
	return s.removeMemberCascade(ctx, groupID, userID)
}

// GroupMemberCandidates lists the users eligible to be added/invited to
// groupID: the actor's VisibleUserIDs, narrowed for admin/user tier groups to
// the parent group's own members (containment), minus current members.
func (s *Service) GroupMemberCandidates(ctx context.Context, principal auth.Token, groupID string) ([]UserRefDTO, error) {
	g, err := s.authorizeGroupManage(ctx, principal, groupID, needGroup)
	if err != nil {
		return nil, err
	}
	visible, err := s.VisibleUserIDs(ctx, principal)
	if err != nil {
		return nil, err
	}
	candidateIDs := visible
	if g.Tier == store.GroupTierAdmin || g.Tier == store.GroupTierUser {
		parentIDs, err := s.memberIDSet(ctx, g.ParentGroupID)
		if err != nil {
			return nil, err
		}
		candidateIDs = intersectIDSets(visible, parentIDs)
	}
	current, err := s.memberIDSet(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]UserRefDTO, 0, len(candidateIDs))
	for uid := range candidateIDs {
		if current[uid] {
			continue
		}
		u, err := s.users.UserByID(ctx, uid)
		if err != nil {
			continue
		}
		out = append(out, UserRefDTO{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GroupMembers lists groupID's CURRENT roster -- both state=member AND
// state=invited rows, each annotated with identity + role flags -- for the
// manage UI's real member/manager/owner picker (as opposed to
// GroupMemberCandidates, which lists only ADD-able users, i.e. NOT current
// members). Authorized identically to every other management op
// (authorizeGroupManage): only the group's owner/manager or a system_admin
// may list it -- a plain member (who can't manage the group) gets
// ErrGroupNotFound, 404-no-leak per spec §6.3, same as a nonexistent group.
func (s *Service) GroupMembers(ctx context.Context, principal auth.Token, groupID string) ([]UserGroupMemberDTO, error) {
	g, err := s.authorizeGroupManage(ctx, principal, groupID, needRead)
	if err != nil {
		return nil, err
	}
	members, err := s.groups.UserGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	// perms maps a co-manager's userID to their stored per-permission flags
	// (per-Admin-Group co-manager permissions, spec 2026-08-10 + Phase B
	// 2026-08-10 + Phase C 2026-08-10 + Resource Groups Phase 1 2026-08-11); a
	// member absent from this map is not a manager at all, so their DTO row's
	// IsManager/CanManageUsers/CanManageGroup/CanManageServers/CanManageServices/CanManageResources
	// all read false (the zero value of the missing-key lookup below).
	var perms map[string]store.UserGroupManagerPerm
	if g.Tier != store.GroupTierSystem {
		list, err := s.groups.UserGroupManagerPerms(ctx, groupID)
		if err != nil {
			return nil, err
		}
		perms = make(map[string]store.UserGroupManagerPerm, len(list))
		for _, p := range list {
			perms[p.UserID] = p
		}
	}
	out := make([]UserGroupMemberDTO, 0, len(members))
	for _, m := range members {
		// A membership row whose user has since vanished (shouldn't happen --
		// there is no hard user delete -- but defensive) is skipped rather
		// than failing the whole roster.
		u, err := s.users.UserByID(ctx, m.UserID)
		if err != nil {
			continue
		}
		p, isManager := perms[m.UserID]
		out = append(out, UserGroupMemberDTO{
			UserID:             u.ID,
			Email:              u.Email,
			DisplayName:        u.DisplayName,
			State:              m.State,
			IsManager:          isManager,
			IsOwner:            g.OwnerUserID != "" && g.OwnerUserID == m.UserID,
			CanManageUsers:     p.CanManageUsers,
			CanManageGroup:     p.CanManageGroup,
			CanManageServers:   p.CanManageServers,
			CanManageServices:  p.CanManageServices,
			CanManageResources: p.CanManageResources,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}

// --- Managers, ownership transfer, succession (Task 8) ---------------------

// promoteOwnerOrSystemAdmin is the shared owner-only gate for
// PromoteManager/DemoteManager/TransferOwnership/SetManagerPermissions: the
// group must already be visible (authorizeGroupManage, needRead -- ANY
// co-manager, regardless of their own CanManageUsers/CanManageGroup flags,
// legitimately sees the group exists here), AND the principal must be either
// its owner or a system_admin — a plain co-manager is visible but gets
// ErrGroupForbidden, never the no-leak ErrGroupNotFound (spec §8: "NICHT
// befördern/degradieren, NICHT Besitz übertragen").
func (s *Service) authorizeOwnerAction(ctx context.Context, principal auth.Token, groupID string) (store.UserGroup, error) {
	g, err := s.authorizeGroupManage(ctx, principal, groupID, needRead)
	if err != nil {
		return store.UserGroup{}, err
	}
	if g.Tier == store.GroupTierSystem {
		return store.UserGroup{}, ErrGroupTierInvalid
	}
	if !isSystem(principal) && g.OwnerUserID != principal.UserID {
		return store.UserGroup{}, ErrGroupForbidden
	}
	return g, nil
}

// PromoteManager promotes userID to co-manager of groupID (owner/system_admin
// only). Eligibility (spec §8): a user-tier promotee must already be a
// MEMBER; an admin-tier promotee must be a MEMBER, hold role admin/
// system_admin, AND be a member of the parent system group.
// canUsers/canGroup/canServers/canServices/canResources set the new
// co-manager's per-permission flags (per-Admin-Group co-manager permissions,
// spec 2026-08-10 + Phase B 2026-08-10 + Phase C 2026-08-10 + Resource
// Groups Phase 1 2026-08-11) -- all five true reproduces the pre-feature "a
// co-manager can do everything" behavior byte-for-byte.
func (s *Service) PromoteManager(ctx context.Context, principal auth.Token, groupID, userID string, canUsers, canGroup, canServers, canServices, canResources bool) error {
	g, err := s.authorizeOwnerAction(ctx, principal, groupID)
	if err != nil {
		return err
	}
	members, err := s.memberIDSet(ctx, groupID)
	if err != nil {
		return err
	}
	if !members[userID] {
		return ErrGroupCandidateInvalid
	}
	if g.Tier == store.GroupTierAdmin {
		u, err := s.users.UserByID(ctx, userID)
		if err != nil || !isAdminRole(u.Role) {
			return ErrGroupCandidateInvalid
		}
		parentMembers, err := s.memberIDSet(ctx, g.ParentGroupID)
		if err != nil {
			return err
		}
		if !parentMembers[userID] {
			return ErrGroupCandidateInvalid
		}
	}
	if err := s.groups.SetUserGroupManager(ctx, groupID, userID); err != nil {
		return err
	}
	return s.groups.SetUserGroupManagerPermissions(ctx, groupID, store.UserGroupManagerPerm{
		UserID: userID, CanManageUsers: canUsers, CanManageGroup: canGroup,
		CanManageServers: canServers, CanManageServices: canServices, CanManageResources: canResources,
	})
}

// SetManagerPermissions narrows or widens an EXISTING co-manager's
// per-permission flags (owner/system_admin only, per-Admin-Group co-manager
// permissions, spec 2026-08-10 + Phase B 2026-08-10 + Phase C 2026-08-10 +
// Resource Groups Phase 1 2026-08-11) -- it never PROMOTES userID to manager
// (PromoteManager alone does that); userID must already hold a co-manager
// row on groupID, else ErrGroupCandidateInvalid (mirrors
// TransferOwnership's "target must be a current manager" rule -- here
// enforced by the store's zero-rows-affected ErrNotFound, since the UPDATE
// only ever touches an EXISTING row).
func (s *Service) SetManagerPermissions(ctx context.Context, principal auth.Token, groupID, userID string, canUsers, canGroup, canServers, canServices, canResources bool) error {
	if _, err := s.authorizeOwnerAction(ctx, principal, groupID); err != nil {
		return err
	}
	if err := s.groups.SetUserGroupManagerPermissions(ctx, groupID, store.UserGroupManagerPerm{
		UserID: userID, CanManageUsers: canUsers, CanManageGroup: canGroup,
		CanManageServers: canServers, CanManageServices: canServices, CanManageResources: canResources,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrGroupCandidateInvalid
		}
		return err
	}
	return nil
}

// DemoteManager removes userID's co-manager status on groupID (owner/
// system_admin only). Idempotent — demoting a non-manager is a no-op success,
// mirroring the underlying store's RemoveUserGroupManager.
func (s *Service) DemoteManager(ctx context.Context, principal auth.Token, groupID, userID string) error {
	if _, err := s.authorizeOwnerAction(ctx, principal, groupID); err != nil {
		return err
	}
	return s.groups.RemoveUserGroupManager(ctx, groupID, userID)
}

// TransferOwnership makes newOwnerID the owner of groupID (owner/system_admin
// only). The new owner must be a CURRENT co-manager (spec §8: "wer neu Besitz
// bekommen soll, wird vorher befördert").
func (s *Service) TransferOwnership(ctx context.Context, principal auth.Token, groupID, newOwnerID string) error {
	g, err := s.authorizeOwnerAction(ctx, principal, groupID)
	if err != nil {
		return err
	}
	managers, err := s.groups.UserGroupManagers(ctx, groupID)
	if err != nil {
		return err
	}
	isManager := false
	for _, m := range managers {
		if m == newOwnerID {
			isManager = true
			break
		}
	}
	if !isManager {
		return ErrGroupCandidateInvalid
	}
	return s.setGroupOwner(ctx, g, newOwnerID)
}

func (s *Service) setGroupOwner(ctx context.Context, g store.UserGroup, ownerID string) error {
	g.OwnerUserID = ownerID
	g.UpdatedAt = s.clock().UTC()
	return s.groups.UpdateUserGroup(ctx, g)
}

// succeedOwner runs owner succession for g per spec §8.1, triggered by the
// owner leaving the group (RemoveGroupMember) or being disabled
// (ReassignGroupsOwnedBy). System-tier groups have no owner concept and are a
// no-op. g is passed BY VALUE from the moment its owner departed — every
// successor scan explicitly excludes g.OwnerUserID so the departing owner can
// never be re-selected as their own successor, which matters because a
// disable (unlike a self-leave) does NOT remove the owner's membership row.
func (s *Service) succeedOwner(ctx context.Context, g store.UserGroup) error {
	switch g.Tier {
	case store.GroupTierUser:
		return s.succeedUserGroupOwner(ctx, g)
	case store.GroupTierAdmin:
		return s.succeedAdminGroupOwner(ctx, g)
	default:
		return nil
	}
}

// eligibleUserGroupSuccessor reports whether userID may succeed a user-tier
// group's owner: they must exist and be ACTIVE (a disabled account can never
// act as an owner) — a user-tier group has no role restriction otherwise, so
// any active user qualifies.
func (s *Service) eligibleUserGroupSuccessor(ctx context.Context, userID string) bool {
	u, err := s.users.UserByID(ctx, userID)
	return err == nil && u.Status == store.UserStatusActive
}

// eligibleAdminGroupSuccessor reports whether userID may succeed an
// admin-tier group's owner: they must exist, be ACTIVE (a disabled account
// can never act), AND still hold an admin/system_admin role (spec §8.1 — a
// plain user must never own an admin group, even if they were promoted to
// co-manager while they still held an admin role and were later downgraded).
func (s *Service) eligibleAdminGroupSuccessor(ctx context.Context, userID string) bool {
	u, err := s.users.UserByID(ctx, userID)
	if err != nil {
		return false
	}
	return u.Status == store.UserStatusActive && isAdminRole(u.Role)
}

func (s *Service) succeedUserGroupOwner(ctx context.Context, g store.UserGroup) error {
	managers, err := s.groups.UserGroupManagers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, m := range managers {
		if m == g.OwnerUserID {
			continue
		}
		if !s.eligibleUserGroupSuccessor(ctx, m) {
			continue
		}
		return s.setGroupOwner(ctx, g, m)
	}
	members, err := s.groups.UserGroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.State != store.GroupStateMember || m.UserID == g.OwnerUserID {
			continue
		}
		if !s.eligibleUserGroupSuccessor(ctx, m.UserID) {
			continue
		}
		return s.setGroupOwner(ctx, g, m.UserID)
	}
	// Nobody eligible left: delete (cascade handles any — normally
	// nonexistent — user-group children).
	if err := s.groups.DeleteUserGroup(ctx, g.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Service) succeedAdminGroupOwner(ctx context.Context, g store.UserGroup) error {
	managers, err := s.groups.UserGroupManagers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, m := range managers {
		if m == g.OwnerUserID {
			continue
		}
		if !s.eligibleAdminGroupSuccessor(ctx, m) {
			continue
		}
		return s.setGroupOwner(ctx, g, m)
	}
	members, err := s.groups.UserGroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.State != store.GroupStateMember || m.UserID == g.OwnerUserID {
			continue
		}
		if !s.eligibleAdminGroupSuccessor(ctx, m.UserID) {
			continue
		}
		return s.setGroupOwner(ctx, g, m.UserID)
	}
	if g.ParentGroupID != "" {
		parentMembers, pErr := s.groups.UserGroupMembers(ctx, g.ParentGroupID)
		if pErr != nil {
			return pErr
		}
		for _, pm := range parentMembers {
			if pm.State != store.GroupStateMember || pm.UserID == g.OwnerUserID {
				continue
			}
			if !s.eligibleAdminGroupSuccessor(ctx, pm.UserID) {
				continue
			}
			return s.setGroupOwner(ctx, g, pm.UserID)
		}
	}
	// Never falls to a plain user (spec §8.1). If truly nobody is eligible,
	// the owner becomes NULL but the group survives as long as it has ANY
	// member row at all (even a disabled ex-owner's own row — deleting would
	// strip real members' peer visibility for no benefit); only a group with
	// literally zero member rows is deleted.
	hasAnyMember := false
	for _, m := range members {
		if m.State == store.GroupStateMember {
			hasAnyMember = true
			break
		}
	}
	if !hasAnyMember {
		if err := s.groups.DeleteUserGroup(ctx, g.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	}
	return s.setGroupOwner(ctx, g, "")
}

// ReassignGroupsOwnedBy runs owner succession for every admin/user group
// owned by userID. Called on user disable and on self-leave-of-owned-group
// (via RemoveGroupMember calling succeedOwner directly for the single
// affected group; ReassignGroupsOwnedBy is the fleet-wide sweep for disable).
// Admin-only: enforced at the HTTP layer (requireWebScope(..., "admin")) at
// its one call site (the disable flow in handleAdminUserItem) and, as of
// PT-2 Part 2, replicated here via isAdmin(principal) as defense-in-depth
// against a future internal caller that bypasses the HTTP gate.
func (s *Service) ReassignGroupsOwnedBy(ctx context.Context, principal auth.Token, userID string) error {
	if !isAdmin(principal) {
		return ErrPrincipalForbidden
	}
	for _, tier := range [...]string{store.GroupTierAdmin, store.GroupTierUser} {
		groups, err := s.groups.ListUserGroupsByTier(ctx, tier)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if g.OwnerUserID != userID {
				continue
			}
			if err := s.succeedOwner(ctx, g); err != nil {
				return err
			}
		}
	}
	return nil
}
