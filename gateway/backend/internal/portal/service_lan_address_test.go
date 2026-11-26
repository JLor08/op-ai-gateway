// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"testing"
)

// lanIfacesJSON is the stored lan_interfaces value for views.
func lanIfacesJSON(t *testing.T, views ...LANInterfaceView) string {
	t.Helper()
	if views == nil {
		views = []LANInterfaceView{}
	}
	raw, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// lanInterfaces stores the agent-reported list of serverID through the
// targeted writer, as the telemetry ingest does.
func (f *clusterFixture) lanInterfaces(serverID string, views ...LANInterfaceView) {
	f.t.Helper()
	if err := f.routes.UpdateServerLANInterfaces(f.ctx, serverID, lanIfacesJSON(f.t, views...)); err != nil {
		f.t.Fatalf("lan interfaces: %v", err)
	}
}

var (
	lanEth0 = LANInterfaceView{ID: "k-eth0", IDKind: "keyed", Name: "eth0", IPv4: []string{"192.0.2.10", "192.0.2.11"}}
	lanEth1 = LANInterfaceView{ID: "k-eth1", IDKind: "keyed", Name: "eth1", IPv4: []string{"198.51.100.20"}, Preferred: true}
	lanDock = LANInterfaceView{ID: "k-docker0", IDKind: "keyed", Name: "docker0", IPv4: []string{"10.0.0.9"}, Virtual: true}
	lanWLAN = LANInterfaceView{ID: "k-wlan0", IDKind: "keyed", Name: "wlan0", IPv4: []string{"203.0.113.5"}}
)

func TestLANResolutionOf(t *testing.T) {
	report := func(views ...LANInterfaceView) string { return lanIfacesJSON(t, views...) }
	nameKind := func(v LANInterfaceView) LANInterfaceView {
		v.ID, v.IDKind = v.Name, "name"
		return v
	}
	for _, tc := range []struct {
		name string
		srv  routing.AIServer
		want LANResolution
	}{
		{
			name: "legacy row with lan_address reads as manual",
			srv:  routing.AIServer{LANAddress: "192.0.2.5", Domain: "a.example.test"},
			want: LANResolution{Source: "manual", Address: "192.0.2.5", IdentityIPs: []string{"192.0.2.5"}},
		},
		{
			name: "legacy row without lan_address reads as auto and keeps the Domain",
			srv:  routing.AIServer{Domain: "a.example.test"},
			want: LANResolution{Source: "domain"},
		},
		{
			name: "manual ignores the report for the address",
			srv:  routing.AIServer{LANSource: "manual", LANAddress: "192.0.2.5", LANInterfaces: report(lanEth0, lanEth1)},
			want: LANResolution{
				Source: "manual", Address: "192.0.2.5", IdentityIPs: []string{"192.0.2.5"},
				Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth0, lanEth1},
			},
		},
		{
			name: "manual without an address falls to the Domain like today",
			srv:  routing.AIServer{LANSource: "manual", Domain: "a.example.test", LANInterfaces: report(lanEth1)},
			want: LANResolution{Source: "domain", Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth1}},
		},
		{
			name: "manual without an address on a NetBird server has none",
			srv:  routing.AIServer{LANSource: "manual", NetbirdEnabled: true, Domain: "a.netbird.test", LANInterfaces: report(lanEth1)},
			want: LANResolution{Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth1}},
		},
		{
			name: "interface found by id",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "k-eth0", Domain: "a.example.test", LANInterfaces: report(lanDock, lanEth0, lanEth1)},
			want: LANResolution{
				Source: "interface", Address: "192.0.2.10", InterfaceID: "k-eth0", InterfaceName: "eth0",
				IdentityIPs: []string{"10.0.0.9", "192.0.2.10", "192.0.2.11", "198.51.100.20"},
				Reported:    true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanDock, lanEth0, lanEth1},
			},
		},
		{
			name: "interface missing falls back to the preferred one",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "k-gone", Domain: "a.example.test", LANInterfaces: report(lanEth0, lanEth1)},
			want: LANResolution{
				Source: "auto", Address: "198.51.100.20", InterfaceID: "k-eth1", InterfaceName: "eth1", Fallback: true,
				IdentityIPs: []string{"192.0.2.10", "192.0.2.11", "198.51.100.20"},
				Reported:    true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth0, lanEth1},
			},
		},
		{
			name: "interface missing without a preferred one takes the first physical one",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "k-gone", NetbirdEnabled: true, LANInterfaces: report(lanDock, lanWLAN)},
			want: LANResolution{
				Source: "auto", Address: "203.0.113.5", InterfaceID: "k-wlan0", InterfaceName: "wlan0", Fallback: true,
				IdentityIPs: []string{"10.0.0.9", "203.0.113.5"},
				Reported:    true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanDock, lanWLAN},
			},
		},
		{
			name: "interface missing with no candidate keeps a non-NetBird Domain",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "k-gone", Domain: "a.example.test", LANInterfaces: report(lanDock)},
			want: LANResolution{Source: "domain", Fallback: true, Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanDock}},
		},
		{
			name: "interface missing with no candidate on a NetBird server has none",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "k-gone", NetbirdEnabled: true, Domain: "a.netbird.test"},
			want: LANResolution{Fallback: true},
		},
		{
			name: "a name-kind stored id resolves by name after the agent gains a key",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "eth0", NetbirdEnabled: true, LANInterfaces: report(lanEth0, lanEth1)},
			want: LANResolution{
				Source: "interface", Address: "192.0.2.10", InterfaceID: "k-eth0", InterfaceName: "eth0",
				IdentityIPs: []string{"192.0.2.10", "192.0.2.11", "198.51.100.20"},
				Reported:    true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth0, lanEth1},
			},
		},
		{
			name: "name-kind ids without a key",
			srv:  routing.AIServer{LANSource: "interface", LANInterfaceID: "eth0", NetbirdEnabled: true, LANInterfaces: report(nameKind(lanEth0), lanEth1)},
			want: LANResolution{
				Source: "interface", Address: "192.0.2.10", InterfaceID: "eth0", InterfaceName: "eth0",
				IdentityIPs: []string{"192.0.2.10", "192.0.2.11", "198.51.100.20"},
				Reported:    true, Interfaces: []LANInterfaceView{nameKind(lanEth0), lanEth1},
			},
		},
		{
			name: "auto on a NetBird server with no report has none",
			srv:  routing.AIServer{LANSource: "auto", NetbirdEnabled: true, Domain: "a.netbird.test"},
			want: LANResolution{},
		},
		{
			name: "auto on a non-NetBird server with a Domain keeps the Domain despite a report",
			srv:  routing.AIServer{LANSource: "auto", Domain: "a.example.test", LANInterfaces: report(lanEth0, lanEth1)},
			want: LANResolution{Source: "domain", Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth0, lanEth1}},
		},
		{
			name: "auto on a NetBird server takes the preferred interface",
			srv:  routing.AIServer{LANSource: "auto", NetbirdEnabled: true, LANInterfaces: report(lanEth0, lanEth1)},
			want: LANResolution{
				Source: "auto", Address: "198.51.100.20", InterfaceID: "k-eth1", InterfaceName: "eth1",
				IdentityIPs: []string{"192.0.2.10", "192.0.2.11", "198.51.100.20"},
				Reported:    true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanEth0, lanEth1},
			},
		},
		{
			name: "auto without a Domain and only virtual interfaces has none",
			srv:  routing.AIServer{LANSource: "auto", LANInterfaces: report(lanDock)},
			want: LANResolution{Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{lanDock}},
		},
		{
			name: "a reported empty list",
			srv:  routing.AIServer{LANSource: "auto", NetbirdEnabled: true, LANInterfaces: "[]"},
			want: LANResolution{Reported: true, KeyedIDs: true, Interfaces: []LANInterfaceView{}},
		},
		{
			name: "an unreadable stored list reads as not reported",
			srv:  routing.AIServer{LANSource: "auto", NetbirdEnabled: true, LANInterfaces: "{not json"},
			want: LANResolution{},
		},
		{
			name: "NetBird addresses never become candidates",
			srv: routing.AIServer{LANSource: "auto", NetbirdEnabled: true, NetbirdIP: "198.51.100.20", LANInterfaces: report(
				LANInterfaceView{ID: "k-wt0", IDKind: "keyed", Name: "wt0", IPv4: []string{"100.64.0.7"}, Preferred: true},
				lanEth1,
				LANInterfaceView{ID: "k-eth2", IDKind: "keyed", Name: "eth2", IPv4: []string{"100.100.0.1", "192.0.2.30", "not-an-ip"}},
			)},
			want: LANResolution{
				Source: "auto", Address: "192.0.2.30", InterfaceID: "k-eth2", InterfaceName: "eth2",
				IdentityIPs: []string{"192.0.2.30"},
				Reported:    true, KeyedIDs: true,
				Interfaces: []LANInterfaceView{{ID: "k-eth2", IDKind: "keyed", Name: "eth2", IPv4: []string{"192.0.2.30"}}},
			},
		},
		{
			name: "identity ips are deduplicated and sorted numerically",
			srv: routing.AIServer{LANSource: "auto", NetbirdEnabled: true, LANInterfaces: report(
				LANInterfaceView{ID: "k-a", IDKind: "keyed", Name: "a", IPv4: []string{"10.0.0.10", "10.0.0.9"}, Preferred: true},
				LANInterfaceView{ID: "k-b", IDKind: "keyed", Name: "b", IPv4: []string{"10.0.0.9", "10.0.0.2"}},
			)},
			want: LANResolution{
				Source: "auto", Address: "10.0.0.10", InterfaceID: "k-a", InterfaceName: "a",
				IdentityIPs: []string{"10.0.0.2", "10.0.0.9", "10.0.0.10"},
				Reported:    true, KeyedIDs: true,
				Interfaces: []LANInterfaceView{
					{ID: "k-a", IDKind: "keyed", Name: "a", IPv4: []string{"10.0.0.10", "10.0.0.9"}, Preferred: true},
					{ID: "k-b", IDKind: "keyed", Name: "b", IPv4: []string{"10.0.0.9", "10.0.0.2"}},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LANResolutionOf(tc.srv); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("LANResolutionOf =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

// TestLANResolutionReadsOnlyTheStoredRow: after a gateway restart, before
// any agent reconnects, the resolution and the derived cluster come from the
// persisted list alone.
func TestLANResolutionReadsOnlyTheStoredRow(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.updateServer(a.ID, func(s *routing.AIServer) { s.NetbirdEnabled, s.LANSource = true, routing.LANSourceAuto })
	f.lanInterfaces(a.ID, lanEth0, lanEth1)

	stored, err := f.routes.AIServerByID(f.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res := LANResolutionOf(stored); res.Address != "198.51.100.20" || res.Source != "auto" || !res.Reported {
		t.Fatalf("resolution from the stored row = %+v", res)
	}
	f.clusterAgents(a.ID)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))
	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if want := fmt.Sprintf("198.51.100.20:%d", f.memberPort(head.ID, a.ID)); !ok || hs.Cluster.Endpoints[0].Address != want {
		t.Fatalf("head cluster = %+v, want endpoint %s", hs.Cluster, want)
	}
}

func TestBindOnlyMemberWithheldWhenInterfaceMissing(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	f.updateServer(a.ID, func(s *routing.AIServer) {
		s.LANSource, s.LANInterfaceID, s.LANBindOnly = routing.LANSourceInterface, "k-eth0", true
	})
	f.lanInterfaces(a.ID, lanEth0, lanEth1)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))
	port := f.memberPort(head.ID, a.ID)

	hs, ok := specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster.Endpoints[0].Address != fmt.Sprintf("192.0.2.10:%d", port) {
		t.Fatalf("head cluster with the chosen interface = %+v", hs.Cluster)
	}
	if ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID)); !ok || ms.RPC.BindIP != "192.0.2.10" {
		t.Fatalf("member rpc with the chosen interface = %+v", ms.RPC)
	}

	// The chosen interface leaves the report: a bind-only member is withheld,
	// never moved to the fallback interface.
	f.lanInterfaces(a.ID, lanEth1)
	if hs, ok := specByID(f.config(f.head.ID), head.ID); ok {
		t.Fatalf("head pushed with cluster %+v while the chosen interface is missing", hs.Cluster)
	}
	if got := rpcServerSpecs(f.config(a.ID)); len(got) != 0 {
		t.Fatalf("bind-only member pushed while its interface is missing: %+v", got)
	}
	if got := f.withheldReason(); got != ClusterWithheldLANInterfaceMissing {
		t.Fatalf("reason = %q, want %q", got, ClusterWithheldLANInterfaceMissing)
	}

	// Without any candidate the reason stays the missing interface.
	f.updateServer(a.ID, func(s *routing.AIServer) { s.NetbirdEnabled = true })
	f.lanInterfaces(a.ID)
	if got := f.withheldReason(); got != ClusterWithheldLANInterfaceMissing {
		t.Fatalf("reason without candidates = %q, want %q", got, ClusterWithheldLANInterfaceMissing)
	}

	// A member listening on every interface uses the fallback.
	f.updateServer(a.ID, func(s *routing.AIServer) { s.NetbirdEnabled, s.LANBindOnly = false, false })
	f.lanInterfaces(a.ID, lanEth1)
	hs, ok = specByID(f.config(f.head.ID), head.ID)
	if !ok || hs.Cluster.Endpoints[0].Address != fmt.Sprintf("198.51.100.20:%d", port) {
		t.Fatalf("head cluster on the fallback = %+v", hs.Cluster)
	}
	if ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID)); !ok || ms.RPC.BindIP != "0.0.0.0" {
		t.Fatalf("member rpc on the fallback = %+v", ms.RPC)
	}

	// The interface returns: the choice applies again.
	f.lanInterfaces(a.ID, lanEth0, lanEth1)
	if hs, ok = specByID(f.config(f.head.ID), head.ID); !ok || hs.Cluster.Endpoints[0].Address != fmt.Sprintf("192.0.2.10:%d", port) {
		t.Fatalf("head cluster after the interface returned = %+v", hs.Cluster)
	}
}

func TestHeadIdentityCarriesAllReportedIPv4s(t *testing.T) {
	f := newClusterFixture(t)
	a := f.member("A")
	f.clusterAgents(a.ID)
	f.netbirdReady(f.head.ID, "100.64.0.1")
	f.updateServer(f.head.ID, func(s *routing.AIServer) { s.LANSource = routing.LANSourceAuto })
	wt0 := LANInterfaceView{ID: "k-wt0", IDKind: "keyed", Name: "wt0", IPv4: []string{"100.64.0.1"}, Virtual: true}
	f.lanInterfaces(f.head.ID, lanEth0, lanEth1, wt0, lanWLAN)
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0)))

	ms, ok := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	want := []string{"192.0.2.10", "192.0.2.11", "198.51.100.20", "203.0.113.5"}
	if !ok || !slices.Equal(ms.RPC.HeadIPs, want) || ms.RPC.HeadDomain != "" {
		t.Fatalf("member rpc = %+v, want head ips %v", ms.RPC, want)
	}

	// A manual head sends exactly its manual address.
	f.updateServer(f.head.ID, func(s *routing.AIServer) { s.LANSource, s.LANAddress = routing.LANSourceManual, "192.0.2.99" })
	ms, ok = specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
	if !ok || !slices.Equal(ms.RPC.HeadIPs, []string{"192.0.2.99"}) || ms.RPC.HeadDomain != "" {
		t.Fatalf("member rpc with a manual head = %+v", ms.RPC)
	}
}

// TestExistingManualAndDomainSetupsDeriveUnchanged: a pre-migration row
// (lan_source "") and its backfilled form derive the same cluster, and an
// agent that starts reporting interfaces changes nothing for a manual server
// or a non-NetBird server on its Domain.
func TestExistingManualAndDomainSetupsDeriveUnchanged(t *testing.T) {
	f := newClusterFixture(t)
	a, b := f.member("A"), f.member("B")
	f.clusterAgents(a.ID, b.ID)
	f.updateServer(a.ID, func(s *routing.AIServer) { s.LANAddress, s.LANBindOnly = "192.0.2.5", true })
	head := f.mustPut(clusterHeadRequest(autoMember(a.ID, 0), autoMember(b.ID, 0)))
	pa, pb := f.memberPort(head.ID, a.ID), f.memberPort(head.ID, b.ID)
	type derived struct {
		endpoints  []AgentClusterEndpointDTO
		generation string
		rpcA, rpcB AgentRPCMemberDTO
	}
	derive := func() derived {
		t.Helper()
		hs, ok := specByID(f.config(f.head.ID), head.ID)
		ma, okA := specByID(f.config(a.ID), automaticMemberSpecID(head.ID, a.ID))
		mb, okB := specByID(f.config(b.ID), automaticMemberSpecID(head.ID, b.ID))
		if !ok || !okA || !okB || hs.Cluster == nil || ma.RPC == nil || mb.RPC == nil {
			t.Fatalf("cluster withheld: reason %q", f.withheldReason())
		}
		return derived{hs.Cluster.Endpoints, hs.Cluster.Generation, *ma.RPC, *mb.RPC}
	}

	before := derive()
	wantEndpoints := []AgentClusterEndpointDTO{
		{Address: fmt.Sprintf("192.0.2.5:%d", pa), DeviceCount: 1},
		{Address: fmt.Sprintf("B.example.test:%d", pb), DeviceCount: 1},
	}
	if !slices.Equal(before.endpoints, wantEndpoints) || before.rpcA.BindIP != "192.0.2.5" || before.rpcB.HeadDomain != "head.example.test" {
		t.Fatalf("pre-change derivation = %+v", before)
	}

	// The migration backfill, then every agent reporting interfaces.
	f.updateServer(a.ID, func(s *routing.AIServer) { s.LANSource = routing.LANSourceManual })
	f.updateServer(b.ID, func(s *routing.AIServer) { s.LANSource = routing.LANSourceAuto })
	f.updateServer(f.head.ID, func(s *routing.AIServer) { s.LANSource = routing.LANSourceAuto })
	for _, id := range []string{a.ID, b.ID, f.head.ID} {
		f.lanInterfaces(id, lanEth0, lanEth1)
	}
	if after := derive(); !reflect.DeepEqual(after, before) {
		t.Fatalf("derivation changed:\n before %+v\n after  %+v", before, after)
	}
}
