// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"testing"
	"time"
)

// netbirdOnlyOn flips the netbird_only runtime toggle on.
func netbirdOnlyOn(t *testing.T, svc *Service) {
	t.Helper()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdOnly: boolPtr(true),
	}); err != nil {
		t.Fatalf("set netbird_only: %v", err)
	}
}

// TestCreateServerNetbirdOnlyForcesNormalAdmin: with netbird_only ON + the module
// enabled, a NON-system-admin creating an off-mesh server (NetbirdEnabled:false,
// no domain) has the flag FORCED on — the server is created (no
// ErrServerDomainRequired) with NetbirdEnabled true. The setup-key hook is a
// best-effort no-op (failing NetBird), which never blocks the create.
func TestCreateServerNetbirdOnlyForcesNormalAdmin(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failSetupKey = true // best-effort hook no-op; create must still succeed
	enableNetbird(t, svc, fake.srv.URL, true)
	netbirdOnlyOn(t, svc)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "Forced", NetbirdEnabled: false, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (netbird_only, normal admin, no domain): %v", err)
	}
	if !dto.NetbirdEnabled {
		t.Fatalf("dto.NetbirdEnabled = false, want true (forced on under netbird_only)")
	}
}

// TestCreateServerNetbirdOnlySystemAdminOverride: a system-admin (the "system"
// scope) may deliberately create an off-mesh server even under netbird_only — the
// requested NetbirdEnabled:false + a domain is honored.
func TestCreateServerNetbirdOnlySystemAdminOverride(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	netbirdOnlyOn(t, svc)

	systemAdmin := auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin", "system"}}
	dto, err := svc.CreateServer(context.Background(), systemAdmin, CreateServerRequest{
		Name: "Override", Domain: "s.local", NetbirdEnabled: false, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (netbird_only, system admin, off-mesh): %v", err)
	}
	if dto.NetbirdEnabled {
		t.Fatalf("dto.NetbirdEnabled = true, want false (system-admin override honored)")
	}
}

// TestCreateServerNetbirdOnlyOffNoForce: netbird_only OFF, so a normal admin's
// explicit NetbirdEnabled:false is honored (no forcing) even with the module
// enabled.
func TestCreateServerNetbirdOnlyOffNoForce(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	enableNetbird(t, svc, fake.srv.URL, true)
	// netbird_only left OFF (default false).

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "Plain", Domain: "s.local", NetbirdEnabled: false, OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer (netbird_only off, normal admin): %v", err)
	}
	if dto.NetbirdEnabled {
		t.Fatalf("dto.NetbirdEnabled = true, want false (no forcing when netbird_only off)")
	}
}
