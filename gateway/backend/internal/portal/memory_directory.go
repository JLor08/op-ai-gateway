// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
	"sync"
	"time"
)

type MemoryDirectory struct {
	mu                sync.RWMutex
	auth              *auth.TokenStore
	users             map[string]store.User
	tokens            map[string]store.TokenRecord
	sessions          map[string]store.Session
	setPasswordTokens map[string]store.SetPasswordToken
	// groups/members/managers back the GroupStore interface (see
	// group_store.go) for the memory driver. members and managers are keyed
	// groupID -> userID so a re-set is a natural upsert (member state) /
	// no-op (manager, mirrors the SQL on-conflict-do-nothing). managers'
	// value carries the co-manager's permission flags (per-Admin-Group
	// co-manager permissions, spec 2026-08-10) + its insertion time, so
	// UserGroupManagerPerms can mirror the SQL "order by created_at, user_id".
	groups   map[string]store.UserGroup
	members  map[string]map[string]store.UserGroupMembership
	managers map[string]map[string]userGroupManagerEntry
	// projects/projectMembers/projectGroups back the ProjectStore interface
	// (see project_store.go). projectMembers/projectGroups are keyed
	// projectID -> memberID -> the time it was added (used only for a
	// stable, insertion-ordered listing — mirrors the SQL `order by
	// created_at, <id>`), so a re-set is a natural upsert (first-seen time
	// kept, matching the SQL on-conflict-do-nothing).
	projects       map[string]store.Project
	projectMembers map[string]map[string]time.Time
	projectGroups  map[string]map[string]time.Time
}

func NewMemoryDirectory(authStore *auth.TokenStore) *MemoryDirectory {
	return &MemoryDirectory{
		auth:              authStore,
		users:             map[string]store.User{},
		tokens:            map[string]store.TokenRecord{},
		sessions:          map[string]store.Session{},
		setPasswordTokens: map[string]store.SetPasswordToken{},
		groups:            map[string]store.UserGroup{},
		members:           map[string]map[string]store.UserGroupMembership{},
		managers:          map[string]map[string]userGroupManagerEntry{},
		projects:          map[string]store.Project{},
		projectMembers:    map[string]map[string]time.Time{},
		projectGroups:     map[string]map[string]time.Time{},
	}
}

func (m *MemoryDirectory) AddUser(user store.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[user.ID] = user
}

func (m *MemoryDirectory) UserByID(ctx context.Context, id string) (store.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	user, ok := m.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return user, nil
}

func (m *MemoryDirectory) TokensByUser(ctx context.Context, userID string) ([]store.TokenRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.TokenRecord, 0)
	for _, token := range m.tokens {
		if token.UserID == userID {
			out = append(out, token)
		}
	}
	sortTokenRecords(out)
	return out, nil
}

func (m *MemoryDirectory) CreatePlainToken(ctx context.Context, token store.TokenRecord, secret string) error {
	if token.Status == "" {
		token.Status = store.TokenStatusActive
	}
	if token.Scopes == "" {
		token.Scopes = "[]"
	}
	// Kind defaults to "user" (mirrors SQLiteStore.CreatePlainToken) so a
	// caller that never sets it (every pre-Service-Accounts call site) keeps
	// token.IsService()==false, unchanged.
	if token.Kind == "" {
		token.Kind = store.TokenKindUser
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}
	if token.UpdatedAt.IsZero() {
		token.UpdatedAt = token.CreatedAt
	}
	token.SecretHash = auth.HashSecret(secret)
	token.SecretPrefix = secretPrefix(secret)

	m.mu.Lock()
	defer m.mu.Unlock()
	// Uniqueness parity with SQLiteStore.CreatePlainToken, whose insert hits the
	// api_tokens PRIMARY KEY (id) and the secret_hash UNIQUE index and maps both
	// to store.ErrConflict. Without these checks the memory driver would silently
	// overwrite an existing token id / accept a colliding secret — a divergence
	// the store conformance harness (TestTokenRepositoryCreatePlainTokenUniqueness)
	// pins. Mirrors CreateUser's id+email pattern above.
	if _, exists := m.tokens[token.ID]; exists {
		return store.ErrConflict
	}
	for _, existing := range m.tokens {
		if existing.SecretHash == token.SecretHash {
			return store.ErrConflict
		}
	}
	m.tokens[token.ID] = token
	m.auth.AddPlainToken(auth.Token{
		ID:               token.ID,
		UserID:           token.UserID,
		Name:             token.Name,
		Active:           token.Status == store.TokenStatusActive,
		Scopes:           recordScopes(token.Scopes),
		ExpiresAt:        token.ExpiresAt,
		ModelOverride:    token.ModelOverride,
		ModelOverrideMap: store.DecodeModelOverrideMap(token.ModelOverrideMap),
		LogCommunication: token.LogCommunication,
		Secret:           token.Secret,
		// ServiceID/Kind mint a Service Account token (Phase 1) as such under
		// the memory driver so token.IsService() works; AllowedModels starts
		// empty (unrestricted) here and is best-effort backfilled right after
		// by the portal layer's mirrorServiceTokenState (see
		// service_services.go's serviceTokenMirror) once the service's
		// CURRENT allowlist is known — SetServiceTokensState below is what
		// actually fills it in.
		ServiceID:                      token.ServiceID,
		Kind:                           token.Kind,
		ProjectID:                      token.ProjectID,
		ProjectName:                    m.projectName(token.ProjectID),
		ServerOverride:                 token.ServerOverride,
		ServerOverrideForceUnreachable: token.ServerOverrideForceUnreachable,
	}, secret)
	return nil
}

// projectName resolves projectID's display name from the in-memory project
// directory (best-effort — empty when projectID is empty or unknown), mirroring
// how ServiceName is resolved for a service token. Callers must already hold
// m.mu (this reads m.projects directly, without locking, so it is safe to call
// from within a method that holds the lock).
func (m *MemoryDirectory) projectName(projectID string) string {
	if projectID == "" {
		return ""
	}
	if p, ok := m.projects[projectID]; ok {
		return p.Name
	}
	return ""
}

// SetServiceTokensState mirrors a Service Account's live disabled-state +
// model allowlist onto every one of its already-minted tokens' cached bearer
// entry (auth.TokenStore has no per-request join like SQLiteStore.LookupBearer
// does over services/service_allowed_models — see the serviceTokenMirror
// interface in internal/portal/service_services.go, which this satisfies).
// disabled forces every matching token's Active flag off regardless of its
// own Status; allowedModels REPLACES the cached slice (nil/empty = every
// model allowed). A serviceID with no tokens is a no-op, not an error.
func (m *MemoryDirectory) SetServiceTokensState(ctx context.Context, serviceID string, disabled bool, allowedModels []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, token := range m.tokens {
		if token.Kind != store.TokenKindService || token.ServiceID != serviceID {
			continue
		}
		m.auth.UpdateToken(auth.Token{
			ID:                             token.ID,
			UserID:                         token.UserID,
			Name:                           token.Name,
			Active:                         !disabled && token.Status == store.TokenStatusActive,
			Scopes:                         recordScopes(token.Scopes),
			ExpiresAt:                      token.ExpiresAt,
			ModelOverride:                  token.ModelOverride,
			ModelOverrideMap:               store.DecodeModelOverrideMap(token.ModelOverrideMap),
			LogCommunication:               token.LogCommunication,
			Secret:                         token.Secret,
			ServiceID:                      token.ServiceID,
			Kind:                           token.Kind,
			AllowedModels:                  allowedModels,
			ProjectID:                      token.ProjectID,
			ProjectName:                    m.projectName(token.ProjectID),
			ServerOverride:                 token.ServerOverride,
			ServerOverrideForceUnreachable: token.ServerOverrideForceUnreachable,
		})
	}
	return nil
}

// DeleteTokensByService removes every token belonging to serviceID from both
// the token directory and the live bearer cache — mirroring the DB-level
// `api_tokens.service_id references services(id) on delete cascade` for the
// memory driver, which has no such FK. A serviceID with no tokens is a no-op.
func (m *MemoryDirectory) DeleteTokensByService(ctx context.Context, serviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, token := range m.tokens {
		if token.Kind != store.TokenKindService || token.ServiceID != serviceID {
			continue
		}
		delete(m.tokens, id)
		m.auth.RemoveToken(id)
	}
	return nil
}

// TokensByService lists a service's tokens (kind="service"), newest first —
// mirrors TokensByUser/SQLiteStore.TokensByService. A service with no tokens
// returns an empty (non-nil) slice, no error.
func (m *MemoryDirectory) TokensByService(ctx context.Context, serviceID string) ([]store.TokenRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.TokenRecord, 0)
	for _, token := range m.tokens {
		if token.Kind == store.TokenKindService && token.ServiceID == serviceID {
			out = append(out, token)
		}
	}
	sortTokenRecords(out)
	return out, nil
}

// TokensByProject lists a project's assigned tokens, newest first -- mirrors
// TokensByService/SQLiteStore.TokensByProject. An empty projectID returns an
// empty slice (no token is ever assigned to "the project with id \"\"").
func (m *MemoryDirectory) TokensByProject(ctx context.Context, projectID string) ([]store.TokenRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.TokenRecord, 0)
	if projectID == "" {
		return out, nil
	}
	for _, token := range m.tokens {
		if token.ProjectID == projectID {
			out = append(out, token)
		}
	}
	sortTokenRecords(out)
	return out, nil
}

func (m *MemoryDirectory) UpdateTokenMetadata(ctx context.Context, token store.TokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.tokens[token.ID]
	if !ok {
		return store.ErrNotFound
	}
	existing.Name = token.Name
	existing.Scopes = token.Scopes
	existing.Status = token.Status
	existing.UpdatedAt = token.UpdatedAt
	existing.ModelOverride = token.ModelOverride
	existing.ModelOverrideMap = token.ModelOverrideMap
	existing.LogCommunication = token.LogCommunication
	existing.Secret = token.Secret
	existing.ProjectID = token.ProjectID
	existing.ServerOverride = token.ServerOverride
	existing.ServerOverrideForceUnreachable = token.ServerOverrideForceUnreachable
	m.tokens[token.ID] = existing
	m.auth.UpdateToken(auth.Token{
		ID:               existing.ID,
		UserID:           existing.UserID,
		Name:             existing.Name,
		Active:           existing.Status == store.TokenStatusActive,
		Scopes:           recordScopes(existing.Scopes),
		ExpiresAt:        existing.ExpiresAt,
		ModelOverride:    existing.ModelOverride,
		ModelOverrideMap: store.DecodeModelOverrideMap(existing.ModelOverrideMap),
		LogCommunication: existing.LogCommunication,
		Secret:           existing.Secret,
		// ServiceID/Kind never change after creation (see store.TokenRecord's
		// doc comment) but must be carried forward here regardless: UpdateToken
		// REPLACES the cached auth.Token wholesale, so omitting them would
		// silently downgrade an existing service token back to Kind=="" on its
		// very next metadata edit.
		ServiceID:                      existing.ServiceID,
		Kind:                           existing.Kind,
		ProjectID:                      existing.ProjectID,
		ProjectName:                    m.projectName(existing.ProjectID),
		ServerOverride:                 existing.ServerOverride,
		ServerOverrideForceUnreachable: existing.ServerOverrideForceUnreachable,
	})
	return nil
}

func (m *MemoryDirectory) RotateTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	existing.SecretHash = secretHash
	existing.SecretPrefix = secretPrefix
	existing.UpdatedAt = updatedAt
	m.tokens[id] = existing
	m.auth.RekeyToken(id, secretHash)
	return nil
}

func (m *MemoryDirectory) DeleteToken(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.tokens, id)
	m.auth.RemoveToken(id)
	return nil
}

func recordScopes(scopesJSON string) []string {
	var scopes []string
	if err := json.Unmarshal([]byte(scopesJSON), &scopes); err != nil {
		return nil
	}
	return append([]string(nil), scopes...)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (m *MemoryDirectory) CreateUser(ctx context.Context, user store.User) error {
	user.Email = normalizeEmail(user.Email)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[user.ID]; exists {
		return store.ErrConflict
	}
	for _, existing := range m.users {
		if existing.Email == user.Email {
			return store.ErrConflict
		}
	}
	m.users[user.ID] = user
	return nil
}

func (m *MemoryDirectory) UserByEmail(ctx context.Context, email string) (store.User, error) {
	target := normalizeEmail(email)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, user := range m.users {
		if user.Email == target {
			return user, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

func (m *MemoryDirectory) UpdateUser(ctx context.Context, user store.User) error {
	user.Email = normalizeEmail(user.Email)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[user.ID]; !exists {
		return store.ErrNotFound
	}
	for id, existing := range m.users {
		if id != user.ID && existing.Email == user.Email {
			return store.ErrConflict
		}
	}
	m.users[user.ID] = user
	return nil
}

func (m *MemoryDirectory) ListUsers(ctx context.Context) ([]store.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.User, 0, len(m.users))
	for _, user := range m.users {
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *MemoryDirectory) TokenByID(ctx context.Context, id string) (store.TokenRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	token, ok := m.tokens[id]
	if !ok {
		return store.TokenRecord{}, store.ErrNotFound
	}
	return token, nil
}

func (m *MemoryDirectory) CreateSession(ctx context.Context, session store.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[session.ID]; exists {
		return store.ErrConflict
	}
	m.sessions[session.ID] = session
	return nil
}

func (m *MemoryDirectory) SessionBySecret(ctx context.Context, secretHash string) (store.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, session := range m.sessions {
		if session.SecretHash == secretHash {
			return session, nil
		}
	}
	return store.Session{}, store.ErrNotFound
}

func (m *MemoryDirectory) TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok {
		return store.ErrNotFound
	}
	session.LastSeenAt = lastSeenAt
	m.sessions[id] = session
	return nil
}

func (m *MemoryDirectory) SetSessionElevation(ctx context.Context, id string, until time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return store.ErrNotFound
	}
	sess.ElevatedUntil = until
	m.sessions[id] = sess
	return nil
}

func (m *MemoryDirectory) DeleteSession(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *MemoryDirectory) DeleteSessionsByUser(ctx context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, session := range m.sessions {
		if session.UserID == userID {
			delete(m.sessions, id)
		}
	}
	return nil
}

func (m *MemoryDirectory) CreateSetPasswordToken(ctx context.Context, tok store.SetPasswordToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.setPasswordTokens[tok.ID]; exists {
		return store.ErrConflict
	}
	m.setPasswordTokens[tok.ID] = tok
	return nil
}

func (m *MemoryDirectory) SetPasswordTokenBySecret(ctx context.Context, secretHash string) (store.SetPasswordToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, tok := range m.setPasswordTokens {
		if tok.SecretHash == secretHash {
			return tok, nil
		}
	}
	return store.SetPasswordToken{}, store.ErrNotFound
}

func (m *MemoryDirectory) MarkSetPasswordTokenUsed(ctx context.Context, id string, usedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.setPasswordTokens[id]
	if !ok {
		return store.ErrNotFound
	}
	tok.UsedAt = &usedAt
	m.setPasswordTokens[id] = tok
	return nil
}

func (m *MemoryDirectory) InvalidateUserSetPasswordTokens(ctx context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, tok := range m.setPasswordTokens {
		if tok.UserID == userID && tok.UsedAt == nil {
			used := tok.CreatedAt
			tok.UsedAt = &used
			m.setPasswordTokens[id] = tok
		}
	}
	return nil
}

// --- GroupStore (see group_store.go) --------------------------------------
//
// The memory driver enforces NO foreign-key-style referential checks on
// parent_group_id/owner_user_id/group_id (consistent with every other
// MemoryDirectory Create* method, which only checks id/email uniqueness) —
// unlike SQLStore, which surfaces a dangling reference as ErrNotFound via a
// real FK violation. DeleteUserGroup instead does a MANUAL recursive cascade
// (delete the group, then every descendant group, dropping each removed
// group's member/manager rows) to mirror the SQL schema's
// `on delete cascade` from user_groups.parent_group_id down to
// user_group_members/user_group_managers via group_id.

func (m *MemoryDirectory) CreateUserGroup(ctx context.Context, g store.UserGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[g.ID]; ok {
		return store.ErrConflict
	}
	m.groups[g.ID] = g
	return nil
}

func (m *MemoryDirectory) UserGroupByID(ctx context.Context, id string) (store.UserGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[id]
	if !ok {
		return store.UserGroup{}, store.ErrNotFound
	}
	return g, nil
}

// UpdateUserGroup replaces name/owner/updated_at only — ParentGroupID and
// Tier are not writable via Update (mirrors SQLStore.UpdateUserGroup, whose
// SQL SET clause never touches those columns).
func (m *MemoryDirectory) UpdateUserGroup(ctx context.Context, g store.UserGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.groups[g.ID]
	if !ok {
		return store.ErrNotFound
	}
	existing.Name = g.Name
	existing.OwnerUserID = g.OwnerUserID
	existing.UpdatedAt = g.UpdatedAt
	m.groups[g.ID] = existing
	return nil
}

func (m *MemoryDirectory) DeleteUserGroup(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[id]; !ok {
		return store.ErrNotFound
	}
	m.deleteGroupCascadeLocked(id)
	return nil
}

// deleteGroupCascadeLocked removes id and recursively every descendant group
// (parent_group_id chain), plus each removed group's member/manager rows.
// Callers must hold m.mu (write lock).
func (m *MemoryDirectory) deleteGroupCascadeLocked(id string) {
	delete(m.groups, id)
	delete(m.members, id)
	delete(m.managers, id)
	m.dropProjectGroupsForGroupLocked(id)
	// Coupled-projects mirror (migration46 ON DELETE SET NULL): any project
	// coupled to the deleted group becomes uncoupled — mirrors the SQL FK's
	// `on delete set null` on projects.coupled_group_id.
	for pid, p := range m.projects {
		if p.CoupledGroupID == id {
			p.CoupledGroupID = ""
			m.projects[pid] = p
		}
	}
	for childID, child := range m.groups {
		if child.ParentGroupID == id {
			m.deleteGroupCascadeLocked(childID)
		}
	}
}

// dropProjectGroupsForGroupLocked removes any project_groups row referencing
// groupID, mirroring the SQL schema's `on delete cascade` from
// project_groups.group_id -> user_groups(id) (see migration45Up). Callers
// must hold m.mu (write lock).
func (m *MemoryDirectory) dropProjectGroupsForGroupLocked(groupID string) {
	for _, groupIDs := range m.projectGroups {
		delete(groupIDs, groupID)
	}
}

func (m *MemoryDirectory) ListUserGroupsByTier(ctx context.Context, tier string) ([]store.UserGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.UserGroup, 0)
	for _, g := range m.groups {
		if g.Tier == tier {
			out = append(out, g)
		}
	}
	sortUserGroups(out)
	return out, nil
}

func (m *MemoryDirectory) ChildUserGroups(ctx context.Context, parentID string) ([]store.UserGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.UserGroup, 0)
	for _, g := range m.groups {
		if g.ParentGroupID == parentID {
			out = append(out, g)
		}
	}
	sortUserGroups(out)
	return out, nil
}

func sortUserGroups(gs []store.UserGroup) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Name == gs[j].Name {
			return gs[i].ID < gs[j].ID
		}
		return gs[i].Name < gs[j].Name
	})
}

// SetUserGroupMember upserts a membership row (state/invited_by updated in
// place on a re-set — mirrors SQLStore's `on conflict do update`).
func (m *MemoryDirectory) SetUserGroupMember(ctx context.Context, groupID, userID, state, invitedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[groupID]; !ok {
		m.members[groupID] = map[string]store.UserGroupMembership{}
	}
	existing, had := m.members[groupID][userID]
	created := time.Now().UTC()
	if had {
		created = existing.CreatedAt
	}
	m.members[groupID][userID] = store.UserGroupMembership{
		GroupID:   groupID,
		UserID:    userID,
		State:     state,
		InvitedBy: invitedBy,
		CreatedAt: created,
	}
	return nil
}

func (m *MemoryDirectory) RemoveUserGroupMember(ctx context.Context, groupID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if group, ok := m.members[groupID]; ok {
		delete(group, userID)
	}
	return nil
}

func (m *MemoryDirectory) UserGroupMembers(ctx context.Context, groupID string) ([]store.UserGroupMembership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.UserGroupMembership, 0)
	for _, mem := range m.members[groupID] {
		out = append(out, mem)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].UserID < out[j].UserID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// UserGroupsForUser returns every group where userID has a membership row,
// optionally narrowed to a specific tier and/or membership state (either or
// both may be "" to mean "any" — an empty state must never filter out any
// state, and an "invited" membership must never satisfy a "member" filter).
func (m *MemoryDirectory) UserGroupsForUser(ctx context.Context, userID, tier, state string) ([]store.UserGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.UserGroup, 0)
	for groupID, byUser := range m.members {
		mem, ok := byUser[userID]
		if !ok {
			continue
		}
		if state != "" && mem.State != state {
			continue
		}
		g, ok := m.groups[groupID]
		if !ok {
			continue
		}
		if tier != "" && g.Tier != tier {
			continue
		}
		out = append(out, g)
	}
	sortUserGroups(out)
	return out, nil
}

// userGroupManagerEntry is the memory-driver's co-manager row: its
// permission flags (per-Admin-Group co-manager permissions, spec 2026-08-10 +
// Phase B 2026-08-10 + Phase C 2026-08-10 + Resource Groups Phase 1
// 2026-08-11) plus the time it was first added, mirroring the SQL row's
// can_manage_users/can_manage_group/can_manage_servers/can_manage_services/can_manage_resources/created_at.
type userGroupManagerEntry struct {
	canManageUsers     bool
	canManageGroup     bool
	canManageServers   bool
	canManageServices  bool
	canManageResources bool
	createdAt          time.Time
}

func (m *MemoryDirectory) SetUserGroupManager(ctx context.Context, groupID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.managers[groupID]; !ok {
		m.managers[groupID] = map[string]userGroupManagerEntry{}
	}
	// Mirrors the SQL "on conflict (group_id, user_id) do nothing": an
	// EXISTING row's permission flags are left untouched by a re-set; only a
	// brand-new row picks up the "full co-manager rights" default (mirrors
	// migration v48/v49/v51/v53's `default 1` on all five columns).
	if _, ok := m.managers[groupID][userID]; !ok {
		m.managers[groupID][userID] = userGroupManagerEntry{
			canManageUsers:     true,
			canManageGroup:     true,
			canManageServers:   true,
			canManageServices:  true,
			canManageResources: true,
			createdAt:          time.Now().UTC(),
		}
	}
	return nil
}

func (m *MemoryDirectory) RemoveUserGroupManager(ctx context.Context, groupID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if group, ok := m.managers[groupID]; ok {
		delete(group, userID)
	}
	return nil
}

func (m *MemoryDirectory) UserGroupManagers(ctx context.Context, groupID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0)
	for userID := range m.managers[groupID] {
		out = append(out, userID)
	}
	sort.Strings(out)
	return out, nil
}

// UserGroupManagerPerms mirrors SQLiteStore.UserGroupManagerPerms: every
// co-manager row of groupID with its permission flags, ordered like the SQL
// "order by created_at, user_id". A group with no co-managers returns an
// empty (non-nil) slice.
func (m *MemoryDirectory) UserGroupManagerPerms(ctx context.Context, groupID string) ([]store.UserGroupManagerPerm, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type indexed struct {
		perm      store.UserGroupManagerPerm
		createdAt time.Time
	}
	entries := make([]indexed, 0, len(m.managers[groupID]))
	for userID, e := range m.managers[groupID] {
		entries = append(entries, indexed{
			perm: store.UserGroupManagerPerm{
				UserID:             userID,
				CanManageUsers:     e.canManageUsers,
				CanManageGroup:     e.canManageGroup,
				CanManageServers:   e.canManageServers,
				CanManageServices:  e.canManageServices,
				CanManageResources: e.canManageResources,
			},
			createdAt: e.createdAt,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].createdAt.Equal(entries[j].createdAt) {
			return entries[i].perm.UserID < entries[j].perm.UserID
		}
		return entries[i].createdAt.Before(entries[j].createdAt)
	})
	out := make([]store.UserGroupManagerPerm, len(entries))
	for i, e := range entries {
		out[i] = e.perm
	}
	return out, nil
}

// SetUserGroupManagerPermissions mirrors SQLiteStore.SetUserGroupManagerPermissions:
// it updates an EXISTING co-manager row's permission flags and returns
// ErrNotFound (never creates a row) when the group/user pair has none —
// the memory-driver analog of a zero-rows-affected SQL UPDATE. perm.UserID
// identifies the row; perm's five Can* flags are the new values.
func (m *MemoryDirectory) SetUserGroupManagerPermissions(ctx context.Context, groupID string, perm store.UserGroupManagerPerm) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	group, ok := m.managers[groupID]
	if !ok {
		return store.ErrNotFound
	}
	e, ok := group[perm.UserID]
	if !ok {
		return store.ErrNotFound
	}
	e.canManageUsers = perm.CanManageUsers
	e.canManageGroup = perm.CanManageGroup
	e.canManageServers = perm.CanManageServers
	e.canManageServices = perm.CanManageServices
	e.canManageResources = perm.CanManageResources
	group[perm.UserID] = e
	return nil
}

// --- ProjectStore (see project_store.go) -----------------------------------
//
// Like the GroupStore section above, the memory driver enforces NO
// foreign-key-style referential checks on owner_user_id/project_id/user_id/
// group_id — only id uniqueness on Create. DeleteProject does a manual
// cascade (drop the project's member/group rows), mirroring the SQL schema's
// `on delete cascade` from project_members.project_id/project_groups.
// project_id -> projects(id) (migration45Up). The reverse direction — a
// user_group delete cascading into project_groups.group_id — is handled by
// dropProjectGroupsForGroupLocked, hooked into deleteGroupCascadeLocked
// above.

func (m *MemoryDirectory) CreateProject(ctx context.Context, p store.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[p.ID]; ok {
		return store.ErrConflict
	}
	m.projects[p.ID] = p
	return nil
}

func (m *MemoryDirectory) ProjectByID(ctx context.Context, id string) (store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return store.Project{}, store.ErrNotFound
	}
	return p, nil
}

// UpdateProject replaces name/description/owner/updated_at only (mirrors
// UpdateProject's SQL SET clause in sqlite_projects.go).
func (m *MemoryDirectory) UpdateProject(ctx context.Context, p store.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.projects[p.ID]
	if !ok {
		return store.ErrNotFound
	}
	existing.Name = p.Name
	existing.Description = p.Description
	existing.OwnerUserID = p.OwnerUserID
	existing.UpdatedAt = p.UpdatedAt
	m.projects[p.ID] = existing
	return nil
}

func (m *MemoryDirectory) DeleteProject(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.projects, id)
	delete(m.projectMembers, id)
	delete(m.projectGroups, id)
	return nil
}

func (m *MemoryDirectory) ListProjects(ctx context.Context) ([]store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.Project, 0)
	for _, p := range m.projects {
		out = append(out, p)
	}
	sortProjects(out)
	return out, nil
}

func sortProjects(ps []store.Project) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Name == ps[j].Name {
			return ps[i].ID < ps[j].ID
		}
		return ps[i].Name < ps[j].Name
	})
}

func (m *MemoryDirectory) SetProjectMember(ctx context.Context, projectID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projectMembers[projectID]; !ok {
		m.projectMembers[projectID] = map[string]time.Time{}
	}
	if _, had := m.projectMembers[projectID][userID]; !had {
		m.projectMembers[projectID][userID] = time.Now().UTC()
	}
	return nil
}

func (m *MemoryDirectory) RemoveProjectMember(ctx context.Context, projectID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if members, ok := m.projectMembers[projectID]; ok {
		delete(members, userID)
	}
	return nil
}

func (m *MemoryDirectory) ProjectMembers(ctx context.Context, projectID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedByFirstSeen(m.projectMembers[projectID]), nil
}

func (m *MemoryDirectory) SetProjectGroup(ctx context.Context, projectID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projectGroups[projectID]; !ok {
		m.projectGroups[projectID] = map[string]time.Time{}
	}
	if _, had := m.projectGroups[projectID][groupID]; !had {
		m.projectGroups[projectID][groupID] = time.Now().UTC()
	}
	return nil
}

func (m *MemoryDirectory) RemoveProjectGroup(ctx context.Context, projectID, groupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if groups, ok := m.projectGroups[projectID]; ok {
		delete(groups, groupID)
	}
	return nil
}

func (m *MemoryDirectory) ProjectGroups(ctx context.Context, projectID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedByFirstSeen(m.projectGroups[projectID]), nil
}

// sortedByFirstSeen returns byID's keys ordered by their recorded time (ties
// broken by id), mirroring the SQL `order by created_at, <id>` used by
// ProjectMembers/ProjectGroups in sqlite_projects.go. Ranging over a nil map
// (an unknown projectID) is a documented no-op, so this returns an empty
// slice rather than erroring.
func sortedByFirstSeen(byID map[string]time.Time) []string {
	out := make([]string, 0, len(byID))
	for id := range byID {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := byID[out[i]], byID[out[j]]
		if ti.Equal(tj) {
			return out[i] < out[j]
		}
		return ti.Before(tj)
	})
	return out
}

// ProjectsByOwnerOrMember returns every project where userID is the owner OR
// has a direct project_members row (mirrors the SQL OR-exists query in
// sqlite_projects.go; the group part is composed in the service — Task 3).
func (m *MemoryDirectory) ProjectsByOwnerOrMember(ctx context.Context, userID string) ([]store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.Project, 0)
	for id, p := range m.projects {
		if p.OwnerUserID == userID {
			out = append(out, p)
			continue
		}
		if _, ok := m.projectMembers[id][userID]; ok {
			out = append(out, p)
		}
	}
	sortProjects(out)
	return out, nil
}

// ProjectsByGroup returns every project with groupID assigned.
func (m *MemoryDirectory) ProjectsByGroup(ctx context.Context, groupID string) ([]store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.Project, 0)
	for id, p := range m.projects {
		if _, ok := m.projectGroups[id][groupID]; ok {
			out = append(out, p)
		}
	}
	sortProjects(out)
	return out, nil
}

// CoupledProjectsByGroup returns every project coupled to groupID
// (CoupledGroupID == groupID) — mirrors SQLStore.CoupledProjectsByGroup.
func (m *MemoryDirectory) CoupledProjectsByGroup(ctx context.Context, groupID string) ([]store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]store.Project, 0)
	if groupID == "" {
		return out, nil
	}
	for _, p := range m.projects {
		if p.CoupledGroupID == groupID {
			out = append(out, p)
		}
	}
	sortProjects(out)
	return out, nil
}
