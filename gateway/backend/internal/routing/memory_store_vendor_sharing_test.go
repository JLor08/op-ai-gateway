// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/storeerr"
	"testing"
	"time"
)

// memoryVendorShare reads MemoryStore's share_level / system_group_id state for
// an account. Neither is a field of VendorAccount (no reader exposes them yet),
// so the memory half of the vendor-account sharing contract is observed on the
// store's own state, here in package routing; the SQL half is asserted through
// the columns in package store (vendor_account_sharing_test.go). Absent state
// reads as the column DEFAULTs: "none" and the empty string.
func memoryVendorShare(m *MemoryStore, id string) (level, systemGroupID string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	level = m.vendorAccountShareLevels[id]
	if level == "" {
		level = VendorShareLevelNone
	}
	return level, m.vendorAccountSystemGroups[id]
}

// TestMemoryStoreVendorAccountSharing covers what the cross-driver suite in
// package store cannot read through routing.Store: the share_level and
// system_group_id state of the in-memory driver, through the two narrow writers,
// ClearVendorAccountSharing, the account-delete cascade and a re-create under
// the same id.
func TestMemoryStoreVendorAccountSharing(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	m := NewMemoryStore()
	for _, id := range []string{"va_m", "va_m_by"} {
		if err := m.CreateVendorAccount(ctx, VendorAccount{ID: id, OwnerUserID: "u_m", Vendor: VendorOpenAI, AuthType: VendorAuthAPIKey, Name: id, Status: VendorAccountStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateVendorAccount(%s): %v", id, err)
		}
	}
	if err := m.CreateResourceGroup(ctx, ResourceGroup{ID: "rg_m", Name: "RG", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateResourceGroup: %v", err)
	}

	// A fresh account carries the column DEFAULTs.
	if level, root := memoryVendorShare(m, "va_m"); level != VendorShareLevelNone || root != "" {
		t.Fatalf("fresh account share state = %q/%q, want none/\"\"", level, root)
	}

	// Unknown ids are ErrNotFound on both writers and on Clear.
	if err := m.UpdateVendorAccountShareLevel(ctx, "va_missing", VendorShareLevelShared); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateVendorAccountShareLevel(unknown) = %v, want ErrNotFound", err)
	}
	if err := m.UpdateVendorAccountSystemGroup(ctx, "va_missing", "ugrp_sys"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("UpdateVendorAccountSystemGroup(unknown) = %v, want ErrNotFound", err)
	}
	if err := m.ClearVendorAccountSharing(ctx, "va_missing"); !errors.Is(err, storeerr.ErrNotFound) {
		t.Fatalf("ClearVendorAccountSharing(unknown) = %v, want ErrNotFound", err)
	}
	// A rejected write leaves no phantom state behind for the unknown id.
	if level, root := memoryVendorShare(m, "va_missing"); level != VendorShareLevelNone || root != "" {
		t.Fatalf("unknown id grew share state %q/%q", level, root)
	}

	for _, id := range []string{"va_m", "va_m_by"} {
		if err := m.UpdateVendorAccountShareLevel(ctx, id, VendorShareLevelFull); err != nil {
			t.Fatalf("UpdateVendorAccountShareLevel(%s): %v", id, err)
		}
		if err := m.UpdateVendorAccountSystemGroup(ctx, id, "ugrp_sys"); err != nil {
			t.Fatalf("UpdateVendorAccountSystemGroup(%s): %v", id, err)
		}
		if err := m.SetVendorAccountAdminGroup(ctx, id, "ugrp_a"); err != nil {
			t.Fatalf("SetVendorAccountAdminGroup(%s): %v", id, err)
		}
		if err := m.SetResourceGroupVendorAccount(ctx, "rg_m", id); err != nil {
			t.Fatalf("SetResourceGroupVendorAccount(%s): %v", id, err)
		}
	}
	if level, root := memoryVendorShare(m, "va_m"); level != VendorShareLevelFull || root != "ugrp_sys" {
		t.Fatalf("share state after the writers = %q/%q, want full/ugrp_sys", level, root)
	}

	// Clear resets level and root and both link sets -- for that account only.
	if err := m.ClearVendorAccountSharing(ctx, "va_m"); err != nil {
		t.Fatalf("ClearVendorAccountSharing: %v", err)
	}
	if level, root := memoryVendorShare(m, "va_m"); level != VendorShareLevelNone || root != "" {
		t.Fatalf("share state after Clear = %q/%q, want none/\"\"", level, root)
	}
	if got, _ := m.VendorAccountAdminGroups(ctx, "va_m"); len(got) != 0 {
		t.Fatalf("admin groups after Clear = %v, want empty", got)
	}
	if got, _ := m.ResourceGroupVendorAccounts(ctx, "rg_m"); len(got) != 1 || got[0] != "va_m_by" {
		t.Fatalf("resource-group members after Clear = %v, want [va_m_by]", got)
	}
	if level, root := memoryVendorShare(m, "va_m_by"); level != VendorShareLevelFull || root != "ugrp_sys" {
		t.Fatalf("bystander share state after Clear = %q/%q, want full/ugrp_sys", level, root)
	}

	// Deleting the account drops its share state, so a re-create under the same id
	// starts private -- fixed ids are routine in dev/e2e fixtures.
	if err := m.DeleteVendorAccount(ctx, "va_m_by"); err != nil {
		t.Fatalf("DeleteVendorAccount: %v", err)
	}
	if level, root := memoryVendorShare(m, "va_m_by"); level != VendorShareLevelNone || root != "" {
		t.Fatalf("share state after the account delete = %q/%q, want the defaults", level, root)
	}
	if err := m.CreateVendorAccount(ctx, VendorAccount{ID: "va_m_by", OwnerUserID: "u_m", Vendor: VendorOpenAI, AuthType: VendorAuthAPIKey, Name: "again", Status: VendorAccountStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("re-CreateVendorAccount: %v", err)
	}
	if level, root := memoryVendorShare(m, "va_m_by"); level != VendorShareLevelNone || root != "" {
		t.Fatalf("re-created account share state = %q/%q, want none/\"\"", level, root)
	}
	if got, _ := m.VendorAccountAdminGroups(ctx, "va_m_by"); len(got) != 0 {
		t.Fatalf("re-created account admin groups = %v, want empty", got)
	}
	if got, _ := m.ResourceGroupsByVendorAccount(ctx, "va_m_by"); len(got) != 0 {
		t.Fatalf("re-created account resource groups = %+v, want empty", got)
	}
}
