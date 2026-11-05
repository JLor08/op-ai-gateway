// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestServerRPCFieldsRoundTrip proves the LAN address, bind-only flag, RPC
// member consent and allowed head groups ride create + update and come back
// on the DTO, and that netbird_ip is surfaced read-only from the store.
func TestServerRPCFieldsRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	ctx := context.Background()

	lan, bindOnly, member := "192.168.1.20", true, true
	heads := []string{testAdminGroupID}
	dto, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{testAdminGroupID},
		LANAddress: &lan, LANBindOnly: &bindOnly, RPCMemberEnabled: &member, RPCAllowedHeadGroups: &heads,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.LANAddress != lan || !dto.LANBindOnly || !dto.RPCMemberEnabled || !reflect.DeepEqual(dto.RPCAllowedHeadGroups, heads) {
		t.Fatalf("create DTO = %+v", dto)
	}

	// A no-touch create defaults to empty/false and a non-nil empty list.
	plain, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "P", Domain: "p.example.test", AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer(plain): %v", err)
	}
	if plain.LANAddress != "" || plain.LANBindOnly || plain.RPCMemberEnabled || plain.RPCAllowedHeadGroups == nil || len(plain.RPCAllowedHeadGroups) != 0 {
		t.Fatalf("plain DTO = %+v", plain)
	}
	stored, _ := routeStore.AIServerByID(ctx, plain.ID)
	if stored.RPCAllowedHeadGroups != "[]" {
		t.Fatalf("stored head groups = %q, want [] (empty normalised on write)", stored.RPCAllowedHeadGroups)
	}

	// Update: change everything, then clear; the list is stored as JSON.
	newLAN, off := "10.0.0.9", false
	none := []string{}
	got, err := svc.UpdateServer(ctx, systemAdminToken(), dto.ID, UpdateServerRequest{
		LANAddress: &newLAN, LANBindOnly: &off, RPCAllowedHeadGroups: &none,
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if got.LANAddress != newLAN || got.LANBindOnly || !got.RPCMemberEnabled || len(got.RPCAllowedHeadGroups) != 0 {
		t.Fatalf("update DTO = %+v", got)
	}
	empty := ""
	got, err = svc.UpdateServer(ctx, systemAdminToken(), dto.ID, UpdateServerRequest{LANAddress: &empty})
	if err != nil || got.LANAddress != "" {
		t.Fatalf("clear LAN address = %+v, %v", got, err)
	}

	// netbird_ip is read-only: it comes from the store, never from a request.
	if err := routeStore.UpdateServerNetbirdState(ctx, dto.ID, "s.example.test", "peer-1", "100.64.0.5", true); err != nil {
		t.Fatalf("seed netbird state: %v", err)
	}
	got, err = svc.GetServer(ctx, systemAdminToken(), dto.ID)
	if err != nil || got.NetbirdIP != "100.64.0.5" {
		t.Fatalf("netbird_ip = %q, %v; want 100.64.0.5", got.NetbirdIP, err)
	}
}

// TestServerRPCFieldValidation covers lan_address_invalid and unknown head
// groups on both write paths.
func TestServerRPCFieldValidation(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	ctx := context.Background()
	srv := createTestServer(t, svc, "S", "s.example.test")

	for _, bad := range []string{"host.example.test", "300.1.1.1", "fe80::1", "::ffff:10.0.0.1", "10.0.0.0/24", "10.0.0.1:80"} {
		v := bad
		_, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &v})
		if !errors.Is(err, ErrServerLANAddressInvalid) {
			t.Fatalf("UpdateServer(lan=%q) = %v, want ErrServerLANAddressInvalid", bad, err)
		}
		_, err = svc.CreateServer(ctx, adminToken(), CreateServerRequest{
			Name: "X", Domain: "x.example.test", AdminGroupIDs: []string{testAdminGroupID}, LANAddress: &v,
		})
		if !errors.Is(err, ErrServerLANAddressInvalid) {
			t.Fatalf("CreateServer(lan=%q) = %v, want ErrServerLANAddressInvalid", bad, err)
		}
	}

	unknown := []string{"grp_does_not_exist"}
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{RPCAllowedHeadGroups: &unknown}); !errors.Is(err, ErrServerAdminGroupInvalid) {
		t.Fatalf("UpdateServer(unknown head group) = %v, want ErrServerAdminGroupInvalid", err)
	}
	// A system-tier group is not an admin group.
	sys := []string{testSystemGroupID}
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{RPCAllowedHeadGroups: &sys}); !errors.Is(err, ErrServerAdminGroupInvalid) {
		t.Fatalf("UpdateServer(system group) = %v, want ErrServerAdminGroupInvalid", err)
	}
	if _, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "X", Domain: "x.example.test", AdminGroupIDs: []string{testAdminGroupID}, RPCAllowedHeadGroups: &unknown,
	}); !errors.Is(err, ErrServerAdminGroupInvalid) {
		t.Fatalf("CreateServer(unknown head group) = %v, want ErrServerAdminGroupInvalid", err)
	}
}

// TestUpdateServerNotifiesRuntimeOnRPCFields proves a save touching only the
// RPC member fields still announces a runtime-config change (the agent's
// document derives its RPC member bind from them).
func TestUpdateServerNotifiesRuntimeOnRPCFields(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	srv := createTestServer(t, svc, "S", "s.example.test")
	var notified []string
	svc.SetRuntimeConfigChangedHook(func(id string) { notified = append(notified, id) })

	member, lan := true, "192.168.1.5"
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), srv.ID, UpdateServerRequest{RPCMemberEnabled: &member, LANAddress: &lan}); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if len(notified) != 1 || notified[0] != srv.ID {
		t.Fatalf("notified = %v, want [%s]", notified, srv.ID)
	}
}

// TestUpdateServerNetbirdOffClearsMeshIP proves turning NetBird off through
// UpdateServer drops the stored mesh IP (the peer sync no longer refreshes it).
func TestUpdateServerNetbirdOffClearsMeshIP(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	ctx := context.Background()
	srv := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpdateServerNetbirdLink(ctx, srv.ID, true, "peer-1"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := routeStore.UpdateServerNetbirdState(ctx, srv.ID, "s.example.test", "peer-1", "100.64.0.5", true); err != nil {
		t.Fatalf("state: %v", err)
	}
	off := false
	got, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{NetbirdEnabled: &off})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if got.NetbirdIP != "" {
		t.Fatalf("netbird_ip after disable = %q, want empty", got.NetbirdIP)
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.NetbirdIP != "" {
		t.Fatalf("stored netbird_ip = %q, want empty", stored.NetbirdIP)
	}
}

// TestServerHeadGroupsPersistAndLegacyEmpty proves a non-empty head-group
// list survives UpdateServer and a legacy "" stored value reads as [].
func TestServerHeadGroupsPersistAndLegacyEmpty(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	ctx := context.Background()
	srv := createTestServer(t, svc, "S", "s.example.test")

	heads := []string{testAdminGroupID}
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{RPCAllowedHeadGroups: &heads}); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.RPCAllowedHeadGroups != `["`+testAdminGroupID+`"]` {
		t.Fatalf("stored = %q", stored.RPCAllowedHeadGroups)
	}
	got, err := svc.GetServer(ctx, systemAdminToken(), srv.ID)
	if err != nil || !reflect.DeepEqual(got.RPCAllowedHeadGroups, heads) {
		t.Fatalf("DTO heads = %v, %v", got.RPCAllowedHeadGroups, err)
	}

	stored.RPCAllowedHeadGroups = ""
	if err := routeStore.UpdateAIServer(ctx, stored); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	got, err = svc.GetServer(ctx, systemAdminToken(), srv.ID)
	if err != nil || got.RPCAllowedHeadGroups == nil || len(got.RPCAllowedHeadGroups) != 0 {
		t.Fatalf("legacy DTO heads = %#v, %v; want empty non-nil", got.RPCAllowedHeadGroups, err)
	}
}
