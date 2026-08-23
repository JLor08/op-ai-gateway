// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	// netbirdGatewayGroupName is the NetBird policy group the GATEWAY's own peer
	// joins when it self-enrolls with a gateway setup key. It is resolve-or-created
	// and is already excluded from the group pickers by the "op-gw-" prefix filter.
	netbirdGatewayGroupName = "op-gw-portal"
	// netbirdGatewaySetupKeyName is the NetBird label of the gateway peer's setup key.
	netbirdGatewaySetupKeyName = "op-gateway"
	// netbirdAgentIngestPolicyName is the name of the ONE account-wide managed
	// policy that lets every NetBird server's tracking group reach the gateway
	// group on the agent port (server->gateway telemetry, opposite direction +
	// different port from the per-server op-gw-access-<id> policies). Distinct name
	// so it never collides with an op-gw-access-* policy; deliberately NOT counted
	// by NetbirdManagedPolicyPrefix (the status endpoint's managed_policy_count).
	netbirdAgentIngestPolicyName = "op-gw-agent-ingest"
	// netbirdPingGatewayPolicyName is the account-wide managed policy that lets every
	// NetBird server's tracking group ICMP-ping the gateway group (server->gateway
	// reachability probing). Distinct name, ICMP/portless, unidirectional; gated on
	// the netbird_allow_ping_gateway setting.
	netbirdPingGatewayPolicyName = "op-gw-ping-gateway"
	// netbirdPingServersPolicyName is the account-wide managed policy that lets the
	// gateway group ICMP-ping the server tracking groups (gateway->server reachability
	// probing, the /ping action). Destinations = ALL NetBird server groups when
	// netbird_allow_ping_all_servers, else only the per-server NetbirdAllowPing groups.
	netbirdPingServersPolicyName = "op-gw-ping-servers"
)

// NetbirdAgentIngestTLSPolicyName is the name of the ONE account-wide managed
// policy that lets every NetBird server's tracking group reach the gateway
// group on the SEPARATE encrypted agent TLS port (Service.agentTLSPort) —
// the sibling of netbirdAgentIngestPolicyName for the dedicated-TLS-port
// topology (see Service.CertMeshTLSSeparateActive). It exists only while
// separate mode is active; reconcileAgentIngestTLSPolicyWith deletes it the
// moment the effective mode reverts to "combined". Exported (unlike its
// plaintext sibling) so a later task's status/deploy-config surface can
// reference the exact managed-policy name without re-deriving it.
const NetbirdAgentIngestTLSPolicyName = "op-gw-agent-ingest-tls"

// netbirdManagedPolicyDescPrefix labels every gateway-managed NetBird policy's
// Description so an operator browsing the NetBird admin UI can tell at a glance
// that a policy is gateway-owned (and must not be hand-edited) rather than a
// manually created one. See managedPolicyDescription.
const netbirdManagedPolicyDescPrefix = "Managed by the OP AI Gateway"

const (
	// netbirdAccessPolicyPurpose is the managedPolicyDescription purpose for the
	// per-server "op-gw-access-<serverID>" policy.
	netbirdAccessPolicyPurpose = "gateway → AI server data plane (application ports)"
	// netbirdAgentIngestPolicyPurpose is the managedPolicyDescription purpose for
	// the account-wide "op-gw-agent-ingest" policy.
	netbirdAgentIngestPolicyPurpose = "server agents → gateway telemetry/mesh ingress (agent port)"
	// netbirdAgentIngestTLSPolicyPurpose is the managedPolicyDescription purpose
	// for the account-wide "op-gw-agent-ingest-tls" policy (separate mode only).
	netbirdAgentIngestTLSPolicyPurpose = "server agents → gateway encrypted mesh ingress (dedicated TLS port)"
	// netbirdPingGatewayPolicyPurpose is the managedPolicyDescription purpose for
	// the account-wide "op-gw-ping-gateway" policy.
	netbirdPingGatewayPolicyPurpose = "servers ICMP-ping the gateway (reachability)"
	// netbirdPingServersPolicyPurpose is the managedPolicyDescription purpose for
	// the account-wide "op-gw-ping-servers" policy.
	netbirdPingServersPolicyPurpose = "gateway ICMP-pings servers"
)

// managedPolicyDescription formats the standard English Description NetBird stores
// on every gateway-managed policy (and its sole rule): a "this is gateway-owned, do
// not hand-edit" marker plus WHY the policy exists. Set on every desired
// PolicyRequest/PolicyRuleRequest built by this file's reconcile helpers and
// compared in the corresponding *Matches function, so a manual edit to the
// description is drift that gets restored on the next reconcile. Exported behavior
// via package visibility (not capitalized) — reused across policy types within
// package portal, including the later op-gw-agent-ingest-tls policy.
func managedPolicyDescription(purpose string) string {
	return netbirdManagedPolicyDescPrefix + " — " + purpose + ". Do not edit manually."
}

const (
	// NetbirdManagedPolicyPrefix is the name prefix of every gateway-managed NetBird
	// access-control policy. A per-server policy is named "op-gw-access-<serverID>",
	// so the reconcile engine can find "its" policy by name and count managed
	// policies by prefix (the status endpoint's managed_policy_count). Exported so
	// the gateway status handler counts managed policies without duplicating it.
	NetbirdManagedPolicyPrefix = "op-gw-access-"
	// NetbirdDefaultPolicyName is the name of NetBird's built-in catch-all "Default"
	// policy. Deny-by-default enforcement disables it; the status endpoint reports
	// whether it is present + enabled to surface drift. Exported for the status handler.
	NetbirdDefaultPolicyName = "Default"
)

// NetbirdPolicyContextDTO is the create-form-relevant NetBird policy settings snapshot.
type NetbirdPolicyContextDTO struct {
	ManagePolicies       bool   `json:"manage_policies"`
	EffectivePolicyScope string `json:"effective_policy_scope"`
	DenyByDefault        bool   `json:"deny_by_default"`
}

// NetbirdPolicyContext returns the create-form-relevant NetBird policy settings
// (nil-safe; all false/"" resolving to the default scope on a missing store / read
// error). EffectivePolicyScope is the RESOLVED scope (auto → all/selected per deny).
func (s *Service) NetbirdPolicyContext(ctx context.Context) NetbirdPolicyContextDTO {
	var values map[string]string
	if s.settings != nil {
		values, _ = s.settings.SystemSettings(ctx)
	}
	deny := NetbirdDenyByDefault(values)
	return NetbirdPolicyContextDTO{
		ManagePolicies:       NetbirdManagePolicies(values),
		EffectivePolicyScope: EffectiveNetbirdPolicyScope(NetbirdPolicyScope(values), deny),
		DenyByDefault:        deny,
	}
}

// normalizeNetbirdPolicyOverride clamps the per-server policy override to one of
// the three valid values: "" (follow the effective scope), "include" (opt in), or
// "exclude" (opt out). Any other value (including whitespace/junk) becomes "".
func normalizeNetbirdPolicyOverride(raw string) string {
	switch strings.TrimSpace(raw) {
	case "include":
		return "include"
	case "exclude":
		return "exclude"
	default:
		return ""
	}
}

// serverManaged reports whether the gateway should MANAGE (create/update) a NetBird
// access policy for this server, given the effective policy scope. A server is only
// ever managed when it is a NetBird peer WITH a tracking group (NetbirdGroupID — the
// policy's destination). In "all" scope every such server is managed unless it opted
// OUT ("exclude"); in "selected" scope only a server that opted IN ("include") is
// managed. A non-NetBird / group-less server is never managed.
func serverManaged(server routing.AIServer, effectiveScope string) bool {
	if !server.NetbirdEnabled || server.NetbirdGroupID == "" {
		return false
	}
	if effectiveScope == "all" {
		return server.NetbirdPolicyOverride != "exclude"
	}
	// "selected" (the only other effective value): opt-in only.
	return server.NetbirdPolicyOverride == "include"
}

// netbirdServerGroupIDs returns the sorted, deduped tracking-group ids of every
// NetbirdEnabled server that has a group.
func netbirdServerGroupIDs(servers []routing.AIServer) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, srv := range servers {
		if !srv.NetbirdEnabled || srv.NetbirdGroupID == "" {
			continue
		}
		if _, dup := seen[srv.NetbirdGroupID]; dup {
			continue
		}
		seen[srv.NetbirdGroupID] = struct{}{}
		ids = append(ids, srv.NetbirdGroupID)
	}
	sort.Strings(ids)
	return ids
}

// netbirdPingServerGroupIDs is the op-gw-ping-servers destination set: all NetBird
// server groups when allowAll EXCEPT those with NetbirdPingExclude (the per-server
// opt-out), else only servers with NetbirdAllowPing (the per-server opt-in).
func netbirdPingServerGroupIDs(servers []routing.AIServer, allowAll bool) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, srv := range servers {
		if !srv.NetbirdEnabled || srv.NetbirdGroupID == "" {
			continue
		}
		if allowAll {
			if srv.NetbirdPingExclude {
				continue // opt-out: excluded even though all servers are pingable
			}
		} else if !srv.NetbirdAllowPing {
			continue // opt-in: only explicitly allowed servers
		}
		if _, dup := seen[srv.NetbirdGroupID]; dup {
			continue
		}
		seen[srv.NetbirdGroupID] = struct{}{}
		ids = append(ids, srv.NetbirdGroupID)
	}
	sort.Strings(ids)
	return ids
}

// activePortStrings distills a server's applications into the sorted, deduplicated
// set of EFFECTIVE reachable TCP ports (as strings) its ACTIVE applications need
// open. Inactive apps and non-positive ports are excluded; duplicate ports are
// collapsed. Pure (no I/O) so the dedup/sort/exclusion is unit-testable in
// isolation.
//
// P4 proxied-HTTPS: a proxied app (Scheme "https" + non-zero ProxyListenPort,
// the state routing.ApplicationEndpoint routes to the agent's TLS-terminating
// listener) contributes BOTH its plaintext Port AND its ProxyListenPort (union).
// Without ProxyListenPort here, the managed NetBird op-gw-access-<serverID>
// policy would leave that port closed under deny-by-default after a forward
// auto-switch, denying gateway->server:ProxyListenPort (breaking both routing
// and the health probe, which drops the app entirely). Both ports are kept open
// so a later revert back to Port has no policy-lag reachability window; opening
// the local plaintext Port is harmless (the app binds it locally either way).
func activePortStrings(apps []routing.Application) []string {
	seen := map[int]struct{}{}
	nums := make([]int, 0, len(apps))
	add := func(port int) {
		if port <= 0 {
			return
		}
		if _, dup := seen[port]; dup {
			return
		}
		seen[port] = struct{}{}
		nums = append(nums, port)
	}
	for _, a := range apps {
		if a.Status != routing.ServerStatusActive {
			continue
		}
		add(a.Port)
		if a.Scheme == "https" && a.ProxyListenPort != 0 {
			add(a.ProxyListenPort)
		}
	}
	sort.Ints(nums)
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		out = append(out, strconv.Itoa(n))
	}
	return out
}

// activeAppPorts is the store-backed wrapper around activePortStrings: it loads the
// server's applications and returns the sorted/deduped active TCP port set + an ok
// flag. ok=false signals a store ERROR (a transient dependency failure) — the caller
// MUST skip the server and keep its current policy, NOT treat it as "no active ports"
// (which would delete a healthy policy). ok=true with an empty slice is a genuine
// "no active apps" and DOES trigger the delete branch.
func (s *Service) activeAppPorts(ctx context.Context, serverID string) ([]string, bool) {
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		slog.Debug("policy reconcile: list apps", "server_id", serverID, "error", err)
		return nil, false
	}
	return activePortStrings(apps), true
}

// netbirdGroupRefIDs extracts the group-ID strings from a list of read-shape group
// refs (a policy rule's sources/destinations).
func netbirdGroupRefIDs(refs []netbird.GroupRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

// policyMatches reports whether an existing NetBird policy already encodes exactly
// the gateway's desired access rule: a top-level ENABLED policy with a single enabled
// accept/tcp/unidirectional rule whose sole source is the gateway group, whose sole
// destination is the server's tracking group, and whose port set equals the desired
// set (order-insensitive). A mismatch on any of these — including a matching-but-
// top-level-disabled policy — triggers an UpdatePolicy (self-heal: the desired policy
// is always Enabled:true, so a drifted-disabled policy is re-enabled).
func policyMatches(p netbird.Policy, gwGroupID, trackingGroupID string, ports []string) bool {
	if !p.Enabled {
		return false
	}
	if len(p.Rules) != 1 {
		return false
	}
	r := p.Rules[0]
	if !r.Enabled || r.Action != "accept" || r.Bidirectional || r.Protocol != "tcp" {
		return false
	}
	// A gateway-managed policy never uses port ranges (only discrete active-app
	// ports). Any port range is out-of-band drift that widens the grant beyond the
	// registered ports, so treat it as a mismatch: the reconcile then issues an
	// UpdatePolicy that re-sends the desired discrete-ports rule (no ranges),
	// self-healing the over-grant.
	if len(r.PortRanges) != 0 {
		return false
	}
	if len(r.Sources) != 1 || r.Sources[0].ID != gwGroupID {
		return false
	}
	if len(r.Destinations) != 1 || r.Destinations[0].ID != trackingGroupID {
		return false
	}
	got := append([]string(nil), r.Ports...)
	want := append([]string(nil), ports...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return false
	}
	desc := managedPolicyDescription(netbirdAccessPolicyPurpose)
	return p.Description == desc && r.Description == desc
}

// desiredServerAccessPolicyRequest builds the desired "op-gw-access-<serverID>"
// PolicyRequest: gateway group -> server tracking group, a single accept/tcp/
// unidirectional rule on the server's active application ports. Pure (no I/O) so
// it's independently unit-testable and shared by the create/update paths in
// reconcileServerPolicyWith.
func desiredServerAccessPolicyRequest(name, gwGroupID, trackingGroupID string, ports []string) netbird.PolicyRequest {
	desc := managedPolicyDescription(netbirdAccessPolicyPurpose)
	return netbird.PolicyRequest{
		Name:        name,
		Description: desc,
		Enabled:     true,
		Rules: []netbird.PolicyRuleRequest{{
			Name:          name,
			Description:   desc,
			Enabled:       true,
			Action:        "accept",
			Bidirectional: false,
			Protocol:      "tcp",
			Ports:         ports,
			Sources:       []string{gwGroupID},
			Destinations:  []string{trackingGroupID},
		}},
	}
}

// reconcileServerPolicy reconciles the ONE NetBird access policy for a single server
// (best-effort, void). It gates on the module being configured AND policy management
// being enabled, loads the server + the current policy list ONCE, and delegates the
// diff to reconcileServerPolicyWith. Every NetBird call is timeout-bounded; any error
// is Debug-logged and the reconcile simply stops (a settings/NetBird glitch must
// never fault a caller — SetServerNetbird / the event hooks / the sync loop).
func (s *Service) reconcileServerPolicy(ctx context.Context, serverID string) {
	s.netbird.policyMu.Lock()
	defer s.netbird.policyMu.Unlock()
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return
	}
	set := s.NetbirdPolicySettings(ctx, 0)
	if !set.ManagePolicies {
		return
	}
	server, err := s.routes.AIServerByID(ctx, serverID)
	if err != nil {
		slog.Debug("policy reconcile: load server", "server_id", serverID, "error", err)
		return
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	policies, err := netbird.ListPolicies(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		slog.Debug("policy reconcile: list policies", "server_id", serverID, "error", err)
		return
	}
	s.reconcileServerPolicyWith(ctx, ncfg, set, server, policies)
}

// reconcileServerPolicyWith performs the create/update/delete diff for one server's
// policy against an ALREADY-FETCHED policy list (so the fleet pass lists once). It
// finds the server's policy by name ("op-gw-access-<id>"). When the server is NOT
// managed OR has no active ports, an existing policy is DELETED (else nothing). When
// managed with active ports, it resolves the gateway group (the policy source),
// builds the desired accept/tcp rule (source=gateway group, destination=tracking
// group, ports=active), and CREATEs it (no existing) or UPDATEs it (existing differs;
// no-op when it already matches). Best-effort throughout (errors Debug-logged).
func (s *Service) reconcileServerPolicyWith(ctx context.Context, ncfg netbird.Config, set NetbirdPolicySettings, server routing.AIServer, policies []netbird.Policy) {
	name := NetbirdManagedPolicyPrefix + server.ID
	var existing *netbird.Policy
	for i := range policies {
		if policies[i].Name == name {
			existing = &policies[i]
			break
		}
	}
	ports, ok := s.activeAppPorts(ctx, server.ID)
	if !ok {
		// A transient ApplicationsByServer error must NOT tear down a healthy policy
		// (reconcile-loop invariant: keep the resource on a transient dependency
		// error). Skip this server this tick; leave the current policy untouched.
		return
	}
	if !serverManaged(server, set.EffectiveScope) || len(ports) == 0 {
		if existing != nil {
			if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID); err != nil {
				slog.Debug("policy reconcile: delete", "server_id", server.ID, "error", err)
			}
		}
		return
	}
	gwGroupID, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil {
		slog.Debug("policy reconcile: resolve gateway group", "server_id", server.ID, "error", err)
		return
	}
	desired := desiredServerAccessPolicyRequest(name, gwGroupID, server.NetbirdGroupID, ports)
	if existing == nil {
		if _, err := netbird.CreatePolicy(ctx, ncfg, netbirdCallTimeout, desired); err != nil {
			slog.Debug("policy reconcile: create", "server_id", server.ID, "error", err)
		}
		return
	}
	if !policyMatches(*existing, gwGroupID, server.NetbirdGroupID, ports) {
		if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID, desired); err != nil {
			slog.Debug("policy reconcile: update", "server_id", server.ID, "error", err)
		}
	}
}

// desiredAgentIngestPolicyRequest builds the desired "op-gw-agent-ingest"
// PolicyRequest: the given server tracking groups -> the gateway group on the
// agent port, a single accept/tcp/unidirectional rule. Pure (no I/O) so it's
// independently unit-testable and shared by the create/update paths in
// reconcileAgentIngestPolicyWith.
func desiredAgentIngestPolicyRequest(gwGroupID string, sources []string, agentPort string) netbird.PolicyRequest {
	desc := managedPolicyDescription(netbirdAgentIngestPolicyPurpose)
	return netbird.PolicyRequest{
		Name:        netbirdAgentIngestPolicyName,
		Description: desc,
		Enabled:     true,
		Rules: []netbird.PolicyRuleRequest{{
			Name:          netbirdAgentIngestPolicyName,
			Description:   desc,
			Enabled:       true,
			Action:        "accept",
			Bidirectional: false,
			Protocol:      "tcp",
			Ports:         []string{agentPort},
			Sources:       sources,
			Destinations:  []string{gwGroupID},
		}},
	}
}

// reconcileAgentIngestPolicyWith ensures the ONE account-wide op-gw-agent-ingest
// policy that lets every NetBird server's tracking group reach the gateway group on
// the agent port (server->gateway telemetry — the AGENT-INGEST direction, opposite
// and on a different port from the per-server op-gw-access-<id> gateway->server
// policies, so deny-by-default would otherwise drop it).
//
// Sources = the tracking groups of ALL NetBird-enabled servers that HAVE a tracking
// group, INDEPENDENT of policy scope / netbird_policy_override: agent ingest is
// orthogonal to the gateway->server least-privilege — even a server excluded from
// that still needs its agent to reach the gateway. Sorted + deduplicated. A server
// with no tracking group (purely manually linked) is skipped (needs a manual ACL).
//
// Best-effort (every NetBird error is Debug-logged, never surfaced/panicked). It
// reuses the ALREADY-listed policies slice — no extra ListPolicies — so it MUST be
// called under s.netbird.policyMu inside the same fleet-pass critical section (atomic
// with the per-server reconcile against one policy snapshot).
func (s *Service) reconcileAgentIngestPolicyWith(ctx context.Context, ncfg netbird.Config, servers []routing.AIServer, policies []netbird.Policy) {
	// 1. desired sources = sorted, distinct tracking-group ids of every NetBird
	//    server that has one.
	seen := map[string]struct{}{}
	var sources []string
	for _, srv := range servers {
		if !srv.NetbirdEnabled || srv.NetbirdGroupID == "" {
			continue
		}
		if _, dup := seen[srv.NetbirdGroupID]; dup {
			continue
		}
		seen[srv.NetbirdGroupID] = struct{}{}
		sources = append(sources, srv.NetbirdGroupID)
	}
	sort.Strings(sources)

	// 2. find the existing policy by name.
	var existing *netbird.Policy
	for i := range policies {
		if policies[i].Name == netbirdAgentIngestPolicyName {
			existing = &policies[i]
			break
		}
	}

	// 3. no NetBird servers -> the policy serves nothing; delete it if present.
	if len(sources) == 0 {
		if existing != nil {
			if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID); err != nil {
				slog.Debug("agent-ingest reconcile: delete", "error", err)
			}
		}
		return
	}

	// 4. resolve the gateway group (the policy destination).
	gwGroupID, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil {
		slog.Debug("agent-ingest reconcile: resolve gateway group", "error", err)
		return
	}

	// 5. desired policy: every server tracking group -> gateway group on the agent
	//    port, accept/tcp/unidirectional.
	desired := desiredAgentIngestPolicyRequest(gwGroupID, sources, s.agentPort)
	if existing == nil {
		if _, err := netbird.CreatePolicy(ctx, ncfg, netbirdCallTimeout, desired); err != nil {
			slog.Debug("agent-ingest reconcile: create", "error", err)
		}
		return
	}
	if !agentIngestPolicyMatches(*existing, gwGroupID, sources, s.agentPort) {
		if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID, desired); err != nil {
			slog.Debug("agent-ingest reconcile: update", "error", err)
		}
	}
}

// desiredAgentIngestTLSPolicyRequest builds the desired "op-gw-agent-ingest-tls"
// PolicyRequest: the given server tracking groups -> the gateway group on the
// SEPARATE encrypted agent TLS port, a single accept/tcp/unidirectional rule.
// The sibling of desiredAgentIngestPolicyRequest for the dedicated-TLS-port
// topology. Pure (no I/O) so it's independently unit-testable and shared by
// the create/update paths in reconcileAgentIngestTLSPolicyWith.
func desiredAgentIngestTLSPolicyRequest(gwGroupID string, sources []string, agentTLSPort string) netbird.PolicyRequest {
	desc := managedPolicyDescription(netbirdAgentIngestTLSPolicyPurpose)
	return netbird.PolicyRequest{
		Name:        NetbirdAgentIngestTLSPolicyName,
		Description: desc,
		Enabled:     true,
		Rules: []netbird.PolicyRuleRequest{{
			Name:          NetbirdAgentIngestTLSPolicyName,
			Description:   desc,
			Enabled:       true,
			Action:        "accept",
			Bidirectional: false,
			Protocol:      "tcp",
			Ports:         []string{agentTLSPort},
			Sources:       sources,
			Destinations:  []string{gwGroupID},
		}},
	}
}

// reconcileAgentIngestTLSPolicyWith ensures the ONE account-wide
// op-gw-agent-ingest-tls policy that lets every NetBird server's tracking
// group reach the gateway group on the SEPARATE encrypted agent TLS port
// (s.agentTLSPort) — the sibling of reconcileAgentIngestPolicyWith for the
// dedicated-TLS-port topology, existing ONLY while
// Service.CertMeshTLSSeparateActive is true.
//
// Sources = the SAME set reconcileAgentIngestPolicyWith uses: the tracking
// groups of ALL NetBird-enabled servers that HAVE a tracking group,
// INDEPENDENT of policy scope / netbird_policy_override (netbirdServerGroupIDs
// — agent ingest is orthogonal to the gateway->server least-privilege).
//
// On a CertMeshTLSSeparateActive error (a transient control-plane blip) this
// pass does NOT act on the TLS policy at all — it is left exactly as-is,
// mirroring the "don't churn on a blip" discipline used elsewhere in this
// file, rather than risk deleting a live policy on a spurious read error.
// When separate mode is not active (or there are no NetBird servers) the
// policy is DELETED if present (idempotent — a 404 from DeletePolicy is not
// an error). When active, the policy is created/updated to match.
//
// Best-effort (every NetBird error is Debug-logged, never surfaced/panicked).
// It reuses the ALREADY-listed policies slice — no extra ListPolicies — so it
// MUST be called under s.netbird.policyMu inside the same fleet-pass critical
// section (atomic with the per-server reconcile against one policy snapshot).
func (s *Service) reconcileAgentIngestTLSPolicyWith(ctx context.Context, ncfg netbird.Config, servers []routing.AIServer, policies []netbird.Policy) {
	// 1. find the existing policy by name.
	var existing *netbird.Policy
	for i := range policies {
		if policies[i].Name == NetbirdAgentIngestTLSPolicyName {
			existing = &policies[i]
			break
		}
	}

	// 2. resolve the effective separate-mode flag. On error, leave the TLS
	//    policy untouched this pass (never delete a live policy on a blip).
	active, err := s.CertMeshTLSSeparateActive(ctx)
	if err != nil {
		slog.Debug("agent-ingest-tls reconcile: resolve separate mode", "error", err)
		return
	}
	if !active {
		if existing != nil {
			if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID); err != nil {
				slog.Debug("agent-ingest-tls reconcile: delete (combined mode)", "error", err)
			}
		}
		return
	}

	// 3. sources = the same all-server-tracking-groups set the plaintext
	//    agent-ingest policy uses.
	sources := netbirdServerGroupIDs(servers)
	if len(sources) == 0 {
		if existing != nil {
			if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID); err != nil {
				slog.Debug("agent-ingest-tls reconcile: delete (no servers)", "error", err)
			}
		}
		return
	}

	// 4. resolve the gateway group (the policy destination).
	gwGroupID, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil {
		slog.Debug("agent-ingest-tls reconcile: resolve gateway group", "error", err)
		return
	}

	// 5. desired policy: every server tracking group -> gateway group on the
	//    agent TLS port, accept/tcp/unidirectional.
	agentTLSPort := strconv.Itoa(s.agentTLSPort)
	desired := desiredAgentIngestTLSPolicyRequest(gwGroupID, sources, agentTLSPort)
	if existing == nil {
		if _, err := netbird.CreatePolicy(ctx, ncfg, netbirdCallTimeout, desired); err != nil {
			slog.Debug("agent-ingest-tls reconcile: create", "error", err)
		}
		return
	}
	if !agentIngestTLSPolicyMatches(*existing, gwGroupID, sources, agentTLSPort) {
		if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID, desired); err != nil {
			slog.Debug("agent-ingest-tls reconcile: update", "error", err)
		}
	}
}

// desiredPingGatewayPolicyRequest builds the desired "op-gw-ping-gateway"
// PolicyRequest: the given server tracking groups -> the gateway group, a single
// accept/icmp/portless/unidirectional rule. Pure (no I/O) so it's independently
// unit-testable and shared by the create/update paths in
// reconcilePingGatewayPolicyWith.
func desiredPingGatewayPolicyRequest(gwGroupID string, sources []string) netbird.PolicyRequest {
	desc := managedPolicyDescription(netbirdPingGatewayPolicyPurpose)
	return netbird.PolicyRequest{
		Name: netbirdPingGatewayPolicyName, Description: desc, Enabled: true,
		Rules: []netbird.PolicyRuleRequest{{
			Name: netbirdPingGatewayPolicyName, Description: desc, Enabled: true, Action: "accept",
			Bidirectional: false, Protocol: "icmp", Ports: nil,
			Sources: sources, Destinations: []string{gwGroupID},
		}},
	}
}

// reconcilePingGatewayPolicyWith reconciles the account-wide op-gw-ping-gateway ICMP
// policy: every NetBird server tracking group -> the gateway group, accept/icmp/portless/
// unidirectional. Sources = all NetBird server groups (INDEPENDENT of policy scope, like
// agent ingest). Gated on set.AllowPingGateway; with the setting OFF or no sources the
// policy is DELETED when present. Best-effort (every NetBird error Debug-logged). It
// reuses the ALREADY-listed policies slice — no extra ListPolicies — so it MUST be called
// under s.netbird.policyMu inside the same fleet-pass critical section.
func (s *Service) reconcilePingGatewayPolicyWith(ctx context.Context, ncfg netbird.Config, set NetbirdPolicySettings, servers []routing.AIServer, policies []netbird.Policy) {
	var existing *netbird.Policy
	for i := range policies {
		if policies[i].Name == netbirdPingGatewayPolicyName {
			existing = &policies[i]
			break
		}
	}
	sources := netbirdServerGroupIDs(servers)
	if !set.AllowPingGateway || len(sources) == 0 {
		if existing != nil {
			if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID); err != nil {
				slog.Debug("ping-gateway reconcile: delete", "error", err)
			}
		}
		return
	}
	gwGroupID, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil || gwGroupID == "" {
		slog.Debug("ping-gateway reconcile: resolve gw group", "error", err)
		return
	}
	desired := desiredPingGatewayPolicyRequest(gwGroupID, sources)
	if existing == nil {
		if _, err := netbird.CreatePolicy(ctx, ncfg, netbirdCallTimeout, desired); err != nil {
			slog.Debug("ping-gateway reconcile: create", "error", err)
		}
		return
	}
	if !pingGatewayPolicyMatches(*existing, gwGroupID, sources) {
		if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID, desired); err != nil {
			slog.Debug("ping-gateway reconcile: update", "error", err)
		}
	}
}

// desiredPingServersPolicyRequest builds the desired "op-gw-ping-servers"
// PolicyRequest: the gateway group -> the given server tracking groups, a single
// accept/icmp/portless/unidirectional rule. Pure (no I/O) so it's independently
// unit-testable and shared by the create/update paths in
// reconcilePingServersPolicyWith.
func desiredPingServersPolicyRequest(gwGroupID string, destinations []string) netbird.PolicyRequest {
	desc := managedPolicyDescription(netbirdPingServersPolicyPurpose)
	return netbird.PolicyRequest{
		Name: netbirdPingServersPolicyName, Description: desc, Enabled: true,
		Rules: []netbird.PolicyRuleRequest{{
			Name: netbirdPingServersPolicyName, Description: desc, Enabled: true, Action: "accept",
			Bidirectional: false, Protocol: "icmp", Ports: nil,
			Sources: []string{gwGroupID}, Destinations: destinations,
		}},
	}
}

// reconcilePingServersPolicyWith reconciles the account-wide op-gw-ping-servers ICMP
// policy: the gateway group -> the server tracking groups, accept/icmp/portless/
// unidirectional. Destinations = ALL NetBird server groups when set.AllowPingAllServers,
// else only the per-server NetbirdAllowPing groups. An empty destination set DELETES the
// policy when present. Best-effort; must be called under s.netbird.policyMu reusing the
// caller's policy snapshot.
func (s *Service) reconcilePingServersPolicyWith(ctx context.Context, ncfg netbird.Config, set NetbirdPolicySettings, servers []routing.AIServer, policies []netbird.Policy) {
	var existing *netbird.Policy
	for i := range policies {
		if policies[i].Name == netbirdPingServersPolicyName {
			existing = &policies[i]
			break
		}
	}
	destinations := netbirdPingServerGroupIDs(servers, set.AllowPingAllServers)
	if len(destinations) == 0 {
		if existing != nil {
			if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID); err != nil {
				slog.Debug("ping-servers reconcile: delete", "error", err)
			}
		}
		return
	}
	gwGroupID, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil || gwGroupID == "" {
		slog.Debug("ping-servers reconcile: resolve gw group", "error", err)
		return
	}
	desired := desiredPingServersPolicyRequest(gwGroupID, destinations)
	if existing == nil {
		if _, err := netbird.CreatePolicy(ctx, ncfg, netbirdCallTimeout, desired); err != nil {
			slog.Debug("ping-servers reconcile: create", "error", err)
		}
		return
	}
	if !pingServersPolicyMatches(*existing, gwGroupID, destinations) {
		if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, existing.ID, desired); err != nil {
			slog.Debug("ping-servers reconcile: update", "error", err)
		}
	}
}

// agentIngestPolicyMatches reports whether an existing policy already encodes exactly
// the desired agent-ingest rule: a top-level ENABLED policy with a single enabled
// accept/tcp/unidirectional rule whose sole destination is the gateway group, whose
// single port is the agent port, no port ranges, and whose source set equals the
// desired tracking-group set (order-insensitive). It mirrors policyMatches but the
// destination is fixed (the gateway group) and the sources are a SET. A drifted
// top-level-disabled policy fails the match -> UpdatePolicy re-enables it (self-heal,
// like the per-server policyMatches fix in 675b14a).
func agentIngestPolicyMatches(p netbird.Policy, gwGroupID string, sources []string, agentPort string) bool {
	if !p.Enabled || len(p.Rules) != 1 {
		return false
	}
	r := p.Rules[0]
	if !r.Enabled || r.Action != "accept" || r.Bidirectional || r.Protocol != "tcp" {
		return false
	}
	if len(r.Destinations) != 1 || r.Destinations[0].ID != gwGroupID {
		return false
	}
	if len(r.PortRanges) != 0 {
		return false
	}
	if len(r.Ports) != 1 || r.Ports[0] != agentPort {
		return false
	}
	got := make([]string, 0, len(r.Sources))
	for _, src := range r.Sources {
		got = append(got, src.ID)
	}
	want := append([]string(nil), sources...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return false
	}
	desc := managedPolicyDescription(netbirdAgentIngestPolicyPurpose)
	return p.Description == desc && r.Description == desc
}

// agentIngestTLSPolicyMatches reports whether an existing policy already encodes
// exactly the desired op-gw-agent-ingest-tls rule: a top-level ENABLED policy
// with a single enabled accept/tcp/unidirectional rule whose sole destination
// is the gateway group, whose single port is the agent TLS port, no port
// ranges, and whose source set equals the desired tracking-group set
// (order-insensitive). It clones agentIngestPolicyMatches exactly except the
// port and the managed-policy Description (netbirdAgentIngestTLSPolicyPurpose).
// A drifted top-level-disabled policy fails the match -> UpdatePolicy
// re-enables it (self-heal, like agentIngestPolicyMatches).
func agentIngestTLSPolicyMatches(p netbird.Policy, gwGroupID string, sources []string, agentTLSPort string) bool {
	if !p.Enabled || len(p.Rules) != 1 {
		return false
	}
	r := p.Rules[0]
	if !r.Enabled || r.Action != "accept" || r.Bidirectional || r.Protocol != "tcp" {
		return false
	}
	if len(r.Destinations) != 1 || r.Destinations[0].ID != gwGroupID {
		return false
	}
	if len(r.PortRanges) != 0 {
		return false
	}
	if len(r.Ports) != 1 || r.Ports[0] != agentTLSPort {
		return false
	}
	got := make([]string, 0, len(r.Sources))
	for _, src := range r.Sources {
		got = append(got, src.ID)
	}
	want := append([]string(nil), sources...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return false
	}
	desc := managedPolicyDescription(netbirdAgentIngestTLSPolicyPurpose)
	return p.Description == desc && r.Description == desc
}

// pingGatewayPolicyMatches reports whether an existing policy already encodes exactly
// the desired op-gw-ping-gateway rule: a top-level ENABLED policy with a single enabled
// accept/icmp/unidirectional PORTLESS rule whose sole destination is the gateway group
// and whose source set equals the desired tracking-group set (order-insensitive). It
// clones agentIngestPolicyMatches but the protocol is ICMP and there are no ports. A
// drifted top-level-disabled policy fails the match -> UpdatePolicy re-enables it.
func pingGatewayPolicyMatches(p netbird.Policy, gwGroupID string, sources []string) bool {
	if !p.Enabled || len(p.Rules) != 1 {
		return false
	}
	r := p.Rules[0]
	if !r.Enabled || r.Action != "accept" || r.Bidirectional || r.Protocol != "icmp" {
		return false
	}
	if len(r.Ports) != 0 || len(r.PortRanges) != 0 {
		return false
	}
	if len(r.Destinations) != 1 || r.Destinations[0].ID != gwGroupID {
		return false
	}
	got := make([]string, 0, len(r.Sources))
	for _, src := range r.Sources {
		got = append(got, src.ID)
	}
	want := append([]string(nil), sources...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return false
	}
	desc := managedPolicyDescription(netbirdPingGatewayPolicyPurpose)
	return p.Description == desc && r.Description == desc
}

// pingServersPolicyMatches reports whether an existing policy already encodes exactly
// the desired op-gw-ping-servers rule: a top-level ENABLED policy with a single enabled
// accept/icmp/unidirectional PORTLESS rule whose sole source is the gateway group and
// whose destination set equals the desired server-group set (order-insensitive). The
// mirror of pingGatewayPolicyMatches with source/destination swapped.
func pingServersPolicyMatches(p netbird.Policy, gwGroupID string, destinations []string) bool {
	if !p.Enabled || len(p.Rules) != 1 {
		return false
	}
	r := p.Rules[0]
	if !r.Enabled || r.Action != "accept" || r.Bidirectional || r.Protocol != "icmp" {
		return false
	}
	if len(r.Ports) != 0 || len(r.PortRanges) != 0 {
		return false
	}
	if len(r.Sources) != 1 || r.Sources[0].ID != gwGroupID {
		return false
	}
	got := make([]string, 0, len(r.Destinations))
	for _, dst := range r.Destinations {
		got = append(got, dst.ID)
	}
	want := append([]string(nil), destinations...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return false
	}
	desc := managedPolicyDescription(netbirdPingServersPolicyPurpose)
	return p.Description == desc && r.Description == desc
}

// deleteServerPolicy best-effort deletes a server's NetBird access policy (found by
// name "op-gw-access-<id>"), used on disable + server delete. Void: a list/delete
// error is Debug-logged and never propagated (never fails the disable/delete).
func (s *Service) deleteServerPolicy(ctx context.Context, ncfg netbird.Config, serverID string) {
	name := NetbirdManagedPolicyPrefix + serverID
	policies, err := netbird.ListPolicies(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		slog.Debug("policy delete: list", "server_id", serverID, "error", err)
		return
	}
	for i := range policies {
		if policies[i].Name != name {
			continue
		}
		if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, policies[i].ID); err != nil {
			slog.Debug("policy delete: delete", "server_id", serverID, "error", err)
		}
		return
	}
}

// netbirdPolicyGroups returns a peer's POLICY group refs — its actual NetBird group
// membership with the per-server tracking group filtered out (never mirrored to the
// portal). Mirrors cmd/gateway policyGroups (the T6 refactor removes the cmd copy).
func netbirdPolicyGroups(groups []netbird.GroupRef, trackingGroupID string) []netbird.GroupRef {
	out := make([]netbird.GroupRef, 0, len(groups))
	for _, g := range groups {
		if trackingGroupID != "" && g.ID == trackingGroupID {
			continue
		}
		out = append(out, g)
	}
	return out
}

// reconcileAllServerPolicies reconciles ONLY the access policies of every NetBird
// server against a single ListPolicies (no group mirror, no deny-by-default). Used by
// the policy-only sync loop (T6). Best-effort/void; gated on policy management.
func (s *Service) reconcileAllServerPolicies(ctx context.Context) {
	s.netbird.policyMu.Lock()
	defer s.netbird.policyMu.Unlock()
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return
	}
	set := s.NetbirdPolicySettings(ctx, 0)
	if !set.ManagePolicies {
		return
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	policies, err := netbird.ListPolicies(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		slog.Debug("reconcile policies: list", "error", err)
		return
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Debug("reconcile policies: list servers", "error", err)
		return
	}
	for _, server := range servers {
		if server.NetbirdEnabled {
			s.reconcileServerPolicyWith(ctx, ncfg, set, server, policies)
		}
	}
	// Fleet-wide agent-ingest policy: reuse the SAME policy snapshot, still under the
	// deferred netbirdPolicyMu unlock (atomic with the per-server reconcile above).
	s.reconcileAgentIngestPolicyWith(ctx, ncfg, servers, policies)
	// Fleet-wide agent-ingest-TLS policy (separate mode only; same snapshot, same lock).
	s.reconcileAgentIngestTLSPolicyWith(ctx, ncfg, servers, policies)
	// Fleet-wide ICMP ping-allow policies (same snapshot, same lock).
	s.reconcilePingGatewayPolicyWith(ctx, ncfg, set, servers, policies)
	s.reconcilePingServersPolicyWith(ctx, ncfg, set, servers, policies)
}

// netbirdPolicyToRequest converts a read-shape Policy back into a write-shape
// PolicyRequest (group refs → id strings) so a fetched policy can be re-PUT with a
// single field flipped (deny-by-default enable/disable).
//
// NOTE: PortRanges are NOT carried across the read→write conversion (PolicyRuleRequest
// has no port_ranges field). This is acceptable because the only policy this helper
// round-trips is the account catch-all "Default" (All↔All, all-ports, no ranges) for
// the deny-by-default enable/disable flip — port ranges are an explicit feature
// non-goal for gateway-managed policies (which only ever use discrete `Ports`).
func netbirdPolicyToRequest(p netbird.Policy) netbird.PolicyRequest {
	rules := make([]netbird.PolicyRuleRequest, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, netbird.PolicyRuleRequest{
			Name:          r.Name,
			Description:   r.Description,
			Enabled:       r.Enabled,
			Action:        r.Action,
			Bidirectional: r.Bidirectional,
			Protocol:      r.Protocol,
			Ports:         r.Ports,
			Sources:       netbirdGroupRefIDs(r.Sources),
			Destinations:  netbirdGroupRefIDs(r.Destinations),
		})
	}
	return netbird.PolicyRequest{Name: p.Name, Description: p.Description, Enabled: p.Enabled, Rules: rules}
}

// applyDenyByDefault toggles NetBird's built-in catch-all "Default" policy: when
// denyOn is true the Default policy is DISABLED (so only explicit allow policies
// permit traffic); when false it is re-ENABLED. Best-effort/void: it lists policies,
// finds "Default", and re-PUTs it only when its enabled state actually needs to flip
// (no-op when already in the target state or the policy is absent).
func (s *Service) applyDenyByDefault(ctx context.Context, ncfg netbird.Config, denyOn bool) {
	s.netbird.policyMu.Lock()
	defer s.netbird.policyMu.Unlock()
	policies, err := netbird.ListPolicies(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		slog.Debug("deny-by-default apply: list", "error", err)
		return
	}
	for i := range policies {
		if policies[i].Name != NetbirdDefaultPolicyName {
			continue
		}
		want := !denyOn // deny ON => Default disabled
		if policies[i].Enabled == want {
			return
		}
		req := netbirdPolicyToRequest(policies[i])
		req.Enabled = want
		if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, policies[i].ID, req); err != nil {
			slog.Debug("deny-by-default apply: update", "error", err)
		}
		return
	}
}

// enforceDenyByDefault disables NetBird's catch-all "Default" policy (best-effort).
func (s *Service) enforceDenyByDefault(ctx context.Context, ncfg netbird.Config) {
	s.applyDenyByDefault(ctx, ncfg, true)
}
