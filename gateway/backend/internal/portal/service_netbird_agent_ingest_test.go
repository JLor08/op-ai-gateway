// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedAgentIngestReadPolicy adds a READ-shape op-gw-agent-ingest policy: one rule
// with the given enabled state, ports, source tracking-group ids, and the single
// destination (gateway group). The Description is set to the desired managed-policy
// description (matching desiredAgentIngestPolicyRequest) so an otherwise-matching
// seeded policy is a true match; a test asserting description-drift overrides
// "description" on the returned map afterward. Used to seed a "matching" / "stale"
// / "disabled" agent-ingest policy the fleet reconcile should leave / update /
// re-enable.
func seedAgentIngestReadPolicy(f *fakeNetbird, id string, enabled bool, ports, sourceIDs []string, destID string) {
	rulePorts := make([]any, 0, len(ports))
	for _, p := range ports {
		rulePorts = append(rulePorts, p)
	}
	sources := make([]any, 0, len(sourceIDs))
	for _, s := range sourceIDs {
		sources = append(sources, map[string]any{"id": s, "name": s})
	}
	desc := managedPolicyDescription(netbirdAgentIngestPolicyPurpose)
	rule := map[string]any{
		"enabled":       true,
		"description":   desc,
		"action":        "accept",
		"bidirectional": false,
		"protocol":      "tcp",
		"ports":         rulePorts,
		"sources":       sources,
		"destinations":  []any{map[string]any{"id": destID, "name": destID}},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, map[string]any{"id": id, "name": netbirdAgentIngestPolicyName, "description": desc, "enabled": enabled, "rules": []any{rule}})
}

// sortedCopy returns a sorted copy of s (for order-insensitive comparisons).
func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// TestReconcileAgentIngestCreatesForAllServers: a fleet pass with policy management
// on + two NetBird servers (tracking groups g1, g2) creates exactly ONE
// op-gw-agent-ingest policy whose sources are {g1, g2} (sorted), destination is the
// gateway group, protocol tcp, ports = [agent port], accept, bidirectional false.
func TestReconcileAgentIngestCreatesForAllServers(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal") // ResolveGroupID -> deterministic destination id
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	// Two NetBird servers with tracking groups g1/g2 and NO apps: the per-server
	// access reconcile creates nothing (no active ports), so the only CreatePolicy
	// is the fleet-wide agent-ingest one.
	seedManagedNetbirdServer(t, routeStore, "srv1", "GPU-1", "g1", "", now)
	seedManagedNetbirdServer(t, routeStore, "srv2", "GPU-2", "g2", "", now)

	svc.ReconcileAllServerNetbird(context.Background())

	if got := fake.policyCreateCountByName(netbirdAgentIngestPolicyName); got != 1 {
		t.Fatalf("op-gw-agent-ingest creates = %d, want exactly 1", got)
	}
	if got := fake.policyCreateCountByName("op-gw-access-srv1"); got != 0 {
		t.Fatalf("unexpected op-gw-access-srv1 create (%d) — the server has no active ports", got)
	}
	body := fake.createdPolicyByName(netbirdAgentIngestPolicyName)
	if body == nil {
		t.Fatalf("no op-gw-agent-ingest CreatePolicy body recorded")
	}
	if name, _ := body["name"].(string); name != netbirdAgentIngestPolicyName {
		t.Fatalf("policy name = %q, want %q", name, netbirdAgentIngestPolicyName)
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Fatalf("policy enabled = false, want true")
	}
	// Sources = ALL NetBird server tracking groups (sorted, deduped).
	if src := sortedCopy(policyRuleField(t, body, "sources")); !reflect.DeepEqual(src, []string{"g1", "g2"}) {
		t.Fatalf("policy sources = %v, want [g1 g2] (all server tracking groups)", src)
	}
	// Destination = the gateway group.
	if dst := policyRuleField(t, body, "destinations"); !reflect.DeepEqual(dst, []string{"gw-portal"}) {
		t.Fatalf("policy destinations = %v, want [gw-portal] (gateway group)", dst)
	}
	// Port = the effective agent port (default 8081).
	if ports := policyRuleField(t, body, "ports"); !reflect.DeepEqual(ports, []string{"8081"}) {
		t.Fatalf("policy ports = %v, want [8081] (agent port)", ports)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["action"] != "accept" || rule["protocol"] != "tcp" {
		t.Fatalf("rule action/protocol = %v/%v, want accept/tcp", rule["action"], rule["protocol"])
	}
	if bidi, _ := rule["bidirectional"].(bool); bidi {
		t.Fatalf("rule bidirectional = true, want false")
	}
	// No spurious update/delete.
	if fake.policyUpdateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("unexpected update/delete: u=%d d=%d", fake.policyUpdateCount(), fake.deletedPolicyCount())
	}
}

// TestReconcileAgentIngestIndependentOfScope: an EXCLUDED server (all-mode +
// override "exclude", so its per-server op-gw-access policy is NOT managed) is still
// a source of the agent-ingest policy — agent ingest is orthogonal to the
// gateway->server least-privilege scope.
func TestReconcileAgentIngestIndependentOfScope(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	// selected scope: a server is only access-managed if it opted IN ("include").
	enableNetbirdPolicies(t, svc, fake.srv.URL, "selected", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvIn", "In", "g-in", "include", now)
	seedManagedNetbirdServer(t, routeStore, "srvOut", "Out", "g-out", "", now) // not included -> access-unmanaged

	svc.ReconcileAllServerNetbird(context.Background())

	body := fake.createdPolicyByName(netbirdAgentIngestPolicyName)
	if body == nil {
		t.Fatalf("no op-gw-agent-ingest policy created")
	}
	// BOTH servers' tracking groups are sources, regardless of policy scope/override.
	if src := sortedCopy(policyRuleField(t, body, "sources")); !reflect.DeepEqual(src, []string{"g-in", "g-out"}) {
		t.Fatalf("agent-ingest sources = %v, want [g-in g-out] (scope-independent)", src)
	}
}

// TestReconcileAgentIngestSkipsGrouplessServer: a NetBird server WITHOUT a tracking
// group (purely manually linked) is not a source; only servers with a tracking group
// are included.
func TestReconcileAgentIngestSkipsGrouplessServer(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvG", "Grouped", "g-only", "", now)
	// A NetBird server with an EMPTY tracking group (manual link) -> skipped.
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: "srvNoGrp", Name: "Manual", Domain: "m.local", Status: routing.ServerStatusActive, NetbirdEnabled: true, NetbirdPeerID: "peer-m", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	svc.ReconcileAllServerNetbird(context.Background())

	body := fake.createdPolicyByName(netbirdAgentIngestPolicyName)
	if body == nil {
		t.Fatalf("no op-gw-agent-ingest policy created")
	}
	if src := sortedCopy(policyRuleField(t, body, "sources")); !reflect.DeepEqual(src, []string{"g-only"}) {
		t.Fatalf("agent-ingest sources = %v, want [g-only] (groupless server skipped)", src)
	}
}

// TestReconcileAgentIngestUpdatesOnServerSetChange: an existing matching agent-ingest
// policy triggers NO update when the server set is unchanged (order-insensitive); a
// third server added to the set triggers exactly one UpdatePolicy with the new source
// set.
func TestReconcileAgentIngestUpdatesOnServerSetChange(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvA", "A", "g2", "", now)
	seedManagedNetbirdServer(t, routeStore, "srvB", "B", "g1", "", now)
	// Pre-seed a MATCHING agent-ingest policy (source order deliberately reversed:
	// the match is set-based / order-insensitive).
	seedAgentIngestReadPolicy(fake, "pol-ai", true, []string{"8081"}, []string{"g2", "g1"}, "gw-portal")

	// No change -> no update.
	svc.ReconcileAllServerNetbird(context.Background())
	if fake.policyUpdateCount() != 0 {
		t.Fatalf("policy updates = %d after a no-change pass, want 0 (order-insensitive match)", fake.policyUpdateCount())
	}
	if fake.policyCreateCountByName(netbirdAgentIngestPolicyName) != 0 {
		t.Fatalf("agent-ingest was CREATED though a matching policy already exists")
	}

	// Add a third server -> the source set grows -> exactly one update.
	seedManagedNetbirdServer(t, routeStore, "srvC", "C", "g3", "", now)
	svc.ReconcileAllServerNetbird(context.Background())

	if fake.policyUpdateCount() != 1 {
		t.Fatalf("policy updates = %d after adding a server, want 1", fake.policyUpdateCount())
	}
	body := fake.lastUpdatedPolicy()
	if src := sortedCopy(policyRuleField(t, body, "sources")); !reflect.DeepEqual(src, []string{"g1", "g2", "g3"}) {
		t.Fatalf("updated agent-ingest sources = %v, want [g1 g2 g3]", src)
	}
	if name, _ := body["name"].(string); name != netbirdAgentIngestPolicyName {
		t.Fatalf("updated policy name = %q, want %q", name, netbirdAgentIngestPolicyName)
	}
}

// TestReconcileAgentIngestDeletedWhenNoServers: an existing op-gw-agent-ingest policy
// with ZERO NetBird servers is DELETED (it would serve nothing).
func TestReconcileAgentIngestDeletedWhenNoServers(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)
	// A stale agent-ingest policy, but NO NetBird servers exist.
	seedAgentIngestReadPolicy(fake, "pol-stale", true, []string{"8081"}, []string{"g1"}, "gw-portal")

	svc.ReconcileAllServerNetbird(context.Background())

	if !fake.wasPolicyDeleted("pol-stale") {
		t.Fatalf("stale agent-ingest policy pol-stale was not deleted (no NetBird servers)")
	}
	if fake.policyCreateCountByName(netbirdAgentIngestPolicyName) != 0 || fake.policyUpdateCount() != 0 {
		t.Fatalf("unexpected create/update on the delete path: c=%d u=%d", fake.policyCreateCountByName(netbirdAgentIngestPolicyName), fake.policyUpdateCount())
	}
}

// TestReconcileAgentIngestNoCallsWhenManageOff: with policy management OFF, the fleet
// pass makes ZERO policy calls (no list/create/update/delete) — neither access nor
// agent-ingest.
func TestReconcileAgentIngestNoCallsWhenManageOff(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	// Module ON but policy management OFF.
	enableNetbird(t, svc, fake.srv.URL, true)
	seedManagedNetbirdServer(t, routeStore, "srvM1", "M1", "g1", "", now)
	seedManagedNetbirdServer(t, routeStore, "srvM2", "M2", "g2", "", now)

	svc.ReconcileAllServerNetbird(context.Background())

	if fake.policyListCount() != 0 || fake.policyCreateCount() != 0 || fake.policyUpdateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("policy calls with management OFF: list=%d create=%d update=%d delete=%d, want all 0",
			fake.policyListCount(), fake.policyCreateCount(), fake.policyUpdateCount(), fake.deletedPolicyCount())
	}
}

// TestReconcileAgentIngestSelfHealsDisabled: an existing agent-ingest policy that
// matches the desired rule but is top-level DISABLED (out-of-band drift) is UPDATEd
// back to enabled:true (self-heal, like the per-server policyMatches fix).
func TestReconcileAgentIngestSelfHealsDisabled(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvH1", "H1", "g1", "", now)
	seedManagedNetbirdServer(t, routeStore, "srvH2", "H2", "g2", "", now)
	// Rule matches (sources {g1,g2}, port 8081, dest gw-portal) but the policy is
	// DISABLED at the top level.
	seedAgentIngestReadPolicy(fake, "pol-dis", false, []string{"8081"}, []string{"g1", "g2"}, "gw-portal")

	svc.ReconcileAllServerNetbird(context.Background())

	if fake.policyUpdateCount() != 1 {
		t.Fatalf("policy updates = %d, want 1 (self-heal a disabled agent-ingest policy)", fake.policyUpdateCount())
	}
	body := fake.lastUpdatedPolicy()
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Fatalf("updated agent-ingest policy enabled = false, want true (re-enabled)")
	}
	if fake.policyCreateCountByName(netbirdAgentIngestPolicyName) != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("unexpected create/delete on self-heal: c=%d d=%d", fake.policyCreateCountByName(netbirdAgentIngestPolicyName), fake.deletedPolicyCount())
	}
}

// TestReconcileAgentIngestBestEffortOnError: a NetBird CreatePolicy 500 during the
// agent-ingest reconcile is swallowed (best-effort) — no panic, no error surfaced
// (the helper is void).
func TestReconcileAgentIngestBestEffortOnError(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, _ := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.failCreatePolicy = true
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	servers := []routing.AIServer{{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1"}}

	// No existing policy -> attempts a CreatePolicy which 500s. Must not panic.
	svc.reconcileAgentIngestPolicyWith(context.Background(), ncfg, servers, nil)

	if got := fake.policyCreateCount(); got != 1 {
		t.Fatalf("policy create attempts = %d, want 1 (attempted then swallowed the 500)", got)
	}
	if fake.deletedPolicyCount() != 0 || fake.policyUpdateCount() != 0 {
		t.Fatalf("unexpected update/delete on the create-error path: u=%d d=%d", fake.policyUpdateCount(), fake.deletedPolicyCount())
	}
}

// agentIngestReadPolicy builds a READ-shape netbird.Policy for the matcher unit test:
// one accept/tcp/non-bidirectional rule with the given ports, source group ids, and a
// single destination (the gateway group). Description is set to the desired
// managed-policy description on both the policy and its rule so a baseline "matching"
// case is a true match; a caller testing description-drift overrides it explicitly.
func agentIngestReadPolicy(enabled bool, ports, sourceIDs []string, destID string) netbird.Policy {
	srcs := make([]netbird.GroupRef, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		srcs = append(srcs, netbird.GroupRef{ID: id, Name: id})
	}
	desc := managedPolicyDescription(netbirdAgentIngestPolicyPurpose)
	return netbird.Policy{
		Name:        netbirdAgentIngestPolicyName,
		Description: desc,
		Enabled:     enabled,
		Rules: []netbird.PolicyRule{{
			Enabled:       true,
			Description:   desc,
			Action:        "accept",
			Bidirectional: false,
			Protocol:      "tcp",
			Ports:         ports,
			Sources:       srcs,
			Destinations:  []netbird.GroupRef{{ID: destID, Name: destID}},
		}},
	}
}

// TestAgentIngestPolicyMatches exercises the matcher directly. In particular it pins
// the least-privilege self-heal guards that the fleet-reconcile tests can't isolate:
// an out-of-band port_range on the managed agent-ingest policy must be seen as a
// MISMATCH so the reconcile rewrites it back to a single-port grant (mirrors the
// per-server TestPolicyMatchesRejectsPortRanges from 675b14a).
func TestAgentIngestPolicyMatches(t *testing.T) {
	const gw = "gw-portal"
	want := []string{"g1", "g2"}

	// Baseline: an exact match (source order reversed to prove set-insensitivity).
	if !agentIngestPolicyMatches(agentIngestReadPolicy(true, []string{"8081"}, []string{"g2", "g1"}, gw), gw, want, "8081") {
		t.Fatalf("expected a matching agent-ingest policy to match (set-insensitive sources)")
	}

	// port_range present -> mismatch (the guard the mutation lens found unprotected).
	rangePolicy := agentIngestReadPolicy(true, []string{"8081"}, want, gw)
	rangePolicy.Rules[0].PortRanges = []netbird.PortRange{{Start: 8000, End: 9000}}
	if agentIngestPolicyMatches(rangePolicy, gw, want, "8081") {
		t.Fatalf("a policy carrying a port_range must NOT match (self-heal must rewrite it)")
	}

	// Assorted drift that must be seen as a mismatch.
	cases := []struct {
		name string
		p    netbird.Policy
	}{
		{"top-level disabled", agentIngestReadPolicy(false, []string{"8081"}, want, gw)},
		{"wrong port", agentIngestReadPolicy(true, []string{"9999"}, want, gw)},
		{"wrong destination", agentIngestReadPolicy(true, []string{"8081"}, want, "other-group")},
		{"missing a source", agentIngestReadPolicy(true, []string{"8081"}, []string{"g1"}, gw)},
		{"extra source", agentIngestReadPolicy(true, []string{"8081"}, []string{"g1", "g2", "g3"}, gw)},
	}
	for _, c := range cases {
		if agentIngestPolicyMatches(c.p, gw, want, "8081") {
			t.Fatalf("%s: expected a mismatch", c.name)
		}
	}

	// bidirectional / non-tcp / multi-rule drift.
	bidi := agentIngestReadPolicy(true, []string{"8081"}, want, gw)
	bidi.Rules[0].Bidirectional = true
	if agentIngestPolicyMatches(bidi, gw, want, "8081") {
		t.Fatalf("a bidirectional rule must NOT match (least-privilege: one-way ingress only)")
	}
	twoRules := agentIngestReadPolicy(true, []string{"8081"}, want, gw)
	twoRules.Rules = append(twoRules.Rules, twoRules.Rules[0])
	if agentIngestPolicyMatches(twoRules, gw, want, "8081") {
		t.Fatalf("a 2-rule policy must NOT match (managed policy is single-rule)")
	}
}

// TestManagedPoliciesCarryDescription (op-gw-agent-ingest): the desired
// PolicyRequest built by desiredAgentIngestPolicyRequest carries the English
// "Managed by the OP AI Gateway" Description on both the policy and its sole rule.
func TestManagedPoliciesCarryDescriptionAgentIngest(t *testing.T) {
	req := desiredAgentIngestPolicyRequest("gw-portal", []string{"g1", "g2"}, "8081")
	if !strings.Contains(req.Description, "Managed by the OP AI Gateway") {
		t.Fatalf("policy description missing: %q", req.Description)
	}
	if len(req.Rules) == 0 || !strings.Contains(req.Rules[0].Description, "Managed by the OP AI Gateway") {
		t.Fatalf("rule description missing")
	}
}

// TestManagedPolicyDescriptionDriftIsCorrected (op-gw-agent-ingest): a stored policy
// identical except Description="" (policy AND rule) must NOT match -> the reconcile
// issues an UpdatePolicy that restores the description.
func TestManagedPolicyDescriptionDriftIsCorrectedAgentIngest(t *testing.T) {
	desired := desiredAgentIngestPolicyRequest("gw-portal", []string{"g1", "g2"}, "8081")
	stored := agentIngestReadPolicy(true, []string{"8081"}, []string{"g1", "g2"}, "gw-portal")
	stored.Name = desired.Name
	stored.Description = ""
	stored.Rules[0].Description = ""
	if agentIngestPolicyMatches(stored, "gw-portal", []string{"g1", "g2"}, "8081") {
		t.Fatal("a policy with an empty description must be treated as drift")
	}
}

// TestReconcileAgentIngestUsesEffectiveAgentPort: the built policy carries the
// Service's effective agent port, not a hardcoded 8081. The other create tests use
// the "8081" default, so they can't distinguish s.agentPort from a literal "8081".
func TestReconcileAgentIngestUsesEffectiveAgentPort(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	svc.agentPort = "9443" // e.g. OP_AI_GATEWAY_AGENT_ADDR=<ip>:9443
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvP", "P", "g1", "", now)

	svc.ReconcileAllServerNetbird(context.Background())

	body := fake.createdPolicyByName(netbirdAgentIngestPolicyName)
	if body == nil {
		t.Fatalf("no op-gw-agent-ingest policy created")
	}
	if ports := policyRuleField(t, body, "ports"); !reflect.DeepEqual(ports, []string{"9443"}) {
		t.Fatalf("agent-ingest ports = %v, want [9443] (effective agent port, not hardcoded 8081)", ports)
	}
}

// TestReconcileAgentIngestConcurrentPassesNoDuplicate: two fleet passes running
// concurrently create the account-wide agent-ingest policy exactly ONCE — the
// netbirdPolicyMu serializes the list-then-create so the second pass sees the first
// pass's create. Run with -race to exercise the serialization.
func TestReconcileAgentIngestConcurrentPassesNoDuplicate(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srvX", "X", "g1", "", now)
	seedManagedNetbirdServer(t, routeStore, "srvY", "Y", "g2", "", now)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.ReconcileAllServerNetbird(context.Background())
		}()
	}
	wg.Wait()

	if got := fake.policyCreateCountByName(netbirdAgentIngestPolicyName); got != 1 {
		t.Fatalf("op-gw-agent-ingest creates = %d under concurrent fleet passes, want exactly 1", got)
	}
}
