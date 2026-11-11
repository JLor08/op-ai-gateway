// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/routing"
	"slices"
	"time"
)

// ClusterWithheldHeadDisabled is a ClusterPlan's reason for a head spec that
// is disabled: no document carries it, so nothing can start it.
const ClusterWithheldHeadDisabled = "runtime.cluster_head_disabled"

// ClusterPlan is what the gateway's cluster orchestration needs to start one
// cluster head: the head's scope (its spec, mapping, application and server,
// from which the head's agent target is built) and, when the cluster is
// derivable, each member in member order with the agent spec id the runtime
// documents push for it. WithheldReason is the derivation's reason when the
// cluster is not pushed (a ClusterWithheld* code); Members is then empty.
// Network, Transport and Generation are the head's cluster block's.
type ClusterPlan struct {
	Head           RuntimeActionScope
	WithheldReason string
	Network        string
	Transport      string
	Generation     string
	Members        []ClusterPlanMember
}

// ClusterPlanMember is one member of a ClusterPlan: its position in the
// member order, its server and that server's server_agent application (the
// agent router the member routes are sent to), the agent spec id of its
// rpc_server spec and that spec's startup timeout.
type ClusterPlanMember struct {
	Position              int
	Server                routing.AIServer
	Application           routing.Application
	SpecID                string
	StartupTimeoutSeconds int
}

// ClusterPlan returns headSpecID's plan, built from the same derivation the
// runtime documents use (deriveCluster), so the orchestration starts exactly
// the members the documents carry. ErrRuntimeSpecNotFound when the spec does
// not exist, is no cluster head, or its mapping, application or server is
// gone. A derivable cluster whose member server lost its server_agent
// application between the derivation and this read reads as withheld
// (ClusterWithheldMemberUnmanaged), as the derivation itself would.
func (s *Service) ClusterPlan(ctx context.Context, headSpecID string) (ClusterPlan, error) {
	head, ok, err := s.routes.RuntimeSpecByID(ctx, headSpecID)
	if err != nil {
		return ClusterPlan{}, err
	}
	if !ok || !head.ClusterEnabled {
		return ClusterPlan{}, ErrRuntimeSpecNotFound
	}
	scope, ok, err := s.headScope(ctx, head)
	if err != nil {
		return ClusterPlan{}, err
	}
	if !ok {
		return ClusterPlan{}, ErrRuntimeSpecNotFound
	}
	plan := ClusterPlan{Head: scope, Members: []ClusterPlanMember{}}
	if !head.Enabled {
		plan.WithheldReason = ClusterWithheldHeadDisabled
		return plan, nil
	}
	d, err := s.deriveCluster(ctx, head, scope.Server, scope.Server.ID)
	if err != nil {
		return ClusterPlan{}, err
	}
	if d.reason != "" {
		plan.WithheldReason = d.reason
		return plan, nil
	}
	plan.Network, plan.Transport, plan.Generation = d.head.Network, d.head.Transport, d.head.Generation
	rows, _, err := s.routes.ClusterMembers(ctx, head.ID)
	if err != nil {
		return ClusterPlan{}, err
	}
	for _, row := range rows {
		m, reason, err := s.clusterPlanMember(ctx, row, d)
		if err != nil {
			return ClusterPlan{}, err
		}
		if reason != "" {
			return withheldPlan(plan, reason), nil
		}
		plan.Members = append(plan.Members, m)
	}
	return plan, nil
}

// clusterPlanMember is the plan entry of one membership row of a derivable
// cluster d, or the reason it can no longer be planned: its member spec is
// not in the derivation or its server is gone (member missing), or the
// server lost its server_agent application (member unmanaged).
func (s *Service) clusterPlanMember(ctx context.Context, row routing.ClusterMember, d clusterDerivation) (ClusterPlanMember, string, error) {
	spec, ok := d.members[row.ServerID]
	if !ok {
		return ClusterPlanMember{}, ClusterWithheldMemberMissing, nil
	}
	srv, err := s.routes.AIServerByID(ctx, row.ServerID)
	if err != nil {
		if ignoreNotFound(err) == nil {
			return ClusterPlanMember{}, ClusterWithheldMemberMissing, nil
		}
		return ClusterPlanMember{}, "", err
	}
	app, ok, err := s.serverAgentApplication(ctx, row.ServerID)
	if err != nil {
		return ClusterPlanMember{}, "", err
	}
	if !ok {
		return ClusterPlanMember{}, ClusterWithheldMemberUnmanaged, nil
	}
	return ClusterPlanMember{
		Position: row.Position, Server: srv, Application: app,
		SpecID: spec.ID, StartupTimeoutSeconds: spec.StartupTimeoutSeconds,
	}, "", nil
}

func withheldPlan(plan ClusterPlan, reason string) ClusterPlan {
	plan.WithheldReason = reason
	plan.Network, plan.Transport, plan.Generation = "", "", ""
	plan.Members = []ClusterPlanMember{}
	return plan
}

// headScope resolves head -> mapping -> application -> server; ok is false
// when a link is gone or the application is no server_agent one.
func (s *Service) headScope(ctx context.Context, head routing.RuntimeSpec) (RuntimeActionScope, bool, error) {
	mapping, err := s.routes.MappingByID(ctx, head.MappingID)
	if err != nil {
		return RuntimeActionScope{}, false, ignoreNotFound(err)
	}
	app, err := s.routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		return RuntimeActionScope{}, false, ignoreNotFound(err)
	}
	if app.Type != routing.ProviderServerAgent {
		return RuntimeActionScope{}, false, nil
	}
	server, err := s.routes.AIServerByID(ctx, app.ServerID)
	if err != nil {
		return RuntimeActionScope{}, false, ignoreNotFound(err)
	}
	return RuntimeActionScope{Server: server, Application: app, Mapping: mapping, Spec: head}, true, nil
}

// serverAgentApplication returns serverID's server_agent application, the
// one agent router the server runs.
func (s *Service) serverAgentApplication(ctx context.Context, serverID string) (routing.Application, bool, error) {
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return routing.Application{}, false, err
	}
	i := slices.IndexFunc(apps, func(a routing.Application) bool { return a.Type == routing.ProviderServerAgent })
	if i < 0 {
		return routing.Application{}, false, nil
	}
	return apps[i], true, nil
}

// KeptUpClusterHeads lists the enabled cluster heads the gateway keeps up on
// its own: pinned or force_running. For a head these mean that the gateway,
// not the agent, starts it -- an agent never starts a cold head by itself, so
// the gateway's reconciler runs the members-first start for them. In no
// particular order; a server or application whose specs cannot be read fails
// the whole list.
func (s *Service) KeptUpClusterHeads(ctx context.Context) ([]string, error) {
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return nil, err
	}
	var heads []string
	err = s.forEachAgentSpec(ctx, servers, func(_ routing.AIServer, spec routing.RuntimeSpec) error {
		if spec.ClusterEnabled && spec.Enabled && (spec.Pinned || spec.AdminState == adminStateForceRunning) {
			heads = append(heads, spec.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return heads, nil
}

// forEachAgentSpec calls fn for every runtime spec of every server_agent
// application on servers, server by server, in the store's order. The first
// error, of a read or of fn, ends the walk and is returned.
func (s *Service) forEachAgentSpec(ctx context.Context, servers []routing.AIServer, fn func(srv routing.AIServer, spec routing.RuntimeSpec) error) error {
	for _, srv := range servers {
		apps, err := s.routes.ApplicationsByServer(ctx, srv.ID)
		if err != nil {
			return err
		}
		for _, app := range apps {
			if app.Type != routing.ProviderServerAgent {
				continue
			}
			if err := s.forEachSpecOfApplication(ctx, srv, app.ID, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// forEachSpecOfApplication calls fn for every runtime spec of appID, a
// server_agent application on srv.
func (s *Service) forEachSpecOfApplication(ctx context.Context, srv routing.AIServer, appID string, fn func(srv routing.AIServer, spec routing.RuntimeSpec) error) error {
	specs, err := s.routes.RuntimeSpecsByApplication(ctx, appID)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if err := fn(srv, spec); err != nil {
			return err
		}
	}
	return nil
}

// adminStateForceRunning is the admin_state that keeps a spec up.
const adminStateForceRunning = "force_running"

// ReconcileRPCPolicies runs the NetBird policy pass for the RPC cluster
// members synchronously: the cluster orchestration calls it before it starts
// a mesh cluster, so the policy that lets the head reach its members exists
// before the head dials them. A no-op while NetBird or policy management is
// off; failures are logged by the pass, never returned. It returns every
// member server whose policy was written within the last RPCPolicySettle,
// with the write time -- by this pass or by any earlier one (a save's, a
// fleet pass): NetBird distributes a policy write to the peers
// asynchronously, so the orchestration lets such a grant settle before the
// head dials.
func (s *Service) ReconcileRPCPolicies(ctx context.Context) []RPCPolicyWrite {
	return s.reconcileRPCPolicies(ctx)
}

// The states of a cluster head's orchestration (ClusterStatusDTO.State).
const (
	ClusterStateCold        = "cold"
	ClusterStateForming     = "forming"
	ClusterStateFormed      = "formed"
	ClusterStateCoolingDown = "cooling_down"
	ClusterStateInvalid     = "invalid"
)

// The phases of a forming cluster (ClusterStatusDTO.Phase).
const (
	ClusterPhaseMembersStarting = "members_starting"
	ClusterPhaseMembersReady    = "members_ready"
	ClusterPhaseHeadStarting    = "head_starting"
	ClusterPhaseFormed          = "formed"
)

// ClusterStatusDTO is the gateway's orchestration state of one cluster head,
// read-only on the head's runtime spec: State (cold, forming, formed,
// cooling_down, invalid), the Phase of the last or running start, each
// member's state by position (never by name, so a member on a server the
// caller may not see stays anonymous), the last failure and the end of a
// cooldown.
type ClusterStatusDTO struct {
	State         string                   `json:"state"`
	Phase         string                   `json:"phase,omitempty"`
	Members       []ClusterMemberStatusDTO `json:"members"`
	LastFailure   *ClusterFailureDTO       `json:"last_failure,omitempty"`
	CooldownUntil *time.Time               `json:"cooldown_until,omitempty"`
	// Breaches are the policy breaches the members' agents report now, by
	// member position: a peer that connected without being allowed, or a
	// hold by another head. Ordered by position, kind and peer.
	Breaches []ClusterBreachDTO `json:"breaches,omitempty"`
	// PeaksAboveBudget names every stored member peak above what the member's
	// host admits (a GPU budget, the host's memory): the member's next start
	// is refused until the budget is raised, the split changed, or the stored
	// usage reset. Derived by the portal from the stored usage on every read.
	PeaksAboveBudget []ClusterPeakAboveBudgetDTO `json:"peaks_above_budget,omitempty"`
}

// The resources of a ClusterPeakAboveBudgetDTO.
const (
	// ClusterPeakResourceVRAM: one GPU's stored VRAM is above the GPU's
	// operator budget, or above the card when it has none.
	ClusterPeakResourceVRAM = "vram"
	// ClusterPeakResourceRAM: the member's stored RAM is above the host's
	// memory.
	ClusterPeakResourceRAM = "ram"
)

// ClusterPeakAboveBudgetDTO is one stored member peak above its ceiling: the
// member's position, the resource, the GPU's host index for VRAM, the stored
// value and the ceiling it exceeds. A member on a server the reader may not
// manage shows only its position: the figures are facts about that server.
type ClusterPeakAboveBudgetDTO struct {
	Position  int    `json:"position"`
	Resource  string `json:"resource,omitempty"`
	GPUIndex  *int   `json:"gpu_index,omitempty"`
	StoredMB  int    `json:"stored_mb,omitempty"`
	CeilingMB int    `json:"ceiling_mb,omitempty"`
}

// The kinds of a cluster breach (ClusterBreachDTO.Kind).
const (
	// ClusterBreachPeer: a peer the member does not allow connected to it.
	ClusterBreachPeer = "peer"
	// ClusterBreachForeignHold: the member reports a hold by another head
	// than this one -- in direct mode a queued second head takes a member
	// over once the first disconnects.
	ClusterBreachForeignHold = "foreign_hold"
)

// ClusterBreachDTO is one breach a member's agent reports: the member's
// position, the kind and, for a peer breach, the peer's address and when the
// agent saw it. A member on a server the reader may not manage shows only its
// position: the kind, the peer and the time are facts about that server.
type ClusterBreachDTO struct {
	Position int       `json:"position"`
	Kind     string    `json:"kind,omitempty"`
	Peer     string    `json:"peer,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

// ClusterMemberStatusDTO is one member's part of a start: its position, its
// state (pending, starting, ready, held, failed) and, for a failed one, the
// cause's code.
type ClusterMemberStatusDTO struct {
	Position int    `json:"position"`
	State    string `json:"state"`
	Code     string `json:"code,omitempty"`
}

// ClusterFailureDTO is the last failed start: the cause's code, the member
// position that failed (nil when it was the head or the cluster as a whole),
// whether it was the head, and when.
type ClusterFailureDTO struct {
	Code     string    `json:"code"`
	Position *int      `json:"position,omitempty"`
	Head     bool      `json:"head,omitempty"`
	At       time.Time `json:"at"`
}

// ClusterStatusReader is the gateway's cluster orchestration as the portal
// reads it: the state of one head, cold for a head it knows nothing about.
// The gateway injects it (SetClusterStatusReader), so the portal does not
// depend on the gateway package.
type ClusterStatusReader interface {
	ClusterStatus(headSpecID string) ClusterStatusDTO
}

// ClusterMeshDriftHandler is what the gateway's cluster orchestration offers
// beside its ClusterStatusReader: StopDirectMeshClusters stops every direct
// mesh cluster the derivation withholds, head first, then releases and stops
// its members, and returns once that is done (each step bounded). The policy
// pass calls it when it records NetBird's Default policy enabled again,
// before it pushes the documents that withhold those clusters, so no member
// is removed under a running head. The reader set by SetClusterStatusReader
// is asked for it, so the one wiring carries both.
type ClusterMeshDriftHandler interface {
	StopDirectMeshClusters(ctx context.Context)
}

// SetClusterStatusReader sets (or replaces) the reader of the gateway's
// cluster orchestration state; nil leaves a head's cluster_status cold. A
// setter for the same reason as SetRuntimeConfigChangedHook: the gateway
// Server that owns the orchestration is built after this Service.
func (s *Service) SetClusterStatusReader(r ClusterStatusReader) {
	s.clusterStatus = r
}

// headClusterStatus is an enabled head's cluster_status: the orchestration's
// state, and invalid while the derivation withholds the head (withheld != "")
// whatever the orchestration last saw. A breach of a member at one of the
// hidden positions (a server the reader may not manage) keeps only its
// position.
func (s *Service) headClusterStatus(headSpecID, withheld string, hidden []int) *ClusterStatusDTO {
	st := ClusterStatusDTO{State: ClusterStateCold, Members: []ClusterMemberStatusDTO{}}
	if s.clusterStatus != nil {
		st = s.clusterStatus.ClusterStatus(headSpecID)
		if st.Members == nil {
			st.Members = []ClusterMemberStatusDTO{}
		}
	}
	if withheld != "" {
		st.State = ClusterStateInvalid
	}
	if len(st.Breaches) > 0 {
		breaches := make([]ClusterBreachDTO, len(st.Breaches))
		for i, b := range st.Breaches {
			if slices.Contains(hidden, b.Position) {
				b = ClusterBreachDTO{Position: b.Position}
			}
			breaches[i] = b
		}
		st.Breaches = breaches
	}
	return &st
}

// stopDriftedClusters hands a drift of NetBird's Default policy to the
// gateway's cluster orchestration (ClusterMeshDriftHandler), when it has one.
func (s *Service) stopDriftedClusters(ctx context.Context) {
	if h, ok := s.clusterStatus.(ClusterMeshDriftHandler); ok {
		h.StopDirectMeshClusters(ctx)
	}
}
