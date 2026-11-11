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
//   - A run has an identity: the instant it began, stamped on every
//     membership row of the head (run_started_at). Every write of the
//     member usage names the run it read and applies only while the row is
//     still in that run, and a run's end acts only on the rows of that run.
//     So an end that arrives after the next run began changes nothing, and a
//     change of the distribution -- which the portal answers by marking the
//     kept values stale and resetting the run -- can never be overwritten by
//     a value read before it, nor replaced by the old split's peak.
//   - A sample counts only inside a run, only from the server that owns the
//     member (the membership row's server for an automatic member, whose
//     agent spec id names that server; the manual member spec's own server
//     for a manual one), and only while the member reports the hold of that
//     membership's head. The hold gates the sample; it never selects the
//     head.
//   - During a run the stored value only rises, and the run's own peak is
//     kept beside it (run_peak_*), so a gateway restart in the middle of a
//     run keeps both.
//   - When a run that formed ends, its peak replaces the stored value part by
//     part -- also a lower one -- and the member server's document is pushed
//     when that changed what it is pushed: after the head disconnects, its
//     agent no longer holds the value. The stale mark clears only when the
//     run measured every part that holds a stored value. A rise pushes
//     nothing: during the run the agent charges max(live, demand) itself, so
//     a push would only echo the agent its own measurement.
//   - A run that never formed replaces nothing; what it raised stays.
//
// The orchestration (clusterOrchestrator) marks the runs: a cluster start
// begins one while the head has no process, and otherwise -- and when the
// telemetry shows the cluster formed already, a restarted gateway's case --
// continues the stored one; it knows whether the run formed, and ends it
// where it learns the head stopped -- its explicit stop, or the head's exit
// on telemetry. A run nobody ended (the gateway was down when the head
// stopped) is replaced by the next one, which replaces nothing of it.
//
// The same samples keep the session records (rpc_sessions): the hold of a
// member by its head opens one, the end of the hold closes it. A restarted
// gateway knows nothing of the sessions it left open, so the first sample of
// each member server it receives syncs them with the store: a session still
// held is adopted, any other open session of the server is closed -- also
// one of a head or membership deleted meanwhile. A member server whose
// telemetry stays away past the watchdog's stale threshold has its sessions
// closed at its last frame, and syncs again when it comes back.

// clusterHeadInstance is the instance index of a cluster head: a head is
// never replicable, so its one process is instance 0.
const clusterHeadInstance = 0

// clusterUsageWriteAttempts bounds how often a reset re-reads a row whose run
// changed under it.
const clusterUsageWriteAttempts = 3

// errClusterUsageUnavailable: the gateway has no store to keep member usage
// in.
var errClusterUsageUnavailable = errors.New("cluster member usage is not kept by this gateway")

// errClusterUsageContended: a reset kept losing its write to concurrent
// rewrites of the membership.
var errClusterUsageContended = errors.New("cluster member usage changed concurrently; try again")

// clusterRunObserver is told where a cluster head's run begins and ends, and
// keeps the member sessions. Satisfied by *clusterUsage.
type clusterRunObserver interface {
	// runBegan begins a run of head and returns its identity (zero when it
	// could not be recorded: that run then measures nothing).
	runBegan(ctx context.Context, head string) time.Time
	// currentRun is the identity of head's stored run, zero when none is.
	currentRun(ctx context.Context, head string) time.Time
	// runEnded ends the run of head identified by run, with whether it
	// formed.
	runEnded(ctx context.Context, head string, run time.Time, formed bool)
	// closeSilentSessions closes the open sessions of every member server
	// lastFrame reports silent, at that server's last frame.
	closeSilentSessions(ctx context.Context, lastFrame func(serverID string) (time.Time, bool))
	// closeUnheardSessions closes the sessions the store holds open on member
	// servers heard reports unheard since the gateway started at bootAt, and
	// reports whether it could.
	closeUnheardSessions(ctx context.Context, bootAt time.Time, heard func(serverID string) bool) bool
}

// clusterUsage keeps the member usage and the session records. See the top of
// this file.
type clusterUsage struct {
	store routing.RPCClusterStore
	// push pushes a server's runtime document.
	push func(serverID string)
	now  func() time.Time
	// lastSeen, when set, is a server's last telemetry as stored (from
	// before a gateway restart too); ok false when unknown.
	lastSeen func(ctx context.Context, serverID string) (time.Time, bool)

	// locks serialize, per member server, the handling of its samples and,
	// per head, every read-modify-write of the head's membership rows.
	locks keyedLocks

	mu sync.Mutex
	// open is the sessions this gateway opened or adopted and has not closed.
	open map[rpcSessionKey]bool
	// seen is the member servers whose sessions were synced with the store
	// since the gateway started (or since they last went silent): one entry
	// per server that reports, so a frame without member rows costs no store
	// read.
	seen map[string]bool
}

type rpcSessionKey struct{ head, member string }

func newClusterUsage(st routing.RPCClusterStore, push func(serverID string), now func() time.Time) *clusterUsage {
	return &clusterUsage{store: st, push: push, now: now, open: map[rpcSessionKey]bool{}, seen: map[string]bool{}}
}

// keyedLocks is one mutex per key, held in the map only while someone holds
// or waits for it.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*refLock
}

type refLock struct {
	sync.Mutex
	refs int
}

// lock locks key's mutex and returns its unlock, which drops the mutex from
// the map once nobody holds or waits for it.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*refLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &refLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

func headLockKey(head string) string     { return "head\x00" + head }
func serverLockKey(server string) string { return "server\x00" + server }

// runIdentity is a run's identity for the instant t: UTC, to the millisecond,
// so it compares equal after a round trip through every store.
func runIdentity(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// observe handles one telemetry frame of serverID: the usage of every member
// on serverID its head holds, and the session records of every membership
// on serverID. A frame without member rows costs nothing, unless the
// server's sessions are not synced yet or it has a session open.
// Best-effort like every write-back of the ingest: a failure is logged,
// never returned.
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
	byID := make(map[string]RuntimeStatusDTO, len(rows))
	for _, r := range rows {
		if r.Kind == portal.AgentSpecKindRPCServer && r.Instance == 0 {
			byID[r.SpecID] = r
		}
	}
	held := make(map[string]bool, len(memberships))
	for _, m := range memberships {
		row, ok := u.heldRow(ctx, serverID, m, byID)
		held[m.HeadSpecID] = ok
		if ok {
			u.raise(ctx, m, row)
		}
	}
	u.syncSessions(ctx, serverID, held, u.now())
}

// relevant reports whether a frame of serverID can concern member usage or
// sessions: it carries a member row, the server's sessions are not synced,
// or the server has a session open.
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

// raise applies one counted sample of m inside the member's run: the run's
// peak and the stored value rise to what it measured, each only upwards; a
// measurement of 0 is unknown and changes nothing. A GPU the membership does
// not list is ignored. Outside a run nothing counts. An unchanged row is not
// written, so a stable member costs one read per sample; the write applies
// only while the row is still in the run it read.
func (u *clusterUsage) raise(ctx context.Context, m routing.ClusterMember, row RuntimeStatusDTO) {
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
	if err != nil {
		slog.Debug("cluster member usage: reading the membership failed", "head_spec_id", m.HeadSpecID, "server_id", m.ServerID, "err", err)
		return
	}
	if !ok || cur.RunStartedAt.IsZero() {
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
	if stored {
		cur.MeasuredAt = u.now()
	}
	if _, err := u.store.UpdateClusterMemberUsage(ctx, cur, gpus); err != nil {
		slog.Debug("cluster member usage: write failed", "head_spec_id", m.HeadSpecID, "server_id", m.ServerID, "err", err)
	}
}

// runBegan begins a run of head: every member row is stamped with the run's
// identity (now) and its run peaks start from nothing. A run still open from
// before -- one whose end the gateway missed -- is thereby replaced: its
// rises stay, its peak replaces nothing.
func (u *clusterUsage) runBegan(ctx context.Context, head string) time.Time {
	if u == nil || u.store == nil {
		return time.Time{}
	}
	unlock := u.locks.lock(headLockKey(head))
	defer unlock()
	run := runIdentity(u.now())
	if err := u.store.BeginClusterMemberRun(ctx, head, run, clusterHeadInstance); err != nil {
		slog.Warn("cluster member usage: recording a run's start failed", "head_spec_id", head, "err", err)
		return time.Time{}
	}
	return run
}

// currentRun is the identity of head's stored run: the run its member rows
// are stamped with (a begin stamps them all alike), zero when none is.
func (u *clusterUsage) currentRun(ctx context.Context, head string) time.Time {
	if u == nil || u.store == nil {
		return time.Time{}
	}
	members, _, err := u.store.ClusterMembers(ctx, head)
	if err != nil {
		slog.Warn("cluster member usage: reading the stored run failed", "head_spec_id", head, "err", err)
		return time.Time{}
	}
	for _, m := range members {
		if !m.RunStartedAt.IsZero() {
			return m.RunStartedAt
		}
	}
	return time.Time{}
}

// runEnded ends head's run identified by run, on the member rows still in it
// (a row a later run or a change of the distribution took over is left
// alone). When it formed, each part's run peak replaces its stored value
// (replaceByRunPeak); a member server whose pushed demand that changed is
// pushed once. Either way the run is cleared from the row.
func (u *clusterUsage) runEnded(ctx context.Context, head string, run time.Time, formed bool) {
	if u == nil || u.store == nil || run.IsZero() {
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
		if !m.RunStartedAt.Equal(run) {
			continue
		}
		if u.endMemberRun(ctx, m, gpusOf(gpus, m.ServerID), formed, now) && !slices.Contains(pushes, m.ServerID) {
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

// endMemberRun ends m's run (m read in it): a formed run's peaks replace the
// stored values first, then the run is cleared. It reports whether the
// member's pushed demand changed. The caller holds the head's lock.
func (u *clusterUsage) endMemberRun(ctx context.Context, m routing.ClusterMember, gpus []routing.ClusterMemberGPU, formed bool, now time.Time) bool {
	changed := false
	if formed {
		changed = replaceByRunPeak(&m, gpus, now)
		applied, err := u.store.UpdateClusterMemberUsage(ctx, m, gpus)
		if err != nil || !applied {
			if err != nil {
				slog.Warn("cluster member usage: replacing by the run's peak failed", "head_spec_id", m.HeadSpecID, "server_id", m.ServerID, "err", err)
			}
			return false
		}
	}
	if _, err := u.store.EndClusterMemberRun(ctx, m.HeadSpecID, m.ServerID, m.RunStartedAt); err != nil {
		slog.Warn("cluster member usage: recording a run's end failed", "head_spec_id", m.HeadSpecID, "server_id", m.ServerID, "err", err)
	}
	return changed
}

// replaceByRunPeak lets a formed run's peaks replace m's stored values, part
// by part (a part the run did not measure keeps its stored value), and
// reports whether the member's pushed demand changed (a stored value, or the
// stale mark that caps it). The stale mark clears only when the run measured
// every part that holds a stored value: one measured GPU says nothing about
// another GPU's stale value.
func replaceByRunPeak(m *routing.ClusterMember, gpus []routing.ClusterMemberGPU, now time.Time) bool {
	measured, complete, changed := false, true, false
	if p := m.RunPeakRAMMB; p > 0 {
		measured = true
		if m.RAMMeasuredMB != p {
			m.RAMMeasuredMB, changed = p, true
		}
	} else if m.RAMMeasuredMB > 0 {
		complete = false
	}
	for i := range gpus {
		if p := gpus[i].RunPeakVRAMMB; p > 0 {
			measured = true
			if gpus[i].VRAMMeasuredMB != p {
				gpus[i].VRAMMeasuredMB, changed = p, true
			}
		} else if gpus[i].VRAMMeasuredMB > 0 {
			complete = false
		}
	}
	if !measured {
		return false
	}
	m.MeasuredAt = now
	if m.Stale && complete {
		m.Stale, changed = false, true
	}
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
	err := errClusterUsageContended
	for range clusterUsageWriteAttempts {
		m, gpus, ok, rerr := u.member(ctx, head, serverID)
		if rerr == nil && !ok {
			rerr = store.ErrNotFound
		}
		if rerr != nil {
			err = rerr
			break
		}
		m.RAMMeasuredMB, m.MeasuredAt, m.Stale, m.RunPeakRAMMB = 0, time.Time{}, false, 0
		for i := range gpus {
			gpus[i].VRAMMeasuredMB, gpus[i].RunPeakVRAMMB = 0, 0
		}
		applied, werr := u.store.UpdateClusterMemberUsage(ctx, m, gpus)
		if werr != nil || applied {
			err = werr
			break
		}
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

// syncSessions keeps the session records of serverID's memberships after a
// frame; held tells, per head serverID is a member of, whether the frame
// shows that head's hold. A hold without an open session opens one, an open
// session without the hold -- or of a head serverID is no longer a member of
// -- is closed. Until the server's sessions were synced with the store, the
// open sessions are the store's (a session a restarted gateway left open is
// adopted while still held, closed otherwise); a sync that failed in part is
// tried again on the next frame, so it never opens a session twice.
func (u *clusterUsage) syncSessions(ctx context.Context, serverID string, held map[string]bool, now time.Time) {
	u.mu.Lock()
	synced := u.seen[serverID]
	open := map[string]bool{}
	for k := range u.open {
		if k.member == serverID {
			open[k.head] = true
		}
	}
	u.mu.Unlock()
	if !synced {
		stored, err := u.store.OpenRPCSessionsByMember(ctx, serverID)
		if err != nil {
			slog.Debug("cluster member usage: reading the open sessions failed", "server_id", serverID, "err", err)
			return
		}
		for _, s := range stored {
			open[s.HeadSpecID] = true
		}
	}
	ok := true
	for head, h := range held {
		key := rpcSessionKey{head: head, member: serverID}
		switch {
		case h && open[head]:
			u.setOpen(key, true)
		case h:
			if err := u.store.OpenRPCSession(ctx, routing.RPCSession{HeadSpecID: head, Instance: clusterHeadInstance, MemberServerID: serverID, Since: now}); err != nil {
				slog.Debug("cluster member usage: opening a session failed", "head_spec_id", head, "server_id", serverID, "err", err)
				ok = false
				continue
			}
			u.setOpen(key, true)
		case open[head]:
			ok = u.closeSession(ctx, key, now) && ok
		}
	}
	for head := range open {
		if _, member := held[head]; !member {
			ok = u.closeSession(ctx, rpcSessionKey{head: head, member: serverID}, now) && ok
		}
	}
	if ok {
		u.mu.Lock()
		u.seen[serverID] = true
		u.mu.Unlock()
	}
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

// closeSession closes key's open sessions at until and reports whether it
// could.
func (u *clusterUsage) closeSession(ctx context.Context, key rpcSessionKey, until time.Time) bool {
	if err := u.store.CloseRPCSessions(ctx, key.head, clusterHeadInstance, key.member, until); err != nil {
		slog.Debug("cluster member usage: closing a session failed", "head_spec_id", key.head, "server_id", key.member, "err", err)
		return false
	}
	u.setOpen(key, false)
	return true
}

// closeSilentSessions closes the open sessions of every member server whose
// telemetry lastFrame reports silent, at that server's last frame: the
// member is gone or cut off, and its head cannot hold it unseen. The server
// is synced with the store again once it reports.
func (u *clusterUsage) closeSilentSessions(ctx context.Context, lastFrame func(serverID string) (time.Time, bool)) {
	if u == nil || u.store == nil {
		return
	}
	u.mu.Lock()
	var members []string
	for k := range u.open {
		if !slices.Contains(members, k.member) {
			members = append(members, k.member)
		}
	}
	u.mu.Unlock()
	for _, member := range members {
		if _, silent := lastFrame(member); silent {
			u.closeSilent(ctx, member, lastFrame)
		}
	}
}

// closeSilent closes member's open sessions at its last frame, judging the
// silence again under the server's lock: a frame handled since the first
// verdict makes the server heard, and its sessions stay as that frame left
// them.
func (u *clusterUsage) closeSilent(ctx context.Context, member string, lastFrame func(serverID string) (time.Time, bool)) {
	unlock := u.locks.lock(serverLockKey(member))
	defer unlock()
	last, silent := lastFrame(member)
	if !silent {
		return
	}
	u.mu.Lock()
	var keys []rpcSessionKey
	for k := range u.open {
		if k.member == member {
			keys = append(keys, k)
		}
	}
	u.mu.Unlock()
	ok := true
	for _, k := range keys {
		ok = u.closeSession(ctx, k, last) && ok
	}
	if ok {
		slog.Info("cluster member usage: closed the sessions of a member server that went silent", "server_id", member, "last_frame", last)
		u.mu.Lock()
		delete(u.seen, member)
		u.mu.Unlock()
	}
}

// closeUnheardSessions closes the sessions the store holds open on member
// servers that have not reported since the gateway started (heard false):
// sessions a gateway left open before a restart, on a member that never
// reported again, which no frame will sync. A server that sends its first
// frame meanwhile is left to that frame's sync. Each session closes at the
// server's last telemetry before the restart when it is known and not before
// the session began, else at the gateway's start (bootAt) -- the latest
// instant the gateway cannot rule the hold out for. One store listing.
func (u *clusterUsage) closeUnheardSessions(ctx context.Context, bootAt time.Time, heard func(serverID string) bool) bool {
	if u == nil || u.store == nil {
		return true
	}
	sessions, err := u.store.OpenRPCSessions(ctx)
	if err != nil {
		slog.Debug("cluster member usage: listing the open sessions failed", "err", err)
		return false
	}
	ok := true
	for member, mine := range groupByMember(sessions) {
		if heard(member) {
			continue
		}
		ok = u.closeUnheard(ctx, member, mine, bootAt, heard) && ok
	}
	return ok
}

func groupByMember(sessions []routing.RPCSession) map[string][]routing.RPCSession {
	out := map[string][]routing.RPCSession{}
	for _, s := range sessions {
		out[s.MemberServerID] = append(out[s.MemberServerID], s)
	}
	return out
}

// closeUnheard closes the given open sessions of member, under the server's
// lock, unless its first frame arrived meanwhile.
func (u *clusterUsage) closeUnheard(ctx context.Context, member string, sessions []routing.RPCSession, bootAt time.Time, heard func(serverID string) bool) bool {
	unlock := u.locks.lock(serverLockKey(member))
	defer unlock()
	u.mu.Lock()
	synced := u.seen[member]
	u.mu.Unlock()
	if synced || heard(member) {
		return true
	}
	last, known := time.Time{}, false
	if u.lastSeen != nil {
		last, known = u.lastSeen(ctx, member)
	}
	ok := true
	for _, s := range sessions {
		until := bootAt
		if known && !last.Before(s.Since) && last.Before(bootAt) {
			until = last
		}
		// After the gateway's clock stepped backwards its start can lie
		// before the session's; a session never ends before it began, or no
		// lookup of the sessions in force at an instant could place it.
		if until.Before(s.Since) {
			until = s.Since
		}
		if err := u.store.CloseRPCSessions(ctx, s.HeadSpecID, s.Instance, member, until); err != nil {
			slog.Debug("cluster member usage: closing an unheard member's session failed", "head_spec_id", s.HeadSpecID, "server_id", member, "err", err)
			ok = false
		}
	}
	if ok && len(sessions) > 0 {
		slog.Info("cluster member usage: closed the sessions of a member server not heard from since the gateway started", "server_id", member)
	}
	return ok
}
