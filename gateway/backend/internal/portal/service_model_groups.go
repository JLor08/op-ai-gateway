// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
)

// Model-group + per-model-visibility management. Groups are named priority-failover
// synthetic models (resolver expansion lives in internal/routing); this file is the
// portal CRUD + validation + global name uniqueness surface. Visibility is a MODEL
// property (model_settings, keyed by gateway_model_name), NOT a group-membership
// property — a model has one global visibility.
// CodeModelGroupNotFound is ErrModelGroupNotFound's API error code, exported
// so internal/gateway/portal_modelgroup_endpoints.go can share the exact
// value instead of re-hardcoding it.
const CodeModelGroupNotFound = "model_group.not_found"

var (
	ErrModelGroupNotFound      = errors.New(CodeModelGroupNotFound)
	ErrModelGroupNameRequired  = errors.New("model_group.name_required")
	ErrModelGroupNameConflict  = errors.New("model_group.name_conflict")
	ErrModelGroupModeInvalid   = errors.New("model_group.mode_invalid")
	ErrModelGroupMemberInvalid = errors.New("model_group.member_invalid")
	// ErrModelGroupCycle is returned when a group's (prospective) member set would
	// make the group transitively contain itself, including a direct self-reference.
	ErrModelGroupCycle        = errors.New("model_group.cycle")
	ErrModelVisibilityInvalid = errors.New("model_setting.visibility_invalid")
	// ErrModelGroupMemberOrderInvalid is an unrecognized member_order. Unlike
	// Traversal (which fails open to a default), an unknown member_order is
	// REJECTED on write so an operator learns of a typo instead of silently
	// getting priority order.
	ErrModelGroupMemberOrderInvalid = errors.New("model_group.member_order_invalid")
	// ErrModelGroupMinSpeedFallbackInvalid is an unrecognized min_speed_fallback,
	// rejected on write for the same reason as ErrModelGroupMemberOrderInvalid.
	ErrModelGroupMinSpeedFallbackInvalid = errors.New("model_group.min_speed_fallback_invalid")
	// ErrModelGroupMinTokensPerSecondInvalid is a negative min_tokens_per_second
	// (0 is the valid, documented "no floor" off-state).
	ErrModelGroupMinTokensPerSecondInvalid = errors.New("model_group.min_tokens_per_second_invalid")
	// ErrModelGroupClimbSpeedMarginInvalid is a negative climb_speed_margin_percent
	// (0 is a valid, meaningful "no margin required" policy).
	ErrModelGroupClimbSpeedMarginInvalid = errors.New("model_group.climb_speed_margin_percent_invalid")
)

// ModelGroupDTO is the API shape of a model group. Members are ordered by priority
// (array order = priority; index 0 = highest). Per-member visibility does NOT exist
// — visibility lives on the model (see SetModelVisibility / ModelDTO.Visibility).
type ModelGroupDTO struct {
	ID               string `json:"id"`
	GatewayModelName string `json:"gateway_model_name"`
	DisplayName      string `json:"display_name"`
	Status           string `json:"status"`
	FailoverMode     string `json:"failover_mode"`
	// Traversal is the subgroup-expansion strategy ("depth" | "breadth" |
	// "round_robin", default "round_robin") used to flatten a nested group member
	// (another group) into the failover candidate order.
	Traversal string `json:"traversal"`
	// Visibility is the group NAME's global visibility from model_settings
	// ("shown" | "hidden" | "locked"; "shown" when no setting row exists). A group
	// name lives in the same model_settings table as a model.
	Visibility string `json:"visibility"`
	// LoadedOnly restricts selection to members with an already-loaded candidate
	// (see routing.ModelGroup.LoadedOnly).
	LoadedOnly bool `json:"loaded_only"`
	// MemberOrder is how members are ordered for the walk: "priority" or "speed".
	MemberOrder string `json:"member_order"`
	// ClimbSpeedMarginPercent is how much faster a member must be before a
	// SPEED-ordered climb_up leaves an available pin.
	ClimbSpeedMarginPercent int `json:"climb_speed_margin_percent"`
	// MinTokensPerSecond is the minimum effective generation speed a candidate
	// must reach to count as available; 0 disables the floor.
	MinTokensPerSecond float64 `json:"min_tokens_per_second"`
	// MinSpeedFallback is what happens when no candidate reaches the floor:
	// "error" or "ignore".
	MinSpeedFallback string           `json:"min_speed_fallback"`
	Members          []GroupMemberDTO `json:"members"`
}

// GroupMemberDTO is one ordered member of a group (a gateway model NAME). Order in
// ModelGroupDTO.Members is the priority.
type GroupMemberDTO struct {
	MemberGatewayName string `json:"member_gateway_name"`
}

type ModelGroupsResponse struct {
	Data []ModelGroupDTO `json:"data"`
}

// GroupMemberInput is one member on create/update. Order in the request slice is the
// priority (index 0 = highest).
type GroupMemberInput struct {
	MemberGatewayName string `json:"member_gateway_name"`
}

type CreateModelGroupRequest struct {
	GatewayModelName string `json:"gateway_model_name"`
	DisplayName      string `json:"display_name"`
	Status           string `json:"status"`
	FailoverMode     string `json:"failover_mode"`
	// Traversal optionally sets the subgroup-expansion strategy (depth | breadth |
	// round_robin). Empty or unknown defaults to "round_robin".
	Traversal string `json:"traversal"`
	// Visibility optionally sets the group NAME's visibility (shown | hidden |
	// locked). Empty = "shown" (default, no setting row written).
	Visibility string `json:"visibility"`
	// LoadedOnly optionally restricts selection to already-loaded members.
	// Empty (false) is itself the documented default, so no sentinel is needed.
	LoadedOnly bool `json:"loaded_only"`
	// MemberOrder optionally sets the walk order (priority | speed). Empty
	// defaults to "priority"; an unrecognized value is REJECTED (not
	// normalized) so an operator learns of a typo.
	MemberOrder string `json:"member_order"`
	// ClimbSpeedMarginPercent optionally sets the climb margin. nil (omitted)
	// applies the shipped default (routing.DefaultClimbSpeedMarginPercent); an
	// explicit value -- including 0, "no margin required" -- is persisted
	// exactly as given. A pointer is required here because 0 is a legitimate,
	// meaningful value, not an "unset" sentinel indistinguishable from omission.
	ClimbSpeedMarginPercent *int `json:"climb_speed_margin_percent"`
	// MinTokensPerSecond optionally sets the minimum-speed floor; 0 (the zero
	// value, itself the documented off-state) disables it, so no sentinel is
	// needed. Negative is rejected.
	MinTokensPerSecond float64 `json:"min_tokens_per_second"`
	// MinSpeedFallback optionally sets what happens when no candidate reaches
	// the floor (error | ignore). Empty defaults to "error"; an unrecognized
	// value is REJECTED (see MemberOrder).
	MinSpeedFallback string             `json:"min_speed_fallback"`
	Members          []GroupMemberInput `json:"members"`
}

// UpdateModelGroupRequest partially updates a group (nil pointer = leave unchanged).
// A non-nil Members replaces the whole ordered member set.
type UpdateModelGroupRequest struct {
	GatewayModelName *string `json:"gateway_model_name"`
	DisplayName      *string `json:"display_name"`
	Status           *string `json:"status"`
	FailoverMode     *string `json:"failover_mode"`
	// Traversal, when non-nil, sets the subgroup-expansion strategy (depth | breadth
	// | round_robin; empty/unknown normalizes to "round_robin"). Nil = leave unchanged.
	Traversal *string `json:"traversal"`
	// Visibility, when non-nil, sets the group NAME's visibility (shown | hidden |
	// locked). Nil = leave unchanged.
	Visibility *string `json:"visibility"`
	// LoadedOnly, when non-nil, sets the loaded-only restriction. Nil = leave
	// unchanged.
	LoadedOnly *bool `json:"loaded_only"`
	// MemberOrder, when non-nil, sets the walk order (priority | speed; empty
	// normalizes to "priority", unrecognized is REJECTED). Nil = leave unchanged.
	MemberOrder *string `json:"member_order"`
	// ClimbSpeedMarginPercent, when non-nil, sets the climb margin -- including
	// an explicit 0. Nil = leave unchanged (there is no "apply the shipped
	// default" behavior on update, only on create).
	ClimbSpeedMarginPercent *int `json:"climb_speed_margin_percent"`
	// MinTokensPerSecond, when non-nil, sets the minimum-speed floor -- including
	// an explicit 0 (disables it). Nil = leave unchanged. Negative is rejected.
	MinTokensPerSecond *float64 `json:"min_tokens_per_second"`
	// MinSpeedFallback, when non-nil, sets the floor fallback (error | ignore;
	// empty normalizes to "error", unrecognized is REJECTED). Nil = leave
	// unchanged.
	MinSpeedFallback *string             `json:"min_speed_fallback"`
	Members          *[]GroupMemberInput `json:"members"`
}

// SetModelVisibilityRequest sets a model's visibility (shown | hidden | locked).
type SetModelVisibilityRequest struct {
	Visibility string `json:"visibility"`
}

// GroupCache is the in-memory model-group registry the resolver reads on the hot
// path (built + wired in cmd/gateway). After any group or model-setting write the
// Service refreshes it so the offering/routing view is current. A nil cache (not
// wired) is a no-op — the periodic backstop reconcile catches up.
type GroupCache interface {
	RefreshGroups(context.Context) error
}

// refreshGroupCache best-effort refreshes the group registry after a successful
// write. A nil cache (unwired) or a refresh error never fails the write.
func (s *Service) refreshGroupCache(ctx context.Context) {
	if s.groupCache == nil {
		return
	}
	if err := s.groupCache.RefreshGroups(ctx); err != nil {
		slog.Debug("model group cache refresh failed", "error", err)
	}
}

// ListModelGroups returns every group with its ordered members.
func (s *Service) ListModelGroups(ctx context.Context, principal auth.Token) (ModelGroupsResponse, error) {
	groups, err := s.routes.ModelGroups(ctx)
	if err != nil {
		return ModelGroupsResponse{}, err
	}
	out := make([]ModelGroupDTO, 0, len(groups))
	for _, g := range groups {
		dto, err := s.buildModelGroupDTO(ctx, g)
		if err != nil {
			return ModelGroupsResponse{}, err
		}
		out = append(out, dto)
	}
	return ModelGroupsResponse{Data: out}, nil
}

// GetModelGroup returns one group with its ordered members. A missing id collapses
// to ErrModelGroupNotFound (no existence leak beyond the admin scope).
func (s *Service) GetModelGroup(ctx context.Context, principal auth.Token, id string) (ModelGroupDTO, error) {
	group, err := s.routes.ModelGroupByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return ModelGroupDTO{}, ErrModelGroupNotFound
	}
	return s.buildModelGroupDTO(ctx, group)
}

// CreateModelGroup validates + persists a new group and its ordered members. The
// group name must be globally unique against every model name and other group name.
func (s *Service) CreateModelGroup(ctx context.Context, principal auth.Token, req CreateModelGroupRequest) (ModelGroupDTO, error) {
	name := strings.TrimSpace(req.GatewayModelName)
	if name == "" {
		return ModelGroupDTO{}, ErrModelGroupNameRequired
	}
	status, err := normalizeMappingStatus(req.Status)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	mode, err := normalizeFailoverMode(req.FailoverMode)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	traversal := normalizeTraversal(req.Traversal)
	// Validate the optional visibility up-front (empty ⇒ "shown"). A non-empty
	// invalid value rejects the whole create before any mutation.
	visProvided := strings.TrimSpace(req.Visibility) != ""
	vis, err := normalizeVisibility(req.Visibility)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	memberOrder, err := normalizeMemberOrderForWrite(req.MemberOrder)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	minSpeedFallback, err := normalizeMinSpeedFallbackForWrite(req.MinSpeedFallback)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	if req.MinTokensPerSecond < 0 {
		return ModelGroupDTO{}, ErrModelGroupMinTokensPerSecondInvalid
	}
	// Omitted (nil) applies the shipped default; an explicit value -- including
	// 0, "no margin required" -- is persisted exactly as given (see the type's
	// doc comment on CreateModelGroupRequest.ClimbSpeedMarginPercent).
	margin := routing.DefaultClimbSpeedMarginPercent
	if req.ClimbSpeedMarginPercent != nil {
		if *req.ClimbSpeedMarginPercent < 0 {
			return ModelGroupDTO{}, ErrModelGroupClimbSpeedMarginInvalid
		}
		margin = *req.ClimbSpeedMarginPercent
	}
	members, err := s.validateGroupMembers(ctx, req.Members)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	if cyclic, err := s.wouldCreateGroupCycle(ctx, "", name, members); err != nil {
		return ModelGroupDTO{}, err
	} else if cyclic {
		return ModelGroupDTO{}, ErrModelGroupCycle
	}
	taken, err := s.nameTakenGlobally(ctx, name, "")
	if err != nil {
		return ModelGroupDTO{}, err
	}
	if taken {
		return ModelGroupDTO{}, ErrModelGroupNameConflict
	}
	now := s.clock().UTC()
	group := routing.ModelGroup{
		ID:                      "grp_" + compactRandomHex(16),
		GatewayModelName:        name,
		DisplayName:             strings.TrimSpace(req.DisplayName),
		Status:                  status,
		FailoverMode:            mode,
		Traversal:               traversal,
		CreatedAt:               now,
		UpdatedAt:               now,
		LoadedOnly:              req.LoadedOnly,
		MemberOrder:             memberOrder,
		ClimbSpeedMarginPercent: margin,
		MinTokensPerSecond:      req.MinTokensPerSecond,
		MinSpeedFallback:        minSpeedFallback,
	}
	if group.DisplayName == "" {
		group.DisplayName = name
	}
	if err := s.routes.CreateModelGroup(ctx, group); err != nil {
		return ModelGroupDTO{}, err
	}
	if err := s.routes.SetGroupMembers(ctx, group.ID, members); err != nil {
		return ModelGroupDTO{}, err
	}
	// Persist the group NAME's visibility (only when explicitly provided; the
	// default "shown" needs no row). One save sets group + visibility.
	if visProvided {
		if err := s.routes.UpsertModelSetting(ctx, routing.ModelSetting{
			GatewayModelName: name,
			Visibility:       vis,
			CreatedAt:        now,
			UpdatedAt:        now,
		}); err != nil {
			return ModelGroupDTO{}, err
		}
	}
	s.refreshGroupCache(ctx)
	return s.buildModelGroupDTO(ctx, group)
}

// UpdateModelGroup partially updates a group; a non-nil Members replaces the whole
// ordered member set.
func (s *Service) UpdateModelGroup(ctx context.Context, principal auth.Token, id string, req UpdateModelGroupRequest) (ModelGroupDTO, error) {
	group, err := s.routes.ModelGroupByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return ModelGroupDTO{}, ErrModelGroupNotFound
	}
	// Validate everything that can fail BEFORE mutating the loaded group.
	if req.GatewayModelName != nil {
		name := strings.TrimSpace(*req.GatewayModelName)
		if name == "" {
			return ModelGroupDTO{}, ErrModelGroupNameRequired
		}
		taken, err := s.nameTakenGlobally(ctx, name, group.ID)
		if err != nil {
			return ModelGroupDTO{}, err
		}
		if taken {
			return ModelGroupDTO{}, ErrModelGroupNameConflict
		}
		group.GatewayModelName = name
	}
	if req.DisplayName != nil {
		group.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Status != nil {
		status, err := normalizeMappingStatus(*req.Status)
		if err != nil {
			return ModelGroupDTO{}, err
		}
		group.Status = status
	}
	if req.FailoverMode != nil {
		mode, err := normalizeFailoverMode(*req.FailoverMode)
		if err != nil {
			return ModelGroupDTO{}, err
		}
		group.FailoverMode = mode
	}
	if req.Traversal != nil {
		group.Traversal = normalizeTraversal(*req.Traversal)
	}
	// Validate the optional visibility BEFORE any mutation (consistent with the
	// other fields). Applied after the member write below.
	var vis string
	if req.Visibility != nil {
		vis, err = normalizeVisibility(*req.Visibility)
		if err != nil {
			return ModelGroupDTO{}, err
		}
	}
	if req.LoadedOnly != nil {
		group.LoadedOnly = *req.LoadedOnly
	}
	if req.MemberOrder != nil {
		memberOrder, mErr := normalizeMemberOrderForWrite(*req.MemberOrder)
		if mErr != nil {
			return ModelGroupDTO{}, mErr
		}
		group.MemberOrder = memberOrder
	}
	if req.ClimbSpeedMarginPercent != nil {
		if *req.ClimbSpeedMarginPercent < 0 {
			return ModelGroupDTO{}, ErrModelGroupClimbSpeedMarginInvalid
		}
		group.ClimbSpeedMarginPercent = *req.ClimbSpeedMarginPercent
	}
	if req.MinTokensPerSecond != nil {
		if *req.MinTokensPerSecond < 0 {
			return ModelGroupDTO{}, ErrModelGroupMinTokensPerSecondInvalid
		}
		group.MinTokensPerSecond = *req.MinTokensPerSecond
	}
	if req.MinSpeedFallback != nil {
		minSpeedFallback, fErr := normalizeMinSpeedFallbackForWrite(*req.MinSpeedFallback)
		if fErr != nil {
			return ModelGroupDTO{}, fErr
		}
		group.MinSpeedFallback = minSpeedFallback
	}
	var members []routing.GroupMember
	membersChanged := false
	if req.Members != nil {
		members, err = s.validateGroupMembers(ctx, *req.Members)
		if err != nil {
			return ModelGroupDTO{}, err
		}
		if cyclic, err := s.wouldCreateGroupCycle(ctx, group.ID, group.GatewayModelName, members); err != nil {
			return ModelGroupDTO{}, err
		} else if cyclic {
			return ModelGroupDTO{}, ErrModelGroupCycle
		}
		membersChanged = true
	}
	if group.DisplayName == "" {
		group.DisplayName = group.GatewayModelName
	}
	group.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateModelGroup(ctx, group); err != nil {
		return ModelGroupDTO{}, err
	}
	if membersChanged {
		if err := s.routes.SetGroupMembers(ctx, group.ID, members); err != nil {
			return ModelGroupDTO{}, err
		}
	}
	if req.Visibility != nil {
		if err := s.routes.UpsertModelSetting(ctx, routing.ModelSetting{
			GatewayModelName: group.GatewayModelName,
			Visibility:       vis,
			CreatedAt:        group.UpdatedAt,
			UpdatedAt:        group.UpdatedAt,
		}); err != nil {
			return ModelGroupDTO{}, err
		}
	}
	s.refreshGroupCache(ctx)
	return s.buildModelGroupDTO(ctx, group)
}

// DeleteModelGroup removes a group; its members cascade. A missing id collapses to
// ErrModelGroupNotFound.
func (s *Service) DeleteModelGroup(ctx context.Context, principal auth.Token, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrModelGroupNotFound
	}
	// Load the group first so we know its NAME for the best-effort settings reset
	// (the row + members cascade away on delete). A load failure just skips the reset.
	group, loadErr := s.routes.ModelGroupByID(ctx, id)
	if err := s.routes.DeleteModelGroup(ctx, id); err != nil {
		return ErrModelGroupNotFound
	}
	// Best-effort: reset the deleted group name's visibility to "shown" so a later
	// group (or model) reusing the name doesn't inherit a stale hidden/locked state.
	// Never fail the delete on a settings-reset error.
	if loadErr == nil {
		now := s.clock().UTC()
		if err := s.routes.UpsertModelSetting(ctx, routing.ModelSetting{
			GatewayModelName: group.GatewayModelName,
			Visibility:       "shown",
			CreatedAt:        now,
			UpdatedAt:        now,
		}); err != nil {
			slog.Debug("model group visibility reset on delete failed", "error", err)
		}
	}
	s.refreshGroupCache(ctx)
	return nil
}

// SetModelVisibility upserts a model's visibility (shown | hidden | locked). The
// name must be a known gateway model (a mapping exists with that GatewayModelName on
// some server) OR a known model GROUP name (a group name is itself a
// gateway_model_name; its visibility lives in the same model_settings table).
func (s *Service) SetModelVisibility(ctx context.Context, principal auth.Token, name, visibility string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrModelGroupMemberInvalid
	}
	vis, err := normalizeVisibility(visibility)
	if err != nil {
		return err
	}
	// The name must be a known gateway MODEL (a mapping exists with that name) OR a
	// known GROUP name — a group name is itself a gateway_model_name whose visibility
	// lives in the same model_settings table.
	exists, err := s.gatewayModelExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		grpExists, gErr := s.groupNameExists(ctx, name)
		if gErr != nil {
			return gErr
		}
		exists = grpExists
	}
	if !exists {
		return ErrModelGroupMemberInvalid
	}
	now := s.clock().UTC()
	if err := s.routes.UpsertModelSetting(ctx, routing.ModelSetting{
		GatewayModelName: name,
		Visibility:       vis,
		CreatedAt:        now,
		UpdatedAt:        now,
	}); err != nil {
		return err
	}
	s.refreshGroupCache(ctx)
	return nil
}

// buildModelGroupDTO loads a group's ordered members and assembles its DTO.
func (s *Service) buildModelGroupDTO(ctx context.Context, group routing.ModelGroup) (ModelGroupDTO, error) {
	members, err := s.routes.GroupMembersByGroup(ctx, group.ID)
	if err != nil {
		return ModelGroupDTO{}, err
	}
	memberDTOs := make([]GroupMemberDTO, 0, len(members))
	for _, m := range members {
		memberDTOs = append(memberDTOs, GroupMemberDTO{MemberGatewayName: m.MemberGatewayName})
	}
	// The group NAME's visibility from model_settings; "shown" when no row exists.
	visibility := "shown"
	if setting, ok, sErr := s.routes.ModelSettingByName(ctx, group.GatewayModelName); sErr == nil && ok && setting.Visibility != "" {
		visibility = setting.Visibility
	}
	return ModelGroupDTO{
		ID:                      group.ID,
		GatewayModelName:        group.GatewayModelName,
		DisplayName:             group.DisplayName,
		Status:                  group.Status,
		FailoverMode:            group.FailoverMode,
		Traversal:               group.Traversal,
		Visibility:              visibility,
		LoadedOnly:              group.LoadedOnly,
		MemberOrder:             group.MemberOrder,
		ClimbSpeedMarginPercent: group.ClimbSpeedMarginPercent,
		MinTokensPerSecond:      group.MinTokensPerSecond,
		MinSpeedFallback:        group.MinSpeedFallback,
		Members:                 memberDTOs,
	}, nil
}

// validateGroupMembers turns the ordered member inputs into store members: each name
// must be an existing gateway model OR an existing group NAME (nested groups —
// Phase 1; cycle detection is a separate step, see wouldCreateGroupCycle),
// duplicates are dropped (case-insensitive, keep first), and Priority is the final
// slice index.
func (s *Service) validateGroupMembers(ctx context.Context, inputs []GroupMemberInput) ([]routing.GroupMember, error) {
	out := make([]routing.GroupMember, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, in := range inputs {
		name := strings.TrimSpace(in.MemberGatewayName)
		if name == "" {
			return nil, ErrModelGroupMemberInvalid
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			continue // drop duplicate member (case-insensitive), keep first
		}
		seen[key] = struct{}{}
		exists, err := s.gatewayModelExists(ctx, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			grp, gErr := s.groupNameExists(ctx, name)
			if gErr != nil {
				return nil, gErr
			}
			exists = grp
		}
		if !exists {
			return nil, ErrModelGroupMemberInvalid // neither a model nor a group
		}
		out = append(out, routing.GroupMember{
			MemberGatewayName: name,
			Priority:          len(out),
		})
	}
	return out, nil
}

// wouldCreateGroupCycle reports whether making the group named groupName have the
// given members would introduce a cycle (groupName transitively reaching itself,
// including a direct self-reference). Uses the currently stored groups' membership,
// with groupName's adjacency overridden by the prospective members. excludeID is
// the edited group's own id (its stored members are replaced by `members`); pass ""
// on create (the group doesn't exist yet, so there's nothing to exclude).
func (s *Service) wouldCreateGroupCycle(ctx context.Context, excludeID, groupName string, members []routing.GroupMember) (bool, error) {
	groups, err := s.routes.ModelGroups(ctx)
	if err != nil {
		return false, err
	}
	adj := make(map[string][]string, len(groups)+1)
	for _, g := range groups {
		if g.ID == excludeID {
			continue
		}
		ms, err := s.routes.GroupMembersByGroup(ctx, g.ID)
		if err != nil {
			return false, err
		}
		names := make([]string, 0, len(ms))
		for _, m := range ms {
			names = append(names, m.MemberGatewayName)
		}
		adj[strings.ToLower(strings.TrimSpace(g.GatewayModelName))] = names
	}
	prospect := make([]string, 0, len(members))
	for _, m := range members {
		prospect = append(prospect, m.MemberGatewayName)
	}
	adj[strings.ToLower(strings.TrimSpace(groupName))] = prospect

	start := strings.ToLower(strings.TrimSpace(groupName))
	var visit func(node string, seen map[string]struct{}) bool
	visit = func(node string, seen map[string]struct{}) bool {
		for _, m := range adj[strings.ToLower(strings.TrimSpace(node))] {
			lm := strings.ToLower(strings.TrimSpace(m))
			if lm == start {
				return true // reached the start group again → cycle
			}
			if _, isGroup := adj[lm]; !isGroup {
				continue // a model leaf
			}
			if _, ok := seen[lm]; ok {
				continue
			}
			seen[lm] = struct{}{}
			if visit(m, seen) {
				return true
			}
		}
		return false
	}
	return visit(groupName, map[string]struct{}{}), nil
}

// gatewayModelExists reports whether name is an existing gateway model NAME
// (case-insensitive) served by at least one mapping on some server, regardless of
// mapping status. A group member is a loose name reference, so a currently-disabled
// or unreachable model still counts as existing (it simply contributes 0 candidates
// at routing time).
func (s *Service) gatewayModelExists(ctx context.Context, name string) (bool, error) {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return false, nil
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return false, err
	}
	for _, srv := range servers {
		mappings, err := s.routes.MappingsByServer(ctx, srv.ID)
		if err != nil {
			return false, err
		}
		for _, m := range mappings {
			if strings.ToLower(strings.TrimSpace(m.GatewayModelName)) == target {
				return true, nil
			}
		}
	}
	return false, nil
}

// groupNameExists reports whether name equals any group's gateway model name
// (case-insensitive). Used to keep a mapping from shadowing a group name.
func (s *Service) groupNameExists(ctx context.Context, name string) (bool, error) {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return false, nil
	}
	groups, err := s.routes.ModelGroups(ctx)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if strings.ToLower(strings.TrimSpace(g.GatewayModelName)) == target {
			return true, nil
		}
	}
	return false, nil
}

// nameTakenGlobally reports whether name collides (case-insensitive) with ANY
// gateway model name (any mapping, any server) or ANY other group name. This keeps
// the models ∪ groups namespace globally unique. excludeGroupID skips a group's own
// name on update.
func (s *Service) nameTakenGlobally(ctx context.Context, name, excludeGroupID string) (bool, error) {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return false, nil
	}
	groups, err := s.routes.ModelGroups(ctx)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if g.ID == excludeGroupID {
			continue
		}
		if strings.ToLower(strings.TrimSpace(g.GatewayModelName)) == target {
			return true, nil
		}
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return false, err
	}
	for _, srv := range servers {
		taken, err := s.gatewayNameTakenOnServer(ctx, srv.ID, name, "")
		if err != nil {
			return false, err
		}
		if taken {
			return true, nil
		}
	}
	return false, nil
}

// normalizeFailoverMode validates a group failover mode. Empty defaults to "sticky".
func normalizeFailoverMode(raw string) (string, error) {
	mode := strings.TrimSpace(raw)
	switch mode {
	case "":
		return "sticky", nil
	case "sticky", "climb_up":
		return mode, nil
	default:
		return "", ErrModelGroupModeInvalid
	}
}

// normalizeTraversal validates a group's subgroup-traversal strategy. Empty or
// unknown defaults to "round_robin" (lenient, like normalizeLoadedModelsFormat).
func normalizeTraversal(raw string) string {
	switch strings.TrimSpace(raw) {
	case "depth", "breadth", "round_robin":
		return strings.TrimSpace(raw)
	default:
		return "round_robin"
	}
}

// normalizeMemberOrderForWrite validates a group's member-ordering strategy on
// write. Unlike normalizeTraversal (which fails open to a default for an
// unknown value, matching the resolver's fail-open READ path), an
// unrecognized member_order is REJECTED here so an operator learns of a typo
// instead of silently getting priority order. Empty defaults to "priority".
func normalizeMemberOrderForWrite(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	switch v {
	case "":
		return routing.MemberOrderPriority, nil
	case routing.MemberOrderPriority, routing.MemberOrderSpeed:
		return v, nil
	default:
		return "", ErrModelGroupMemberOrderInvalid
	}
}

// normalizeMinSpeedFallbackForWrite validates the minimum-speed-floor
// fallback on write; an unrecognized value is REJECTED (see
// normalizeMemberOrderForWrite). Empty defaults to "error".
func normalizeMinSpeedFallbackForWrite(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	switch v {
	case "":
		return routing.MinSpeedFallbackError, nil
	case routing.MinSpeedFallbackError, routing.MinSpeedFallbackIgnore:
		return v, nil
	default:
		return "", ErrModelGroupMinSpeedFallbackInvalid
	}
}

// normalizeVisibility validates a model visibility. Empty defaults to "shown".
func normalizeVisibility(raw string) (string, error) {
	vis := strings.TrimSpace(raw)
	switch vis {
	case "":
		return "shown", nil
	case "shown", "hidden", "locked":
		return vis, nil
	default:
		return "", ErrModelVisibilityInvalid
	}
}
