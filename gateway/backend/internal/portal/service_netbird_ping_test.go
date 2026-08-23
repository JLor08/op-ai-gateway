// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pingReadPolicy builds a READ-shape netbird.Policy for the ICMP ping-allow matchers:
// one accept/icmp/non-bidirectional PORTLESS rule with the given source + destination
// group ids. name is the policy name (op-gw-ping-gateway / op-gw-ping-servers), which
// also selects the matching managed-policy Description set on both the policy and its
// rule so a baseline "matching" case is a true match; a caller testing
// description-drift overrides it explicitly.
func pingReadPolicy(name string, enabled bool, sourceIDs, destIDs []string) netbird.Policy {
	toRefs := func(ids []string) []netbird.GroupRef {
		out := make([]netbird.GroupRef, 0, len(ids))
		for _, id := range ids {
			out = append(out, netbird.GroupRef{ID: id, Name: id})
		}
		return out
	}
	var desc string
	switch name {
	case netbirdPingGatewayPolicyName:
		desc = managedPolicyDescription(netbirdPingGatewayPolicyPurpose)
	case netbirdPingServersPolicyName:
		desc = managedPolicyDescription(netbirdPingServersPolicyPurpose)
	}
	return netbird.Policy{
		Name:        name,
		Description: desc,
		Enabled:     enabled,
		Rules: []netbird.PolicyRule{{
			Enabled:       true,
			Description:   desc,
			Action:        "accept",
			Bidirectional: false,
			Protocol:      "icmp",
			Sources:       toRefs(sourceIDs),
			Destinations:  toRefs(destIDs),
		}},
	}
}

// enableNetbirdPingPolicies enables the NetBird module + policy management AND the two
// account-wide ping switches in one settings write, then waits for the side effects.
func enableNetbirdPingPolicies(t *testing.T, svc *Service, url, scope string, allowGateway, allowAllServers bool) {
	t.Helper()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled:             boolPtr(true),
		NetbirdURL:                 strPtr(url),
		NetbirdGroups:              &[]string{"gateways"},
		NetbirdToken:               strPtr("nbtok"),
		NetbirdManagePolicies:      boolPtr(true),
		NetbirdPolicyScope:         strPtr(scope),
		NetbirdAllowPingGateway:    boolPtr(allowGateway),
		NetbirdAllowPingAllServers: boolPtr(allowAllServers),
	}); err != nil {
		t.Fatalf("configure netbird ping policies: %v", err)
	}
	svc.waitPolicySideEffects()
}

// seedPingServer creates a NetBird-enabled server with a tracking group + per-server
// allow-ping flag directly in the store.
func seedPingServer(t *testing.T, routeStore *routing.MemoryStore, id, name, trackingGroupID string, allowPing bool, now time.Time) {
	t.Helper()
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{
		ID: id, Name: name, Status: routing.ServerStatusActive,
		NetbirdEnabled: true, NetbirdGroupID: trackingGroupID, NetbirdAllowPing: allowPing,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", id, err)
	}
}

// ---- group-set helpers (pure) --------------------------------------------------

func TestNetbirdServerGroupIDs(t *testing.T) {
	servers := []routing.AIServer{
		{NetbirdEnabled: true, NetbirdGroupID: "g2"},
		{NetbirdEnabled: true, NetbirdGroupID: "g1"},
		{NetbirdEnabled: true, NetbirdGroupID: "g2"},  // duplicate -> collapsed
		{NetbirdEnabled: false, NetbirdGroupID: "g9"}, // not NetBird -> excluded
		{NetbirdEnabled: true, NetbirdGroupID: ""},    // no group -> excluded
	}
	if got := netbirdServerGroupIDs(servers); !reflect.DeepEqual(got, []string{"g1", "g2"}) {
		t.Fatalf("netbirdServerGroupIDs = %v, want [g1 g2] (sorted, deduped, filtered)", got)
	}
}

func TestNetbirdPingServerGroupIDs(t *testing.T) {
	servers := []routing.AIServer{
		{NetbirdEnabled: true, NetbirdGroupID: "g2", NetbirdAllowPing: true},
		{NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdAllowPing: false},
		{NetbirdEnabled: true, NetbirdGroupID: "g3", NetbirdAllowPing: true},
		{NetbirdEnabled: false, NetbirdGroupID: "g9", NetbirdAllowPing: true}, // not NetBird -> excluded
		{NetbirdEnabled: true, NetbirdGroupID: "", NetbirdAllowPing: true},    // no group -> excluded
	}
	// allowAll=false -> only per-server flagged groups.
	if got := netbirdPingServerGroupIDs(servers, false); !reflect.DeepEqual(got, []string{"g2", "g3"}) {
		t.Fatalf("netbirdPingServerGroupIDs(allowAll=false) = %v, want [g2 g3] (flagged only)", got)
	}
	// allowAll=true -> every NetBird server with a group, regardless of the flag.
	if got := netbirdPingServerGroupIDs(servers, true); !reflect.DeepEqual(got, []string{"g1", "g2", "g3"}) {
		t.Fatalf("netbirdPingServerGroupIDs(allowAll=true) = %v, want [g1 g2 g3]", got)
	}
}

// ---- matchers ------------------------------------------------------------------

func TestPingGatewayPolicyMatches(t *testing.T) {
	const gw = "gw-portal"
	want := []string{"g1", "g2"}

	// Baseline: exact match (source order reversed to prove set-insensitivity).
	if !pingGatewayPolicyMatches(pingReadPolicy(netbirdPingGatewayPolicyName, true, []string{"g2", "g1"}, []string{gw}), gw, want) {
		t.Fatalf("expected a matching ping-gateway policy to match (set-insensitive sources)")
	}

	// A stray port -> mismatch (the rule must stay PORTLESS).
	withPort := pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{gw})
	withPort.Rules[0].Ports = []string{"8081"}
	if pingGatewayPolicyMatches(withPort, gw, want) {
		t.Fatalf("a ping-gateway rule carrying a port must NOT match (icmp is portless)")
	}
	// A port_range -> mismatch.
	withRange := pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{gw})
	withRange.Rules[0].PortRanges = []netbird.PortRange{{Start: 1, End: 2}}
	if pingGatewayPolicyMatches(withRange, gw, want) {
		t.Fatalf("a ping-gateway rule carrying a port_range must NOT match")
	}

	cases := []struct {
		name string
		p    netbird.Policy
	}{
		{"top-level disabled", pingReadPolicy(netbirdPingGatewayPolicyName, false, want, []string{gw})},
		{"wrong destination", pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{"other"})},
		{"missing a source", pingReadPolicy(netbirdPingGatewayPolicyName, true, []string{"g1"}, []string{gw})},
		{"extra source", pingReadPolicy(netbirdPingGatewayPolicyName, true, []string{"g1", "g2", "g3"}, []string{gw})},
	}
	for _, c := range cases {
		if pingGatewayPolicyMatches(c.p, gw, want) {
			t.Fatalf("%s: expected a mismatch", c.name)
		}
	}

	// protocol tcp / bidirectional / multi-rule drift.
	tcp := pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{gw})
	tcp.Rules[0].Protocol = "tcp"
	if pingGatewayPolicyMatches(tcp, gw, want) {
		t.Fatalf("a tcp rule must NOT match (ping is icmp)")
	}
	bidi := pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{gw})
	bidi.Rules[0].Bidirectional = true
	if pingGatewayPolicyMatches(bidi, gw, want) {
		t.Fatalf("a bidirectional rule must NOT match")
	}
	twoRules := pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{gw})
	twoRules.Rules = append(twoRules.Rules, twoRules.Rules[0])
	if pingGatewayPolicyMatches(twoRules, gw, want) {
		t.Fatalf("a 2-rule policy must NOT match (managed policy is single-rule)")
	}
}

func TestPingServersPolicyMatches(t *testing.T) {
	const gw = "gw-portal"
	want := []string{"g1", "g2"}

	// Baseline: exact match (destination order reversed to prove set-insensitivity).
	if !pingServersPolicyMatches(pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, []string{"g2", "g1"}), gw, want) {
		t.Fatalf("expected a matching ping-servers policy to match (set-insensitive destinations)")
	}

	// A stray port / port_range -> mismatch.
	withPort := pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, want)
	withPort.Rules[0].Ports = []string{"8081"}
	if pingServersPolicyMatches(withPort, gw, want) {
		t.Fatalf("a ping-servers rule carrying a port must NOT match (icmp is portless)")
	}

	cases := []struct {
		name string
		p    netbird.Policy
	}{
		{"top-level disabled", pingReadPolicy(netbirdPingServersPolicyName, false, []string{gw}, want)},
		{"wrong source", pingReadPolicy(netbirdPingServersPolicyName, true, []string{"other"}, want)},
		{"missing a destination", pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, []string{"g1"})},
		{"extra destination", pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, []string{"g1", "g2", "g3"})},
	}
	for _, c := range cases {
		if pingServersPolicyMatches(c.p, gw, want) {
			t.Fatalf("%s: expected a mismatch", c.name)
		}
	}

	tcp := pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, want)
	tcp.Rules[0].Protocol = "tcp"
	if pingServersPolicyMatches(tcp, gw, want) {
		t.Fatalf("a tcp rule must NOT match (ping is icmp)")
	}
	bidi := pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, want)
	bidi.Rules[0].Bidirectional = true
	if pingServersPolicyMatches(bidi, gw, want) {
		t.Fatalf("a bidirectional rule must NOT match")
	}
}

// TestManagedPoliciesCarryDescription (op-gw-ping-gateway / op-gw-ping-servers): the
// desired PolicyRequest built by desiredPingGatewayPolicyRequest and
// desiredPingServersPolicyRequest carries the English "Managed by the OP AI Gateway"
// Description on both the policy and its sole rule.
func TestManagedPoliciesCarryDescriptionPing(t *testing.T) {
	gwReq := desiredPingGatewayPolicyRequest("gw-portal", []string{"g1", "g2"})
	if !strings.Contains(gwReq.Description, "Managed by the OP AI Gateway") {
		t.Fatalf("ping-gateway policy description missing: %q", gwReq.Description)
	}
	if len(gwReq.Rules) == 0 || !strings.Contains(gwReq.Rules[0].Description, "Managed by the OP AI Gateway") {
		t.Fatalf("ping-gateway rule description missing")
	}

	srvReq := desiredPingServersPolicyRequest("gw-portal", []string{"g1", "g2"})
	if !strings.Contains(srvReq.Description, "Managed by the OP AI Gateway") {
		t.Fatalf("ping-servers policy description missing: %q", srvReq.Description)
	}
	if len(srvReq.Rules) == 0 || !strings.Contains(srvReq.Rules[0].Description, "Managed by the OP AI Gateway") {
		t.Fatalf("ping-servers rule description missing")
	}
}

// TestManagedPolicyDescriptionDriftIsCorrected (op-gw-ping-gateway / op-gw-ping-servers):
// a stored policy identical except Description="" (policy AND rule) must NOT match ->
// the reconcile issues an UpdatePolicy that restores the description.
func TestManagedPolicyDescriptionDriftIsCorrectedPing(t *testing.T) {
	const gw = "gw-portal"
	want := []string{"g1", "g2"}

	gwStored := pingReadPolicy(netbirdPingGatewayPolicyName, true, want, []string{gw})
	gwStored.Description = ""
	gwStored.Rules[0].Description = ""
	if pingGatewayPolicyMatches(gwStored, gw, want) {
		t.Fatal("a ping-gateway policy with an empty description must be treated as drift")
	}

	srvStored := pingReadPolicy(netbirdPingServersPolicyName, true, []string{gw}, want)
	srvStored.Description = ""
	srvStored.Rules[0].Description = ""
	if pingServersPolicyMatches(srvStored, gw, want) {
		t.Fatal("a ping-servers policy with an empty description must be treated as drift")
	}
}

// ---- reconcilePingGatewayPolicyWith -------------------------------------------

func TestReconcilePingGatewayCreatesWhenAllowedWithSources(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	servers := []routing.AIServer{
		{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1"},
		{ID: "s2", NetbirdEnabled: true, NetbirdGroupID: "g2"},
	}

	svc.reconcilePingGatewayPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingGateway: true}, servers, nil)

	if got := fake.policyCreateCountByName(netbirdPingGatewayPolicyName); got != 1 {
		t.Fatalf("ping-gateway creates = %d, want 1", got)
	}
	body := fake.createdPolicyByName(netbirdPingGatewayPolicyName)
	if body == nil {
		t.Fatalf("no ping-gateway CreatePolicy body recorded")
	}
	if src := sortedCopy(policyRuleField(t, body, "sources")); !reflect.DeepEqual(src, []string{"g1", "g2"}) {
		t.Fatalf("ping-gateway sources = %v, want [g1 g2]", src)
	}
	if dst := policyRuleField(t, body, "destinations"); !reflect.DeepEqual(dst, []string{"gw-portal"}) {
		t.Fatalf("ping-gateway destinations = %v, want [gw-portal]", dst)
	}
	if ports := policyRuleField(t, body, "ports"); len(ports) != 0 {
		t.Fatalf("ping-gateway ports = %v, want none (icmp is portless)", ports)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["action"] != "accept" || rule["protocol"] != "icmp" {
		t.Fatalf("rule action/protocol = %v/%v, want accept/icmp", rule["action"], rule["protocol"])
	}
	if bidi, _ := rule["bidirectional"].(bool); bidi {
		t.Fatalf("rule bidirectional = true, want false")
	}
}

func TestReconcilePingGatewayDeletesWhenDisabled(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	servers := []routing.AIServer{{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1"}}
	existing := []netbird.Policy{pingReadPolicy(netbirdPingGatewayPolicyName, true, []string{"g1"}, []string{"gw-portal"})}
	existing[0].ID = "pol-pg"

	// Allow OFF (sources present) -> delete.
	svc.reconcilePingGatewayPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingGateway: false}, servers, existing)

	if !fake.wasPolicyDeleted("pol-pg") {
		t.Fatalf("ping-gateway policy not deleted when the allow switch is OFF")
	}
	if fake.policyCreateCountByName(netbirdPingGatewayPolicyName) != 0 || fake.policyUpdateCount() != 0 {
		t.Fatalf("unexpected create/update on the delete path")
	}
}

func TestReconcilePingGatewayDeletesWhenNoSources(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	existing := []netbird.Policy{pingReadPolicy(netbirdPingGatewayPolicyName, true, []string{"g1"}, []string{"gw-portal"})}
	existing[0].ID = "pol-pg"

	// Allow ON but NO NetBird servers -> empty sources -> delete.
	svc.reconcilePingGatewayPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingGateway: true}, nil, existing)

	if !fake.wasPolicyDeleted("pol-pg") {
		t.Fatalf("ping-gateway policy not deleted when there are no sources")
	}
}

func TestReconcilePingGatewayUpdatesOnDrift(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	servers := []routing.AIServer{
		{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1"},
		{ID: "s2", NetbirdEnabled: true, NetbirdGroupID: "g2"},
	}

	// Existing MATCHES {g1,g2} -> no update.
	match := []netbird.Policy{pingReadPolicy(netbirdPingGatewayPolicyName, true, []string{"g2", "g1"}, []string{"gw-portal"})}
	match[0].ID = "pol-pg"
	svc.reconcilePingGatewayPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingGateway: true}, servers, match)
	if fake.policyUpdateCount() != 0 || fake.policyCreateCountByName(netbirdPingGatewayPolicyName) != 0 {
		t.Fatalf("matching ping-gateway policy triggered a write (u=%d c=%d)", fake.policyUpdateCount(), fake.policyCreateCountByName(netbirdPingGatewayPolicyName))
	}

	// Existing DISABLED (drift) -> exactly one update (self-heal to enabled).
	drift := []netbird.Policy{pingReadPolicy(netbirdPingGatewayPolicyName, false, []string{"g1", "g2"}, []string{"gw-portal"})}
	drift[0].ID = "pol-pg"
	svc.reconcilePingGatewayPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingGateway: true}, servers, drift)
	if fake.policyUpdateCount() != 1 {
		t.Fatalf("ping-gateway updates = %d, want 1 (self-heal a disabled policy)", fake.policyUpdateCount())
	}
	if body := fake.lastUpdatedPolicy(); body != nil {
		if enabled, _ := body["enabled"].(bool); !enabled {
			t.Fatalf("updated ping-gateway policy enabled = false, want true")
		}
	}
}

// ---- reconcilePingServersPolicyWith -------------------------------------------

func TestReconcilePingServersAllServers(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	// Two servers; only s2 has the per-server flag, but allow-all overrides that.
	servers := []routing.AIServer{
		{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdAllowPing: false},
		{ID: "s2", NetbirdEnabled: true, NetbirdGroupID: "g2", NetbirdAllowPing: true},
	}

	svc.reconcilePingServersPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingAllServers: true}, servers, nil)

	body := fake.createdPolicyByName(netbirdPingServersPolicyName)
	if body == nil {
		t.Fatalf("no ping-servers policy created (allow-all)")
	}
	if src := policyRuleField(t, body, "sources"); !reflect.DeepEqual(src, []string{"gw-portal"}) {
		t.Fatalf("ping-servers sources = %v, want [gw-portal] (gateway group)", src)
	}
	if dst := sortedCopy(policyRuleField(t, body, "destinations")); !reflect.DeepEqual(dst, []string{"g1", "g2"}) {
		t.Fatalf("ping-servers destinations = %v, want [g1 g2] (all server groups)", dst)
	}
	if ports := policyRuleField(t, body, "ports"); len(ports) != 0 {
		t.Fatalf("ping-servers ports = %v, want none (icmp is portless)", ports)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["protocol"] != "icmp" {
		t.Fatalf("rule protocol = %v, want icmp", rule["protocol"])
	}
}

func TestReconcilePingServersPerServerFlagsOnly(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	// allow-all OFF -> only the per-server flagged servers are destinations.
	servers := []routing.AIServer{
		{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdAllowPing: false},
		{ID: "s2", NetbirdEnabled: true, NetbirdGroupID: "g2", NetbirdAllowPing: true},
		{ID: "s3", NetbirdEnabled: true, NetbirdGroupID: "g3", NetbirdAllowPing: true},
	}

	svc.reconcilePingServersPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingAllServers: false}, servers, nil)

	body := fake.createdPolicyByName(netbirdPingServersPolicyName)
	if body == nil {
		t.Fatalf("no ping-servers policy created (per-server flags)")
	}
	if dst := sortedCopy(policyRuleField(t, body, "destinations")); !reflect.DeepEqual(dst, []string{"g2", "g3"}) {
		t.Fatalf("ping-servers destinations = %v, want [g2 g3] (flagged servers only)", dst)
	}
}

func TestReconcilePingServersDeletesWhenEmpty(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	// allow-all OFF + no per-server flags -> empty destinations -> delete.
	servers := []routing.AIServer{{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdAllowPing: false}}
	existing := []netbird.Policy{pingReadPolicy(netbirdPingServersPolicyName, true, []string{"gw-portal"}, []string{"g1"})}
	existing[0].ID = "pol-ps"

	svc.reconcilePingServersPolicyWith(context.Background(), ncfg, NetbirdPolicySettings{AllowPingAllServers: false}, servers, existing)

	if !fake.wasPolicyDeleted("pol-ps") {
		t.Fatalf("ping-servers policy not deleted when the destination set is empty")
	}
	if fake.policyCreateCountByName(netbirdPingServersPolicyName) != 0 {
		t.Fatalf("unexpected ping-servers create on the delete path")
	}
}

// ---- fleet-pass wiring ----------------------------------------------------------

// TestReconcileAllServerNetbirdCreatesPingPolicies proves BOTH ping reconciles are
// hooked into the ReconcileAllServerNetbird fleet pass (not just callable directly).
func TestReconcileAllServerNetbirdCreatesPingPolicies(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPingPolicies(t, svc, fake.srv.URL, "all", true, true)

	seedPingServer(t, routeStore, "srv1", "GPU-1", "g1", false, now)
	seedPingServer(t, routeStore, "srv2", "GPU-2", "g2", false, now)

	svc.ReconcileAllServerNetbird(context.Background())

	if got := fake.policyCreateCountByName(netbirdPingGatewayPolicyName); got != 1 {
		t.Fatalf("op-gw-ping-gateway creates = %d, want 1 (hooked into the fleet pass)", got)
	}
	if got := fake.policyCreateCountByName(netbirdPingServersPolicyName); got != 1 {
		t.Fatalf("op-gw-ping-servers creates = %d, want 1 (hooked into the fleet pass)", got)
	}
	// ping-gateway: server groups -> gateway group.
	pg := fake.createdPolicyByName(netbirdPingGatewayPolicyName)
	if src := sortedCopy(policyRuleField(t, pg, "sources")); !reflect.DeepEqual(src, []string{"g1", "g2"}) {
		t.Fatalf("ping-gateway sources = %v, want [g1 g2]", src)
	}
	// ping-servers: gateway group -> all server groups (allow-all).
	ps := fake.createdPolicyByName(netbirdPingServersPolicyName)
	if dst := sortedCopy(policyRuleField(t, ps, "destinations")); !reflect.DeepEqual(dst, []string{"g1", "g2"}) {
		t.Fatalf("ping-servers destinations = %v, want [g1 g2] (allow-all)", dst)
	}
}

// TestReconcileAllServerPoliciesCreatesPingPolicies proves BOTH ping reconciles are
// also hooked into the reconcileAllServerPolicies fleet pass (the settings-PUT /
// async-fleet path) — NOT only ReconcileAllServerNetbird. Without this, dropping the
// ping hooks from reconcileAllServerPolicies would survive the suite (they would only
// converge at the next Loop-B pass).
func TestReconcileAllServerPoliciesCreatesPingPolicies(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPingPolicies(t, svc, fake.srv.URL, "all", true, true)

	// Seed servers AFTER the enable (the settings-PUT side-effect reconcile above ran
	// with zero servers → zero ping creates), then drive the policy-only fleet pass.
	seedPingServer(t, routeStore, "srv1", "GPU-1", "g1", false, now)
	seedPingServer(t, routeStore, "srv2", "GPU-2", "g2", false, now)

	svc.reconcileAllServerPolicies(context.Background())

	if got := fake.policyCreateCountByName(netbirdPingGatewayPolicyName); got != 1 {
		t.Fatalf("op-gw-ping-gateway creates = %d, want 1 (hooked into reconcileAllServerPolicies)", got)
	}
	if got := fake.policyCreateCountByName(netbirdPingServersPolicyName); got != 1 {
		t.Fatalf("op-gw-ping-servers creates = %d, want 1 (hooked into reconcileAllServerPolicies)", got)
	}
	pg := fake.createdPolicyByName(netbirdPingGatewayPolicyName)
	if src := sortedCopy(policyRuleField(t, pg, "sources")); !reflect.DeepEqual(src, []string{"g1", "g2"}) {
		t.Fatalf("ping-gateway sources = %v, want [g1 g2]", src)
	}
	ps := fake.createdPolicyByName(netbirdPingServersPolicyName)
	if dst := sortedCopy(policyRuleField(t, ps, "destinations")); !reflect.DeepEqual(dst, []string{"g1", "g2"}) {
		t.Fatalf("ping-servers destinations = %v, want [g1 g2] (allow-all)", dst)
	}
}

// TestReconcileAllServerNetbirdNoPingWhenOff proves the ping reconciles are gated on
// the account-wide switches: with both switches OFF, no ping policy is created (the
// agent-ingest policy is still created — orthogonal).
func TestReconcileAllServerNetbirdNoPingWhenOff(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPingPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedPingServer(t, routeStore, "srv1", "GPU-1", "g1", true, now) // per-server flag set, but allow-all off + no ping-gateway

	svc.ReconcileAllServerNetbird(context.Background())

	if got := fake.policyCreateCountByName(netbirdPingGatewayPolicyName); got != 0 {
		t.Fatalf("op-gw-ping-gateway created (%d) though the switch is OFF", got)
	}
	// ping-servers destinations are driven by the per-server flag (s1 is flagged) even
	// with allow-all off, so ping-servers IS created here — assert on ping-gateway only.
}

// ---- SetServerNetbird per-server allow-ping persistence ------------------------

// TestSetServerNetbirdPersistsAllowPing proves the linkage editor writes the passed
// allowPing value on BOTH the enable and disable branches (not hardcoded).
func TestSetServerNetbirdPersistsAllowPing(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	run := func(t *testing.T, enabled, allowPing bool) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		// Seed a NetBird server WITH a domain (the disable path requires one) and the
		// OPPOSITE allow-ping value, so a successful write is observable.
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{
			ID: "srvP", Name: "P", Domain: "p.local", Status: routing.ServerStatusActive,
			NetbirdEnabled: true, NetbirdAllowPing: !allowPing, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		dto, err := svc.SetServerNetbird(ctx, systemToken(), "srvP", enabled, "", nil, false, "", allowPing, false)
		if err != nil {
			t.Fatalf("SetServerNetbird(enabled=%v, allowPing=%v): %v", enabled, allowPing, err)
		}
		svc.waitPolicySideEffects()
		if dto.NetbirdAllowPing != allowPing {
			t.Fatalf("DTO NetbirdAllowPing = %v, want %v", dto.NetbirdAllowPing, allowPing)
		}
		got, err := routeStore.AIServerByID(ctx, "srvP")
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if got.NetbirdAllowPing != allowPing {
			t.Fatalf("stored NetbirdAllowPing = %v, want %v (enabled=%v)", got.NetbirdAllowPing, allowPing, enabled)
		}
	}

	t.Run("enable_allow_true", func(t *testing.T) { run(t, true, true) })
	t.Run("enable_allow_false", func(t *testing.T) { run(t, true, false) })
	t.Run("disable_allow_true", func(t *testing.T) { run(t, false, true) })
	t.Run("disable_allow_false", func(t *testing.T) { run(t, false, false) })
}

// TestNetbirdPingServerGroupIDsExclude proves the per-server ping opt-out: when the
// account-wide allow-all is ON, a NetbirdPingExclude server's group is dropped from the
// op-gw-ping-servers destination set; when allow-all is OFF, only NetbirdAllowPing
// servers are included (an excluded, allow=false server is absent either way).
func TestNetbirdPingServerGroupIDsExclude(t *testing.T) {
	servers := []routing.AIServer{
		{NetbirdEnabled: true, NetbirdGroupID: "g1", NetbirdAllowPing: true},   // opt-in
		{NetbirdEnabled: true, NetbirdGroupID: "g2", NetbirdPingExclude: true}, // opt-out (allow=false, normalized)
		{NetbirdEnabled: true, NetbirdGroupID: "g3"},                           // default
		{NetbirdEnabled: false, NetbirdGroupID: "g9", NetbirdAllowPing: true},  // not NetBird -> excluded
		{NetbirdEnabled: true, NetbirdGroupID: "", NetbirdAllowPing: true},     // no group -> excluded
	}
	// allowAll=true -> every NetBird+grouped server EXCEPT the opt-out (g2).
	if got := netbirdPingServerGroupIDs(servers, true); !reflect.DeepEqual(got, []string{"g1", "g3"}) {
		t.Fatalf("netbirdPingServerGroupIDs(allowAll=true) = %v, want [g1 g3] (opt-out g2 excluded)", got)
	}
	// allowAll=false -> only per-server allow-flagged servers (g1); an excluded/default
	// server is absent even without the opt-out taking effect (allow=false).
	if got := netbirdPingServerGroupIDs(servers, false); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Fatalf("netbirdPingServerGroupIDs(allowAll=false) = %v, want [g1] (allow-flagged only)", got)
	}
}

// TestSetServerNetbirdPingMutualExclusion proves the linkage editor normalizes the two
// ping flags to a clean 3-state: a both-true request resolves to "never" (exclude wins,
// allow forced false); allow=true+exclude=false leaves allow set + exclude clear.
func TestSetServerNetbirdPingMutualExclusion(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	run := func(t *testing.T, allowPing, pingExclude, wantAllow, wantExclude bool) {
		svc, routeStore := newNetbirdServerTestService(t, now)
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{
			ID: "srvX", Name: "X", Domain: "x.local", Status: routing.ServerStatusActive,
			NetbirdEnabled: true, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if _, err := svc.SetServerNetbird(ctx, systemToken(), "srvX", true, "", nil, false, "", allowPing, pingExclude); err != nil {
			t.Fatalf("SetServerNetbird(allowPing=%v, pingExclude=%v): %v", allowPing, pingExclude, err)
		}
		svc.waitPolicySideEffects()
		got, err := routeStore.AIServerByID(ctx, "srvX")
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if got.NetbirdAllowPing != wantAllow || got.NetbirdPingExclude != wantExclude {
			t.Fatalf("stored allow=%v exclude=%v, want allow=%v exclude=%v",
				got.NetbirdAllowPing, got.NetbirdPingExclude, wantAllow, wantExclude)
		}
	}

	// both true -> exclude wins, allow forced false.
	t.Run("both_true_exclude_wins", func(t *testing.T) { run(t, true, true, false, true) })
	// allow only -> allow stays, exclude clear.
	t.Run("allow_only", func(t *testing.T) { run(t, true, false, true, false) })
}
