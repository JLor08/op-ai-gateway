// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"time"
)

// Vendor account group statuses.
const (
	VendorAccountGroupStatusActive   = "active"
	VendorAccountGroupStatusDisabled = "disabled"
)

// DefaultVendorGroupThresholdPct is the default switch-over threshold (percent
// of a limit window used) of a vendor account group.
const DefaultVendorGroupThresholdPct = 99

// VendorAccountGroup is a personal ordered set of one user's vendor accounts of a
// single vendor, routed across as one logical account. The members live in the
// vendor_account_group_members link table (see VendorAccountGroupMember).
type VendorAccountGroup struct {
	ID          string
	OwnerUserID string
	Vendor      string
	Name        string
	ModelPrefix string
	Status      string
	// ThresholdFiveHourPct, ThresholdWeeklyPct and ThresholdCreditsPct are the
	// used-percent values (1..100) at which routing moves on to the next member.
	ThresholdFiveHourPct int
	ThresholdWeeklyPct   int
	ThresholdCreditsPct  int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// VendorAccountGroupMember places one vendor account in a group. Position is
// 1-based and unique within the group; lower positions are preferred.
type VendorAccountGroupMember struct {
	GroupID   string
	AccountID string
	Position  int
}

// VendorAccountGroupStore is the persistence of vendor account groups and their
// ordered members.
type VendorAccountGroupStore interface {
	// CreateVendorAccountGroup stores a new group; a duplicate id is ErrConflict.
	CreateVendorAccountGroup(ctx context.Context, g VendorAccountGroup) error
	// UpdateVendorAccountGroup writes name, model_prefix, status, the three
	// thresholds and updated_at; owner_user_id, vendor and created_at are
	// immutable. An unknown id is ErrNotFound.
	UpdateVendorAccountGroup(ctx context.Context, g VendorAccountGroup) error
	// DeleteVendorAccountGroup removes the group and its member rows. An unknown
	// id is ErrNotFound.
	DeleteVendorAccountGroup(ctx context.Context, id string) error
	// VendorAccountGroupByID returns the group or ErrNotFound.
	VendorAccountGroupByID(ctx context.Context, id string) (VendorAccountGroup, error)
	// VendorAccountGroupsByOwner lists userID's groups ordered by id; never nil.
	VendorAccountGroupsByOwner(ctx context.Context, userID string) ([]VendorAccountGroup, error)
	// VendorAccountGroupMembers lists the group's members in position order;
	// never nil.
	VendorAccountGroupMembers(ctx context.Context, groupID string) ([]VendorAccountGroupMember, error)
	// SetVendorAccountGroupMembers atomically replaces the group's member set;
	// positions are 1..n in slice order. An unknown group or account is
	// ErrNotFound, a duplicate account id is ErrConflict; a rejected set leaves
	// the previous one untouched.
	SetVendorAccountGroupMembers(ctx context.Context, groupID string, accountIDs []string) error
}
