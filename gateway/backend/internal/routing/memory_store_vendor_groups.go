// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/storeerr"
	"slices"
	"sort"
)

// --- VendorAccountGroupStore: personal vendor account groups (migration 114) ---

// CreateVendorAccountGroup stores a new group; a duplicate id is ErrConflict.
func (m *MemoryStore) CreateVendorAccountGroup(_ context.Context, g VendorAccountGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vendorGroups[g.ID]; ok {
		return storeerr.ErrConflict
	}
	m.vendorGroups[g.ID] = g
	return nil
}

// UpdateVendorAccountGroup writes the mutable columns only (owner, vendor and
// created_at are immutable, as in the SQL drivers). An unknown id is ErrNotFound.
func (m *MemoryStore) UpdateVendorAccountGroup(_ context.Context, g VendorAccountGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.vendorGroups[g.ID]
	if !ok {
		return storeerr.ErrNotFound
	}
	cur.Name = g.Name
	cur.ModelPrefix = g.ModelPrefix
	cur.Status = g.Status
	cur.ThresholdFiveHourPct = g.ThresholdFiveHourPct
	cur.ThresholdWeeklyPct = g.ThresholdWeeklyPct
	cur.ThresholdCreditsPct = g.ThresholdCreditsPct
	cur.UpdatedAt = g.UpdatedAt
	m.vendorGroups[g.ID] = cur
	return nil
}

// DeleteVendorAccountGroup removes the group and its member list, mirroring the
// member table's ON DELETE CASCADE. An unknown id is ErrNotFound.
func (m *MemoryStore) DeleteVendorAccountGroup(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vendorGroups[id]; !ok {
		return storeerr.ErrNotFound
	}
	delete(m.vendorGroups, id)
	delete(m.vendorGroupMembers, id)
	return nil
}

// VendorAccountGroupByID returns the group or ErrNotFound.
func (m *MemoryStore) VendorAccountGroupByID(_ context.Context, id string) (VendorAccountGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.vendorGroups[id]
	if !ok {
		return VendorAccountGroup{}, storeerr.ErrNotFound
	}
	return g, nil
}

// VendorAccountGroupsByOwner lists userID's groups ordered by id; never nil.
func (m *MemoryStore) VendorAccountGroupsByOwner(_ context.Context, userID string) ([]VendorAccountGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]VendorAccountGroup, 0)
	for _, g := range m.vendorGroups {
		if g.OwnerUserID == userID {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// VendorAccountGroupMembers lists the group's members in position order; never
// nil. An unknown group simply has no members.
func (m *MemoryStore) VendorAccountGroupMembers(_ context.Context, groupID string) ([]VendorAccountGroupMember, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := m.vendorGroupMembers[groupID]
	out := make([]VendorAccountGroupMember, 0, len(ids))
	for i, id := range ids {
		out = append(out, VendorAccountGroupMember{GroupID: groupID, AccountID: id, Position: i + 1})
	}
	return out, nil
}

// SetVendorAccountGroupMembers atomically replaces the member list. The group
// and every account must exist (ErrNotFound) and ids must be distinct
// (ErrConflict); a rejected set leaves the previous list in place.
func (m *MemoryStore) SetVendorAccountGroupMembers(_ context.Context, groupID string, accountIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vendorGroups[groupID]; !ok {
		return storeerr.ErrNotFound
	}
	seen := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if _, ok := m.vendorAccounts[id]; !ok {
			return storeerr.ErrNotFound
		}
		if _, dup := seen[id]; dup {
			return storeerr.ErrConflict
		}
		seen[id] = struct{}{}
	}
	m.vendorGroupMembers[groupID] = slices.Clone(accountIDs)
	return nil
}

// dropVendorAccountFromGroupsLocked removes accountID from every group's member
// list (the vendor_account_id ON DELETE CASCADE). Positions are implied by list
// order, so the survivors close up. Callers must hold m.mu for writing.
func (m *MemoryStore) dropVendorAccountFromGroupsLocked(accountID string) {
	for groupID, ids := range m.vendorGroupMembers {
		m.vendorGroupMembers[groupID] = slices.DeleteFunc(ids, func(id string) bool { return id == accountID })
	}
}
