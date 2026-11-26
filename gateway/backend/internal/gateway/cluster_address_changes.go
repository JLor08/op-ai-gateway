// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/portal"
	"slices"
	"strings"
)

// The address-change notice of lan clusters: the orchestration compares each
// plan it keeps of a head with the one before (rememberPlanLocked) and
// records every changed member dial address and every changed head identity,
// newest first, a few per head. The record lives in memory only -- it
// describes recent changes, not an audit trail -- and shows on the head's
// cluster_status (ClusterStatus) and on each server whose address changed
// (AddressChangesForServer).

// recordAddressChangesLocked records the lan address changes from prev to
// next, two plans of st's head that are not withheld: the head's identity
// when it differs (applied live by the members' agents, so nothing
// relaunches), and each member server's dial address when it differs (in
// the head's endpoint list, so a formed head's agent relaunches it; a head
// that is not formed relaunches nothing, and its next start dials the new
// address). It only records: nothing is started, stopped or pushed. A
// member is matched by
// its server, so a reordered membership is no address change; a member
// added or removed, a cluster that is or was no lan cluster, and an
// address one plan lacks record nothing. The caller holds o.mu.
func (o *clusterOrchestrator) recordAddressChangesLocked(st *clusterHeadState, prev, next portal.ClusterPlan) {
	if prev.Network != clusterNetworkLAN || next.Network != clusterNetworkLAN {
		return
	}
	at := o.deps.now()
	var changes []portal.ClusterAddressChangeDTO
	if prev.Head.Server.ID == next.Head.Server.ID && len(prev.HeadIdentity) > 0 && len(next.HeadIdentity) > 0 &&
		!slices.Equal(prev.HeadIdentity, next.HeadIdentity) {
		changes = append(changes, portal.ClusterAddressChangeDTO{
			Head: true, Server: next.Head.Server.Name, ServerID: next.Head.Server.ID,
			From: strings.Join(prev.HeadIdentity, ", "), To: strings.Join(next.HeadIdentity, ", "), At: at,
		})
	}
	for _, m := range next.Members {
		i := slices.IndexFunc(prev.Members, func(p portal.ClusterPlanMember) bool { return p.Server.ID == m.Server.ID })
		if i < 0 {
			continue
		}
		from := prev.Members[i].Address
		if from == "" || m.Address == "" || from == m.Address {
			continue
		}
		pos := m.Position
		changes = append(changes, portal.ClusterAddressChangeDTO{
			Position: &pos, Server: m.Server.Name, ServerID: m.Server.ID,
			From: from, To: m.Address, At: at, Relaunched: st.formed,
		})
	}
	if len(changes) == 0 {
		return
	}
	st.addressChanges = append(changes, st.addressChanges...)
	if len(st.addressChanges) > portal.ClusterAddressChangesKept {
		st.addressChanges = slices.Clip(st.addressChanges[:portal.ClusterAddressChangesKept])
	}
}

// cloneAddressChanges is a copy of changes that shares no position with it,
// never nil.
func cloneAddressChanges(changes []portal.ClusterAddressChangeDTO) []portal.ClusterAddressChangeDTO {
	out := make([]portal.ClusterAddressChangeDTO, len(changes))
	for i, c := range changes {
		if c.Position != nil {
			p := *c.Position
			c.Position = &p
		}
		out[i] = c
	}
	return out
}

// AddressChangesForServer is every address change the orchestration holds
// whose server is serverID, across every head, newest first (ties by head
// spec id, then in the head's order): as a member its dial address changed,
// as a head (AsHead) its identity. HeadName stays empty: the portal names
// the head for its reader (portal.ClusterStatusReader). Never nil.
func (o *clusterOrchestrator) AddressChangesForServer(serverID string) []portal.ServerLANAddressChangeDTO {
	out := []portal.ServerLANAddressChangeDTO{}
	if o == nil || serverID == "" {
		return out
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	heads := make([]string, 0, len(o.heads))
	for head := range o.heads {
		heads = append(heads, head)
	}
	slices.Sort(heads)
	for _, head := range heads {
		for _, c := range o.heads[head].addressChanges {
			if c.ServerID != serverID {
				continue
			}
			out = append(out, portal.ServerLANAddressChangeDTO{
				HeadSpecID: head, AsHead: c.Head, From: c.From, To: c.To, At: c.At, Relaunched: c.Relaunched,
			})
		}
	}
	slices.SortStableFunc(out, func(a, b portal.ServerLANAddressChangeDTO) int { return b.At.Compare(a.At) })
	return out
}

// refreshLANPlans reads again, once per runtime document change (docGen),
// the plan of every lan head the orchestration keeps a plan of, and keeps
// each that is not withheld (rememberPlanLocked): a running cluster is
// started and stopped by nothing when one of its addresses changes, so
// without this pass its change would only be noticed at its next start or
// stop. It only reads and records: it starts, stops, restages and pushes
// nothing, and leaves the head's state and gate view as they were. A read
// that fails is skipped; the next document change reads again.
func (o *clusterOrchestrator) refreshLANPlans(ctx context.Context) {
	o.mu.Lock()
	if o.plansGen == o.docGen {
		o.mu.Unlock()
		return
	}
	o.plansGen = o.docGen
	var heads []string
	for head, st := range o.heads {
		if st.lastPlan != nil && st.lastPlan.Network == clusterNetworkLAN {
			heads = append(heads, head)
		}
	}
	o.mu.Unlock()
	slices.Sort(heads)
	for _, head := range heads {
		rctx, cancel := context.WithTimeout(ctx, clusterReadTimeout)
		plan, err := o.deps.portal.ClusterPlan(rctx, head)
		cancel()
		if err != nil {
			continue
		}
		o.mu.Lock()
		if st := o.heads[head]; st != nil {
			o.rememberPlanLocked(st, plan)
		}
		o.mu.Unlock()
	}
}

var _ portal.ClusterStatusReader = (*clusterOrchestrator)(nil)
