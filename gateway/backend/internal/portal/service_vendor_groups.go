// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"time"
)

// Vendor account group member states and reasons (VendorAccountGroupMemberDTO).
const (
	VendorGroupStateActive        = "active"
	VendorGroupStateAvailable     = "available"
	VendorGroupStateOverThreshold = "over_threshold"
	VendorGroupStateCooldown      = "cooldown"
	VendorGroupStateNotUsable     = "not_usable"

	VendorGroupReasonDisabled              = "disabled"
	VendorGroupReasonNeedsReconnect        = "needs_reconnect"
	VendorGroupReasonSubscriptionsDisabled = "subscriptions_disabled"
	VendorGroupReasonNotReachable          = "not_reachable"
)

// CreateVendorAccountGroupRequest creates a group. Nil thresholds default to
// routing.DefaultVendorGroupThresholdPct, an empty Status to active. Vendor is
// fixed at create.
type CreateVendorAccountGroupRequest struct {
	Name                 string   `json:"name"`
	Vendor               string   `json:"vendor"`
	ModelPrefix          string   `json:"model_prefix"`
	Status               string   `json:"status"`
	ThresholdFiveHourPct *int     `json:"threshold_five_hour_pct"`
	ThresholdWeeklyPct   *int     `json:"threshold_weekly_pct"`
	ThresholdCreditsPct  *int     `json:"threshold_credits_pct"`
	AccountIDs           []string `json:"account_ids"`
}

// UpdateVendorAccountGroupRequest is a partial update; nil fields stay. The vendor
// and the members are not editable here (members: SetVendorAccountGroupMembers).
type UpdateVendorAccountGroupRequest struct {
	Name                 *string `json:"name"`
	ModelPrefix          *string `json:"model_prefix"`
	Status               *string `json:"status"`
	ThresholdFiveHourPct *int    `json:"threshold_five_hour_pct"`
	ThresholdWeeklyPct   *int    `json:"threshold_weekly_pct"`
	ThresholdCreditsPct  *int    `json:"threshold_credits_pct"`
}

// SetVendorAccountGroupMembersRequest is the body of the member replace; the order
// of AccountIDs is the member order.
type SetVendorAccountGroupMembersRequest struct {
	AccountIDs []string `json:"account_ids"`
}

// VendorGroupThresholdsDTO are a group's switch-over thresholds (percent used).
type VendorGroupThresholdsDTO struct {
	FiveHourPct int `json:"five_hour_pct"`
	WeeklyPct   int `json:"weekly_pct"`
	CreditsPct  int `json:"credits_pct"`
}

// VendorCooldownDTO is one active limit cooldown of a member for an upstream model.
type VendorCooldownDTO struct {
	Model string    `json:"model"`
	Until time.Time `json:"until"`
}

// VendorAccountGroupMemberDTO is one member with its computed status (spec section 9).
// Position is the 1-based index in member order. OwnerName is set only for a member
// owned by someone else. Pct and Usage are never set for a member whose usage the
// group owner is not entitled to see (a shared account without the owner's usage
// opt-in); its Cooldowns and FreeAt carry the cooldowns' public end (Retry-After or
// the default), never a scraped window reset (memberVendorCooldowns).
type VendorAccountGroupMemberDTO struct {
	AccountID   string                 `json:"account_id"`
	AccountName string                 `json:"account_name"`
	AuthType    string                 `json:"auth_type"`
	OwnerName   string                 `json:"owner_name"`
	Position    int                    `json:"position"`
	State       string                 `json:"state"`
	Reason      string                 `json:"reason"`
	Pct         *float64               `json:"pct,omitempty"`
	FreeAt      *time.Time             `json:"free_at,omitempty"`
	Cooldowns   []VendorCooldownDTO    `json:"cooldowns"`
	Usage       *VendorAccountUsageDTO `json:"usage,omitempty"`
}

// VendorAccountGroupDTO is the portal view of a vendor account group.
// ActiveMemberID is the member routing would pick ignoring the model (spec section 9),
// "" when no member is usable.
type VendorAccountGroupDTO struct {
	ID             string                        `json:"id"`
	Vendor         string                        `json:"vendor"`
	Name           string                        `json:"name"`
	ModelPrefix    string                        `json:"model_prefix"`
	Status         string                        `json:"status"`
	Thresholds     VendorGroupThresholdsDTO      `json:"thresholds"`
	ActiveMemberID string                        `json:"active_member_id"`
	Members        []VendorAccountGroupMemberDTO `json:"members"`
	CreatedAt      time.Time                     `json:"created_at"`
	UpdatedAt      time.Time                     `json:"updated_at"`
}

// VendorAccountGroupListResponse is the group list.
type VendorAccountGroupListResponse struct {
	Groups []VendorAccountGroupDTO `json:"groups"`
}

func normalizeVendorGroupThreshold(v *int) (int, error) {
	if v == nil {
		return routing.DefaultVendorGroupThresholdPct, nil
	}
	if *v < 1 || *v > 100 {
		return 0, ErrVendorGroupThresholdInvalid
	}
	return *v, nil
}

func normalizeVendorGroupName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", ErrVendorGroupNameRequired
	}
	return name, nil
}

func normalizeVendorGroupPrefix(raw string) (string, error) {
	prefix, err := normalizeVendorAccountModelPrefix(raw)
	if err != nil {
		return "", ErrVendorGroupModelPrefixInvalid
	}
	return prefix, nil
}

func normalizeVendorGroupStatus(raw string) (string, error) {
	switch status := strings.TrimSpace(raw); status {
	case routing.VendorAccountGroupStatusActive, routing.VendorAccountGroupStatusDisabled:
		return status, nil
	default:
		return "", ErrVendorGroupStatusInvalid
	}
}

// vendorGroupOwner is the module-flag and owner gate every group method starts
// with. It does not rely on the store to reject a missing owner (SQL answers
// not found, the memory store accepts).
func (s *Service) vendorGroupOwner(ctx context.Context, principal auth.Token) (string, error) {
	if err := s.requireVendorAccountsEnabled(ctx); err != nil {
		return "", err
	}
	return principal.UserID, nil
}

// ownedVendorGroup loads group id for its owner; a stranger's group, an unknown id
// and a principal without a user are all ErrVendorGroupNotFound.
func (s *Service) ownedVendorGroup(ctx context.Context, principal auth.Token, id string) (routing.VendorAccountGroup, error) {
	if _, err := s.vendorGroupOwner(ctx, principal); err != nil {
		return routing.VendorAccountGroup{}, err
	}
	g, err := s.routes.VendorAccountGroupByID(ctx, strings.TrimSpace(id))
	if errors.Is(err, store.ErrNotFound) || (err == nil && (principal.UserID == "" || g.OwnerUserID != principal.UserID)) {
		return routing.VendorAccountGroup{}, ErrVendorGroupNotFound
	}
	if err != nil {
		return routing.VendorAccountGroup{}, err
	}
	return g, nil
}

// ListVendorAccountGroups lists the caller's own groups (id order).
func (s *Service) ListVendorAccountGroups(ctx context.Context, principal auth.Token) (VendorAccountGroupListResponse, error) {
	if _, err := s.vendorGroupOwner(ctx, principal); err != nil {
		return VendorAccountGroupListResponse{}, err
	}
	out := VendorAccountGroupListResponse{Groups: []VendorAccountGroupDTO{}}
	if principal.UserID == "" {
		return out, nil
	}
	groups, err := s.routes.VendorAccountGroupsByOwner(ctx, principal.UserID)
	if err != nil {
		return out, err
	}
	for _, g := range groups {
		dto, err := s.vendorAccountGroupDTO(ctx, g)
		if err != nil {
			return VendorAccountGroupListResponse{Groups: []VendorAccountGroupDTO{}}, err
		}
		out.Groups = append(out.Groups, dto)
	}
	return out, nil
}

// GetVendorAccountGroup returns one of the caller's groups with its member states.
func (s *Service) GetVendorAccountGroup(ctx context.Context, principal auth.Token, id string) (VendorAccountGroupDTO, error) {
	g, err := s.ownedVendorGroup(ctx, principal, id)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	return s.vendorAccountGroupDTO(ctx, g)
}

// CreateVendorAccountGroup creates a group owned by the caller with the given
// members (in order).
func (s *Service) CreateVendorAccountGroup(ctx context.Context, principal auth.Token, req CreateVendorAccountGroupRequest) (VendorAccountGroupDTO, error) {
	owner, err := s.vendorGroupOwner(ctx, principal)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if owner == "" {
		return VendorAccountGroupDTO{}, ErrVendorGroupForbidden
	}
	name, err := normalizeVendorGroupName(req.Name)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	vendor, err := normalizeVendorAccountVendor(req.Vendor)
	if err != nil {
		return VendorAccountGroupDTO{}, ErrVendorGroupVendorInvalid
	}
	prefix, err := normalizeVendorGroupPrefix(req.ModelPrefix)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	status := routing.VendorAccountGroupStatusActive
	if strings.TrimSpace(req.Status) != "" {
		if status, err = normalizeVendorGroupStatus(req.Status); err != nil {
			return VendorAccountGroupDTO{}, err
		}
	}
	now := s.clock().UTC()
	g := routing.VendorAccountGroup{
		ID: "vag_" + compactRandomHex(16), OwnerUserID: owner, Vendor: vendor, Name: name,
		ModelPrefix: prefix, Status: status, CreatedAt: now, UpdatedAt: now,
	}
	if g.ThresholdFiveHourPct, err = normalizeVendorGroupThreshold(req.ThresholdFiveHourPct); err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if g.ThresholdWeeklyPct, err = normalizeVendorGroupThreshold(req.ThresholdWeeklyPct); err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if g.ThresholdCreditsPct, err = normalizeVendorGroupThreshold(req.ThresholdCreditsPct); err != nil {
		return VendorAccountGroupDTO{}, err
	}
	ids, err := s.validateVendorGroupMembers(ctx, g, req.AccountIDs)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if err := s.checkVendorGroupPrefixConflict(ctx, g, ids); err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if err := s.routes.CreateVendorAccountGroup(ctx, g); err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if err := s.routes.SetVendorAccountGroupMembers(ctx, g.ID, ids); err != nil {
		// Do not leave a memberless group behind a refused create.
		_ = s.routes.DeleteVendorAccountGroup(context.WithoutCancel(ctx), g.ID)
		return VendorAccountGroupDTO{}, mapVendorGroupMemberStoreError(err)
	}
	return s.vendorAccountGroupDTO(ctx, g)
}

// UpdateVendorAccountGroup applies a partial update to one of the caller's groups.
func (s *Service) UpdateVendorAccountGroup(ctx context.Context, principal auth.Token, id string, req UpdateVendorAccountGroupRequest) (VendorAccountGroupDTO, error) {
	g, err := s.ownedVendorGroup(ctx, principal, id)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	prefixChanged := false
	if req.Name != nil {
		if g.Name, err = normalizeVendorGroupName(*req.Name); err != nil {
			return VendorAccountGroupDTO{}, err
		}
	}
	if req.ModelPrefix != nil {
		prefix, err := normalizeVendorGroupPrefix(*req.ModelPrefix)
		if err != nil {
			return VendorAccountGroupDTO{}, err
		}
		prefixChanged = prefix != g.ModelPrefix
		g.ModelPrefix = prefix
	}
	if req.Status != nil {
		if g.Status, err = normalizeVendorGroupStatus(*req.Status); err != nil {
			return VendorAccountGroupDTO{}, err
		}
	}
	for _, t := range []struct {
		in  *int
		out *int
	}{
		{req.ThresholdFiveHourPct, &g.ThresholdFiveHourPct},
		{req.ThresholdWeeklyPct, &g.ThresholdWeeklyPct},
		{req.ThresholdCreditsPct, &g.ThresholdCreditsPct},
	} {
		if t.in == nil {
			continue
		}
		if *t.in < 1 || *t.in > 100 {
			return VendorAccountGroupDTO{}, ErrVendorGroupThresholdInvalid
		}
		*t.out = *t.in
	}
	if prefixChanged {
		ids, err := s.vendorGroupMemberIDs(ctx, g.ID)
		if err != nil {
			return VendorAccountGroupDTO{}, err
		}
		if err := s.checkVendorGroupPrefixConflict(ctx, g, ids); err != nil {
			return VendorAccountGroupDTO{}, err
		}
	}
	g.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateVendorAccountGroup(ctx, g); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return VendorAccountGroupDTO{}, ErrVendorGroupNotFound
		}
		return VendorAccountGroupDTO{}, err
	}
	return s.vendorAccountGroupDTO(ctx, g)
}

// DeleteVendorAccountGroup deletes one of the caller's groups. A token policy that
// still names it simply matches nothing afterwards.
func (s *Service) DeleteVendorAccountGroup(ctx context.Context, principal auth.Token, id string) error {
	g, err := s.ownedVendorGroup(ctx, principal, id)
	if err != nil {
		return err
	}
	if err := s.routes.DeleteVendorAccountGroup(ctx, g.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrVendorGroupNotFound
		}
		return err
	}
	return nil
}

// SetVendorAccountGroupMembers replaces the member set and order of one of the
// caller's groups.
func (s *Service) SetVendorAccountGroupMembers(ctx context.Context, principal auth.Token, id string, accountIDs []string) (VendorAccountGroupDTO, error) {
	g, err := s.ownedVendorGroup(ctx, principal, id)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	ids, err := s.validateVendorGroupMembers(ctx, g, accountIDs)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if err := s.checkVendorGroupPrefixConflict(ctx, g, ids); err != nil {
		return VendorAccountGroupDTO{}, err
	}
	if err := s.routes.SetVendorAccountGroupMembers(ctx, g.ID, ids); err != nil {
		return VendorAccountGroupDTO{}, mapVendorGroupMemberStoreError(err)
	}
	return s.vendorAccountGroupDTO(ctx, g)
}

func mapVendorGroupMemberStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrConflict):
		return ErrVendorGroupMemberDuplicate
	case errors.Is(err, store.ErrNotFound):
		return ErrVendorGroupMemberUnreachable
	default:
		return err
	}
}

// vendorGroupMemberIDs is the group's member account ids in member order.
func (s *Service) vendorGroupMemberIDs(ctx context.Context, groupID string) ([]string, error) {
	members, err := s.routes.VendorAccountGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.AccountID
	}
	return ids, nil
}

// validateVendorGroupMembers checks a requested member list for group g and returns
// the trimmed ids: no duplicates, every account reachable by the group owner right
// now (VendorAccountsForRouting, own or shared), and of the group's vendor.
func (s *Service) validateVendorGroupMembers(ctx context.Context, g routing.VendorAccountGroup, raw []string) ([]string, error) {
	ids := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: %q", ErrVendorGroupMemberDuplicate, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ids, nil
	}
	reachable, err := s.reachableVendorAccountsByID(ctx, userTokenPrincipal(auth.Token{UserID: g.OwnerUserID}))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		acc, ok := reachable[id]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrVendorGroupMemberUnreachable, id)
		}
		if acc.Vendor != g.Vendor {
			return nil, fmt.Errorf("%w: %q is a %s account", ErrVendorGroupMemberVendorMismatch, id, acc.Vendor)
		}
	}
	return ids, nil
}

// vendorGroupNames is the set of public names prefix + upstream model over the
// models of the given accounts.
func (s *Service) vendorGroupNames(ctx context.Context, prefix string, accountIDs []string) (map[string]struct{}, error) {
	names := map[string]struct{}{}
	for _, id := range accountIDs {
		models, err := s.routes.VendorAccountModels(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			names[prefix+m.UpstreamModel] = struct{}{}
		}
	}
	return names, nil
}

// checkVendorGroupPrefixConflict refuses group g (with the members ids) when one of
// its public names is also a name of an own account outside the group or of another
// own group (spec 5.1). A name shared with one of its own members is fine: the group
// shadows the member. Every status counts, so enabling an account or a group later
// cannot create a clash the save-time check let through.
func (s *Service) checkVendorGroupPrefixConflict(ctx context.Context, g routing.VendorAccountGroup, ids []string) error {
	mine, err := s.vendorGroupNames(ctx, g.ModelPrefix, ids)
	if err != nil || len(mine) == 0 {
		return err
	}
	clash := func(other map[string]struct{}, what string) error {
		for name := range other {
			if _, hit := mine[name]; hit {
				return fmt.Errorf("%w: %q is also served by %s", ErrVendorGroupPrefixConflict, name, what)
			}
		}
		return nil
	}
	own, err := s.routes.VendorAccountsByOwner(ctx, g.OwnerUserID)
	if err != nil {
		return err
	}
	for _, acc := range own {
		if slices.Contains(ids, acc.ID) {
			continue
		}
		names, err := s.vendorGroupNames(ctx, acc.ModelPrefix, []string{acc.ID})
		if err != nil {
			return err
		}
		if err := clash(names, fmt.Sprintf("account %q", acc.Name)); err != nil {
			return err
		}
	}
	groups, err := s.routes.VendorAccountGroupsByOwner(ctx, g.OwnerUserID)
	if err != nil {
		return err
	}
	for _, other := range groups {
		if other.ID == g.ID {
			continue
		}
		otherIDs, err := s.vendorGroupMemberIDs(ctx, other.ID)
		if err != nil {
			return err
		}
		names, err := s.vendorGroupNames(ctx, other.ModelPrefix, otherIDs)
		if err != nil {
			return err
		}
		if err := clash(names, fmt.Sprintf("group %q", other.Name)); err != nil {
			return err
		}
	}
	return nil
}

// vendorAccountGroupDTO renders g with the computed state of each member.
//
// Reach is the owner's live routing grant; "usable" is spec 5.3 rules 1 and 2
// (reachable, active, subscriptions switch on). The active member is the one
// SelectVendorGroupMember picks with the fallback on and the model ignored (rule 3
// is not part of a candidate's Usable here), so it is model-independent. A member's
// cooldown covers every model it is cooling down for.
func (s *Service) vendorAccountGroupDTO(ctx context.Context, g routing.VendorAccountGroup) (VendorAccountGroupDTO, error) {
	members, err := s.routes.VendorAccountGroupMembers(ctx, g.ID)
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	reachable, err := s.reachableVendorAccountsByID(ctx, userTokenPrincipal(auth.Token{UserID: g.OwnerUserID}))
	if err != nil {
		return VendorAccountGroupDTO{}, err
	}
	subscriptionsOn := s.VendorSubscriptionsEnabled(ctx)
	now := s.clock()
	th := g.Thresholds()
	dto := VendorAccountGroupDTO{
		ID: g.ID, Vendor: g.Vendor, Name: g.Name, ModelPrefix: g.ModelPrefix, Status: g.Status,
		Thresholds: VendorGroupThresholdsDTO{FiveHourPct: th.FiveHourPct, WeeklyPct: th.WeeklyPct, CreditsPct: th.CreditsPct},
		Members:    make([]VendorAccountGroupMemberDTO, len(members)),
		CreatedAt:  g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
	cands := make([]routing.VendorGroupCandidate, len(members))
	cooldowns := make([][]routing.VendorCooldown, len(members))
	for i, m := range members {
		member := VendorAccountGroupMemberDTO{AccountID: m.AccountID, Position: i + 1, Cooldowns: []VendorCooldownDTO{}}
		cands[i].AccountID = m.AccountID
		acc, ok := reachable[m.AccountID]
		switch {
		case !ok || acc.Vendor != g.Vendor:
			member.State, member.Reason = VendorGroupStateNotUsable, VendorGroupReasonNotReachable
		default:
			member.AccountName, member.AuthType = acc.Name, acc.AuthType
			if acc.OwnerUserID != g.OwnerUserID {
				member.OwnerName = s.vendorAccountOwnerName(ctx, acc)
			}
			switch {
			case acc.Status == routing.VendorAccountStatusNeedsReconnect:
				member.State, member.Reason = VendorGroupStateNotUsable, VendorGroupReasonNeedsReconnect
			case acc.Status != routing.VendorAccountStatusActive:
				member.State, member.Reason = VendorGroupStateNotUsable, VendorGroupReasonDisabled
			case acc.AuthType == routing.VendorAuthSubscription && !subscriptionsOn:
				member.State, member.Reason = VendorGroupStateNotUsable, VendorGroupReasonSubscriptionsDisabled
			default:
				cands[i].Usable = true
				cands[i].ThresholdsApply = acc.OwnerUserID == g.OwnerUserID || acc.ShareUsageWithMembers
			}
		}
		if cands[i].Usable {
			if u, found, err := s.routes.VendorAccountUsageByID(ctx, m.AccountID); err != nil {
				return VendorAccountGroupDTO{}, err
			} else if found {
				cands[i].Usage, cands[i].HasUsage = u, true
			}
			cooldowns[i] = s.memberVendorCooldowns(m.AccountID, cands[i].ThresholdsApply, now)
			for _, c := range cooldowns[i] {
				member.Cooldowns = append(member.Cooldowns, VendorCooldownDTO{Model: c.UpstreamModel, Until: c.Until})
				if c.Until.After(cands[i].CooldownUntil) {
					cands[i].CooldownUntil = c.Until
				}
			}
		}
		dto.Members[i] = member
	}
	active, hasActive := routing.SelectVendorGroupMember(cands, th, now, true)
	for i := range dto.Members {
		if !cands[i].Usable {
			continue
		}
		member := &dto.Members[i]
		blocks := routing.VendorGroupMemberBlocks(cands[i], th, now)
		if cands[i].ThresholdsApply && cands[i].HasUsage {
			usage, err := s.vendorAccountUsageDTO(ctx, cands[i].AccountID)
			if err != nil {
				return VendorAccountGroupDTO{}, err
			}
			member.Usage = usage
		}
		switch {
		case hasActive && i == active:
			member.State = VendorGroupStateActive
			dto.ActiveMemberID = member.AccountID
		case len(blocks) == 0:
			member.State = VendorGroupStateAvailable
		default:
			applyVendorGroupBlocks(member, blocks)
		}
	}
	return dto, nil
}

// memberVendorCooldowns is the active cooldowns of member accountID as the group
// owner may see them: with the routing end when the member's usage counts for the
// owner (thresholdsApply), else with the public end only -- Retry-After or the
// default, never a scraped window reset (spec section 9) -- so neither the cooldown
// list nor the member's FreeAt reveals the usage of an account the owner is not
// entitled to. Routing itself keeps honouring the routing end.
func (s *Service) memberVendorCooldowns(accountID string, thresholdsApply bool, now time.Time) []routing.VendorCooldown {
	if thresholdsApply {
		return s.vendorCooldowns.Active(accountID, now)
	}
	return s.vendorCooldowns.ActivePublic(accountID, now)
}

// applyVendorGroupBlocks sets a blocked member's state: over_threshold with the first
// threshold block's category, percent and the moment every block lifts, else cooldown
// ending when the last cooldown does.
func applyVendorGroupBlocks(member *VendorAccountGroupMemberDTO, blocks []routing.VendorMemberBlock) {
	if freeAt, known := routing.VendorGroupMemberFreeAt(blocks); known {
		member.FreeAt = &freeAt
	}
	first := blocks[0]
	if first.Category == routing.VendorBlockCooldown {
		member.State, member.Reason = VendorGroupStateCooldown, routing.VendorBlockCooldown
		return
	}
	pct := first.Pct
	member.State, member.Reason, member.Pct = VendorGroupStateOverThreshold, first.Category, &pct
}
