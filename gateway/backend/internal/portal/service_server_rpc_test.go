// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"reflect"
	"slices"
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

func TestUpdateServerLANSourceRoundTrip(t *testing.T) {
	svc, routeStore := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	srv := createTestServer(t, svc, "S", "s.example.test")
	if srv.LANSource != "auto" || srv.LANInterfaceID != "" {
		t.Fatalf("fresh server source = %q/%q, want auto", srv.LANSource, srv.LANInterfaceID)
	}
	src, id := "interface", "if-1"
	got, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANSource: &src, LANInterfaceID: &id})
	if err != nil || got.LANSource != "interface" || got.LANInterfaceID != "if-1" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	stored, _ := routeStore.AIServerByID(ctx, srv.ID)
	if stored.LANSource != "interface" || stored.LANInterfaceID != "if-1" {
		t.Fatalf("stored = %q/%q", stored.LANSource, stored.LANInterfaceID)
	}
	// Id already stored: switching source alone is enough.
	auto := "auto"
	if got, err = svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANSource: &auto}); err != nil || got.LANSource != "auto" {
		t.Fatalf("to auto = %+v, %v", got, err)
	}
	src = "interface"
	if got, err = svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANSource: &src}); err != nil || got.LANSource != "interface" {
		t.Fatalf("back to interface with stored id = %+v, %v", got, err)
	}
}

func TestUpdateServerRejectsBadLANSource(t *testing.T) {
	svc, _ := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	srv := createTestServer(t, svc, "S", "s.example.test")
	bad := "dhcp"
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), srv.ID, UpdateServerRequest{LANSource: &bad}); !errors.Is(err, ErrServerLANSourceInvalid) {
		t.Fatalf("err = %v, want ErrServerLANSourceInvalid", err)
	}
}

func TestUpdateServerInterfaceSourceNeedsID(t *testing.T) {
	svc, _ := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	srv := createTestServer(t, svc, "S", "s.example.test")
	src, empty := "interface", "  "
	for _, req := range []UpdateServerRequest{{LANSource: &src}, {LANSource: &src, LANInterfaceID: &empty}} {
		if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), srv.ID, req); !errors.Is(err, ErrServerLANInterfaceRequired) {
			t.Fatalf("err = %v, want ErrServerLANInterfaceRequired", err)
		}
	}
}

func TestServerDTOCarriesLANResolution(t *testing.T) {
	svc, routeStore := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	srv := createTestServer(t, svc, "S", "s.example.test")
	if srv.LAN.IdentityIPs == nil || srv.LAN.Interfaces == nil {
		t.Fatalf("slices must not be nil: %+v", srv.LAN)
	}
	list := `[{"id":"if-1","id_kind":"name","name":"eth0","ipv4":["192.0.2.10"],"virtual":false,"preferred":true}]`
	if err := routeStore.UpdateServerLANInterfaces(ctx, srv.ID, list); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetServer(ctx, systemAdminToken(), srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LAN.Reported || len(got.LAN.Interfaces) != 1 || got.LAN.Interfaces[0].Name != "eth0" {
		t.Fatalf("lan = %+v", got.LAN)
	}
}

func TestUpdateServerTypedLANAddressSwitchesToManual(t *testing.T) {
	svc, _ := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	srv := createTestServer(t, svc, "S", "s.example.test")
	lan := "192.0.2.7"
	got, err := svc.UpdateServer(context.Background(), systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &lan})
	if err != nil || got.LANSource != "manual" || got.LAN.Source != "manual" || got.LAN.Address != lan {
		t.Fatalf("got = %+v, %v", got, err)
	}
}

func TestUpdateServerClearedLANAddressSwitchesToAuto(t *testing.T) {
	svc, _ := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	srv := createTestServer(t, svc, "S", "s.example.test")
	lan, empty := "192.0.2.7", ""
	ctx := context.Background()
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &lan}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &empty})
	if err != nil || got.LANSource != "auto" || got.LANAddress != "" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	// Clearing under interface keeps the source.
	src, id := "interface", "if-1"
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANSource: &src, LANInterfaceID: &id}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &empty})
	if err != nil || got.LANSource != "interface" {
		t.Fatalf("got = %+v, %v", got, err)
	}
}

func TestCreateServerWithLANAddressIsManual(t *testing.T) {
	svc, _ := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	lan := "192.0.2.8"
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{testAdminGroupID}, LANAddress: &lan,
	})
	if err != nil || dto.LANSource != "manual" || dto.LAN.Source != "manual" {
		t.Fatalf("dto = %+v, %v", dto, err)
	}
}

func TestUpdateServerRejectsNetbirdIPAsManualLANAddress(t *testing.T) {
	svc, routeStore := newServerTestService(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	srv := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpdateServerNetbirdState(ctx, srv.ID, "s.example.test", "peer-1", "100.64.0.5", true); err != nil {
		t.Fatal(err)
	}
	same := "100.64.0.5"
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &same}); !errors.Is(err, ErrServerLANAddressInvalid) {
		t.Fatalf("err = %v, want ErrServerLANAddressInvalid", err)
	}
	other := "100.64.0.6"
	if _, err := svc.UpdateServer(ctx, systemAdminToken(), srv.ID, UpdateServerRequest{LANAddress: &other}); err != nil {
		t.Fatalf("other CGNAT address must stay allowed: %v", err)
	}
}

// serverAddressChanges is a ClusterStatusReader whose orchestration holds
// address changes per server.
type serverAddressChanges struct {
	fakeClusterStatus
	byServer map[string][]ServerLANAddressChangeDTO
}

func (r serverAddressChanges) AddressChangesForServer(serverID string) []ServerLANAddressChangeDTO {
	return slices.Clone(r.byServer[serverID])
}

// TestServerDTOHidesHeadNameWithoutSpecAccess pins the server's address
// changes: every change the orchestration holds for the server, in its
// order, naming the head (spec id and gateway model) only for a reader who
// may see the head's spec -- for a reader who sees the server but not the
// head, and for a head that is gone, the head is anonymous: no spec id, no
// name. Without a reader the list is empty, not null.
func TestServerDTOHidesHeadNameWithoutSpecAccess(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID)))

	got, err := f.svc.GetServer(f.ctx, ownerToken(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LANAddressChanges == nil || len(got.LANAddressChanges) != 0 {
		t.Fatalf("changes without a reader = %#v, want an empty list", got.LANAddressChanges)
	}

	at := clusterTestNow.Add(time.Minute)
	changes := []ServerLANAddressChangeDTO{
		{HeadSpecID: head.ID, From: "192.0.2.10", To: "192.0.2.11", At: at, Relaunched: true},
		{HeadSpecID: "rspec_gone", From: "192.0.2.9", To: "192.0.2.10", At: at.Add(-time.Minute), Relaunched: true},
	}
	f.svc.SetClusterStatusReader(serverAddressChanges{byServer: map[string][]ServerLANAddressChangeDTO{a.ID: changes}})
	// usr_other may see server A, but not the head's server and so not its spec.
	if err := f.routes.SetServerOwners(f.ctx, a.ID, []string{"usr_owner", "usr_other"}); err != nil {
		t.Fatal(err)
	}

	got, err = f.svc.GetServer(f.ctx, ownerToken(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	named := slices.Clone(changes)
	named[0].HeadName = "big"
	named[1].HeadSpecID = ""
	if !reflect.DeepEqual(got.LANAddressChanges, named) {
		t.Fatalf("owner changes = %+v, want %+v", got.LANAddressChanges, named)
	}
	anonymous := slices.Clone(changes)
	for i := range anonymous {
		anonymous[i].HeadSpecID = ""
	}
	other, err := f.svc.GetServer(f.ctx, otherToken(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(other.LANAddressChanges, anonymous) {
		t.Fatalf("other changes = %+v, want anonymous heads", other.LANAddressChanges)
	}
	list, err := f.svc.ListServers(f.ctx, otherToken())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list.Data, func(s ServerDTO) bool { return s.ID == a.ID })
	if i < 0 || !reflect.DeepEqual(list.Data[i].LANAddressChanges, anonymous) {
		t.Fatalf("listed servers = %+v, want A with anonymous heads", list.Data)
	}
}
