// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var rpcPolicyNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// rpcPolicyEnv is a gateway with NetBird policy management on (deny-by-default
// enforced) against a fake NetBird, and helpers to store RPC clusters directly.
type rpcPolicyEnv struct {
	t      *testing.T
	ctx    context.Context
	svc    *Service
	routes *routing.MemoryStore
	fake   *fakeNetbird

	mu       sync.Mutex
	notified int
}

func newRPCPolicyEnv(t *testing.T, managePolicies bool) *rpcPolicyEnv {
	t.Helper()
	svc, routes := newNetbirdServerTestService(t, rpcPolicyNow)
	fake := newFakeNetbird(t)
	if managePolicies {
		enableNetbirdPolicies(t, svc, fake.srv.URL, "all", true, true)
	} else {
		enableNetbird(t, svc, fake.srv.URL, true)
	}
	e := &rpcPolicyEnv{t: t, ctx: context.Background(), svc: svc, routes: routes, fake: fake}
	svc.agentFeatures = fakeAgentFeatures{}
	svc.SetRuntimeConfigChangedHook(func(string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.notified++
	})
	return e
}

func (e *rpcPolicyEnv) notifications() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.notified
}

// group is the tracking group of a server seeded by server.
func rpcGroup(serverID string) string { return "g-" + serverID }

// rpcPolicyAdminGroup links every server of the env, so each member's
// default consent admits every head.
const rpcPolicyAdminGroup = "ugrp_rpc_policy"

// server seeds a NetBird-managed server whose cluster derives: a mesh IP, an
// agent that declares rpc_cluster, RPC membership on with the env's consent,
// and a head and a member binary of one build.
func (e *rpcPolicyEnv) server(id string) {
	e.t.Helper()
	seedManagedNetbirdServer(e.t, e.routes, id, id, rpcGroup(id), "", rpcPolicyNow)
	srv, err := e.routes.AIServerByID(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	srv.RPCMemberEnabled = true
	if err := e.routes.UpdateAIServer(e.ctx, srv); err != nil {
		e.t.Fatal(err)
	}
	ip := fmt.Sprintf("100.64.0.%d", len(e.svc.agentFeatures.(fakeAgentFeatures))+1)
	if err := e.routes.UpdateServerNetbirdState(e.ctx, id, srv.Domain, srv.NetbirdPeerID, ip, true); err != nil {
		e.t.Fatal(err)
	}
	if err := e.routes.SetServerAdminGroup(e.ctx, id, rpcPolicyAdminGroup); err != nil {
		e.t.Fatal(err)
	}
	for path, kind := range map[string]string{"/opt/llama/llama-server": BinaryKindLlamaServer, "/opt/llama/ggml-rpc-server": BinaryKindRPCServer} {
		if err := e.routes.UpsertManualServerBinary(e.ctx, routing.ServerBinary{
			ServerID: id, Path: path, Kind: kind, Version: "version: 1 (build 1, commit abc1234)", Build: "1", Commit: "abc1234",
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	e.svc.agentFeatures.(fakeAgentFeatures)[id] = []string{AgentFeatureRPCCluster}
}

// agentApp returns serverID's server_agent application, seeding one first.
func (e *rpcPolicyEnv) agentApp(serverID string) routing.Application {
	e.t.Helper()
	apps, err := e.routes.ApplicationsByServer(e.ctx, serverID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range apps {
		if a.Type == routing.ProviderServerAgent {
			return a
		}
	}
	return seedServerAgentApplication(e.t, e.routes, serverID, rpcPolicyNow)
}

type rpcMemberSeed struct {
	server string
	port   int
}

// cluster stores an enabled cluster head on headServer (with a server_agent
// application, mapping and spec) and its membership, and returns the head
// spec id and the mapping id.
func (e *rpcPolicyEnv) cluster(headServer, network, transport string, members ...rpcMemberSeed) (specID, mappingID string) {
	e.t.Helper()
	app := e.agentApp(headServer)
	mapping, err := e.svc.CreateMapping(e.ctx, systemToken(), app.ID, CreateMappingRequest{GatewayModelName: "big-" + headServer, AppModelName: "big"})
	if err != nil {
		e.t.Fatalf("mapping: %v", err)
	}
	specID = "rspec_" + headServer
	if err := e.routes.UpsertRuntimeSpec(e.ctx, routing.RuntimeSpec{
		ID: specID, MappingID: mapping.ID, Enabled: true, Binary: "/opt/llama/llama-server", Args: "[]", Env: "{}",
		ClusterEnabled: true, ClusterNetwork: network, RPCTransport: transport,
		CreatedAt: rpcPolicyNow, UpdatedAt: rpcPolicyNow,
	}); err != nil {
		e.t.Fatalf("spec: %v", err)
	}
	e.setMembers(specID, members...)
	return specID, mapping.ID
}

func (e *rpcPolicyEnv) setMembers(specID string, members ...rpcMemberSeed) {
	e.t.Helper()
	rows := make([]routing.ClusterMember, len(members))
	for i, m := range members {
		e.agentApp(m.server)
		rows[i] = routing.ClusterMember{HeadSpecID: specID, Position: i, ServerID: m.server, Port: m.port}
	}
	if err := e.routes.SetClusterMembers(e.ctx, specID, rows, nil); err != nil {
		e.t.Fatalf("members: %v", err)
	}
}

func (e *rpcPolicyEnv) reconcile() {
	e.t.Helper()
	e.svc.reconcileRPCPolicies(e.ctx)
}

// seedRPCReadPolicy adds a read-shape op-gw-rpc policy for the grants.
func seedRPCReadPolicy(f *fakeNetbird, id, serverID string, rules ...rpcRule) {
	desc := managedPolicyDescription(netbirdRPCPolicyPurpose)
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]any{
			"enabled": true, "description": desc, "action": "accept", "bidirectional": false, "protocol": "tcp",
			"ports":        []any{strconv.Itoa(r.port)},
			"sources":      []any{map[string]any{"id": r.sourceGroupID, "name": r.sourceGroupID}},
			"destinations": []any{map[string]any{"id": r.destGroupID, "name": r.destGroupID}},
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, map[string]any{"id": id, "name": NetbirdRPCPolicyPrefix + serverID, "description": desc, "enabled": true, "rules": out})
}

// policyGrants decodes the rules of a recorded write-shape policy body into
// "source>destination:port" strings, sorted.
func policyGrants(t *testing.T, body map[string]any) []string {
	t.Helper()
	rules, _ := body["rules"].([]any)
	var out []string
	for _, raw := range rules {
		r, _ := raw.(map[string]any)
		if r["action"] != "accept" || r["protocol"] != "tcp" || r["bidirectional"] != false || r["enabled"] != true {
			t.Fatalf("rule shape = %v", r)
		}
		src, _ := r["sources"].([]any)
		dst, _ := r["destinations"].([]any)
		ports, _ := r["ports"].([]any)
		if len(src) != 1 || len(dst) != 1 || len(ports) != 1 {
			t.Fatalf("rule must be single source/destination/port: %v", r)
		}
		out = append(out, src[0].(string)+">"+dst[0].(string)+":"+ports[0].(string))
	}
	sort.Strings(out)
	return out
}

func TestRPCPolicyPrefixDoesNotCollideWithAccessPrefix(t *testing.T) {
	if strings.HasPrefix(NetbirdRPCPolicyPrefix, NetbirdManagedPolicyPrefix) || strings.HasPrefix(NetbirdManagedPolicyPrefix, NetbirdRPCPolicyPrefix) {
		t.Fatalf("%q starts with %q: the status counter would count RPC policies", NetbirdRPCPolicyPrefix, NetbirdManagedPolicyPrefix)
	}
}

func TestRPCPolicyCreatedPerMemberServerWithExactPorts(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a", "b"} {
		e.server(id)
	}
	// Direct: the member's RPC port. Relay: the member's relay port. Either
	// way the row's Port is what the head dials.
	e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001}, rpcMemberSeed{"b", 47055})
	e.reconcile()

	a := e.fake.createdPolicyByName("op-gw-rpc-a")
	if a == nil {
		t.Fatal("no policy created for member a")
	}
	if got := policyGrants(t, a); !slices.Equal(got, []string{"g-head>g-a:47001"}) {
		t.Fatalf("a grants = %v", got)
	}
	if got := policyGrants(t, e.fake.createdPolicyByName("op-gw-rpc-b")); !slices.Equal(got, []string{"g-head>g-b:47055"}) {
		t.Fatalf("b grants = %v", got)
	}
	if a["enabled"] != true || a["description"] != managedPolicyDescription(netbirdRPCPolicyPurpose) {
		t.Fatalf("policy header = %v", a)
	}
	if e.fake.policyCreateCountByName("op-gw-rpc-head") != 0 {
		t.Fatal("the head server must not get a policy")
	}
}

func TestRPCPolicyRelayClusterUsesRelayPort(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47301})
	e.reconcile()
	if got := policyGrants(t, e.fake.createdPolicyByName("op-gw-rpc-a")); !slices.Equal(got, []string{"g-head>g-a:47301"}) {
		t.Fatalf("grants = %v", got)
	}
}

func TestRPCPolicyUpdatedOnPortChangeAndLeftAloneWhenMatching(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	spec, _ := e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
	seedRPCReadPolicy(e.fake, "pol-a", "a", rpcRule{"g-head", "g-a", 47001})

	e.reconcile()
	if e.fake.policyUpdateCount() != 0 || e.fake.policyCreateCountByName("op-gw-rpc-a") != 0 {
		t.Fatalf("a matching policy was touched: updates=%d", e.fake.policyUpdateCount())
	}

	e.setMembers(spec, rpcMemberSeed{"a", 47009})
	e.reconcile()
	upd := e.fake.updatedPolicyByName("op-gw-rpc-a")
	if upd == nil {
		t.Fatal("port change did not update the policy")
	}
	if got := policyGrants(t, upd); !slices.Equal(got, []string{"g-head>g-a:47009"}) {
		t.Fatalf("updated grants = %v", got)
	}
}

func TestRPCPolicyTwoMembersOnOneServerAreTwoRules(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"h1", "h2", "a"} {
		e.server(id)
	}
	e.cluster("h1", "mesh", "direct", rpcMemberSeed{"a", 47001})
	e.cluster("h2", "mesh", "relay", rpcMemberSeed{"a", 47002})
	e.reconcile()
	if n := e.fake.policyCreateCountByName("op-gw-rpc-a"); n != 1 {
		t.Fatalf("policy creates for a = %d, want 1", n)
	}
	want := []string{"g-h1>g-a:47001", "g-h2>g-a:47002"}
	if got := policyGrants(t, e.fake.createdPolicyByName("op-gw-rpc-a")); !slices.Equal(got, want) {
		t.Fatalf("grants = %v, want %v", got, want)
	}
}

func TestRPCPolicyDriftIsUpdatedAndOrphansSwept(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
	// A stale policy with an extra rule is rewritten to exactly the desired set.
	seedRPCReadPolicy(e.fake, "pol-a", "a", rpcRule{"g-head", "g-a", 47001}, rpcRule{"g-evil", "g-a", 47001})
	// A policy of a server with no mesh member is an orphan.
	seedRPCReadPolicy(e.fake, "pol-ghost", "ghost", rpcRule{"g-head", "g-ghost", 47002})
	// A manual policy outside the prefix is never touched.
	e.fake.seedPolicy("pol-access", "op-gw-access-a", true, []string{"9000"}, "g-gw", "g-a")

	e.reconcile()

	if got := policyGrants(t, e.fake.updatedPolicyByName("op-gw-rpc-a")); !slices.Equal(got, []string{"g-head>g-a:47001"}) {
		t.Fatalf("drifted policy not restored: %v", got)
	}
	if !e.fake.wasPolicyDeleted("pol-ghost") {
		t.Fatal("orphan op-gw-rpc policy was not swept")
	}
	if e.fake.wasPolicyDeleted("pol-access") || e.fake.wasPolicyDeleted("pol-a") {
		t.Fatal("a live or foreign policy was deleted")
	}
}

func TestRPCPolicyMatchesDriftTable(t *testing.T) {
	desc := managedPolicyDescription(netbirdRPCPolicyPurpose)
	rules := []rpcRule{{"gh", "ga", 47001}}
	base := func() netbird.Policy {
		return netbird.Policy{Name: "op-gw-rpc-a", Description: desc, Enabled: true, Rules: []netbird.PolicyRule{{
			Enabled: true, Description: desc, Action: "accept", Protocol: "tcp", Ports: []string{"47001"},
			Sources:      []netbird.GroupRef{{ID: "gh"}},
			Destinations: []netbird.GroupRef{{ID: "ga"}},
		}}}
	}
	if !rpcPolicyMatches(base(), rules) {
		t.Fatal("the desired policy must match")
	}
	cases := map[string]func(*netbird.Policy){
		"disabled policy": func(p *netbird.Policy) { p.Enabled = false },
		"disabled rule":   func(p *netbird.Policy) { p.Rules[0].Enabled = false },
		"bidirectional":   func(p *netbird.Policy) { p.Rules[0].Bidirectional = true },
		"udp":             func(p *netbird.Policy) { p.Rules[0].Protocol = "udp" },
		"drop action":     func(p *netbird.Policy) { p.Rules[0].Action = "drop" },
		"other port":      func(p *netbird.Policy) { p.Rules[0].Ports = []string{"47002"} },
		"extra port":      func(p *netbird.Policy) { p.Rules[0].Ports = []string{"47001", "47002"} },
		"port range":      func(p *netbird.Policy) { p.Rules[0].PortRanges = []netbird.PortRange{{Start: 1, End: 65535}} },
		"other source":    func(p *netbird.Policy) { p.Rules[0].Sources = []netbird.GroupRef{{ID: "gx"}} },
		"extra destination": func(p *netbird.Policy) {
			p.Rules[0].Destinations = append(p.Rules[0].Destinations, netbird.GroupRef{ID: "gx"})
		},
		"edited description": func(p *netbird.Policy) { p.Description = "mine" },
		"edited rule desc":   func(p *netbird.Policy) { p.Rules[0].Description = "mine" },
		"extra rule":         func(p *netbird.Policy) { p.Rules = append(p.Rules, p.Rules[0]) },
		"no rules":           func(p *netbird.Policy) { p.Rules = nil },
		"non-numeric port":   func(p *netbird.Policy) { p.Rules[0].Ports = []string{"http"} },
		"swapped direction": func(p *netbird.Policy) {
			p.Rules[0].Sources, p.Rules[0].Destinations = p.Rules[0].Destinations, p.Rules[0].Sources
		},
	}
	for name, mutate := range cases {
		p := base()
		mutate(&p)
		if rpcPolicyMatches(p, rules) {
			t.Errorf("%s: drift accepted", name)
		}
	}
}

func TestRPCPolicyLanClusterGetsNoPolicy(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "lan", "direct", rpcMemberSeed{"a", 47001})
	// A stale policy for that member goes away: the cluster is not on the mesh.
	seedRPCReadPolicy(e.fake, "pol-a", "a", rpcRule{"g-head", "g-a", 47001})
	e.reconcile()
	if e.fake.policyCreateCount() != 0 || e.fake.policyUpdateCount() != 0 {
		t.Fatal("a lan cluster produced a policy write")
	}
	if !e.fake.wasPolicyDeleted("pol-a") {
		t.Fatal("the policy of a member without a mesh cluster was not removed")
	}
}

func TestRPCPolicyUntouchedWithoutManagePolicies(t *testing.T) {
	e := newRPCPolicyEnv(t, false)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
	seedRPCReadPolicy(e.fake, "pol-ghost", "ghost", rpcRule{"g-head", "g-ghost", 47002})

	e.reconcile()
	e.svc.reconcileAllServerPolicies(e.ctx)
	e.svc.ReconcileAllServerNetbird(e.ctx)

	if e.fake.policyListCount() != 0 || e.fake.policyCreateCount() != 0 || e.fake.deletedPolicyCount() != 0 {
		t.Fatalf("policies touched with manage-policies off: lists=%d creates=%d deletes=%d",
			e.fake.policyListCount(), e.fake.policyCreateCount(), e.fake.deletedPolicyCount())
	}
}

func TestRPCPolicyFamilyRunsInBothFleetPasses(t *testing.T) {
	// ReconcileAllServerNetbird lists once more for the deny-by-default
	// enforcement that follows its policy section.
	passes := map[string]struct {
		run   func(*Service, context.Context)
		lists int32
	}{
		"reconcileAllServerPolicies": {(*Service).reconcileAllServerPolicies, 1},
		"ReconcileAllServerNetbird":  {(*Service).ReconcileAllServerNetbird, 2},
	}
	for name, pass := range passes {
		t.Run(name, func(t *testing.T) {
			e := newRPCPolicyEnv(t, true)
			for _, id := range []string{"head", "a"} {
				e.server(id)
			}
			e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
			seedRPCReadPolicy(e.fake, "pol-ghost", "ghost", rpcRule{"g-head", "g-ghost", 47002})
			listsBefore := e.fake.policyListCount()

			pass.run(e.svc, e.ctx)

			if got := policyGrants(t, e.fake.createdPolicyByName("op-gw-rpc-a")); !slices.Equal(got, []string{"g-head>g-a:47001"}) {
				t.Fatalf("grants = %v", got)
			}
			if !e.fake.wasPolicyDeleted("pol-ghost") {
				t.Fatal("orphan not swept")
			}
			if lists := e.fake.policyListCount() - listsBefore; lists != pass.lists {
				t.Fatalf("the pass listed policies %d times, want %d (one shared snapshot for the families)", lists, pass.lists)
			}
		})
	}
}

func TestRPCPolicyHookRunsOnClusterWritesAndDeletes(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	_, mapping := e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
	seedRPCReadPolicy(e.fake, "pol-a", "a", rpcRule{"g-head", "g-a", 47001})

	if err := e.svc.DeleteRuntimeSpec(e.ctx, systemToken(), mapping); err != nil {
		t.Fatalf("DeleteRuntimeSpec: %v", err)
	}
	if !e.fake.wasPolicyDeleted("pol-a") {
		t.Fatal("deleting the head did not remove its members' policy synchronously")
	}
}

func TestRPCPolicyHookNeverFailsWhenNetbirdIsDown(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	_, mapping := e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
	e.fake.failListPolicies = true
	if err := e.svc.DeleteRuntimeSpec(e.ctx, systemToken(), mapping); err != nil {
		t.Fatalf("DeleteRuntimeSpec must not fail on a NetBird error: %v", err)
	}
}

func TestRPCPolicyMemberWithoutTrackingGroupGetsNoRule(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	e.server("head")
	if err := e.routes.CreateAIServer(e.ctx, routing.AIServer{ID: "plain", Name: "plain", Status: routing.ServerStatusActive, CreatedAt: rpcPolicyNow, UpdatedAt: rpcPolicyNow}); err != nil {
		t.Fatal(err)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"plain", 47001})
	e.reconcile()
	if e.fake.policyCreateCount() != 0 {
		t.Fatal("a member without a tracking group got a policy")
	}
}

func TestSettingsRefuseSwitchOffWhileDirectMeshClusterExists(t *testing.T) {
	offs := map[string]UpdateSystemSettingsRequest{
		"manage policies":     {NetbirdManagePolicies: boolPtr(false)},
		"deny by default":     {NetbirdDenyByDefault: boolPtr(false)},
		"deny enforcement":    {NetbirdDenyByDefaultEnforce: boolPtr(false)},
		"manage with another": {NetbirdManagePolicies: boolPtr(false), Theme: strPtr("light")},
	}
	for name, req := range offs {
		t.Run(name, func(t *testing.T) {
			e := newRPCPolicyEnv(t, true)
			for _, id := range []string{"head", "a"} {
				e.server(id)
			}
			e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
			before := e.svc.systemSettingValues(e.ctx)

			_, err := e.svc.UpdateSystemSettings(e.ctx, systemToken(), req)
			e.svc.waitPolicySideEffects()
			if !errors.Is(err, ErrNetbirdDirectClusterExists) {
				t.Fatalf("err = %v, want ErrNetbirdDirectClusterExists", err)
			}
			after := e.svc.systemSettingValues(e.ctx)
			for k, v := range before {
				if after[k] != v {
					t.Fatalf("setting %s changed from %q to %q on a refused save", k, v, after[k])
				}
			}
		})
	}
}

// TestSettingsSwitchOffCheckFailsOnUnreadableSettings: the guard that keeps
// a direct mesh cluster's protection on reads the current switches strictly.
// A failed read is the save's error, never every switch read as off, which
// would let the save turn the protection off under a running cluster.
func TestSettingsSwitchOffCheckFailsOnUnreadableSettings(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
	e.svc.settings = &failingSettings{SystemSettingsStore: e.svc.settings, fail: true}
	err := e.svc.checkDirectClusterSwitches(e.ctx, UpdateSystemSettingsRequest{NetbirdManagePolicies: boolPtr(false)})
	if err == nil || errors.Is(err, ErrNetbirdDirectClusterExists) {
		t.Fatalf("err = %v, want the settings read error", err)
	}
}

func TestSettingsAllowSwitchOffWithoutDirectMeshCluster(t *testing.T) {
	setup := map[string]func(*rpcPolicyEnv){
		"no cluster":    func(*rpcPolicyEnv) {},
		"relay cluster": func(e *rpcPolicyEnv) { e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001}) },
		"lan cluster":   func(e *rpcPolicyEnv) { e.cluster("head", "lan", "direct", rpcMemberSeed{"a", 47001}) },
	}
	for name, seed := range setup {
		t.Run(name, func(t *testing.T) {
			e := newRPCPolicyEnv(t, true)
			for _, id := range []string{"head", "a"} {
				e.server(id)
			}
			seed(e)
			if _, err := e.svc.UpdateSystemSettings(e.ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdManagePolicies: boolPtr(false)}); err != nil {
				t.Fatalf("switch-off refused: %v", err)
			}
			e.svc.waitPolicySideEffects()
			if NetbirdManagePolicies(e.svc.systemSettingValues(e.ctx)) {
				t.Fatal("manage_policies still on")
			}
		})
	}
}

func TestSettingsSwitchAlreadyOffIsNotRefused(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
	// Saving the switch as it already is (or on) must pass even with the cluster.
	if _, err := e.svc.UpdateSystemSettings(e.ctx, systemToken(), UpdateSystemSettingsRequest{NetbirdManagePolicies: boolPtr(true), NetbirdDenyByDefault: boolPtr(true)}); err != nil {
		t.Fatalf("keeping the switches on was refused: %v", err)
	}
	e.svc.waitPolicySideEffects()
}

func TestDefaultPolicyStateRecordedAndClustersNotifiedOnChange(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
	e.fake.seedDefaultPolicy("pol-default", false)

	state := func() (string, time.Time) { return e.svc.NetbirdDefaultPolicyStateView(e.ctx) }
	e.svc.ReconcileAllServerNetbird(e.ctx)
	if got, at := state(); got != NetbirdDefaultPolicyDisabled || at.IsZero() {
		t.Fatalf("state = %q at %v after a pass that saw Default disabled", got, at)
	}

	// Unchanged state: no notification, no rewrite.
	before := e.notifications()
	e.svc.ReconcileAllServerNetbird(e.ctx)
	if e.notifications() != before {
		t.Fatal("an unchanged Default state notified the clusters")
	}

	// Default turned back on out of band: the next pass records it and the
	// head/member documents are re-derived.
	e.fake.mu.Lock()
	for _, p := range e.fake.policies {
		if p["name"] == "Default" {
			p["enabled"] = true
		}
	}
	e.fake.mu.Unlock()
	before = e.notifications()
	e.svc.reconcileAllServerPolicies(e.ctx)
	if got, _ := state(); got != NetbirdDefaultPolicyEnabled {
		t.Fatalf("state = %q, want enabled", got)
	}
	if e.notifications() == before {
		t.Fatal("a changed Default state did not notify the cluster servers")
	}
	if direct, err := e.svc.directMeshAvailable(e.ctx); err != nil || direct {
		t.Fatal("direct mesh must be unavailable while Default is enabled")
	}
}

func TestDefaultPolicyStateRecordedByEveryPass(t *testing.T) {
	passes := map[string]func(*rpcPolicyEnv){
		"single server": func(e *rpcPolicyEnv) { e.svc.reconcileServerPolicy(e.ctx, "head") },
		"rpc hook":      func(e *rpcPolicyEnv) { e.reconcile() },
		"fleet policy":  func(e *rpcPolicyEnv) { e.svc.reconcileAllServerPolicies(e.ctx) },
	}
	for name, pass := range passes {
		t.Run(name, func(t *testing.T) {
			e := newRPCPolicyEnv(t, true)
			e.server("head")
			e.fake.seedDefaultPolicy("pol-default", true)
			pass(e)
			if got, _ := e.svc.NetbirdDefaultPolicyStateView(e.ctx); got != NetbirdDefaultPolicyEnabled {
				t.Fatalf("state = %q, want enabled", got)
			}
		})
	}
}

// Enforcing deny-by-default flips Default off; the recorded state follows the
// flip instead of keeping what the snapshot showed.
func TestDefaultPolicyStateFollowsDenyByDefaultFlip(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	e.server("head")
	e.fake.seedDefaultPolicy("pol-default", true)
	e.svc.ReconcileAllServerNetbird(e.ctx)
	if e.fake.policyUpdateCount() == 0 {
		t.Fatal("deny-by-default enforcement did not disable Default")
	}
	if got, _ := e.svc.NetbirdDefaultPolicyStateView(e.ctx); got != NetbirdDefaultPolicyDisabled {
		t.Fatalf("state = %q, want disabled after the flip", got)
	}
}

func TestDefaultPolicyStateAbsentPolicyCountsAsDisabled(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	e.server("head")
	e.reconcile()
	if got, _ := e.svc.NetbirdDefaultPolicyStateView(e.ctx); got != NetbirdDefaultPolicyDisabled {
		t.Fatalf("state = %q, want disabled when no Default policy exists", got)
	}
}

func TestRPCPolicyDuplicateNamedPoliciesAreCollapsed(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
	seedRPCReadPolicy(e.fake, "pol-a1", "a", rpcRule{"g-head", "g-a", 47001})
	seedRPCReadPolicy(e.fake, "pol-a2", "a", rpcRule{"g-evil", "g-a", 47001})
	e.reconcile()
	if !e.fake.wasPolicyDeleted("pol-a2") || e.fake.wasPolicyDeleted("pol-a1") {
		t.Fatal("the duplicate was not the only policy removed")
	}
	if e.fake.policyUpdateCount() != 0 {
		t.Fatal("the kept matching policy was rewritten")
	}
}

// The deny-by-default flip records only the final state: one notification
// burst for enabled -> disabled, never a transient write in between.
func TestDenyByDefaultFlipRecordsOnlyFinalState(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
	e.fake.seedDefaultPolicy("pol-default", true)

	n0 := e.notifications()
	e.svc.reconcileAllServerPolicies(e.ctx) // records enabled: one burst
	burst := e.notifications() - n0
	if got, _ := e.svc.NetbirdDefaultPolicyStateView(e.ctx); got != NetbirdDefaultPolicyEnabled || burst == 0 {
		t.Fatalf("setup: state %q burst %d", got, burst)
	}

	n1 := e.notifications()
	e.svc.applyDenyByDefault(e.ctx, netbird.Config{URL: e.fake.srv.URL, Token: "nbtok"}, true)
	if got, _ := e.svc.NetbirdDefaultPolicyStateView(e.ctx); got != NetbirdDefaultPolicyDisabled {
		t.Fatalf("state = %q, want disabled", got)
	}
	if got := e.notifications() - n1; got != burst {
		t.Fatalf("flip notified %d times, want one burst of %d", got, burst)
	}
}

// driftRecorder is a ClusterStatusReader that also handles the mesh drift: it
// records how many runtime documents were already notified when it ran, and
// holds the drift's stop until gate closes.
type driftRecorder struct {
	fakeClusterStatus
	e       *rpcPolicyEnv
	gate    chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	calls   []int
}

func (d *driftRecorder) StopDirectMeshClusters(context.Context) {
	d.mu.Lock()
	d.calls = append(d.calls, d.e.notifications())
	d.mu.Unlock()
	d.entered <- struct{}{}
	<-d.gate
}

func (d *driftRecorder) recorded() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// TestDefaultPolicyDriftStopsDirectClustersBeforeNotifying pins the drift: once
// a pass sees NetBird's Default policy enabled again, the gateway stops its
// direct mesh clusters, head first, BEFORE the documents that withhold them
// are pushed -- so no member is removed under a running head -- and it does
// so off the policy lock: the pass returns, and another policy pass runs,
// while the stop is under way. A pass that sees Default disabled stops
// nothing.
func TestDefaultPolicyDriftStopsDirectClustersBeforeNotifying(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "direct", rpcMemberSeed{"a", 47001})
	e.fake.seedDefaultPolicy("pol-default", false)
	d := &driftRecorder{e: e, gate: make(chan struct{}), entered: make(chan struct{}, 4)}
	e.svc.SetClusterStatusReader(d)
	e.svc.ReconcileAllServerNetbird(e.ctx)
	e.svc.waitPolicySideEffects()
	if calls := d.recorded(); len(calls) != 0 {
		t.Fatalf("drift handler calls = %v, want none while Default is disabled", calls)
	}

	e.fake.mu.Lock()
	for _, p := range e.fake.policies {
		if p["name"] == "Default" {
			p["enabled"] = true
		}
	}
	e.fake.mu.Unlock()
	passDone := make(chan struct{})
	go func() {
		e.svc.reconcileAllServerPolicies(e.ctx)
		close(passDone)
	}()
	select {
	case <-d.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the drift handler was not called")
	}
	select {
	case <-passDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the policy pass waited for the drift's cluster stops")
	}
	second := make(chan struct{})
	go func() {
		e.svc.reconcileAllServerPolicies(e.ctx)
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("a policy pass was blocked while the drift's cluster stops ran")
	}
	atHandler := d.recorded()[0]
	close(d.gate)
	e.svc.waitPolicySideEffects()
	if calls := d.recorded(); len(calls) != 1 {
		t.Fatalf("drift handler calls = %v, want one", calls)
	}
	if e.notifications() <= atHandler {
		t.Fatalf("notifications = %d after the drift, want more than the %d seen when the clusters were stopped",
			e.notifications(), atHandler)
	}
}

// TestRPCPolicyPassOnlyForClusterWrites: a write that touches no cluster makes
// no RPC policy pass, so a deployment without clusters never waits on NetBird
// for a model save. The writes that also reconcile the server's own access
// policy list once for that, never a second time.
func TestRPCPolicyPassOnlyForClusterWrites(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	e.server("plain")
	app := e.agentApp("plain")
	mapping, err := e.svc.CreateMapping(e.ctx, systemToken(), app.ID, CreateMappingRequest{GatewayModelName: "small", AppModelName: "small"})
	if err != nil {
		t.Fatal(err)
	}
	lists := func(what string, want int32, op func() error) {
		t.Helper()
		before := e.fake.policyListCount()
		if err := op(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := e.fake.policyListCount() - before; got != want {
			t.Fatalf("%s listed policies %d times, want %d", what, got, want)
		}
	}
	lists("PutRuntimeSpec", 0, func() error {
		_, err := e.svc.PutRuntimeSpec(e.ctx, systemToken(), mapping.ID, PutRuntimeSpecRequest{Enabled: true, Binary: "/opt/llama/llama-server"})
		return err
	})
	lists("DeleteRuntimeSpec", 0, func() error { return e.svc.DeleteRuntimeSpec(e.ctx, systemToken(), mapping.ID) })
	lists("UpdateApplication", 1, func() error {
		port := 9001
		_, err := e.svc.UpdateApplication(e.ctx, systemToken(), app.ID, UpdateApplicationRequest{Port: &port})
		return err
	})
	lists("DeleteApplication", 1, func() error { return e.svc.DeleteApplication(e.ctx, systemToken(), app.ID) })
	lists("DeleteServer", 1, func() error {
		_, err := e.svc.DeleteServer(e.ctx, systemToken(), "plain", false)
		return err
	})
}

// TestRPCPolicyPassForClusterApplicationWrites: an application write on a
// server that takes part in a cluster still reconciles the RPC policies.
func TestRPCPolicyPassForClusterApplicationWrites(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 47001})
	apps, err := e.routes.ApplicationsByServer(e.ctx, "head")
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps = %+v err=%v", apps, err)
	}
	before := e.fake.policyListCount()
	port := 9001
	if _, err := e.svc.UpdateApplication(e.ctx, systemToken(), apps[0].ID, UpdateApplicationRequest{Port: &port}); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.policyListCount() - before; got != 2 {
		t.Fatalf("UpdateApplication on a head server listed policies %d times, want 2 (access policy + RPC family)", got)
	}
}

// TestRPCPolicyWithheldClusterGetsNoGrant: a cluster the runtime documents
// withhold runs nowhere, so its member's port is not opened -- here after
// the member turned RPC membership off.
func TestRPCPolicyWithheldClusterGetsNoGrant(t *testing.T) {
	e := newRPCPolicyEnv(t, true)
	for _, id := range []string{"head", "a"} {
		e.server(id)
	}
	e.cluster("head", "mesh", "relay", rpcMemberSeed{"a", 26001})
	seedRPCReadPolicy(e.fake, "pol-a", "a", rpcRule{"g-head", "g-a", 26001})
	srv, err := e.routes.AIServerByID(e.ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	srv.RPCMemberEnabled = false
	if err := e.routes.UpdateAIServer(e.ctx, srv); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if !e.fake.wasPolicyDeleted("pol-a") {
		t.Fatal("the grant of a withheld cluster's member was kept")
	}
	if e.fake.policyCreateCountByName("op-gw-rpc-a") != 0 {
		t.Fatal("a withheld cluster's member got a policy")
	}
}
