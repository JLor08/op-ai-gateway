// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"sync"
	"time"
)

// Member usage of a llama.cpp RPC cluster: the VRAM per member GPU and the
// RAM of each member, measured by the member's own agent while its head
// holds it and stored with the head on the membership row (the member's
// demand pushed to its host is max(stored usage, estimate)). A member's
// process falls back to its idle footprint once the head disconnects, so the
// latest sample is not the value to keep; the stored value follows the peak
// rule instead, per run -- one head process from its cluster start until it
// stops:
//
//   - A sample counts only from the server that owns the member (the
//     membership row's server for an automatic member, whose agent spec id
//     names that server; the manual member spec's own server for a manual
//     one) and only while the member reports the hold of that membership's
//     head. The hold gates the sample; it never selects the head.
//   - During a run the stored value only rises, and the run's own peak is
//     kept beside it (run_peak_*, with the run's start), so a gateway restart
//     in the middle of a run keeps both.
//   - When a run that formed ends, its peak replaces the stored value -- also
//     a lower one, and one kept stale from before a change of the
//     distribution -- and the member server's document is pushed when that
//     changed what it is pushed: after the head disconnects, its agent no
//     longer holds the value. A rise pushes nothing: during the run the agent
//     charges max(live, demand) itself, so a push would only echo the agent
//     its own measurement.
//   - A run that never formed replaces nothing; what it raised stays.
//
// The orchestration (clusterOrchestrator) marks the runs: it begins one where
// a cluster start begins, knows whether it formed, and ends it where it
// learns the head stopped -- its explicit stop, or the head's exit on
// telemetry. A run nobody ended (the gateway was down when the head stopped)
// ends as unformed when the next one begins.
//
// The same samples keep the session records (rpc_sessions): the hold of a
// member by its head opens one, the end of the hold closes it. A restarted
// gateway knows nothing of the sessions it left open, so the first sample of
// each member server it receives closes those of members that no longer
// report the hold and adopts those that still do.

// clusterHeadInstance is the instance index of a cluster head: a head is
// never replicable, so its one process is instance 0.
const clusterHeadInstance = 0

// errClusterUsageUnavailable: the gateway has no store to keep member usage
// in.
var errClusterUsageUnavailable = errors.New("cluster member usage is not kept by this gateway")

// clusterRunObserver is told where a cluster head's run begins and ends:
// runBegan at the start of a cluster start, runEnded once the head stopped,
// with whether the run formed. Satisfied by *clusterUsage.
type clusterRunObserver interface {
	runBegan(ctx context.Context, head string)
	runEnded(ctx context.Context, head string, formed bool)
}

// clusterUsage keeps the member usage and the session records. See the top of
// this file.
type clusterUsage struct {
	store routing.RPCClusterStore
	// push pushes a server's runtime document.
	push func(serverID string)
	now  func() time.Time

	// locks serialize, per member server, the handling of its samples and,
	// per head, every read-modify-write of the head's membership rows.
	locks keyedLocks

	mu sync.Mutex
	// open is the sessions this gateway opened or adopted and has not closed.
	open map[rpcSessionKey]bool
	// seen is the member servers whose first sample since the gateway started
	// was handled.
	seen map[string]bool
}

type rpcSessionKey struct{ head, member string }

func newClusterUsage(st routing.RPCClusterStore, push func(serverID string), now func() time.Time) *clusterUsage {
	return &clusterUsage{store: st, push: push, now: now, open: map[rpcSessionKey]bool{}, seen: map[string]bool{}}
}

// keyedLocks is one mutex per key, created on first use.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock locks key's mutex and returns its unlock.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l := k.m[key]
	if l == nil {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func headLockKey(head string) string     { return "head\x00" + head }
func serverLockKey(server string) string { return "server\x00" + server }

// observe handles one telemetry frame of serverID: the usage of every member
// on serverID its head holds, and the session records of every membership
// on serverID. A frame without member rows costs nothing, unless it is the
// server's first since the gateway started or the server has an open
// session. Best-effort like every write-back of the ingest: a failure is
// logged, never returned.
func (u *clusterUsage) observe(ctx context.Context, serverID string, rows []RuntimeStatusDTO) {
	if u == nil || u.store == nil || !u.relevant(serverID, rows) {
		return
	}
	unlock := u.locks.lock(serverLockKey(serverID))
	defer unlock()
	memberships, err := u.store.ClusterMembershipsByServer(ctx, serverID)
	if err != nil {
		slog.Debug("cluster member usage: reading the server's memberships failed", "server_id", serverID, "err", err)
		return
	}
	u.mu.Lock()
	first := !u.seen[serverID]
	u.seen[serverID] = true
	u.mu.Unlock()
	now := u.now()
	byID := make(map[string]RuntimeStatusDTO, len(rows))
	for _, r := range rows {
		if r.Kind == portal.AgentSpecKindRPCServer && r.Instance == 0 {
			byID[r.SpecID] = r
		}
	}
	heads := make([]string, 0, len(memberships))
	for _, m := range memberships {
		heads = append(heads, m.HeadSpecID)
		row, held := u.heldRow(ctx, serverID, m, byID)
		u.trackSession(ctx, m.HeadSpecID, serverID, held, first, now)
		if held {
			u.raise(ctx, m, row, now)
		}
	}
	u.closeLeftSessions(ctx, serverID, heads, now)
}

// relevant reports whether a frame of serverID can concern member usage or
// sessions: it carries a member row, it is the server's first since the
// gateway started, or the server has a session open.
func (u *clusterUsage) relevant(serverID string, rows []RuntimeStatusDTO) bool {
	if slices.ContainsFunc(rows, func(r RuntimeStatusDTO) bool { return r.Kind == portal.AgentSpecKindRPCServer }) {
		return true
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.seen[serverID] {
		return true
	}
	for k := range u.open {
		if k.member == serverID {
			return true
		}
	}
	return false
}

// heldRow returns m's row in the frame of serverID and whether it counts: the
// row reports the hold of m's head, and serverID owns the member. An
// automatic member is serverID's by its membership row (the reverse index
// read it for serverID) and its agent spec id, which names the server; a
// manual member must also be a spec of serverID's own, since the membership
// row only records where the spec was when it was chosen.
func (u *clusterUsage) heldRow(ctx context.Context, serverID string, m routing.ClusterMember, rows map[string]RuntimeStatusDTO) (RuntimeStatusDTO, bool) {
	row, ok := rows[portal.MemberAgentSpecID(m)]
	if !ok || row.RPC == nil || row.RPC.HeldBy != m.HeadSpecID {
		return row, false
	}
	if m.MemberSpecID == "" {
		return row, true
	}
	spec, ok, err := u.store.RPCMemberSpecByID(ctx, m.MemberSpecID)
	switch {
	case err != nil:
		slog.Debug("cluster member usage: reading the member spec failed", "server_id", serverID, "spec_id", m.MemberSpecID, "err", err)
		return row, false
	case !ok:
		return row, false
	case spec.ServerID != serverID:
		slog.Warn("cluster member usage rejected: the member spec belongs to a different server",
			"server_id", serverID, "spec_id", m.MemberSpecID, "owner_server_id", spec.ServerID)
		return row, false
	}
	return row, true
}

// member reads the membership row of serverID under head and its GPU rows;
// ok false when there is none.
func (u *clusterUsage) member(ctx context.Context, head, serverID string) (routing.ClusterMember, []routing.ClusterMemberGPU, bool, error) {
	members, gpus, err := u.store.ClusterMembers(ctx, head)
	if err != nil {
		return routing.ClusterMember{}, nil, false, err
	}
	i := slices.IndexFunc(members, func(m routing.ClusterMember) bool { return m.ServerID == serverID })
	if i < 0 {
		return routing.ClusterMember{}, nil, false, nil
	}
	return members[i], gpusOf(gpus, serverID), true, nil
}

// gpusOf returns the GPU rows of serverID's member.
func gpusOf(gpus []routing.ClusterMemberGPU, serverID string) []routing.ClusterMemberGPU {
	var out []routing.ClusterMemberGPU
	for _, g := range gpus {
		if g.ServerID == serverID {
			out = append(out, g)
		}
	}
	return out
}

// raise applies one counted sample of m: the run's peak and the stored value
// rise to what it measured, each only upwards; a measurement of 0 is unknown
// and changes nothing. A GPU the membership does not list is ignored. An
// unchanged row is not written, so a stable member costs one read per sample.
// The first counted sample of a run nobody began (one that was running before
// the gateway kept runs) records the run's start.
func (u *clusterUsage) raise(ctx context.Context, m routing.ClusterMember, row RuntimeStatusDTO, now time.Time) {
	vram := make(map[int]int, len(row.GPUs))
	for _, g := range row.GPUs[:min(len(row.GPUs), maxRuntimeGPUsPerSample)] {
		if g.VRAMMeasuredMB > 0 {
			vram[g.Index] = max(vram[g.Index], g.VRAMMeasuredMB)
		}
	}
	if row.RAMMeasuredMB <= 0 && len(vram) == 0 {
		return
	}
	unlock := u.locks.lock(headLockKey(m.HeadSpecID))
	defer unlock()
	cur, gpus, ok, err := u.member(ctx, m.HeadSpecID, m.ServerID)
	if err != nil || !ok {
		if err != nil {
			slog.Debug("cluster member usage: reading the membership failed", "head_spec_id", m.HeadSpecID, "server_id", m.ServerID, "err", err)
		}
		return
	}
	peak, stored := false, false
	if r := row.RAMMeasuredMB; r > 0 {
		if r > cur.RunPeakRAMMB {
			cur.RunPeakRAMMB, peak = r, true
		}
		if r > cur.RAMMeasuredMB {
			cur.RAMMeasuredMB, stored = r, true
		}
	}
	for i := range gpus {
		v := vram[gpus[i].GPUIndex]
		if v > gpus[i].RunPeakVRAMMB {
			gpus[i].RunPeakVRAMMB, peak = v, true
		}
		if v > gpus[i].VRAMMeasuredMB {
			gpus[i].VRAMMeasuredMB, stored = v, true
		}
	}
	if !peak && !stored {
		return
	}
	if cur.RunStartedAt.IsZero() {
		cur.RunStartedAt, cur.RunHeadInstance = now, clusterHeadInstance
	}
	if stored {
		cur.MeasuredAt = now
	}
	if err := u.store.UpdateClusterMemberUsage(ctx, cur, gpus); err != nil {
		slog.Debug("cluster member usage: write failed", "head_spec_id", m.HeadSpecID, "server_id", m.ServerID, "err", err)
	}
}

// runBegan begins a run of head: every member's run peak starts from nothing
// at now. A run still open from before -- one whose end the gateway missed --
// thereby ends as unformed: its rises stay, its peak replaces nothing.
func (u *clusterUsage) runBegan(ctx context.Context, head string) {
	if u == nil || u.store == nil {
		return
	}
	unlock := u.locks.lock(headLockKey(head))
	defer unlock()
	members, gpus, err := u.store.ClusterMembers(ctx, head)
	if err != nil {
		slog.Warn("cluster member usage: reading the membership at a run's start failed", "head_spec_id", head, "err", err)
		return
	}
	now := u.now()
	for _, m := range members {
		mg := gpusOf(gpus, m.ServerID)
		m.RunStartedAt, m.RunHeadInstance, m.RunPeakRAMMB = now, clusterHeadInstance, 0
		for i := range mg {
			mg[i].RunPeakVRAMMB = 0
		}
		if err := u.store.UpdateClusterMemberUsage(ctx, m, mg); err != nil {
			slog.Warn("cluster member usage: recording a run's start failed", "head_spec_id", head, "server_id", m.ServerID, "err", err)
		}
	}
}

// runEnded ends head's run. When it formed, each member's run peak replaces
// its stored value, GPU by GPU (a part the run did not measure keeps its
// stored value), and a member the run measured is no longer stale; a member
// server whose pushed demand that changed is pushed once. Either way the
// run's fields are cleared.
func (u *clusterUsage) runEnded(ctx context.Context, head string, formed bool) {
	if u == nil || u.store == nil {
		return
	}
	unlock := u.locks.lock(headLockKey(head))
	members, gpus, err := u.store.ClusterMembers(ctx, head)
	if err != nil {
		unlock()
		slog.Warn("cluster member usage: reading the membership at a run's end failed", "head_spec_id", head, "err", err)
		return
	}
	now := u.now()
	var pushes []string
	for _, m := range members {
		mg := gpusOf(gpus, m.ServerID)
		open := !m.RunStartedAt.IsZero() || m.RunPeakRAMMB > 0 ||
			slices.ContainsFunc(mg, func(g routing.ClusterMemberGPU) bool { return g.RunPeakVRAMMB > 0 })
		if !open {
			continue
		}
		changed := false
		if formed {
			changed = replaceByRunPeak(&m, mg, now)
		}
		m.RunStartedAt, m.RunHeadInstance, m.RunPeakRAMMB = time.Time{}, 0, 0
		for i := range mg {
			mg[i].RunPeakVRAMMB = 0
		}
		if err := u.store.UpdateClusterMemberUsage(ctx, m, mg); err != nil {
			slog.Warn("cluster member usage: recording a run's end failed", "head_spec_id", head, "server_id", m.ServerID, "err", err)
			continue
		}
		if changed && !slices.Contains(pushes, m.ServerID) {
			pushes = append(pushes, m.ServerID)
		}
	}
	unlock()
	if u.push == nil {
		return
	}
	for _, id := range pushes {
		u.push(id)
	}
}

// replaceByRunPeak lets a formed run's peaks replace m's stored values and
// reports whether the member's pushed demand changed (a stored value, or the
// stale mark that caps it).
func replaceByRunPeak(m *routing.ClusterMember, gpus []routing.ClusterMemberGPU, now time.Time) bool {
	measured, changed := false, false
	if p := m.RunPeakRAMMB; p > 0 {
		measured = true
		if m.RAMMeasuredMB != p {
			m.RAMMeasuredMB, changed = p, true
		}
	}
	for i := range gpus {
		if p := gpus[i].RunPeakVRAMMB; p > 0 {
			measured = true
			if gpus[i].VRAMMeasuredMB != p {
				gpus[i].VRAMMeasuredMB, changed = p, true
			}
		}
	}
	if !measured {
		return false
	}
	if m.Stale {
		m.Stale, changed = false, true
	}
	m.MeasuredAt = now
	return changed
}

// reset clears the stored usage of serverID's member of head -- the stored
// values, their stale mark and the current run's peaks; the run itself stays
// open -- and pushes the member server's document, whose demand that
// changes. store.ErrNotFound when serverID is no member of head.
func (u *clusterUsage) reset(ctx context.Context, head, serverID string) error {
	if u == nil || u.store == nil {
		return errClusterUsageUnavailable
	}
	unlock := u.locks.lock(headLockKey(head))
	m, gpus, ok, err := u.member(ctx, head, serverID)
	if err == nil && !ok {
		err = store.ErrNotFound
	}
	if err == nil {
		m.RAMMeasuredMB, m.MeasuredAt, m.Stale, m.RunPeakRAMMB = 0, time.Time{}, false, 0
		for i := range gpus {
			gpus[i].VRAMMeasuredMB, gpus[i].RunPeakVRAMMB = 0, 0
		}
		err = u.store.UpdateClusterMemberUsage(ctx, m, gpus)
	}
	unlock()
	if err != nil {
		return err
	}
	if u.push != nil {
		u.push(serverID)
	}
	return nil
}

// trackSession keeps the session record of member's hold by head: a hold
// this gateway has no open session for opens one -- unless, on the server's
// first frame since the gateway started, the store still has it open, which
// is adopted -- and the end of a hold closes it. On the first frame a member
// without the hold closes whatever the store has open for it.
func (u *clusterUsage) trackSession(ctx context.Context, head, member string, held, first bool, now time.Time) {
	key := rpcSessionKey{head: head, member: member}
	u.mu.Lock()
	open := u.open[key]
	u.mu.Unlock()
	switch {
	case held && !open:
		if first && u.sessionOpenInStore(ctx, head, member, now) {
			u.setOpen(key, true)
			return
		}
		if err := u.store.OpenRPCSession(ctx, routing.RPCSession{HeadSpecID: head, Instance: clusterHeadInstance, MemberServerID: member, Since: now}); err != nil {
			slog.Debug("cluster member usage: opening a session failed", "head_spec_id", head, "server_id", member, "err", err)
			return
		}
		u.setOpen(key, true)
	case !held && (open || first):
		u.closeSession(ctx, key, now)
	}
}

// sessionOpenInStore reports whether the store has a session of head on
// member open at now.
func (u *clusterUsage) sessionOpenInStore(ctx context.Context, head, member string, now time.Time) bool {
	sessions, err := u.store.RPCSessionsAt(ctx, head, clusterHeadInstance, now)
	if err != nil {
		slog.Debug("cluster member usage: reading the open sessions failed", "head_spec_id", head, "err", err)
		return false
	}
	return slices.ContainsFunc(sessions, func(s routing.RPCSession) bool { return s.MemberServerID == member && s.Until == nil })
}

func (u *clusterUsage) setOpen(key rpcSessionKey, open bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if open {
		u.open[key] = true
	} else {
		delete(u.open, key)
	}
}

func (u *clusterUsage) closeSession(ctx context.Context, key rpcSessionKey, now time.Time) {
	if err := u.store.CloseRPCSessions(ctx, key.head, clusterHeadInstance, key.member, now); err != nil {
		slog.Debug("cluster member usage: closing a session failed", "head_spec_id", key.head, "server_id", key.member, "err", err)
		return
	}
	u.setOpen(key, false)
}

// closeLeftSessions closes the open sessions on serverID of heads it is no
// longer a member of (heads lists those it is): a membership removed while
// its member was held.
func (u *clusterUsage) closeLeftSessions(ctx context.Context, serverID string, heads []string, now time.Time) {
	u.mu.Lock()
	var left []rpcSessionKey
	for k := range u.open {
		if k.member == serverID && !slices.Contains(heads, k.head) {
			left = append(left, k)
		}
	}
	u.mu.Unlock()
	for _, k := range left {
		u.closeSession(ctx, k, now)
	}
}
