// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"testing"
	"time"
)

// systemAdminToken is a system-admin principal (the "system" scope) — exempt from
// the create-time policy-override enforcement.
func systemAdminToken() auth.Token {
	return auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin", "system"}}
}

// TestCreateServerPolicyForcedIncludeNormalAdmin: under deny-by-default + "selected"
// scope, a normal admin creating a NetBird server has its policy override FORCED to
// "include" (else the server would have no access policy = unreachable). An explicit
// "exclude" is stripped first, then still forced to "include".
func TestCreateServerPolicyForcedIncludeNormalAdmin(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true // best-effort hook no-op; create must still succeed
	enableNetbirdPolicies(t, svc, fake.srv.URL, "selected", true, false)

	// Empty override -> forced "include".
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "F", NetbirdEnabled: true, NetbirdPolicyOverride: "", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (empty override): %v", err)
	}
	if dto.NetbirdPolicyOverride != "include" {
		t.Fatalf("NetbirdPolicyOverride = %q, want include (forced under deny+selected)", dto.NetbirdPolicyOverride)
	}

	// Explicit "exclude" -> stripped then still forced "include".
	dto2, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "F2", NetbirdEnabled: true, NetbirdPolicyOverride: "exclude", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (exclude override): %v", err)
	}
	if dto2.NetbirdPolicyOverride != "include" {
		t.Fatalf("NetbirdPolicyOverride = %q, want include (exclude stripped then forced)", dto2.NetbirdPolicyOverride)
	}
}

// TestCreateServerPolicyStripExcludeNormalAdmin: in "all" scope with deny off, the
// include-force does NOT apply, but a normal admin can never opt a server OUT — an
// "exclude" is stripped to "".
func TestCreateServerPolicyStripExcludeNormalAdmin(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", NetbirdEnabled: true, NetbirdPolicyOverride: "exclude", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.NetbirdPolicyOverride != "" {
		t.Fatalf("NetbirdPolicyOverride = %q, want \"\" (exclude stripped for a normal admin)", dto.NetbirdPolicyOverride)
	}
}

// TestCreateServerPolicySystemAdminOverrideHonored: a system-admin (the "system"
// scope) is exempt — its "exclude" is honored as sent, not stripped.
func TestCreateServerPolicySystemAdminOverrideHonored(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	dto, err := svc.CreateServer(context.Background(), systemAdminToken(), CreateServerRequest{
		Name: "S", NetbirdEnabled: true, NetbirdPolicyOverride: "exclude", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.NetbirdPolicyOverride != "exclude" {
		t.Fatalf("NetbirdPolicyOverride = %q, want exclude (system-admin honored)", dto.NetbirdPolicyOverride)
	}
}

// TestCreateServerPolicyNotForcedWhenDenyOff: "selected" scope with deny OFF does
// NOT force include — a normal admin's empty override stays "".
func TestCreateServerPolicyNotForcedWhenDenyOff(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true
	enableNetbirdPolicies(t, svc, fake.srv.URL, "selected", false, false)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", NetbirdEnabled: true, NetbirdPolicyOverride: "", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.NetbirdPolicyOverride != "" {
		t.Fatalf("NetbirdPolicyOverride = %q, want \"\" (not forced when deny off)", dto.NetbirdPolicyOverride)
	}
}

// TestCreateServerPolicyNonNetbirdUnaffected: a non-NetBird create (NetbirdEnabled
// false) is not subject to the override enforcement even under deny+selected. With
// netbird_only off, the flag stays false, so the enforcement block is skipped; the
// stored override is "".
func TestCreateServerPolicyNonNetbirdUnaffected(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true
	enableNetbirdPolicies(t, svc, fake.srv.URL, "selected", true, false)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "Plain", Domain: "s.local", NetbirdEnabled: false, NetbirdPolicyOverride: "", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (non-netbird): %v", err)
	}
	if dto.NetbirdEnabled {
		t.Fatalf("dto.NetbirdEnabled = true, want false (netbird_only off, req false)")
	}
	if dto.NetbirdPolicyOverride != "" {
		t.Fatalf("NetbirdPolicyOverride = %q, want \"\" (include not forced for a non-netbird create)", dto.NetbirdPolicyOverride)
	}
}
