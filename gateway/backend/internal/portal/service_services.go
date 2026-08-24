// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
	"time"
)

// serviceLLMInvokeScope is the fixed, non-negotiable scope every Service
// Account token carries (Phase 1 §5.1 — never gateway:use/admin/system).
const serviceLLMInvokeScope = "llm:invoke"

// Admin-group linkage (service WRITE path, Phase C, spec 2026-08-10) --
// mirrors the server sentinels (ErrServerAdminGroup*, service.go) exactly,
// keyed on "service" instead of "server".
// ErrServiceAdminGroupRequired: the (post-dedup) admin_group_ids set is
// empty -- every service, regardless of the creating/editing principal's
// scope, must be linked to at least one admin-tier group.
// ErrServiceAdminGroupInvalid: an id does not resolve to an existing
// ADMIN-tier group, or (for a non-system principal) is not one the
// principal may manage services through (serviceManageGroupIDs).
// ErrServiceAdminGroupParentMismatch: the chosen groups do not all share
// ONE parent (system-tier) group, or (system-scope only) contradict an
// explicitly-supplied SystemGroupID cross-check.
var (
	ErrServiceAdminGroupRequired       = errors.New("service.admin_group_required")
	ErrServiceAdminGroupInvalid        = errors.New("service.admin_group_invalid")
	ErrServiceAdminGroupParentMismatch = errors.New("service.admin_group_parent_mismatch")
)

// ServiceDelegateDTO is one Service Account delegate (§6.5): UserName is a
// best-effort display lookup (never an existence guarantee — a delegate row
// referencing a since-deleted user id still round-trips with an empty name).
type ServiceDelegateDTO struct {
	UserID            string `json:"user_id"`
	UserName          string `json:"user_name"`
	CanManageSettings bool   `json:"can_manage_settings"`
}

// ServiceDelegateInput is the wire shape for a delegate entry on a
// create/update request.
type ServiceDelegateInput struct {
	UserID            string `json:"user_id"`
	CanManageSettings bool   `json:"can_manage_settings"`
}

// ServiceDTO is the read/list shape of a Service Account (§6.5). AllowedModels
// is always non-nil ([] = every model allowed, the default).
type ServiceDTO struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Description   string               `json:"description"`
	Status        string               `json:"status"`
	Delegates     []ServiceDelegateDTO `json:"delegates"`
	AllowedModels []string             `json:"allowed_models"`
	TokenCount    int                  `json:"token_count"`
	// Limits + LimitsUsage are Task 4's (Phase 2) rate/quota/budget config +
	// its current-period usage — see LimitConfigDTO/LimitUsageDTO. A service
	// with no limits configured reads back the zero value for both.
	Limits      LimitConfigDTO `json:"limits"`
	LimitsUsage LimitUsageDTO  `json:"limits_usage"`
	// AdminGroups/SystemGroupID/SystemGroupName are the admin-group
	// permissions Phase C linkage (spec 2026-08-10), mirroring ServerDTO's
	// AdminGroups/SystemGroupID/SystemGroupName exactly: AdminGroups is
	// always non-nil ([] when unlinked); SystemGroupID/SystemGroupName are
	// "" for an ungrouped (legacy/pre-Task-4) service.
	AdminGroups     []GroupRefDTO `json:"admin_groups"`
	SystemGroupID   string        `json:"system_group_id"`
	SystemGroupName string        `json:"system_group_name"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// CreateServiceRequest is CreateService's body. Status "" defaults to
// active (normalizeServiceStatus). Delegates/AllowedModels are validated
// (unknown user id / unknown gateway model -> ErrServiceValidation) before
// anything is persisted.
type CreateServiceRequest struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Status        string                 `json:"status,omitempty"`
	Delegates     []ServiceDelegateInput `json:"delegates"`
	AllowedModels []string               `json:"allowed_models"`
	// Limits is optional (design spec §7.1): nil = no limits configured (the
	// service reads back the zero LimitConfigDTO), a value validates + persists
	// immediately after creation.
	Limits *LimitConfigDTO `json:"limits,omitempty"`
	// AdminGroupIDs is the set of ADMIN-tier groups the new service is linked
	// to (service_admin_groups, migration v52) -- mandatory for EVERY caller,
	// including system-scope (Phase C, spec 2026-08-10, mirrors
	// CreateServerRequest.AdminGroupIDs). Every chosen group must share one
	// parent (system-tier) group, which becomes the service's SystemGroupID
	// containment root; see validateServiceAdminGroupIDs.
	AdminGroupIDs []string `json:"admin_group_ids"`
	// SystemGroupID is an optional system-admin convenience cross-check: when
	// set (system-scope only), every chosen AdminGroupIDs entry's parent must
	// equal it, or the create is rejected as a parent mismatch.
	SystemGroupID string `json:"system_group_id"`
}

// UpdateServiceRequest is UpdateService's body — pointer-based partial PATCH,
// mirroring UpdateServerRequest: nil = keep the current value, a supplied
// (possibly empty) slice REPLACES Delegates/AllowedModels wholesale.
type UpdateServiceRequest struct {
	Name          *string                 `json:"name,omitempty"`
	Description   *string                 `json:"description,omitempty"`
	Status        *string                 `json:"status,omitempty"`
	Delegates     *[]ServiceDelegateInput `json:"delegates,omitempty"`
	AllowedModels *[]string               `json:"allowed_models,omitempty"`
	// Limits is optional (design spec §7.1), pointer-based like every other
	// partial-PATCH field here: nil = keep the currently-stored limits
	// untouched, a (possibly all-zero) value validates + REPLACES them
	// wholesale via SetPrincipalLimits (an all-zero value clears every limit —
	// see the Task 4 zero-config-clears decision on portal.validateLimitConfig).
	Limits *LimitConfigDTO `json:"limits,omitempty"`
}

// ServiceTokenDTO is a Service Account token's read shape (§6.5) — mirrors
// TokenDTO but carries ServiceID instead of a user id, and NEVER the secret
// value (only SecretPrefix; guarded by TestServiceTokenDTOJSONNeverLeaksSecret).
type ServiceTokenDTO struct {
	ID            string     `json:"id"`
	ServiceID     string     `json:"service_id"`
	Name          string     `json:"name"`
	SecretPrefix  string     `json:"secret_prefix"`
	Status        string     `json:"status"`
	Scopes        []string   `json:"scopes"`
	ExpiresAt     *time.Time `json:"expires_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	CreatedAt     time.Time  `json:"created_at"`
	ModelOverride string     `json:"model_override"`
	// ModelOverrideMap rows carry the same object shape as TokenDTO's — see
	// there; a client that reads and writes a token back keeps both switches.
	ModelOverrideMap map[string]store.ModelOverrideRule `json:"model_override_map,omitempty"`
	LogCommunication bool                               `json:"log_communication"`
	Secret           bool                               `json:"secret"`
	// The unknown-model redirect, identical in meaning to TokenDTO's — the
	// setting belongs to the token, and a service token is a token. See there.
	// LastUsedModel is READ-ONLY here too: written by the inference path alone
	// (LookupBearer already carries all four onto a service token's auth.Token,
	// so the runtime half needs nothing from this DTO).
	LastUsedModel               string `json:"last_used_model,omitempty"`
	UnknownModelRedirect        bool   `json:"unknown_model_redirect,omitempty"`
	UnknownModelRedirectBlocked bool   `json:"unknown_model_redirect_blocked,omitempty"`
	UnknownModelFallback        string `json:"unknown_model_fallback,omitempty"`
}

// CreateServiceTokenRequest is CreateServiceToken's body (§6.3): scopes are
// NEVER accepted from the caller — they are always the fixed
// [serviceLLMInvokeScope].
type CreateServiceTokenRequest struct {
	Name             string                             `json:"name"`
	ExpiresAt        *time.Time                         `json:"expires_at"`
	ModelOverride    string                             `json:"model_override"`
	ModelOverrideMap map[string]store.ModelOverrideRule `json:"model_override_map"`
	LogCommunication bool                               `json:"log_communication"`
	Secret           bool                               `json:"secret"`
	// The unknown-model redirect settings — see ServiceTokenDTO. There is
	// deliberately no last_used_model: the marker is written by the inference
	// path only. Both sub-settings are ignored whenever UnknownModelRedirect is
	// false, and a non-empty fallback is validated in CreateServiceToken.
	UnknownModelRedirect        bool   `json:"unknown_model_redirect"`
	UnknownModelRedirectBlocked bool   `json:"unknown_model_redirect_blocked"`
	UnknownModelFallback        string `json:"unknown_model_fallback"`
}

// CreateServiceTokenResponse carries the plaintext secret ONCE (§6.5), exactly
// like CreateTokenResponse — never persisted, never present again.
type CreateServiceTokenResponse struct {
	Token  ServiceTokenDTO `json:"token"`
	Secret string          `json:"secret"`
}

// authorizeServiceRead loads the service and returns ErrServiceNotFound
// unless the principal is system-scoped, a delegate at EITHER stage (Token-
// or Full-Delegate), or a can_manage_services owner/co-manager of one of the
// service's linked admin groups (serviceManageGroupIDs) -- the *Read*
// object-gate (§6.1), re-scoped by admin-group permissions Phase C (spec
// 2026-08-10). This REPLACES the prior "any admin manages every service"
// global bypass (HasScope("admin")): a plain admin (no "system" scope) who
// is neither a delegate nor a can_manage_services manager of a linked group
// now gets the SAME ErrServiceNotFound (404-no-leak) as a stranger --
// mirroring authorizeServer's Phase B rewrite exactly. No existence leak: a
// stranger gets the identical error as an unknown id.
func (s *Service) authorizeServiceRead(ctx context.Context, principal auth.Token, id string) (routing.Service, error) {
	svc, err := s.routes.ServiceByID(ctx, id)
	if err != nil {
		return routing.Service{}, ErrServiceNotFound
	}
	if isSystem(principal) {
		return svc, nil
	}
	hasAny, _, err := s.serviceDelegateLevel(ctx, id, principal.UserID)
	if err != nil {
		return routing.Service{}, err
	}
	if hasAny {
		return svc, nil
	}
	groupIDs, err := s.routes.ServiceAdminGroups(ctx, id)
	if err != nil {
		return routing.Service{}, err
	}
	if len(groupIDs) > 0 {
		manageGroups, err := s.serviceManageGroupIDs(ctx, principal)
		if err != nil {
			return routing.Service{}, err
		}
		for _, gid := range groupIDs {
			if manageGroups[gid] {
				return svc, nil
			}
		}
	}
	return routing.Service{}, ErrServiceNotFound
}

// authorizeServiceTokens is the *Tokens* object-gate (§6.1): identical
// authorization to authorizeServiceRead (admin OR a delegate at either
// stage — a Token-Delegate manages tokens too) but named distinctly so
// call sites read as "this endpoint is at the Tokens level", matching the
// design's three-gate vocabulary (§7's per-route gate labels).
func (s *Service) authorizeServiceTokens(ctx context.Context, principal auth.Token, id string) (routing.Service, error) {
	return s.authorizeServiceRead(ctx, principal, id)
}

// authorizeServiceSettings is the *Settings* object-gate (§6.1):
// system-scoped, a FULL delegate (CanManageSettings==true), or a
// can_manage_services owner/co-manager of one of the service's linked admin
// groups — the group grant is FULL, equivalent to a Full-Delegate, mirroring
// authorizeServiceRead's group branch (Phase C, spec 2026-08-10). This
// REPLACES the prior "any admin manages every service" global bypass
// (HasScope("admin")). A Token-Delegate, or a co-manager of a linked group
// WITHOUT can_manage_services, gets the same 404 as a stranger.
func (s *Service) authorizeServiceSettings(ctx context.Context, principal auth.Token, id string) (routing.Service, error) {
	svc, err := s.routes.ServiceByID(ctx, id)
	if err != nil {
		return routing.Service{}, ErrServiceNotFound
	}
	if isSystem(principal) {
		return svc, nil
	}
	_, isFull, err := s.serviceDelegateLevel(ctx, id, principal.UserID)
	if err != nil {
		return routing.Service{}, err
	}
	if isFull {
		return svc, nil
	}
	groupIDs, err := s.routes.ServiceAdminGroups(ctx, id)
	if err != nil {
		return routing.Service{}, err
	}
	if len(groupIDs) > 0 {
		manageGroups, err := s.serviceManageGroupIDs(ctx, principal)
		if err != nil {
			return routing.Service{}, err
		}
		for _, gid := range groupIDs {
			if manageGroups[gid] {
				return svc, nil
			}
		}
	}
	return routing.Service{}, ErrServiceNotFound
}

// serviceDelegateLevel reports whether userID delegates serviceID at all
// (hasAny) and, if so, whether at the Full stage (isFull).
func (s *Service) serviceDelegateLevel(ctx context.Context, serviceID, userID string) (hasAny, isFull bool, err error) {
	delegates, err := s.routes.ServiceDelegates(ctx, serviceID)
	if err != nil {
		return false, false, err
	}
	for _, d := range delegates {
		if d.UserID == userID {
			return true, d.CanManageSettings, nil
		}
	}
	return false, false, nil
}

// CreateService creates a new Service Account. Authorization (Phase C, spec
// 2026-08-10, mirrors CreateServer's Phase B rewrite): allowed for a
// system-scope principal OR one who may manage services through at least
// one admin group (serviceManageGroupIDs); a principal with neither gets
// ErrServiceForbidden. This REPLACES the prior admin-scope-only gate -- a
// delegate can still never self-provision a service (delegation is granted
// AFTER creation, via Delegates). Every create -- regardless of scope --
// must additionally link the service to >=1 existing admin-tier group
// (req.AdminGroupIDs, validated by validateServiceAdminGroupIDs); a
// rejection there happens BEFORE the service row is created, so a rejected
// create never leaves an orphan.
func (s *Service) CreateService(ctx context.Context, principal auth.Token, req CreateServiceRequest) (ServiceDTO, error) {
	if !isSystem(principal) {
		manageGroups, err := s.serviceManageGroupIDs(ctx, principal)
		if err != nil {
			return ServiceDTO{}, err
		}
		if len(manageGroups) == 0 {
			return ServiceDTO{}, ErrServiceForbidden
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ServiceDTO{}, ErrServiceValidation
	}
	status, err := normalizeServiceStatus(req.Status)
	if err != nil {
		return ServiceDTO{}, err
	}
	delegates, err := s.validateServiceDelegates(ctx, req.Delegates)
	if err != nil {
		return ServiceDTO{}, err
	}
	models, err := s.validateServiceAllowedModels(ctx, principal, req.AllowedModels)
	if err != nil {
		return ServiceDTO{}, err
	}
	// Limits (Task 4, design spec §7.1) is optional: validated up front,
	// alongside every other request-shape check, BEFORE anything persists.
	var limitsCfg routing.LimitConfig
	var hasLimits bool
	if req.Limits != nil {
		limitsCfg, err = validateLimitConfig(*req.Limits)
		if err != nil {
			return ServiceDTO{}, err
		}
		hasLimits = true
	}
	// Admin-group linkage (Phase C, spec 2026-08-10): validated LAST among
	// the request-shape checks, still strictly BEFORE the service row is
	// created -- see the function doc.
	adminGroupIDs, systemGroupID, err := s.validateServiceAdminGroupIDs(ctx, principal, req.AdminGroupIDs, req.SystemGroupID)
	if err != nil {
		return ServiceDTO{}, err
	}
	now := s.clock().UTC()
	svc := routing.Service{
		ID:          "svc_" + compactRandomHex(16),
		Name:        name,
		Description: strings.TrimSpace(req.Description),
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.routes.CreateService(ctx, svc); err != nil {
		return ServiceDTO{}, err
	}
	if err := s.routes.SetServiceDelegates(ctx, svc.ID, delegates); err != nil {
		return ServiceDTO{}, err
	}
	if err := s.routes.SetServiceAllowedModels(ctx, svc.ID, models); err != nil {
		return ServiceDTO{}, err
	}
	if hasLimits {
		if err := s.routes.SetPrincipalLimits(ctx, routing.PrincipalTypeService, svc.ID, limitsCfg); err != nil {
			return ServiceDTO{}, err
		}
	}
	// Admin-group linkage persist (Phase C, spec 2026-08-10): AFTER the
	// service row exists (service_admin_groups.service_id is an FK), BEFORE
	// returning.
	if err := s.routes.UpdateServiceSystemGroup(ctx, svc.ID, systemGroupID); err != nil {
		return ServiceDTO{}, err
	}
	svc.SystemGroupID = systemGroupID
	for _, gid := range adminGroupIDs {
		if err := s.routes.SetServiceAdminGroup(ctx, svc.ID, gid); err != nil {
			return ServiceDTO{}, err
		}
	}
	return s.serviceDTO(ctx, svc)
}

// ListServices returns every service the principal may manage: system-scope
// sees ALL (unconditional bypass); anyone else sees the union of
// ServicesByDelegate(principal) and ServicesByAdminGroups(serviceManageGroupIDs),
// deduped by service id (first occurrence wins: delegate before group-linked)
// — re-scoped by admin-group permissions Phase C, mirroring ListServers'
// union (spec 2026-08-10). This REPLACES the prior "admin scope sees all"
// bypass (HasScope("admin")).
func (s *Service) ListServices(ctx context.Context, principal auth.Token) ([]ServiceDTO, error) {
	var services []routing.Service
	if isSystem(principal) {
		all, err := s.routes.Services(ctx)
		if err != nil {
			return nil, err
		}
		services = all
	} else {
		byDelegate, err := s.routes.ServicesByDelegate(ctx, principal.UserID)
		if err != nil {
			return nil, err
		}
		manageGroups, err := s.serviceManageGroupIDs(ctx, principal)
		if err != nil {
			return nil, err
		}
		groupIDs := make([]string, 0, len(manageGroups))
		for gid := range manageGroups {
			groupIDs = append(groupIDs, gid)
		}
		var byGroup []routing.Service
		if len(groupIDs) > 0 {
			byGroup, err = s.routes.ServicesByAdminGroups(ctx, groupIDs)
			if err != nil {
				return nil, err
			}
		}
		seen := make(map[string]bool, len(byDelegate)+len(byGroup))
		services = make([]routing.Service, 0, len(byDelegate)+len(byGroup))
		for _, list := range [][]routing.Service{byDelegate, byGroup} {
			for _, svc := range list {
				if seen[svc.ID] {
					continue
				}
				seen[svc.ID] = true
				services = append(services, svc)
			}
		}
	}
	out := make([]ServiceDTO, 0, len(services))
	for _, svc := range services {
		dto, err := s.serviceDTO(ctx, svc)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

// GetService is the *Read* object-gate over a single service.
func (s *Service) GetService(ctx context.Context, principal auth.Token, id string) (ServiceDTO, error) {
	svc, err := s.authorizeServiceRead(ctx, principal, id)
	if err != nil {
		return ServiceDTO{}, err
	}
	return s.serviceDTO(ctx, svc)
}

// UpdateService updates name/description/status/allowlist/delegate-list —
// the *Settings* object-gate (admin or a Full-Delegate; §6.2). Everything
// that can fail is validated BEFORE anything is persisted.
func (s *Service) UpdateService(ctx context.Context, principal auth.Token, id string, req UpdateServiceRequest) (ServiceDTO, error) {
	svc, err := s.authorizeServiceSettings(ctx, principal, id)
	if err != nil {
		return ServiceDTO{}, err
	}
	name := svc.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			return ServiceDTO{}, ErrServiceValidation
		}
	}
	status := svc.Status
	if req.Status != nil {
		status, err = normalizeServiceStatus(*req.Status)
		if err != nil {
			return ServiceDTO{}, err
		}
	}
	var delegates []routing.ServiceDelegate
	if req.Delegates != nil {
		delegates, err = s.validateServiceDelegates(ctx, *req.Delegates)
		if err != nil {
			return ServiceDTO{}, err
		}
	}
	var models []string
	if req.AllowedModels != nil {
		models, err = s.validateServiceAllowedModels(ctx, principal, *req.AllowedModels)
		if err != nil {
			return ServiceDTO{}, err
		}
	}
	// Limits (Task 4, design spec §7.1): nil = leave the stored config
	// untouched; a value (possibly all-zero, clearing every limit — see
	// validateLimitConfig's doc) is validated here, alongside every other
	// field, BEFORE anything persists.
	var limitsCfg routing.LimitConfig
	if req.Limits != nil {
		limitsCfg, err = validateLimitConfig(*req.Limits)
		if err != nil {
			return ServiceDTO{}, err
		}
	}
	svc.Name = name
	if req.Description != nil {
		svc.Description = strings.TrimSpace(*req.Description)
	}
	svc.Status = status
	svc.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateService(ctx, svc); err != nil {
		return ServiceDTO{}, err
	}
	if req.Delegates != nil {
		if err := s.routes.SetServiceDelegates(ctx, svc.ID, delegates); err != nil {
			return ServiceDTO{}, err
		}
	}
	if req.AllowedModels != nil {
		if err := s.routes.SetServiceAllowedModels(ctx, svc.ID, models); err != nil {
			return ServiceDTO{}, err
		}
	}
	if req.Limits != nil {
		if err := s.routes.SetPrincipalLimits(ctx, routing.PrincipalTypeService, svc.ID, limitsCfg); err != nil {
			return ServiceDTO{}, err
		}
	}
	// Best-effort mirror onto the memory-driver's live bearer cache (no-op for
	// SQLite/Postgres, whose LookupBearer already resolves this live — see
	// serviceTokenMirror). Read the CURRENT allowlist rather than trust
	// `models` so a status-only update (req.AllowedModels == nil) still
	// mirrors with the unchanged allowlist, not an empty one.
	s.mirrorServiceTokenState(ctx, svc.ID, svc.Status)
	return s.serviceDTO(ctx, svc)
}

// SetServiceAdminGroups replaces a service's linked admin-group set (Phase
// C, spec 2026-08-10 -- the linkage editor's write path, mirrors
// SetServerAdminGroups exactly). authorizeServiceSettings gates FIRST
// (404-no-leak: only a current Full-Delegate/can_manage_services
// owner-or-co-manager/system principal may see or edit the linkage at all --
// a Token-Delegate can't re-link either), THEN the new set is validated by
// the SAME rules CreateService uses (validateServiceAdminGroupIDs: each id
// existing ADMIN-tier +, for a non-system caller, in serviceManageGroupIDs;
// every chosen group sharing one parent; >=1 required). The delta vs the
// service's CURRENT admin groups is applied (SetServiceAdminGroup for
// additions, RemoveServiceAdminGroup for removals).
//
// Containment root is IMMUTABLE once set (mirrors the server-side guard,
// spec non-goal "Kein Reparenting der System-Gruppe eines Services ueber
// verschiedene Tenants"): for an ALREADY-grouped service
// (svc.SystemGroupID != "") the new set's derived common parent must equal
// the service's CURRENT root, or the call is rejected as
// ErrServiceAdminGroupParentMismatch -- checked EXPLICITLY below,
// independent of the caller's scope (NOT via validateServiceAdminGroupIDs's
// systemGroupHint parameter, which only applies its cross-check under
// system-scope -- a plain admin who happens to own/co-manage admin groups
// in two different tenants would otherwise be able to swap a grouped
// service's linked groups for ones under a DIFFERENT system group and
// thereby relocate its containment root; that is exactly the scenario this
// guard closes, for EVERY principal, including system).
// UpdateServiceSystemGroup therefore fires ONLY the very first time an
// UNGROUPED legacy service (SystemGroupID=="") gets its first link -- once
// set, the guard above holds it fixed on every later call.
func (s *Service) SetServiceAdminGroups(ctx context.Context, principal auth.Token, id string, groupIDs []string) (ServiceDTO, error) {
	svc, err := s.authorizeServiceSettings(ctx, principal, id)
	if err != nil {
		return ServiceDTO{}, err
	}
	ids, systemGroupID, err := s.validateServiceAdminGroupIDs(ctx, principal, groupIDs, "")
	if err != nil {
		return ServiceDTO{}, err
	}
	if svc.SystemGroupID != "" && systemGroupID != svc.SystemGroupID {
		return ServiceDTO{}, ErrServiceAdminGroupParentMismatch
	}
	current, err := s.routes.ServiceAdminGroups(ctx, id)
	if err != nil {
		return ServiceDTO{}, err
	}
	currentSet := make(map[string]bool, len(current))
	for _, gid := range current {
		currentSet[gid] = true
	}
	wantSet := make(map[string]bool, len(ids))
	for _, gid := range ids {
		wantSet[gid] = true
	}
	for _, gid := range ids {
		if !currentSet[gid] {
			if err := s.routes.SetServiceAdminGroup(ctx, id, gid); err != nil {
				return ServiceDTO{}, err
			}
		}
	}
	for _, gid := range current {
		if !wantSet[gid] {
			if err := s.routes.RemoveServiceAdminGroup(ctx, id, gid); err != nil {
				return ServiceDTO{}, err
			}
		}
	}
	// Reached ONLY on the first grouping of a previously-ungrouped service
	// (svc.SystemGroupID=="" here): the immutability guard above already
	// rejected any attempt to move an ALREADY-grouped service's root, so
	// this branch can no longer fire on a later call for the same service.
	if systemGroupID != svc.SystemGroupID {
		if err := s.routes.UpdateServiceSystemGroup(ctx, id, systemGroupID); err != nil {
			return ServiceDTO{}, err
		}
		svc.SystemGroupID = systemGroupID
	}
	return s.serviceDTO(ctx, svc)
}

// ServiceAdminGroupCandidates lists the admin-tier groups the caller may
// create/link a service into (drives the create-service / linkage-editor
// picker's auto-select-one / mandatory-choose-many / no-groups-hint logic;
// mirrors ServerAdminGroupCandidates exactly). A system-scope principal gets
// EVERY admin-tier group (may link into any of them, per
// validateServiceAdminGroupIDs); anyone else gets exactly the groups
// serviceManageGroupIDs returns (owner or can_manage_services co-manager).
func (s *Service) ServiceAdminGroupCandidates(ctx context.Context, principal auth.Token) ([]AdminGroupCandidateDTO, error) {
	var groups []store.UserGroup
	if isSystem(principal) {
		all, err := s.groups.ListUserGroupsByTier(ctx, store.GroupTierAdmin)
		if err != nil {
			return nil, err
		}
		groups = all
	} else {
		manageGroups, err := s.serviceManageGroupIDs(ctx, principal)
		if err != nil {
			return nil, err
		}
		for gid := range manageGroups {
			g, err := s.groups.UserGroupByID(ctx, gid)
			if err != nil {
				// linked group vanished between the enumeration and this
				// lookup; skip rather than fail the whole candidate list.
				continue
			}
			groups = append(groups, g)
		}
	}
	out := make([]AdminGroupCandidateDTO, 0, len(groups))
	for _, g := range groups {
		parentName := ""
		if g.ParentGroupID != "" {
			if parent, err := s.groups.UserGroupByID(ctx, g.ParentGroupID); err == nil {
				parentName = parent.Name
			}
		}
		out = append(out, AdminGroupCandidateDTO{
			ID: g.ID, Name: g.Name, ParentGroupID: g.ParentGroupID, ParentGroupName: parentName,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteService removes a service — the *Settings* object-gate. The
// underlying store cascades service_delegates/service_allowed_models/tokens
// (SQLite/Postgres via a DB FK; the memory driver via
// serviceTokenMirror.DeleteTokensByService, since it has no FK to rely on).
func (s *Service) DeleteService(ctx context.Context, principal auth.Token, id string) error {
	if _, err := s.authorizeServiceSettings(ctx, principal, id); err != nil {
		return err
	}
	if err := s.routes.DeleteService(ctx, id); err != nil {
		return err
	}
	s.mirrorServiceTokenDeletion(ctx, id)
	// Best-effort cleanup of any configured rate/quota/budget limits (design
	// spec §4: "DeleteService … entfernen die passende Zeile"). principal_limits
	// has no FK (it can reference either a Service or a User), so this is a
	// deliberate courtesy, not a correctness requirement — the service row is
	// already gone the moment the delete above succeeds, so its id can never
	// authenticate again and a leftover orphaned row (on a failed cleanup) is
	// permanently harmless.
	_ = s.routes.DeletePrincipalLimits(ctx, routing.PrincipalTypeService, id)
	return nil
}

// CreateServiceToken mints a new token for serviceID — the *Tokens*
// object-gate (admin or ANY delegate). Scopes are always exactly
// [serviceLLMInvokeScope]; kind is always "service"; there is no user_id.
// The plaintext secret is returned exactly once.
func (s *Service) CreateServiceToken(ctx context.Context, principal auth.Token, serviceID string, req CreateServiceTokenRequest) (CreateServiceTokenResponse, error) {
	svc, err := s.authorizeServiceTokens(ctx, principal, serviceID)
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return CreateServiceTokenResponse{}, ErrServiceValidation
	}
	// One offering lookup for the catch-all and every rule target (see
	// callableModelNames).
	callable := s.callableModelNames(ctx, principal)
	override, err := validateModelOverride(callable, req.ModelOverride)
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	overrideRules, err := validateModelOverrideRules(callable, req.ModelOverrideMap)
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	// The redirect's fallback is checked against the CREATING PRINCIPAL's
	// callable set, exactly like the override target above it — never against
	// the service's own allowlist. A token that does not exist yet has no
	// reachability of its own to compute, and the delegate picking the value is
	// the one whose model list the UI shows.
	//
	// That is safe because the check is a usability guard, not the security
	// boundary: the redirect's RESULT passes every admission gate at request
	// time — the service allowlist included — so a fallback the service may not
	// use is refused there, exactly as a directly requested model would be. It
	// can never widen what this token may reach.
	redirect, redirectBlocked, fallback, err := validateUnknownModelRedirect(
		callable, req.UnknownModelRedirect, req.UnknownModelRedirectBlocked, req.UnknownModelFallback)
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	now := s.clock().UTC()
	secret, err := s.secretGenerator()
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	scopesJSON, err := json.Marshal([]string{serviceLLMInvokeScope})
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	record := store.TokenRecord{
		ID:               s.idGenerator(),
		ServiceID:        svc.ID,
		Kind:             store.TokenKindService,
		Name:             name,
		Status:           store.TokenStatusActive,
		Scopes:           string(scopesJSON),
		ExpiresAt:        req.ExpiresAt,
		CreatedAt:        now,
		UpdatedAt:        now,
		ModelOverride:    override,
		ModelOverrideMap: store.EncodeModelOverrideRules(overrideRules),
		LogCommunication: req.LogCommunication,
		Secret:           req.Secret,
		// LastUsedModel is deliberately absent: a fresh token has never routed
		// anything, and only the inference path ever writes the marker.
		UnknownModelRedirect:        redirect,
		UnknownModelRedirectBlocked: redirectBlocked,
		UnknownModelFallback:        fallback,
	}
	if err := s.tokens.CreatePlainToken(ctx, record, secret); err != nil {
		return CreateServiceTokenResponse{}, err
	}
	record.SecretPrefix = secretPrefix(secret)
	// Best-effort backfill of the service's CURRENT disabled-state + allowlist
	// onto the freshly-minted token's memory-driver bearer cache (a no-op for
	// SQLite/Postgres — see mirrorServiceTokenState).
	s.mirrorServiceTokenState(ctx, svc.ID, svc.Status)
	return CreateServiceTokenResponse{Token: serviceTokenDTO(record), Secret: secret}, nil
}

// ListServiceTokens is the *Tokens* object-gate's read side.
func (s *Service) ListServiceTokens(ctx context.Context, principal auth.Token, serviceID string) ([]ServiceTokenDTO, error) {
	if _, err := s.authorizeServiceTokens(ctx, principal, serviceID); err != nil {
		return nil, err
	}
	records, err := s.tokens.TokensByService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceTokenDTO, 0, len(records))
	for _, record := range records {
		out = append(out, serviceTokenDTO(record))
	}
	return out, nil
}

// RotateServiceToken replaces tokenID's secret in place (same id/name/scopes/
// settings), returning the fresh plaintext secret once — mirrors RotateToken.
// tokenID MUST belong to serviceID (no cross-service access): a token of a
// DIFFERENT service (or an unknown id) is ErrTokenNotFound, matching
// RotateToken's own not-found sentinel for the analogous user-token case.
func (s *Service) RotateServiceToken(ctx context.Context, principal auth.Token, serviceID, tokenID string) (CreateServiceTokenResponse, error) {
	if _, err := s.authorizeServiceTokens(ctx, principal, serviceID); err != nil {
		return CreateServiceTokenResponse{}, err
	}
	record, err := s.serviceTokenByID(ctx, serviceID, tokenID)
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	secret, err := s.secretGenerator()
	if err != nil {
		return CreateServiceTokenResponse{}, err
	}
	now := s.clock().UTC()
	if err := s.tokens.RotateTokenSecret(ctx, record.ID, auth.HashSecret(secret), secretPrefix(secret), now); err != nil {
		return CreateServiceTokenResponse{}, err
	}
	record.SecretPrefix = secretPrefix(secret)
	record.UpdatedAt = now
	return CreateServiceTokenResponse{Token: serviceTokenDTO(record), Secret: secret}, nil
}

// DeleteServiceToken removes tokenID, which MUST belong to serviceID (no
// cross-service access — see serviceTokenByID).
func (s *Service) DeleteServiceToken(ctx context.Context, principal auth.Token, serviceID, tokenID string) error {
	if _, err := s.authorizeServiceTokens(ctx, principal, serviceID); err != nil {
		return err
	}
	record, err := s.serviceTokenByID(ctx, serviceID, tokenID)
	if err != nil {
		return err
	}
	return s.tokens.DeleteToken(ctx, record.ID)
}

// serviceTokenByID resolves tokenID and verifies it is a service-kind token
// belonging to EXACTLY serviceID — the cross-service-access guard shared by
// RotateServiceToken/DeleteServiceToken. Reuses ErrTokenNotFound (the
// existing, already HTTP-mapped 404 sentinel for "no such token") rather than
// introducing a new one: a service-scoped 404 for a token that either does
// not exist, is not a service token, or belongs to ANOTHER service are all
// indistinguishable to the caller (no existence/ownership leak).
func (s *Service) serviceTokenByID(ctx context.Context, serviceID, tokenID string) (store.TokenRecord, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return store.TokenRecord{}, ErrTokenNotFound
	}
	record, err := s.tokens.TokenByID(ctx, tokenID)
	if err != nil || record.Kind != store.TokenKindService || record.ServiceID != serviceID {
		return store.TokenRecord{}, ErrTokenNotFound
	}
	return record, nil
}

// validateServiceDelegates trims + validates a delegate list: every user id
// must exist (ErrServiceValidation otherwise) and no user id may repeat
// within the same request (a repeat is rejected rather than silently
// deduped/overwritten, so the caller is never left guessing which
// CanManageSettings value "won").
func (s *Service) validateServiceDelegates(ctx context.Context, raw []ServiceDelegateInput) ([]routing.ServiceDelegate, error) {
	out := make([]routing.ServiceDelegate, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, d := range raw {
		userID := strings.TrimSpace(d.UserID)
		if userID == "" {
			return nil, ErrServiceValidation
		}
		if _, dup := seen[userID]; dup {
			return nil, ErrServiceValidation
		}
		if _, err := s.users.UserByID(ctx, userID); err != nil {
			return nil, ErrServiceValidation
		}
		seen[userID] = struct{}{}
		out = append(out, routing.ServiceDelegate{UserID: userID, CanManageSettings: d.CanManageSettings})
	}
	return out, nil
}

// validateServiceAllowedModels trims + validates a model allowlist: every
// non-blank entry must be a currently-active gateway model as the LISTING sees
// it. This deliberately stays on Models() rather than following the override
// targets onto callableModelNames: an allowlist is an admin picking from what
// the management UI shows, not a name the gateway routes to on a client's
// behalf. Blank entries are dropped,
// duplicates de-duped. An empty/all-blank input returns nil (the "every model
// allowed" default, unchanged).
func (s *Service) validateServiceAllowedModels(ctx context.Context, principal auth.Token, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	known := make(map[string]struct{})
	for _, m := range s.Models(ctx, principal).Data {
		known[m.ID] = struct{}{}
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		if _, ok := known[name]; !ok {
			return nil, ErrServiceValidation
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// normalizeServiceStatus mirrors normalizeServerStatus but only ever accepts
// the two-value Service vocabulary (§4.1 — no "maintenance").
func normalizeServiceStatus(raw string) (string, error) {
	status := strings.TrimSpace(raw)
	if status == "" {
		return routing.ServerStatusActive, nil
	}
	switch status {
	case routing.ServerStatusActive, routing.ServerStatusDisabled:
		return status, nil
	default:
		return "", ErrServiceValidation
	}
}

// validateServiceAdminGroupIDs validates the admin-group set a service is
// (or is being re-)linked into -- shared by CreateService and
// SetServiceAdminGroups (Phase C, spec 2026-08-10 -- mirrors
// validateAdminGroupIDs, service.go, exactly, keyed on "service" instead of
// "server"). rawIDs is trimmed + deduped first; an empty result is
// ErrServiceAdminGroupRequired (every service needs >=1 admin group,
// regardless of the caller's scope). Each remaining id must resolve to an
// EXISTING ADMIN-tier group (else ErrServiceAdminGroupInvalid); for a
// non-system principal, each must ALSO be one they may manage services
// through (serviceManageGroupIDs -- a system-scope principal skips this
// check and may link into ANY admin-tier group). Every chosen group must
// share exactly ONE ParentGroupID -- the service's containment root -- or
// the call is rejected as ErrServiceAdminGroupParentMismatch; when the
// caller is system-scope and supplied a non-empty systemGroupHint (a
// convenience cross-check, CreateServiceRequest.SystemGroupID), that
// resolved root must equal it too. Returns the deduped ids (order
// preserved) and the resolved systemGroupID.
func (s *Service) validateServiceAdminGroupIDs(ctx context.Context, principal auth.Token, rawIDs []string, systemGroupHint string) ([]string, string, error) {
	return s.validateAdminGroupScope(ctx, principal, rawIDs, systemGroupHint, s.serviceManageGroupIDs, adminGroupSentinels{
		Required:       ErrServiceAdminGroupRequired,
		Invalid:        ErrServiceAdminGroupInvalid,
		ParentMismatch: ErrServiceAdminGroupParentMismatch,
	})
}

// serviceDTO assembles the full read DTO for svc: delegates (with a
// best-effort display-name lookup), the current allowlist, and the token
// count (0 when s.tokens is nil, e.g. a Routes-only test fixture).
func (s *Service) serviceDTO(ctx context.Context, svc routing.Service) (ServiceDTO, error) {
	delegates, err := s.routes.ServiceDelegates(ctx, svc.ID)
	if err != nil {
		return ServiceDTO{}, err
	}
	delegateDTOs := make([]ServiceDelegateDTO, 0, len(delegates))
	for _, d := range delegates {
		name := ""
		if s.users != nil {
			if u, err := s.users.UserByID(ctx, d.UserID); err == nil {
				name = u.DisplayName
			}
		}
		delegateDTOs = append(delegateDTOs, ServiceDelegateDTO{UserID: d.UserID, UserName: name, CanManageSettings: d.CanManageSettings})
	}
	models, err := s.routes.ServiceAllowedModels(ctx, svc.ID)
	if err != nil {
		return ServiceDTO{}, err
	}
	tokenCount := 0
	if s.tokens != nil {
		tokens, err := s.tokens.TokensByService(ctx, svc.ID)
		if err != nil {
			return ServiceDTO{}, err
		}
		tokenCount = len(tokens)
	}
	limitsCfg, err := principalLimits(ctx, s.routes, routing.PrincipalTypeService, svc.ID)
	if err != nil {
		return ServiceDTO{}, err
	}
	groupIDs, err := s.routes.ServiceAdminGroups(ctx, svc.ID)
	if err != nil {
		return ServiceDTO{}, err
	}
	adminGroups := make([]GroupRefDTO, 0, len(groupIDs))
	for _, gid := range groupIDs {
		if s.groups == nil {
			break
		}
		g, err := s.groups.UserGroupByID(ctx, gid)
		if err != nil {
			// linked group vanished; skip rather than fail the whole DTO
			continue
		}
		adminGroups = append(adminGroups, GroupRefDTO{ID: g.ID, Name: g.Name})
	}
	systemGroupName := ""
	if svc.SystemGroupID != "" && s.groups != nil {
		if g, err := s.groups.UserGroupByID(ctx, svc.SystemGroupID); err == nil {
			systemGroupName = g.Name
		}
	}
	return ServiceDTO{
		ID:              svc.ID,
		Name:            svc.Name,
		Description:     svc.Description,
		Status:          svc.Status,
		Delegates:       delegateDTOs,
		AllowedModels:   models,
		TokenCount:      tokenCount,
		Limits:          limitConfigDTO(limitsCfg),
		LimitsUsage:     limitUsage(ctx, s.routes, routing.PrincipalTypeService, svc.ID, limitsCfg, s.clock().UTC()),
		AdminGroups:     adminGroups,
		SystemGroupID:   svc.SystemGroupID,
		SystemGroupName: systemGroupName,
		CreatedAt:       svc.CreatedAt,
		UpdatedAt:       svc.UpdatedAt,
	}, nil
}

// serviceTokenDTO maps a store.TokenRecord (kind="service") onto its DTO —
// NEVER carries the secret value, only SecretPrefix.
func serviceTokenDTO(record store.TokenRecord) ServiceTokenDTO {
	return ServiceTokenDTO{
		ID:                          record.ID,
		ServiceID:                   record.ServiceID,
		Name:                        record.Name,
		SecretPrefix:                record.SecretPrefix,
		Status:                      record.Status,
		Scopes:                      parseScopes(record.Scopes),
		ExpiresAt:                   record.ExpiresAt,
		LastUsedAt:                  record.LastUsedAt,
		CreatedAt:                   record.CreatedAt,
		ModelOverride:               record.ModelOverride,
		ModelOverrideMap:            store.DecodeModelOverrideRules(record.ModelOverrideMap),
		LogCommunication:            record.LogCommunication,
		Secret:                      record.Secret,
		LastUsedModel:               record.LastUsedModel,
		UnknownModelRedirect:        record.UnknownModelRedirect,
		UnknownModelRedirectBlocked: record.UnknownModelRedirectBlocked,
		UnknownModelFallback:        record.UnknownModelFallback,
	}
}

// serviceTokenMirror is implemented ONLY by *MemoryDirectory. SQLite/Postgres
// resolve a service token's live disabled-state + allowlist on every lookup
// (LookupBearer joins services/service_allowed_models — see store's
// sqlite_token.go) and need no push; the memory driver's bearer store
// (*auth.TokenStore, a flat map with no join capability) instead needs the
// portal layer to PUSH the current state onto every already-minted token
// whenever it changes (status toggle, allowlist edit, a fresh mint) or is
// removed (service delete, which has no DB-level FK to rely on in memory).
type serviceTokenMirror interface {
	// SetServiceTokensState pushes disabled + allowedModels onto every
	// existing token of serviceID's live bearer cache entry.
	SetServiceTokensState(ctx context.Context, serviceID string, disabled bool, allowedModels []string) error
	// DeleteTokensByService removes every token of serviceID from both the
	// token directory and the live bearer cache.
	DeleteTokensByService(ctx context.Context, serviceID string) error
}

// mirrorServiceTokenState is s.tokens' serviceTokenMirror push, called after
// a service's status and/or allowlist persists (best-effort, never surfaced —
// see serviceTokenMirror). It re-reads the CURRENT allowlist from s.routes
// (rather than trusting a caller-supplied value) so it stays correct
// regardless of which field actually changed. A nil/non-implementing s.tokens
// (SQLite/Postgres, or a Routes-only test fixture) is a correct no-op — the
// ServiceAllowedModels read is skipped entirely in that case.
func (s *Service) mirrorServiceTokenState(ctx context.Context, serviceID, status string) {
	mirror, ok := s.tokens.(serviceTokenMirror)
	if !ok {
		return
	}
	models, err := s.routes.ServiceAllowedModels(ctx, serviceID)
	if err != nil {
		return
	}
	_ = mirror.SetServiceTokensState(ctx, serviceID, status == routing.ServerStatusDisabled, models)
}

// mirrorServiceTokenDeletion is s.tokens' serviceTokenMirror cleanup, called
// after a service row is deleted (best-effort, never surfaced).
func (s *Service) mirrorServiceTokenDeletion(ctx context.Context, serviceID string) {
	mirror, ok := s.tokens.(serviceTokenMirror)
	if !ok {
		return
	}
	_ = mirror.DeleteTokensByService(ctx, serviceID)
}
