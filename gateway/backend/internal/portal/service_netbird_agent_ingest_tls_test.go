// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
	"time"
)

// seedAgentIngestTLSReadPolicy is seedAgentIngestReadPolicy's sibling for the
// op-gw-agent-ingest-tls policy: one rule with the given enabled state, ports,
// source tracking-group ids, and the single destination (gateway group). The
// Description is set to the desired managed-policy description (matching
// desiredAgentIngestTLSPolicyRequest) so an otherwise-matching seeded policy is
// a true match.
func seedAgentIngestTLSReadPolicy(f *fakeNetbird, id string, enabled bool, ports, sourceIDs []string, destID string) {
	rulePorts := make([]any, 0, len(ports))
	for _, p := range ports {
		rulePorts = append(rulePorts, p)
	}
	sources := make([]any, 0, len(sourceIDs))
	for _, s := range sourceIDs {
		sources = append(sources, map[string]any{"id": s, "name": s})
	}
	desc := managedPolicyDescription(netbirdAgentIngestTLSPolicyPurpose)
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
	f.policies = append(f.policies, map[string]any{"id": id, "name": NetbirdAgentIngestTLSPolicyName, "description": desc, "enabled": enabled, "rules": []any{rule}})
}

// policyIDByName returns the id of the fake's stored policy with the given
// name (empty if none), used when a test needs to assert a specific policy —
// created earlier in the same test — was later deleted.
func policyIDByName(f *fakeNetbird, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.policies {
		if n, _ := p["name"].(string); n == name {
			id, _ := p["id"].(string)
			return id
		}
	}
	return ""
}

// agentIngestTLSReadPolicy builds a READ-shape netbird.Policy for the matcher
// unit test: one accept/tcp/non-bidirectional rule with the given ports,
// source group ids, and a single destination (the gateway group). It mirrors
// agentIngestReadPolicy but names/describes the policy as
// op-gw-agent-ingest-tls.
func agentIngestTLSReadPolicy(enabled bool, ports, sourceIDs []string, destID string) netbird.Policy {
	srcs := make([]netbird.GroupRef, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		srcs = append(srcs, netbird.GroupRef{ID: id, Name: id})
	}
	desc := managedPolicyDescription(netbirdAgentIngestTLSPolicyPurpose)
	return netbird.Policy{
		Name:        NetbirdAgentIngestTLSPolicyName,
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

// TestAgentIngestTLSPolicyCreatedOnlyInSeparateMode: the account-wide
// op-gw-agent-ingest-tls policy is created ONLY while
// Service.CertMeshTLSSeparateActive is true (sources = all server tracking
// groups, destination = the gateway group, tcp/unidirectional, the managed
// description, ports = [agentTLSPort]); while combined (the default here) it
// is never created, and switching an existing one back to combined deletes it.
func TestAgentIngestTLSPolicyCreatedOnlyInSeparateMode(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newNetbirdServerTestService(t, now)
	svc.agentTLSPort = 8443 // newNetbirdServerTestService doesn't wire ServiceDeps.AgentTLSPort
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal") // ResolveGroupID -> deterministic destination id
	enableNetbirdPolicies(t, svc, fake.srv.URL, "all", false, false)

	seedManagedNetbirdServer(t, routeStore, "srv1", "GPU-1", "g1", "", now)
	seedManagedNetbirdServer(t, routeStore, "srv2", "GPU-2", "g2", "", now)

	// Combined (the default: svc.agentTLSSeparateDefault=false, mode unset) ->
	// byte-neutral: no op-gw-agent-ingest-tls is ever created.
	svc.ReconcileAllServerNetbird(context.Background())
	if got := fake.policyCreateCountByName(NetbirdAgentIngestTLSPolicyName); got != 0 {
		t.Fatalf("combined mode: op-gw-agent-ingest-tls creates = %d, want 0", got)
	}

	// Flip to separate -> the fleet pass creates it.
	sep := "separate"
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CertMeshTLSMode: &sep}); err != nil {
		t.Fatalf("switch to separate: %v", err)
	}
	svc.ReconcileAllServerNetbird(context.Background())

	body := fake.createdPolicyByName(NetbirdAgentIngestTLSPolicyName)
	if body == nil {
		t.Fatalf("no op-gw-agent-ingest-tls CreatePolicy body recorded once separate mode is active")
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Fatalf("policy enabled = false, want true")
	}
	if src := sortedCopy(policyRuleField(t, body, "sources")); !reflect.DeepEqual(src, []string{"g1", "g2"}) {
		t.Fatalf("policy sources = %v, want [g1 g2] (all server tracking groups)", src)
	}
	if dst := policyRuleField(t, body, "destinations"); !reflect.DeepEqual(dst, []string{"gw-portal"}) {
		t.Fatalf("policy destinations = %v, want [gw-portal] (gateway group)", dst)
	}
	if ports := policyRuleField(t, body, "ports"); !reflect.DeepEqual(ports, []string{"8443"}) {
		t.Fatalf("policy ports = %v, want [8443] (the agent TLS port, converted from int)", ports)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["action"] != "accept" || rule["protocol"] != "tcp" {
		t.Fatalf("rule action/protocol = %v/%v, want accept/tcp", rule["action"], rule["protocol"])
	}
	if bidi, _ := rule["bidirectional"].(bool); bidi {
		t.Fatalf("rule bidirectional = true, want false (unidirectional)")
	}
	wantDesc := managedPolicyDescription(netbirdAgentIngestTLSPolicyPurpose)
	if desc, _ := body["description"].(string); desc != wantDesc {
		t.Fatalf("policy description = %q, want %q", desc, wantDesc)
	}
	if desc, _ := rule["description"].(string); desc != wantDesc {
		t.Fatalf("rule description = %q, want %q", desc, wantDesc)
	}

	createdID := policyIDByName(fake, NetbirdAgentIngestTLSPolicyName)
	if createdID == "" {
		t.Fatalf("could not resolve the created op-gw-agent-ingest-tls policy id")
	}

	// Flip back to combined -> the fleet pass DELETES it, and never creates a
	// second one.
	comb := "combined"
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CertMeshTLSMode: &comb}); err != nil {
		t.Fatalf("switch to combined: %v", err)
	}
	svc.ReconcileAllServerNetbird(context.Background())

	if !fake.wasPolicyDeleted(createdID) {
		t.Fatalf("op-gw-agent-ingest-tls (id %q) was not deleted after reverting to combined mode", createdID)
	}
	if got := fake.policyCreateCountByName(NetbirdAgentIngestTLSPolicyName); got != 1 {
		t.Fatalf("op-gw-agent-ingest-tls creates after reverting to combined = %d, want still 1 (never re-created)", got)
	}
}

// TestAgentIngestTLSPolicyPortMatch: agentIngestTLSPolicyMatches requires
// EXACTLY [agentTLSPort] — no port_range, no extra/missing/wrong port — mirrors
// TestAgentIngestPolicyMatches' least-privilege guards for the TLS sibling.
func TestAgentIngestTLSPolicyPortMatch(t *testing.T) {
	const gw = "gw-portal"
	want := []string{"g1", "g2"}

	// Baseline: an exact match (source order reversed to prove set-insensitivity).
	if !agentIngestTLSPolicyMatches(agentIngestTLSReadPolicy(true, []string{"8443"}, []string{"g2", "g1"}, gw), gw, want, "8443") {
		t.Fatalf("expected a matching agent-ingest-tls policy to match (set-insensitive sources)")
	}

	// port_range present -> mismatch (self-heal must rewrite it away).
	rangePolicy := agentIngestTLSReadPolicy(true, []string{"8443"}, want, gw)
	rangePolicy.Rules[0].PortRanges = []netbird.PortRange{{Start: 8000, End: 9000}}
	if agentIngestTLSPolicyMatches(rangePolicy, gw, want, "8443") {
		t.Fatalf("a policy carrying a port_range must NOT match (self-heal must rewrite it)")
	}

	cases := []struct {
		name string
		p    netbird.Policy
	}{
		{"wrong port", agentIngestTLSReadPolicy(true, []string{"9999"}, want, gw)},
		{"the plaintext agent port instead of the TLS port", agentIngestTLSReadPolicy(true, []string{"8081"}, want, gw)},
		{"extra port alongside the correct one", agentIngestTLSReadPolicy(true, []string{"8443", "9999"}, want, gw)},
		{"no ports at all", agentIngestTLSReadPolicy(true, []string{}, want, gw)},
		{"top-level disabled", agentIngestTLSReadPolicy(false, []string{"8443"}, want, gw)},
		{"wrong destination", agentIngestTLSReadPolicy(true, []string{"8443"}, want, "other-group")},
		{"missing a source", agentIngestTLSReadPolicy(true, []string{"8443"}, []string{"g1"}, gw)},
		{"extra source", agentIngestTLSReadPolicy(true, []string{"8443"}, []string{"g1", "g2", "g3"}, gw)},
	}
	for _, c := range cases {
		if agentIngestTLSPolicyMatches(c.p, gw, want, "8443") {
			t.Fatalf("%s: expected a mismatch", c.name)
		}
	}

	// bidirectional / non-tcp / multi-rule drift.
	bidi := agentIngestTLSReadPolicy(true, []string{"8443"}, want, gw)
	bidi.Rules[0].Bidirectional = true
	if agentIngestTLSPolicyMatches(bidi, gw, want, "8443") {
		t.Fatalf("a bidirectional rule must NOT match (least-privilege: one-way ingress only)")
	}
	twoRules := agentIngestTLSReadPolicy(true, []string{"8443"}, want, gw)
	twoRules.Rules = append(twoRules.Rules, twoRules.Rules[0])
	if agentIngestTLSPolicyMatches(twoRules, gw, want, "8443") {
		t.Fatalf("a 2-rule policy must NOT match (managed policy is single-rule)")
	}

	// A managed op-gw-agent-ingest-tls policy on the TLS port must not be
	// confused with a plaintext op-gw-agent-ingest one on the same source/dest
	// set: description differs by purpose, so an agent-ingest-tls-purpose
	// description is required even when every other field lines up (covered
	// by the baseline match above using agentIngestTLSReadPolicy's own
	// description). A stale/empty description is drift.
	stale := agentIngestTLSReadPolicy(true, []string{"8443"}, want, gw)
	stale.Description = ""
	stale.Rules[0].Description = ""
	if agentIngestTLSPolicyMatches(stale, gw, want, "8443") {
		t.Fatalf("a policy with an empty description must be treated as drift")
	}
}

// TestAgentIngestTLSPolicyLeftUntouchedOnSettingsError: a
// CertMeshTLSSeparateActive error (a transient system-settings store blip)
// must leave an existing op-gw-agent-ingest-tls policy completely untouched
// this pass -- neither deleted nor updated -- mirroring the "don't churn on a
// control-plane blip" discipline used elsewhere in this file (never delete a
// live policy on a spurious read error).
func TestAgentIngestTLSPolicyLeftUntouchedOnSettingsError(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: erroringSettings{}, AgentTLSPort: 8443})
	fake := newFakeNetbird(t)
	fake.seedGroup("gw-portal", "op-gw-portal")
	seedAgentIngestTLSReadPolicy(fake, "pol-tls", true, []string{"8443"}, []string{"g1"}, "gw-portal")

	ncfg := netbird.Config{URL: fake.srv.URL, Token: "nbtok"}
	policies, err := netbird.ListPolicies(context.Background(), ncfg, netbirdCallTimeout)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	servers := []routing.AIServer{{ID: "s1", NetbirdEnabled: true, NetbirdGroupID: "g1"}}

	svc.reconcileAgentIngestTLSPolicyWith(context.Background(), ncfg, servers, policies)

	if fake.policyCreateCount() != 0 || fake.policyUpdateCount() != 0 || fake.deletedPolicyCount() != 0 {
		t.Fatalf("policy calls on a CertMeshTLSSeparateActive error: create=%d update=%d delete=%d, want all 0",
			fake.policyCreateCount(), fake.policyUpdateCount(), fake.deletedPolicyCount())
	}
}
