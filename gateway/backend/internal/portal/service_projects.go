// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"sort"
	"strings"
	"time"
)

// Sentinel errors for the project CRUD/membership/eligibility service surface
// (spec §9 lists the intended HTTP mapping, applied by the gateway handlers in
// a later task): ErrProjectNotFound->404, ErrProjectNameConflict->409,
// ErrProjectMemberNotVisible/ErrProjectGroupNotVisible->400,
// ErrProjectForbidden->403 (or 404-no-leak, depending on the call site),
// ErrProjectNotMember->403 (consumed by the token-assign service, Task 5).
var (
	ErrProjectNotFound         = errors.New("project.not_found")
	ErrProjectNameConflict     = errors.New("project.name_conflict")
	ErrProjectMemberNotVisible = errors.New("project.member_not_visible")
	ErrProjectGroupNotVisible  = errors.New("project.group_not_visible")
	ErrProjectForbidden        = errors.New("project.forbidden")
	// ErrProjectNotMember is returned when a token's owner is not a member of
	// the project they are trying to attribute the token to (Task 5:
	// CreateToken/UpdateToken's project_id assignment).
	ErrProjectNotMember = errors.New("token.project_not_member")
	// ErrProjectTransferTargetNotMember is returned by TransferProject when the
	// proposed new owner is not a current member of the project -- distinct from
	// ErrProjectMemberNotVisible (which means "not in the ASSIGNER's visible set",
	// the AddProjectMembers/AddProjectGroups gate) so the gateway handler (Task 4)
	// can map this to its own, more specific error code.
	ErrProjectTransferTargetNotMember = errors.New("project.transfer_not_member")
	// ErrProjectCoupleGroupInvalid is returned by CreateProject when
	// CoupledGroupID does not name a user-tier group the caller OWNS (spec
	// 2026-08-09: coupling eligibility).
	ErrProjectCoupleGroupInvalid = errors.New("project.couple_group_invalid")
	// ErrProjectCoupleAmbiguous is returned when a coupled create supplies
	// BOTH CoupledGroupID and CreateCoupledGroup -- the caller must choose
	// exactly one.
	ErrProjectCoupleAmbiguous = errors.New("project.couple_ambiguous")
	// ErrProjectCoupled is returned by membership-mutating calls (Task 3) on
	// a project that is coupled to a group -- membership must be managed via
	// the group, not directly on the project.
	ErrProjectCoupled = errors.New("project.coupled")
)

// CreateProjectInput is CreateProject's request.
type CreateProjectInput struct {
	Name        string
	Description string
	// Coupling (spec 2026-08-09): mutually exclusive. CoupledGroupID couples
	// to an existing user-tier group the caller owns; CreateCoupledGroup
	// creates a user-tier group (caller becomes owner+member) then couples.
	// Both nil/empty -> a normal project (unchanged behavior).
	CoupledGroupID     string
	CreateCoupledGroup *NewCoupledGroup
}

// NewCoupledGroup requests a NEW user-tier group be created (owned by the
// calling principal) and immediately coupled to the project being created.
type NewCoupledGroup struct {
	Name          string
	ParentGroupID string // optional; required only when the caller is in >1 admin group (createUserGroup rule)
}

// ProjectDTO is the read-model for a single project, from the perspective of
// the principal that requested it (MyRole/CanManage are principal-relative,
// mirroring UserGroupDTO).
type ProjectDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	OwnerUserID string `json:"owner_user_id"`
	// MyRole is "owner" | "member" | "none" (per spec §5: never the empty
	// string -- an uninvolved principal that can still SEE the project via
	// admin scope reads "none", not "").
	MyRole      string `json:"my_role"`
	CanManage   bool   `json:"can_manage"`
	MemberCount int    `json:"member_count"`
	GroupCount  int    `json:"group_count"`
	// CoupledGroupID/CoupledGroupName (spec 2026-08-09): non-empty iff this
	// project is coupled to a user-tier group -- OwnerUserID above is then
	// DERIVED from that group's current owner (see effectiveProjectOwner),
	// not stored directly on the project.
	CoupledGroupID   string `json:"coupled_group_id"`
	CoupledGroupName string `json:"coupled_group_name"`
	// TotalTokens is this project's TRUE all-time token usage: the SUM over
	// EVERY usage_events row with this project's project_id (input+output+
	// cached+cache-write, the same TotalTokens convention as
	// ProjectTokenUsageTotalDTO/UsageGroups) -- attached in bulk by
	// ListProjects, best-effort (0 when the usage store is unavailable or the
	// aggregation errors; never fails the list). Not populated by any other
	// method that returns a ProjectDTO (CreateProject/UpdateProject/etc. --
	// those return 0, since they build a single fresh DTO outside the list's
	// bulk aggregation and callers don't need the total on that path).
	TotalTokens int `json:"total_tokens"`
}

// ProjectRefDTO is a minimal project reference used by the token-assign
// picker (MyProjects).
type ProjectRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GroupRefDTO is a minimal, non-secret user-group reference used by
// ProjectMembersView/ProjectCandidates (id + display name only).
type GroupRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectMembersDTO is a project's current roster (resolved names), returned
// by ProjectMembersView.
type ProjectMembersDTO struct {
	Users  []UserRefDTO  `json:"users"`
	Groups []GroupRefDTO `json:"groups"`
	// TransferCandidates is every user who is EFFECTIVELY a member of the
	// project -- the direct members (Users) UNIONED with the member-state users
	// of every assigned group (Groups). This is exactly the set TransferProject
	// accepts as a new owner (mirrors isProjectMember's definition), so the UI's
	// ownership-transfer picker can offer a group-only member (a valid target
	// the direct-only Users list would miss). Name-hydrated + sorted; the owner
	// may appear here (if a direct/group member) and is excluded by the picker.
	TransferCandidates []UserRefDTO `json:"transfer_candidates"`
}

// ProjectTokenDTO is one API token attached to a project (owner/admin view).
// Never carries a secret/hash -- SecretPrefix is the same non-secret display
// prefix the owner's own token list shows. The usage fields (RequestCount/
// InputTokens/OutputTokens/TotalTokens) are this token's ALL-TIME usage
// ATTRIBUTED TO THIS PROJECT -- usage_events rows where project_id equals
// this project's id AND token_id equals this token's id. A token with no
// matching usage reads all zeros (never omitted from the list).
type ProjectTokenDTO struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	SecretPrefix string     `json:"secret_prefix"`
	OwnerUserID  string     `json:"owner_user_id"`
	OwnerName    string     `json:"owner_name"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RequestCount int        `json:"request_count"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
	TotalTokens  int        `json:"total_tokens"`
}

// ProjectTokenUsageTotalDTO is a project's TRUE total token usage: the SUM
// over EVERY usage_events row with this project's project_id, regardless of
// whether the row's token is still attached to the project. This can EXCEED
// the sum of the per-token rows in ProjectTokensView.Tokens when usage exists
// from a token that has since been detached (or deleted) -- an intentional,
// honest audit number, not a bug: a project's total spend does not shrink
// just because a contributing token was later unlinked.
type ProjectTokenUsageTotalDTO struct {
	RequestCount int `json:"request_count"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ProjectTokensView is ProjectTokens' response: the project's currently-
// attached tokens (each carrying its OWN project-attributed usage) plus the
// project's TRUE total usage (Total, which may exceed the sum of Tokens'
// per-row totals -- see ProjectTokenUsageTotalDTO).
type ProjectTokensView struct {
	Tokens []ProjectTokenDTO         `json:"tokens"`
	Total  ProjectTokenUsageTotalDTO `json:"total"`
}

// projectsOwnedBy returns every project owned (not merely member-of) by
// ownerID, via ProjectsByOwnerOrMember filtered down to the OwnerUserID rows
// -- there is no dedicated store method for "owned only" (see the Task 2
// report), and this is the narrowest call that covers it.
func (s *Service) projectsOwnedBy(ctx context.Context, ownerID string) ([]store.Project, error) {
	all, err := s.projects.ProjectsByOwnerOrMember(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	owned := all[:0:0]
	for _, p := range all {
		if p.OwnerUserID == ownerID {
			owned = append(owned, p)
		}
	}
	return owned, nil
}

// projectNameConflict reports whether name (case-insensitive) already exists
// among siblings, excluding excludeID (so a no-op/case-only rename of the
// project itself is allowed). Mirrors groupNameConflict.
func projectNameConflict(siblings []store.Project, name, excludeID string) bool {
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

// CreateProject creates a project owned by the calling principal, OR (spec
// 2026-08-09) a project COUPLED to a user-tier group the caller owns/creates
// -- see the coupled branch below. Name uniqueness is enforced
// case-insensitively PER (EFFECTIVE) OWNER (spec §3.1) -- the same name is
// free for use by a different owner.
func (s *Service) CreateProject(ctx context.Context, principal auth.Token, in CreateProjectInput) (ProjectDTO, error) {
	name := strings.TrimSpace(in.Name)

	// --- Coupled project path (spec 2026-08-09) ---
	if in.CoupledGroupID != "" || in.CreateCoupledGroup != nil {
		if in.CoupledGroupID != "" && in.CreateCoupledGroup != nil {
			return ProjectDTO{}, ErrProjectCoupleAmbiguous
		}
		groupID := in.CoupledGroupID
		if in.CreateCoupledGroup != nil {
			// Route through the PUBLIC CreateGroup entry point (not the private
			// createUserGroup) so the blank/whitespace-name guard runs (fix round 1,
			// 2026-08-09 review): createUserGroup has NO name validation of its own --
			// CreateGroup trims + rejects an empty name with ErrGroupNameInvalid BEFORE
			// dispatching to createUserGroup, so a blank create_coupled_group.name is
			// refused before any group/project row is created.
			gdto, err := s.CreateGroup(ctx, principal, CreateGroupInput{
				Tier:          store.GroupTierUser,
				Name:          in.CreateCoupledGroup.Name,
				ParentGroupID: in.CreateCoupledGroup.ParentGroupID,
			})
			if err != nil {
				return ProjectDTO{}, err // surfaces the existing group-create errors (name/parent/etc.)
			}
			groupID = gdto.ID
		} else {
			// Existing group: must be a user-tier group the caller OWNS.
			g, err := s.groups.UserGroupByID(ctx, groupID)
			if err != nil || g.Tier != store.GroupTierUser || g.OwnerUserID == "" || g.OwnerUserID != principal.UserID {
				return ProjectDTO{}, ErrProjectCoupleGroupInvalid
			}
		}
		// Name uniqueness per EFFECTIVE owner (the caller owns the group -> the project).
		owned, err := s.effectiveOwnedProjects(ctx, principal.UserID)
		if err != nil {
			return ProjectDTO{}, err
		}
		if projectNameConflict(owned, name, "") {
			return ProjectDTO{}, ErrProjectNameConflict
		}
		now := s.clock().UTC()
		p := store.Project{
			ID:             "proj_" + s.idGenerator(),
			Name:           name,
			Description:    strings.TrimSpace(in.Description),
			OwnerUserID:    "", // derived from the coupled group
			CoupledGroupID: groupID,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.projects.CreateProject(ctx, p); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return ProjectDTO{}, ErrProjectNameConflict
			}
			return ProjectDTO{}, err
		}
		// Carry the coupled group as a normal assignment so membership resolution reuses it.
		if err := s.projects.SetProjectGroup(ctx, p.ID, groupID); err != nil {
			return ProjectDTO{}, err
		}
		return s.projectDTO(ctx, principal, p), nil
	}

	// --- Normal project path (unchanged) ---
	owned, err := s.projectsOwnedBy(ctx, principal.UserID)
	if err != nil {
		return ProjectDTO{}, err
	}
	if projectNameConflict(owned, name, "") {
		return ProjectDTO{}, ErrProjectNameConflict
	}
	now := s.clock().UTC()
	p := store.Project{
		ID:          "proj_" + s.idGenerator(),
		Name:        name,
		Description: strings.TrimSpace(in.Description),
		OwnerUserID: principal.UserID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.projects.CreateProject(ctx, p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ProjectDTO{}, ErrProjectNameConflict
		}
		return ProjectDTO{}, err
	}
	return s.projectDTO(ctx, principal, p), nil
}

// effectiveOwnedProjects returns the projects the user effectively OWNS:
// normal projects with owner_user_id == userID, plus every project coupled to
// a user-tier group the user owns (coupled projects store an empty owner).
// Used for coupled-create name uniqueness.
func (s *Service) effectiveOwnedProjects(ctx context.Context, userID string) ([]store.Project, error) {
	owned, err := s.projectsOwnedBy(ctx, userID)
	if err != nil {
		return nil, err
	}
	myGroups, err := s.groups.UserGroupsForUser(ctx, userID, store.GroupTierUser, store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range owned {
		seen[p.ID] = true
	}
	for _, g := range myGroups {
		if g.OwnerUserID != userID {
			continue
		}
		coupled, err := s.projects.CoupledProjectsByGroup(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range coupled {
			if !seen[p.ID] {
				seen[p.ID] = true
				owned = append(owned, p)
			}
		}
	}
	return owned, nil
}

// authorizeProjectManage loads the project and enforces the "may manage this
// project" gate (spec §5): the OWNER (for a coupled project, the coupled
// group's CURRENT owner -- see effectiveProjectOwner) or any principal with
// the "admin" scope (which every admin AND system_admin token carries).
// Anyone else -- including a mere member -- gets ErrProjectNotFound,
// 404-no-leak (never a distinguishable forbidden), same convention as
// authorizeGroupManage.
func (s *Service) authorizeProjectManage(ctx context.Context, principal auth.Token, id string) (store.Project, error) {
	p, err := s.projects.ProjectByID(ctx, id)
	if err != nil {
		return store.Project{}, ErrProjectNotFound
	}
	ownerID := s.effectiveProjectOwner(ctx, p)
	if isAdmin(principal) || (ownerID != "" && ownerID == principal.UserID) {
		return p, nil
	}
	return store.Project{}, ErrProjectNotFound
}

// rejectIfCoupled returns ErrProjectCoupled when p is coupled -- membership and
// ownership of a coupled project are managed via its group, not directly.
func rejectIfCoupled(p store.Project) error {
	if p.CoupledGroupID != "" {
		return ErrProjectCoupled
	}
	return nil
}

// visibleGroupIDs returns the set of user-group ids visible to principal, per
// their own group landscape (ListGroups: system groups they belong to [or all,
// for a system_admin] + every admin/user group they own, co-manage, or are a
// member of). Used to validate AddProjectGroups/ProjectCandidates' assignable
// group set (spec §4: "Gruppen aus seinem Gruppen-Landscape").
func (s *Service) visibleGroupIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	landscape, err := s.ListGroups(ctx, principal)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, tier := range [][]UserGroupDTO{landscape.System, landscape.Admin, landscape.User} {
		for _, g := range tier {
			out[g.ID] = true
		}
	}
	return out, nil
}

// stringSet converts a slice of ids into a lookup set.
func stringSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// projectMemberViaGroups reports whether userID is a member (state=member) of
// any of the given group ids, via the user-groups store. Empty groupIDs is a
// fast no-op.
func (s *Service) projectMemberViaGroups(ctx context.Context, userID string, groupIDs []string) (bool, error) {
	if len(groupIDs) == 0 {
		return false, nil
	}
	myGroups, err := s.groups.UserGroupsForUser(ctx, userID, "", store.GroupStateMember)
	if err != nil {
		return false, err
	}
	myGroupIDs := make([]string, len(myGroups))
	for i, g := range myGroups {
		myGroupIDs[i] = g.ID
	}
	mine := stringSet(myGroupIDs)
	for _, gid := range groupIDs {
		if mine[gid] {
			return true, nil
		}
	}
	return false, nil
}

// memberProjectIDs returns every project id userID is a MEMBER of per spec
// §4: owner ∪ direct project_members row ∪ member (state=member, any group
// tier) of a group assigned to the project (project_groups). Composed from
// ProjectsByOwnerOrMember (covers rules 1+2) unioned with, for every group
// userID actively belongs to, ProjectsByGroup(group) (rule 3) -- the
// N+1-avoiding shape the store's ProjectsByGroup method exists for (Task 2).
func (s *Service) memberProjectIDs(ctx context.Context, userID string) (map[string]bool, error) {
	out := map[string]bool{}
	direct, err := s.projects.ProjectsByOwnerOrMember(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, p := range direct {
		out[p.ID] = true
	}
	groups, err := s.groups.UserGroupsForUser(ctx, userID, "", store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		viaGroup, err := s.projects.ProjectsByGroup(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range viaGroup {
			out[p.ID] = true
		}
	}
	return out, nil
}

// isProjectMember reports whether userID is a member (§4) of projectID. Used
// by the token-assign service (Task 5) to enforce ErrProjectNotMember at
// CreateToken/UpdateToken.
func (s *Service) isProjectMember(ctx context.Context, userID, projectID string) (bool, error) {
	ids, err := s.memberProjectIDs(ctx, userID)
	if err != nil {
		return false, err
	}
	return ids[projectID], nil
}

// effectiveProjectOwner returns the user id that OWNS p. For a coupled project
// the owner is ALWAYS the coupled group's current owner (derived on read, zero
// drift); if the group can't be read (e.g. just deleted) it falls back to the
// stored owner (empty -> ownerless). For a normal project it is p.OwnerUserID.
func (s *Service) effectiveProjectOwner(ctx context.Context, p store.Project) string {
	if p.CoupledGroupID == "" {
		return p.OwnerUserID
	}
	g, err := s.groups.UserGroupByID(ctx, p.CoupledGroupID)
	if err != nil {
		return p.OwnerUserID
	}
	return g.OwnerUserID
}

// projectDTO computes the principal-relative view (MyRole/CanManage) of p,
// plus its member/group counts and (for a coupled project) the derived owner
// + the coupled group's display name. One extra store round trip per project
// (like groupDTO) -- acceptable at this scale.
// coupledGroupMemberCount counts the member-state users of a coupled project's
// group (invited-state excluded, mirroring ProjectMembersView/groupDTO's filter).
// Best-effort: returns 0 on a store error (no worse than the direct-count default).
func (s *Service) coupledGroupMemberCount(ctx context.Context, groupID string) int {
	gm, err := s.groups.UserGroupMembers(ctx, groupID)
	if err != nil {
		return 0
	}
	n := 0
	for _, m := range gm {
		if m.State == store.GroupStateMember {
			n++
		}
	}
	return n
}

func (s *Service) projectDTO(ctx context.Context, principal auth.Token, p store.Project) ProjectDTO {
	ownerID := s.effectiveProjectOwner(ctx, p)
	dto := ProjectDTO{
		ID:             p.ID,
		Name:           p.Name,
		Description:    p.Description,
		OwnerUserID:    ownerID,
		MyRole:         "none",
		CanManage:      isAdmin(principal) || (ownerID != "" && ownerID == principal.UserID),
		CoupledGroupID: p.CoupledGroupID,
	}
	if p.CoupledGroupID != "" {
		if g, err := s.groups.UserGroupByID(ctx, p.CoupledGroupID); err == nil {
			dto.CoupledGroupName = g.Name
		}
	}
	members, _ := s.projects.ProjectMembers(ctx, p.ID)
	dto.MemberCount = len(members)
	groupIDs, _ := s.projects.ProjectGroups(ctx, p.ID)
	dto.GroupCount = len(groupIDs)
	if p.CoupledGroupID != "" {
		// A coupled project has no direct members; its effective members are the
		// coupled group's member-state users. Report THAT as member_count so the
		// stat matches the (group-resolved) member list the UI shows -- otherwise
		// it would always read 0 next to a non-empty roster.
		dto.MemberCount = s.coupledGroupMemberCount(ctx, p.CoupledGroupID)
	}

	if ownerID != "" && ownerID == principal.UserID {
		dto.MyRole = "owner"
		return dto
	}
	isMember := false
	for _, uid := range members {
		if uid == principal.UserID {
			isMember = true
			break
		}
	}
	if !isMember {
		isMember, _ = s.projectMemberViaGroups(ctx, principal.UserID, groupIDs)
	}
	if isMember {
		dto.MyRole = "member"
	}
	return dto
}

func sortProjectDTOs(list []ProjectDTO) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].ID < list[j].ID
	})
}

// ListProjects returns the principal's full project landscape (spec §9): the
// projects they OWN plus the projects they are a MEMBER of (direct or via a
// group) -- i.e. exactly memberProjectIDs, resolved + rendered. Note this is
// principal-relative, NOT "every project" even for an admin (admin scope only
// widens CanManage/authorizeProjectManage on projects the admin can already
// reach through this listing or a direct id -- it does not enumerate other
// users' unrelated projects here).
func (s *Service) ListProjects(ctx context.Context, principal auth.Token) ([]ProjectDTO, error) {
	var projects []store.Project
	if isAdmin(principal) {
		// An admin (or system_admin) can manage EVERY project
		// (authorizeProjectManage widens CanManage on the admin scope), so the
		// list surfaces every project rather than only the admin's own/member-of
		// ones -- consistent with how admins see the whole fleet of users,
		// servers, etc. projectDTO still computes the principal-relative view:
		// an uninvolved admin reads MyRole="none" + CanManage=true.
		all, err := s.projects.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		projects = all
	} else {
		// A non-admin sees ONLY the projects they own or are a member of
		// (direct or via an assigned group) -- the privacy-preserving default.
		ids, err := s.memberProjectIDs(ctx, principal.UserID)
		if err != nil {
			return nil, err
		}
		projects = make([]store.Project, 0, len(ids))
		for id := range ids {
			p, err := s.projects.ProjectByID(ctx, id)
			if err != nil {
				continue
			}
			projects = append(projects, p)
		}
	}
	out := make([]ProjectDTO, 0, len(projects))
	for _, p := range projects {
		out = append(out, s.projectDTO(ctx, principal, p))
	}
	sortProjectDTOs(out)
	s.attachProjectTotalTokens(ctx, out)
	return out, nil
}

// attachProjectTotalTokens fills each dto's TotalTokens with its TRUE all-time
// usage total, via ONE bulk aggregation across the ALREADY-VISIBLE project ids
// in dtos (never an N+1 per-project query). No-leak by construction: the
// aggregation's ProjectIDs IN-list is exactly the caller's own returned dtos
// -- it can never surface anything about a project the caller cannot already
// see (ListProjects has already applied the owner/admin visibility rule
// before this is called). ScopeAll:true only drops the per-request user_id
// pin (a project's total spans every member's usage, which every member is
// authorized to see per the project privacy model) -- it does NOT widen the
// project set, which stays bounded by ProjectIDs. Best-effort: a nil usage
// store or an aggregation error leaves every TotalTokens at its zero value
// rather than failing the whole list.
func (s *Service) attachProjectTotalTokens(ctx context.Context, dtos []ProjectDTO) {
	if s.usage == nil || len(dtos) == 0 {
		return
	}
	ids := make([]string, len(dtos))
	for i, d := range dtos {
		ids[i] = d.ID
	}
	buckets, err := s.usage.UsageGroups(ctx, usage.Query{
		ScopeAll:   true,
		ProjectIDs: ids,
	}, "project")
	if err != nil {
		return
	}
	totalByProject := make(map[string]int, len(buckets))
	for _, b := range buckets {
		totalByProject[b.Key] += b.InputTokens + b.OutputTokens + b.CachedTokens + b.CacheWriteTokens
	}
	for i := range dtos {
		dtos[i].TotalTokens = totalByProject[dtos[i].ID]
	}
}

// MyProjects returns the slim {id,name} list of projects the principal is a
// MEMBER of (§4, includes ownership) -- the token-assign picker's option set
// (spec §6/§9 GET /api/portal/projects/mine).
func (s *Service) MyProjects(ctx context.Context, principal auth.Token) ([]ProjectRefDTO, error) {
	ids, err := s.memberProjectIDs(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectRefDTO, 0, len(ids))
	for id := range ids {
		p, err := s.projects.ProjectByID(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, ProjectRefDTO{ID: p.ID, Name: p.Name})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// UpdateProject renames/redescribes a project (owner or admin, via
// authorizeProjectManage). A blank (post-trim) name is treated as "no rename
// requested" and leaves the stored name untouched -- there is no dedicated
// "name required" sentinel in this surface, so a project's name can never be
// blanked out via this call. Description is always a full replace (blank is
// a legitimate value -- the store's own default). Name uniqueness is
// re-checked per the project's OWNER (unaffected by who is doing the
// renaming, e.g. an admin editing someone else's project).
func (s *Service) UpdateProject(ctx context.Context, principal auth.Token, id, name, description string) (ProjectDTO, error) {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return ProjectDTO{}, err
	}
	name = strings.TrimSpace(name)
	if name != "" && !strings.EqualFold(name, p.Name) {
		// Name uniqueness is checked per EFFECTIVE owner (fix round 2,
		// 2026-08-09 review): for a COUPLED project (p.OwnerUserID=="",
		// derived from the group) projectsOwnedBy("") would scan ALL
		// ownerless/coupled projects globally -- collapsing every distinct
		// group-owner's coupled projects into one bucket. Branching mirrors
		// the coupled-create path's own effectiveOwnedProjects use.
		var owned []store.Project
		var err error
		if p.CoupledGroupID != "" {
			owned, err = s.effectiveOwnedProjects(ctx, s.effectiveProjectOwner(ctx, p))
		} else {
			owned, err = s.projectsOwnedBy(ctx, p.OwnerUserID)
		}
		if err != nil {
			return ProjectDTO{}, err
		}
		if projectNameConflict(owned, name, p.ID) {
			return ProjectDTO{}, ErrProjectNameConflict
		}
	}
	if name != "" {
		p.Name = name
	}
	p.Description = strings.TrimSpace(description)
	p.UpdatedAt = s.clock().UTC()
	if err := s.projects.UpdateProject(ctx, p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ProjectDTO{}, ErrProjectNameConflict
		}
		if errors.Is(err, store.ErrNotFound) {
			return ProjectDTO{}, ErrProjectNotFound
		}
		return ProjectDTO{}, err
	}
	return s.projectDTO(ctx, principal, p), nil
}

// TransferProject makes newOwnerID the new owner of id. Deliberately
// OWNER-ONLY -- narrower than authorizeProjectManage's owner-OR-admin gate
// (spec: "nur Eigentümer"): authorizeProjectManage is used ONLY for the
// visibility/no-leak check (an admin who cannot own-transfer can still SEE
// the project exists), and a non-owner caller who passes that visibility
// check is then explicitly refused with ErrProjectForbidden rather than
// silently succeeding. newOwnerID must be a CURRENT member of the project
// (§5) -- this is the app-level guard that plugs the store's missing
// owner-FK-on-update check (see the Task 2 report).
func (s *Service) TransferProject(ctx context.Context, principal auth.Token, id, newOwnerID string) error {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return err
	}
	if err := rejectIfCoupled(p); err != nil {
		return err
	}
	if p.OwnerUserID == "" || p.OwnerUserID != principal.UserID {
		return ErrProjectForbidden
	}
	newOwnerID = strings.TrimSpace(newOwnerID)
	if newOwnerID == "" {
		return ErrProjectTransferTargetNotMember
	}
	isMember, err := s.isProjectMember(ctx, newOwnerID, p.ID)
	if err != nil {
		return err
	}
	if !isMember {
		return ErrProjectTransferTargetNotMember
	}
	p.OwnerUserID = newOwnerID
	p.UpdatedAt = s.clock().UTC()
	if err := s.projects.UpdateProject(ctx, p); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrProjectNotFound
		}
		return err
	}
	return nil
}

// DeleteProject deletes a project (owner or admin; FK-cascade removes its
// member/group rows -- routing/usage_events.project_id survives via
// ON DELETE SET NULL, per spec §3.4/§3.5).
func (s *Service) DeleteProject(ctx context.Context, principal auth.Token, id string) error {
	if _, err := s.authorizeProjectManage(ctx, principal, id); err != nil {
		return err
	}
	if err := s.projects.DeleteProject(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrProjectNotFound
		}
		return err
	}
	return nil
}

// ProjectMembersView lists a project's current roster (resolved user +
// group names) -- owner/admin only (authorizeProjectManage, 404-no-leak).
func (s *Service) ProjectMembersView(ctx context.Context, principal auth.Token, id string) (ProjectMembersDTO, error) {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return ProjectMembersDTO{}, err
	}
	userIDs, err := s.projects.ProjectMembers(ctx, p.ID)
	if err != nil {
		return ProjectMembersDTO{}, err
	}
	users := make([]UserRefDTO, 0, len(userIDs))
	for _, uid := range userIDs {
		u, err := s.users.UserByID(ctx, uid)
		if err != nil {
			continue
		}
		users = append(users, UserRefDTO{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName})
	}
	sortUserRefs(users)

	groupIDs, err := s.projects.ProjectGroups(ctx, p.ID)
	if err != nil {
		return ProjectMembersDTO{}, err
	}
	groups := make([]GroupRefDTO, 0, len(groupIDs))
	for _, gid := range groupIDs {
		g, err := s.groups.UserGroupByID(ctx, gid)
		if err != nil {
			continue
		}
		groups = append(groups, GroupRefDTO{ID: g.ID, Name: g.Name})
	}
	sortGroupRefs(groups)

	// Effective-member set (= isProjectMember for this project): the direct
	// members plus every assigned group's MEMBER-state users (invited-state
	// excluded, mirroring memberProjectIDs' GroupStateMember filter). This is
	// the exact set TransferProject accepts as a new owner, so the transfer
	// picker can offer a group-only member.
	memberSet := make(map[string]bool, len(userIDs))
	for _, uid := range userIDs {
		memberSet[uid] = true
	}
	for _, gid := range groupIDs {
		gm, err := s.groups.UserGroupMembers(ctx, gid)
		if err != nil {
			return ProjectMembersDTO{}, err
		}
		for _, m := range gm {
			if m.State == store.GroupStateMember {
				memberSet[m.UserID] = true
			}
		}
	}
	transfer := make([]UserRefDTO, 0, len(memberSet))
	for uid := range memberSet {
		u, err := s.users.UserByID(ctx, uid)
		if err != nil {
			continue
		}
		transfer = append(transfer, UserRefDTO{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName})
	}
	sortUserRefs(transfer)
	return ProjectMembersDTO{Users: users, Groups: groups, TransferCandidates: transfer}, nil
}

// ProjectCandidates lists the users/groups eligible to be ADDED to id: the
// assigner's (principal's) own visible-user set and group landscape, minus
// the project's current members/groups. Owner/admin only.
func (s *Service) ProjectCandidates(ctx context.Context, principal auth.Token, id string) ([]UserRefDTO, []GroupRefDTO, error) {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return nil, nil, err
	}
	visibleUsers, err := s.VisibleUserIDs(ctx, principal)
	if err != nil {
		return nil, nil, err
	}
	currentMembers, err := s.projects.ProjectMembers(ctx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	currentMemberSet := stringSet(currentMembers)
	users := make([]UserRefDTO, 0, len(visibleUsers))
	for uid := range visibleUsers {
		if currentMemberSet[uid] {
			continue
		}
		u, err := s.users.UserByID(ctx, uid)
		if err != nil {
			continue
		}
		users = append(users, UserRefDTO{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName})
	}
	sortUserRefs(users)

	visibleGroups, err := s.visibleGroupIDs(ctx, principal)
	if err != nil {
		return nil, nil, err
	}
	currentGroups, err := s.projects.ProjectGroups(ctx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	currentGroupSet := stringSet(currentGroups)
	groups := make([]GroupRefDTO, 0, len(visibleGroups))
	for gid := range visibleGroups {
		if currentGroupSet[gid] {
			continue
		}
		g, err := s.groups.UserGroupByID(ctx, gid)
		if err != nil {
			continue
		}
		groups = append(groups, GroupRefDTO{ID: g.ID, Name: g.Name})
	}
	sortGroupRefs(groups)
	return users, groups, nil
}

// AddProjectMembers adds the given userIDs to id, each validated against the
// assigner's VisibleUserIDs (spec §4: "nur Benutzer aus seinem sichtbaren
// Set"). Whole-batch validated before any write, mirroring AddGroupMembers'
// all-or-nothing containment check.
func (s *Service) AddProjectMembers(ctx context.Context, principal auth.Token, id string, userIDs []string) error {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return err
	}
	if err := rejectIfCoupled(p); err != nil {
		return err
	}
	visible, err := s.VisibleUserIDs(ctx, principal)
	if err != nil {
		return err
	}
	for _, uid := range userIDs {
		if !visible[uid] {
			return ErrProjectMemberNotVisible
		}
	}
	for _, uid := range userIDs {
		if err := s.projects.SetProjectMember(ctx, p.ID, uid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ErrProjectNotFound
			}
			return err
		}
	}
	return nil
}

// RemoveProjectMember removes userID from id (owner/admin only). Idempotent,
// mirroring the underlying store's RemoveProjectMember.
func (s *Service) RemoveProjectMember(ctx context.Context, principal auth.Token, id, userID string) error {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return err
	}
	if err := rejectIfCoupled(p); err != nil {
		return err
	}
	return s.projects.RemoveProjectMember(ctx, p.ID, userID)
}

// AddProjectGroups adds the given groupIDs to id, each validated against the
// assigner's own group landscape (spec §4: "Gruppen aus seinem
// Gruppen-Landscape (sichtbare Gruppen)"). Whole-batch validated before any
// write.
func (s *Service) AddProjectGroups(ctx context.Context, principal auth.Token, id string, groupIDs []string) error {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return err
	}
	if err := rejectIfCoupled(p); err != nil {
		return err
	}
	visible, err := s.visibleGroupIDs(ctx, principal)
	if err != nil {
		return err
	}
	for _, gid := range groupIDs {
		if !visible[gid] {
			return ErrProjectGroupNotVisible
		}
	}
	for _, gid := range groupIDs {
		if err := s.projects.SetProjectGroup(ctx, p.ID, gid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ErrProjectNotFound
			}
			return err
		}
	}
	return nil
}

// RemoveProjectGroup removes groupID from id (owner/admin only). Idempotent.
func (s *Service) RemoveProjectGroup(ctx context.Context, principal auth.Token, id, groupID string) error {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return err
	}
	if err := rejectIfCoupled(p); err != nil {
		return err
	}
	return s.projects.RemoveProjectGroup(ctx, p.ID, groupID)
}

// ProjectTokens lists the API tokens currently attached to id (owner/admin
// only, authorizeProjectManage's 404-no-leak gate), each enriched with its
// OWN project-attributed usage, plus the project's TRUE total usage (see
// ProjectTokensView/ProjectTokenUsageTotalDTO). Never leaks a secret/hash --
// ProjectTokenDTO carries only the same non-secret prefix the owner's own
// token list already shows. OwnerName resolution is best-effort: a
// stale/unresolvable owner (should not happen -- api_tokens.user_id is a
// live FK) never fails the call, it just yields an empty display name.
//
// Usage aggregation is likewise best-effort: a usage-store error (or a nil
// s.usage, e.g. a Service built without a Usage dep) never fails the call --
// it just leaves every usage field at its zero value. The aggregation
// deliberately bypasses applyUsageScope/the caller's own-rows pin -- the
// authorizeProjectManage gate above is already the sole authority for who
// may see this project's usage (an owner/admin manager sees ALL of the
// project's usage, not just their own rows within it) -- so the query is
// issued with ScopeAll:true.
func (s *Service) ProjectTokens(ctx context.Context, principal auth.Token, id string) (ProjectTokensView, error) {
	p, err := s.authorizeProjectManage(ctx, principal, id)
	if err != nil {
		return ProjectTokensView{}, err
	}
	recs, err := s.tokens.TokensByProject(ctx, p.ID)
	if err != nil {
		return ProjectTokensView{}, err
	}
	out := make([]ProjectTokenDTO, 0, len(recs))
	for _, rec := range recs {
		ownerName := ""
		if u, err := s.users.UserByID(ctx, rec.UserID); err == nil {
			ownerName = u.DisplayName
		}
		out = append(out, ProjectTokenDTO{
			ID:           rec.ID,
			Name:         rec.Name,
			SecretPrefix: rec.SecretPrefix,
			OwnerUserID:  rec.UserID,
			OwnerName:    ownerName,
			Status:       rec.Status,
			CreatedAt:    rec.CreatedAt,
			LastUsedAt:   rec.LastUsedAt,
		})
	}

	var total ProjectTokenUsageTotalDTO
	if s.usage != nil {
		if buckets, uerr := s.usage.UsageGroups(ctx, usage.Query{
			ScopeAll:          true,
			HasProjectIDExact: true,
			ProjectIDExact:    p.ID,
		}, "token"); uerr == nil {
			type agg struct {
				count, input, output, cached, write int
			}
			byToken := make(map[string]*agg, len(out))
			var all agg
			for _, b := range buckets {
				a := byToken[b.Key]
				if a == nil {
					a = &agg{}
					byToken[b.Key] = a
				}
				a.count += b.Count
				a.input += b.InputTokens
				a.output += b.OutputTokens
				a.cached += b.CachedTokens
				a.write += b.CacheWriteTokens
				all.count += b.Count
				all.input += b.InputTokens
				all.output += b.OutputTokens
				all.cached += b.CachedTokens
				all.write += b.CacheWriteTokens
			}
			// Attach each CURRENTLY-ATTACHED token's OWN bucket (Key ==
			// token_id, from the group-by-token dimension). A usage row whose
			// token_id does not match any currently-attached token (e.g. the
			// token was since detached) contributes to `all`/Total below but
			// is intentionally NOT attached to any per-token row here.
			for i := range out {
				if a, ok := byToken[out[i].ID]; ok {
					out[i].RequestCount = a.count
					out[i].InputTokens = a.input
					out[i].OutputTokens = a.output
					out[i].TotalTokens = a.input + a.output + a.cached + a.write
				}
			}
			total = ProjectTokenUsageTotalDTO{
				RequestCount: all.count,
				InputTokens:  all.input,
				OutputTokens: all.output,
				TotalTokens:  all.input + all.output + all.cached + all.write,
			}
		}
		// A usage-store error is swallowed here (best-effort): the token list
		// itself is still valid and authorizeProjectManage already succeeded,
		// so a usage-layer hiccup should not 500 the whole view -- it just
		// leaves every usage field at zero.
	}
	return ProjectTokensView{Tokens: out, Total: total}, nil
}

// DetachProjectToken clears tokenID's project attribution (owner/admin only,
// authorizeProjectManage's 404-no-leak gate). tokenID must currently be
// attached to id -- a token attached to a DIFFERENT project (or no project,
// or an unknown id) is ErrTokenNotFound, both refusing the detach AND never
// revealing to the caller that the token exists elsewhere (no cross-project
// reach). The token's secret hash/status/scopes/etc. are untouched --
// UpdateTokenMetadata is a full-row metadata write, so the fetched record is
// round-tripped with ONLY ProjectID cleared.
func (s *Service) DetachProjectToken(ctx context.Context, principal auth.Token, projectID, tokenID string) error {
	if _, err := s.authorizeProjectManage(ctx, principal, projectID); err != nil {
		return err
	}
	rec, err := s.tokens.TokenByID(ctx, tokenID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrTokenNotFound
		}
		return err
	}
	if rec.ProjectID != projectID {
		return ErrTokenNotFound
	}
	rec.ProjectID = ""
	rec.UpdatedAt = s.clock().UTC()
	if err := s.tokens.UpdateTokenMetadata(ctx, rec); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrTokenNotFound
		}
		return err
	}
	return nil
}

func sortUserRefs(users []UserRefDTO) {
	sort.Slice(users, func(i, j int) bool {
		if users[i].DisplayName != users[j].DisplayName {
			return users[i].DisplayName < users[j].DisplayName
		}
		return users[i].ID < users[j].ID
	})
}

func sortGroupRefs(groups []GroupRefDTO) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Name != groups[j].Name {
			return groups[i].Name < groups[j].Name
		}
		return groups[i].ID < groups[j].ID
	})
}
