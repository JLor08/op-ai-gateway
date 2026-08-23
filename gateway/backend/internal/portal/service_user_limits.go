// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
)

// UserLimitsDTO is the GET/PUT response for a user's rate/quota/budget limits
// (design spec §7.2): Limits is the stored config, Usage is the CURRENT
// calendar-period usage against it (read-only, see LimitUsageDTO).
type UserLimitsDTO struct {
	Limits LimitConfigDTO `json:"limits"`
	Usage  LimitUsageDTO  `json:"usage"`
}

// UserLimits reads userID's rate/quota/budget limits and current-period usage
// (design spec §7.2 — "Benutzerverwaltung"). Admin-only: enforced at the HTTP
// layer (requireWebScope(..., "admin")), NOT here — there is deliberately NO
// self-service path at all, so this method carries no auth.Token parameter
// and performs no scope check of its own; a normal/gateway:use-only
// principal simply never reaches it, because the one route that calls it
// always requires the admin scope, regardless of whose id is in the path.
//
// ErrLimitUserNotFound when userID is blank or does not resolve to a real
// user.
func (s *Service) UserLimits(ctx context.Context, userID string) (UserLimitsDTO, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return UserLimitsDTO{}, ErrLimitUserNotFound
	}
	if _, err := s.users.UserByID(ctx, userID); err != nil {
		return UserLimitsDTO{}, ErrLimitUserNotFound
	}
	cfg, err := principalLimits(ctx, s.routes, routing.PrincipalTypeUser, userID)
	if err != nil {
		return UserLimitsDTO{}, err
	}
	return UserLimitsDTO{
		Limits: limitConfigDTO(cfg),
		Usage:  limitUsage(ctx, s.routes, routing.PrincipalTypeUser, userID, cfg, s.clock().UTC()),
	}, nil
}

// SetUserLimits validates and persists userID's rate/quota/budget limits
// (design spec §7.2), returning the same shape UserLimits does. Same
// admin-only requirement as UserLimits (no self-service — see its doc), but
// unlike UserLimits this is a MUTATING call, so — PT-2 Part 2 — it also
// checks isAdmin(principal) itself (ErrPrincipalForbidden otherwise) as
// defense-in-depth behind the HTTP-layer requireWebScope("admin") gate,
// which is unchanged and still the only thing a normal request ever hits.
// A fully-zero req is valid: it clears every limit (SetPrincipalLimits is
// called unconditionally with the validated config, zero or not — a stored
// all-zero row is behaviorally identical to no row at all, since a zero
// routing.LimitConfig is already documented as a full admission no-op).
func (s *Service) SetUserLimits(ctx context.Context, principal auth.Token, userID string, req LimitConfigDTO) (UserLimitsDTO, error) {
	if !isAdmin(principal) {
		return UserLimitsDTO{}, ErrPrincipalForbidden
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return UserLimitsDTO{}, ErrLimitUserNotFound
	}
	if _, err := s.users.UserByID(ctx, userID); err != nil {
		return UserLimitsDTO{}, ErrLimitUserNotFound
	}
	cfg, err := validateLimitConfig(req)
	if err != nil {
		return UserLimitsDTO{}, err
	}
	if err := s.routes.SetPrincipalLimits(ctx, routing.PrincipalTypeUser, userID, cfg); err != nil {
		return UserLimitsDTO{}, err
	}
	return UserLimitsDTO{
		Limits: limitConfigDTO(cfg),
		Usage:  limitUsage(ctx, s.routes, routing.PrincipalTypeUser, userID, cfg, s.clock().UTC()),
	}, nil
}
