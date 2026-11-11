// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"log/slog"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// NetbirdRPCPolicyPrefix is the name prefix of the managed policy that lets
	// llama.cpp RPC cluster heads reach one member server: "op-gw-rpc-<member
	// server id>". It does not start with NetbirdManagedPolicyPrefix, so the
	// status endpoint's managed_policy_count never counts these.
	NetbirdRPCPolicyPrefix = "op-gw-rpc-"
	// netbirdRPCPolicyPurpose is the managedPolicyDescription purpose of the
	// per-member-server "op-gw-rpc-<serverID>" policy.
	netbirdRPCPolicyPurpose = "cluster head → RPC member (member port)"
)

// rpcRule is one grant of an RPC member policy: the head server's tracking
// group may open the member's port on the member server's tracking group.
type rpcRule struct {
	sourceGroupID string
	destGroupID   string
	port          int
}

// rpcCluster is a stored cluster head with the members it dials and the
// network and transport it resolves to. derived is set when the runtime
// documents carry the cluster (deriveCluster withholds it for no reason);
// only rpcClusters with derive set fills it.
type rpcCluster struct {
	head       routing.RuntimeSpec
	headServer routing.AIServer
	members    []routing.ClusterMember
	servers    map[string]routing.AIServer
	network    string
	transport  string
	derived    bool
}

// rpcClusters lists every enabled cluster head with its membership, resolving
// the network and transport the way the derivation does; with derive it also
// derives each cluster, as no particular server's document, to tell whether
// the runtime documents carry it. A store error is returned whole: a caller
// that acts on a partial list would delete policies of clusters it failed to
// read.
func (s *Service) rpcClusters(ctx context.Context, servers []routing.AIServer, derive bool) ([]rpcCluster, error) {
	byID := make(map[string]routing.AIServer, len(servers))
	for _, srv := range servers {
		byID[srv.ID] = srv
	}
	var out []rpcCluster
	for _, srv := range servers {
		apps, err := s.routes.ApplicationsByServer(ctx, srv.ID)
		if err != nil {
			return nil, err
		}
		for _, app := range apps {
			if app.Type != routing.ProviderServerAgent {
				continue
			}
			specs, err := s.routes.RuntimeSpecsByApplication(ctx, app.ID)
			if err != nil {
				return nil, err
			}
			for _, spec := range specs {
				if !spec.ClusterEnabled || !spec.Enabled {
					continue
				}
				members, _, err := s.routes.ClusterMembers(ctx, spec.ID)
				if err != nil {
					return nil, err
				}
				c := rpcCluster{head: spec, headServer: srv, members: members, servers: byID}
				all := []routing.AIServer{srv}
				for _, m := range members {
					all = append(all, byID[m.ServerID])
				}
				if c.network, c.transport, err = s.resolveClusterNetwork(ctx, spec, all); err != nil {
					return nil, err
				}
				if derive {
					d, err := s.deriveCluster(ctx, spec, srv, "")
					if err != nil {
						return nil, err
					}
					c.derived = d.reason == ""
				}
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// resolveClusterNetwork is the network and transport of a head: the stored
// choice, else the defaults over its head and member servers. Shared by the
// derivation and the policy pass so they cannot disagree.
func (s *Service) resolveClusterNetwork(ctx context.Context, head routing.RuntimeSpec, servers []routing.AIServer) (network, transport string, err error) {
	network, transport = head.ClusterNetwork, head.RPCTransport
	if network == "" {
		network = defaultClusterNetwork(servers)
	}
	if transport == "" {
		if transport, err = s.defaultRPCTransport(ctx, network); err != nil {
			return "", "", err
		}
	}
	return network, transport, nil
}

// directMeshClusterExists reports whether a cluster relies on NetBird's policy
// enforcement: a direct cluster on the mesh exposes every member's
// unauthenticated RPC port to the whole mesh unless the catch-all policy is
// off and the managed policies are the only grants. Every enabled head
// counts, also one withheld right now: it is pushed again as soon as its
// reason goes away.
func (s *Service) directMeshClusterExists(ctx context.Context) (bool, error) {
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return false, err
	}
	clusters, err := s.rpcClusters(ctx, servers, false)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(clusters, func(c rpcCluster) bool {
		return c.network == clusterNetworkMesh && c.transport == rpcTransportDirect
	}), nil
}

// desiredRPCRules groups the grants every mesh cluster needs by member server.
// Only a cluster the runtime documents carry gets grants: one withheld --
// a member that withdrew its consent or turned RPC membership off, a missing
// binary -- runs nowhere, and its member's port stays closed. A member needs
// a tracking group on both ends and an allocated port; the grants are
// deduplicated (several heads of one server may dial one port) and sorted so
// the desired policy is stable.
func desiredRPCRules(clusters []rpcCluster) map[string][]rpcRule {
	out := map[string][]rpcRule{}
	for _, c := range clusters {
		if !c.derived || c.network != clusterNetworkMesh || !serverHasGroup(c.headServer) {
			continue
		}
		for _, m := range c.members {
			member, ok := c.servers[m.ServerID]
			if !ok || !serverHasGroup(member) || m.Port <= 0 || m.Port > 65535 {
				continue
			}
			r := rpcRule{sourceGroupID: c.headServer.NetbirdGroupID, destGroupID: member.NetbirdGroupID, port: m.Port}
			if !slices.Contains(out[member.ID], r) {
				out[member.ID] = append(out[member.ID], r)
			}
		}
	}
	for id := range out {
		sortRPCRules(out[id])
	}
	return out
}

func serverHasGroup(srv routing.AIServer) bool {
	return srv.NetbirdEnabled && srv.NetbirdGroupID != ""
}

func sortRPCRules(rules []rpcRule) {
	sort.Slice(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if a.sourceGroupID != b.sourceGroupID {
			return a.sourceGroupID < b.sourceGroupID
		}
		if a.destGroupID != b.destGroupID {
			return a.destGroupID < b.destGroupID
		}
		return a.port < b.port
	})
}

// desiredRPCPolicyRequest builds the "op-gw-rpc-<member server id>" policy:
// one accept/tcp/unidirectional rule per grant, each opening one port. The
// rules are named by position, which the sorted grants keep stable.
func desiredRPCPolicyRequest(memberServer routing.AIServer, rules []rpcRule) netbird.PolicyRequest {
	name := NetbirdRPCPolicyPrefix + memberServer.ID
	desc := managedPolicyDescription(netbirdRPCPolicyPurpose)
	out := make([]netbird.PolicyRuleRequest, 0, len(rules))
	for i, r := range rules {
		out = append(out, netbird.PolicyRuleRequest{
			Name:          name + "-" + strconv.Itoa(i+1),
			Description:   desc,
			Enabled:       true,
			Action:        "accept",
			Bidirectional: false,
			Protocol:      "tcp",
			Ports:         []string{strconv.Itoa(r.port)},
			Sources:       []string{r.sourceGroupID},
			Destinations:  []string{r.destGroupID},
		})
	}
	return netbird.PolicyRequest{Name: name, Description: desc, Enabled: true, Rules: out}
}

// rpcPolicyMatches reports whether p grants exactly the given rules and
// nothing else: an enabled policy whose rules are enabled accept/tcp/
// unidirectional single-port grants (no port ranges) equal, as a set, to the
// desired ones. Any other rule, or a changed description, is drift.
func rpcPolicyMatches(p netbird.Policy, rules []rpcRule) bool {
	desc := managedPolicyDescription(netbirdRPCPolicyPurpose)
	if !p.Enabled || p.Description != desc || len(p.Rules) != len(rules) {
		return false
	}
	got := make([]rpcRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		if !r.Enabled || r.Action != "accept" || r.Bidirectional || r.Protocol != "tcp" || r.Description != desc ||
			len(r.PortRanges) != 0 || len(r.Ports) != 1 || len(r.Sources) != 1 || len(r.Destinations) != 1 {
			return false
		}
		port, err := strconv.Atoi(r.Ports[0])
		if err != nil {
			return false
		}
		got = append(got, rpcRule{sourceGroupID: r.Sources[0].ID, destGroupID: r.Destinations[0].ID, port: port})
	}
	want := slices.Clone(rules)
	sortRPCRules(got)
	sortRPCRules(want)
	return slices.Equal(got, want)
}

// reconcileRPCPoliciesWith creates, updates and deletes the per-member RPC
// policies against an already-fetched policy list. It must run under
// policyMu, like every other family reconciled from one snapshot. Policies
// with the prefix whose server has no mesh member anymore are removed. A
// store error skips the whole pass: nothing is created or deleted from a
// cluster list that could not be read. Best-effort, errors are Debug-logged.
func (s *Service) reconcileRPCPoliciesWith(ctx context.Context, ncfg netbird.Config, servers []routing.AIServer, policies []netbird.Policy) {
	clusters, err := s.rpcClusters(ctx, servers, true)
	if err != nil {
		slog.Debug("rpc policy reconcile: read clusters", "error", err)
		return
	}
	desired := desiredRPCRules(clusters)
	byID := make(map[string]routing.AIServer, len(servers))
	for _, srv := range servers {
		byID[srv.ID] = srv
	}
	existing := map[string]netbird.Policy{}
	var extras []netbird.Policy
	for _, p := range policies {
		if !strings.HasPrefix(p.Name, NetbirdRPCPolicyPrefix) {
			continue
		}
		id := strings.TrimPrefix(p.Name, NetbirdRPCPolicyPrefix)
		if _, dup := existing[id]; dup {
			// A second policy of the same name would grant on its own whatever
			// it holds; only the first is kept.
			extras = append(extras, p)
			continue
		}
		existing[id] = p
	}
	for _, p := range extras {
		if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, p.ID); err != nil {
			slog.Debug("rpc policy reconcile: delete duplicate", "policy_id", p.ID, "error", err)
		}
	}
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		req := desiredRPCPolicyRequest(byID[id], desired[id])
		cur, ok := existing[id]
		switch {
		case !ok:
			if _, err := netbird.CreatePolicy(ctx, ncfg, netbirdCallTimeout, req); err != nil {
				slog.Debug("rpc policy reconcile: create", "server_id", id, "error", err)
			}
		case !rpcPolicyMatches(cur, desired[id]):
			if _, err := netbird.UpdatePolicy(ctx, ncfg, netbirdCallTimeout, cur.ID, req); err != nil {
				slog.Debug("rpc policy reconcile: update", "server_id", id, "error", err)
			}
		}
	}
	for id, p := range existing {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := netbird.DeletePolicy(ctx, ncfg, netbirdCallTimeout, p.ID); err != nil {
			slog.Debug("rpc policy reconcile: delete", "server_id", id, "error", err)
		}
	}
}

// reconcileRPCPolicies is the synchronous hook of every cluster, membership
// and member write: it reconciles the RPC policy family on its own policy
// snapshot, so a head that was just saved can reach its members before it
// starts. Gated on the module and on policy management; best-effort and never
// fails the write that called it.
func (s *Service) reconcileRPCPolicies(ctx context.Context) {
	s.netbird.policyMu.Lock()
	defer s.netbird.policyMu.Unlock()
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return
	}
	if !s.NetbirdPolicySettings(ctx, 0).ManagePolicies {
		return
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	policies, err := netbird.ListPolicies(ctx, ncfg, netbirdCallTimeout)
	if err != nil {
		slog.Debug("rpc policy reconcile: list policies", "error", err)
		return
	}
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Debug("rpc policy reconcile: list servers", "error", err)
		return
	}
	s.recordDefaultPolicyState(ctx, policies)
	s.reconcileRPCPoliciesWith(ctx, ncfg, servers, policies)
}

// recordDefaultPolicyState stores what a policy pass saw of NetBird's built-in
// "Default" policy, and re-derives every cluster when that changed: a direct
// mesh cluster is only pushed while the catch-all is off, and only a policy
// pass talks to NetBird. Without a Default policy nothing allows all traffic,
// so that counts as disabled. The value is rewritten only on a change, so its
// timestamp is when the state was first seen.
func (s *Service) recordDefaultPolicyState(ctx context.Context, policies []netbird.Policy) {
	if s.settings == nil {
		return
	}
	enabled := slices.ContainsFunc(policies, func(p netbird.Policy) bool {
		return p.Name == NetbirdDefaultPolicyName && p.Enabled
	})
	s.storeDefaultPolicyState(ctx, defaultPolicyState(enabled))
}

func defaultPolicyState(enabled bool) string {
	if enabled {
		return NetbirdDefaultPolicyEnabled
	}
	return NetbirdDefaultPolicyDisabled
}

// clusterDriftStopTimeout bounds the cluster stops and the notification that
// follow a drift of NetBird's Default policy (storeDefaultPolicyState): each
// head's drain is bounded by the orchestration's own stop bound, the heads
// stop in parallel.
const clusterDriftStopTimeout = 5 * time.Minute

// storeDefaultPolicyState writes state when it differs from the stored one and
// then notifies every cluster server. A change to enabled is a drift: the
// derivation now withholds every direct mesh cluster, so those are stopped
// head first (stopDriftedClusters) before the documents that withhold them
// are pushed, which would otherwise remove the members under running heads.
// Every caller holds the policy lock, and the stops take a minute or more, so
// they and the notification after them run on their own goroutine, under
// their own deadline and never the caller's context (a request's may end
// with its response); that goroutine never takes the policy lock.
func (s *Service) storeDefaultPolicyState(ctx context.Context, state string) {
	if s.settings == nil {
		return
	}
	if parseNetbirdDefaultPolicyState(s.systemSettingValues(ctx)[netbirdDefaultPolicyStateKey]).State == state {
		return
	}
	now := s.clock()
	raw, err := json.Marshal(netbirdDefaultPolicyState{State: state, At: now})
	if err != nil {
		return
	}
	if err := s.settings.SetSystemSetting(ctx, netbirdDefaultPolicyStateKey, string(raw), now); err != nil {
		slog.Debug("default policy state: store", "state", state, "error", err)
		return
	}
	if state != NetbirdDefaultPolicyEnabled {
		s.notifyAllClusterServers(ctx)
		return
	}
	s.netbird.policySideEffectWG.Add(1)
	go func() {
		defer s.netbird.policySideEffectWG.Done()
		dctx, cancel := context.WithTimeout(context.Background(), clusterDriftStopTimeout)
		defer cancel()
		s.stopDriftedClusters(dctx)
		s.notifyAllClusterServers(dctx)
	}()
}

// NetbirdDefaultPolicyStateView returns the last recorded state of NetBird's
// Default policy ("unknown" until a policy pass has seen it) and when that
// state was first seen (zero when unknown).
func (s *Service) NetbirdDefaultPolicyStateView(ctx context.Context) (state string, at time.Time) {
	st := parseNetbirdDefaultPolicyState(s.systemSettingValues(ctx)[netbirdDefaultPolicyStateKey])
	return st.State, st.At
}
