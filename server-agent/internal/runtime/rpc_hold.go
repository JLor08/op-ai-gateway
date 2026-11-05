// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// An rpc_server member serves no model: a cluster head on another host
// offloads to it. Its lifecycle is driven by the gateway through the member
// routes (EnsureMember, ReleaseMember, StopMember, PurgeMemberCache) and by
// what the agent observes of the head's connection (SetMemberHold).
//
// Two claims keep a running member from leaving on the agent's own initiative:
//
//   - the RESERVATION, armed by the gateway's ensure for a bounded time: it
//     covers the gap between the member coming up and the head connecting (the
//     head's own admission, start and reconnects), and ends at its deadline, on
//     ReleaseMember, or when the member's process exits;
//   - the HOLD, set while a head is connected to the member (seen by the relay,
//     or in direct mode by the connection observer), and ended by them, by
//     ReleaseMember, or when the member's process exits.
//
// While either stands the member is never idle-unloaded (scanIdle) and never an
// eviction victim: it reads pinned to admission (pinnedForAdmission), which
// every victim choice honours. A stop of a claimed member is refused unless it
// is forced, because removing a member aborts its head; the head is what goes
// first. A local admission that could only proceed by evicting a claimed member
// waits, and names that member on its own row (Status.AdmissionBlockedBy), so
// the gateway can stop the head if its own rules allow and release the member.

// ErrMemberInUse refuses a stop, or a cache purge, of a member that is held or
// reserved. Wire code runtime.cluster_member_in_use (409).
var ErrMemberInUse = errors.New("runtime.cluster_member_in_use")

// ErrCachePurgeFailed reports a member cache that could not be removed: the
// purge was allowed, but the filesystem refused it. Wire code
// runtime.cache_purge_failed (500).
var ErrCachePurgeFailed = errors.New("runtime.cache_purge_failed")

// maxMemberReservation bounds a reservation: it covers one cluster start, which
// is minutes, and a larger value is a caller error rather than a longer start.
const maxMemberReservation = 24 * time.Hour

// memberClaim is a member's hold and reservation. Owned by the owner goroutine
// like the rest of specState.
type memberClaim struct {
	heldBy    string    // the head spec holding the member; "" when none
	heldSince time.Time // when the hold began; zero when none
	// reservedUntil is the reservation's deadline; zero when none.
	// reservationTimer ends it then (cmdReservationExpired); it is registered
	// in m.wg, so it is cancelled whenever the reservation ends early.
	reservedUntil    time.Time
	reservationTimer *time.Timer
	// endedAt is when the last claim on the running member ended. The idle
	// timeout counts from it (scanIdle), so the member stays warm for a head
	// that reconnects soon, while an admission that wants its room may still
	// evict it at once: eviction reads lastUsed, which a claim never moves.
	endedAt time.Time
}

// idleSince is when a member's idle timeout starts counting: its last use, or
// the end of its last claim when that came later.
func (c memberClaim) idleSince(lastUsed time.Time) time.Time {
	if c.endedAt.After(lastUsed) {
		return c.endedAt
	}
	return lastUsed
}

// claimed reports whether a hold or a reservation stands on st.
func (st *specState) claimed() bool {
	return st.claim.heldBy != "" || !st.claim.reservedUntil.IsZero()
}

// cmdStopMember asks the owner to stop a member (Manager.StopMember).
type cmdStopMember struct {
	specID string
	force  bool
	reply  chan stopOutcome
}

// cmdReleaseMember asks the owner to end a member's claim
// (Manager.ReleaseMember).
type cmdReleaseMember struct {
	specID string
	reply  chan error
}

// cmdSetMemberHold reports a member's hold (Manager.SetMemberHold).
type cmdSetMemberHold struct {
	specID string
	holder string
	since  time.Time
}

// cmdReservationExpired is a reservation's deadline passing; until names the
// reservation, so a timer that lost a race with a newer one changes nothing.
type cmdReservationExpired struct {
	specID string
	until  time.Time
}

// cmdPurgeMemberCache asks the owner whether a member's cache may be purged,
// and where it is (Manager.PurgeMemberCache).
type cmdPurgeMemberCache struct {
	specID string
	reply  chan purgeOutcome
}

// purgeOutcome is cmdPurgeMemberCache's answer: the directory to remove ("" for
// none), or a refusal.
type purgeOutcome struct {
	dir string
	err error
}

// isCommand marks each member command as an owner command; the markers have no
// body.
func (cmdStopMember) isCommand()         {}
func (cmdReleaseMember) isCommand()      {}
func (cmdSetMemberHold) isCommand()      {}
func (cmdReservationExpired) isCommand() {}
func (cmdPurgeMemberCache) isCommand()   {}

// askOwner posts c and waits for its answer on reply. The owner answers in the
// very turn it reads the command, before it could stop and close done, so a
// closed done never hides a sent answer; the fallback only reads it first.
func askOwner[T any](ctx context.Context, m *Manager, c command, reply chan T) (T, error) {
	var zero T
	select {
	case m.cmds <- c:
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-m.done:
		return zero, ErrManagerClosed
	}
	select {
	case out := <-reply:
		return out, nil
	case <-m.done:
		select {
		case out := <-reply:
			return out, nil
		default:
			return zero, ErrManagerClosed
		}
	}
}

// EnsureMember starts the rpc_server member specID if it is not running and
// returns once it accepts TCP connections; with reserve above 0 it then holds a
// reservation until reserve from now (an already later one is kept). specID is
// a spec ID, never a model name, and only an rpc_server spec answers to it;
// anything else is ErrModelNotManaged. The start is an ordinary one: a
// force_stopped member is refused with ErrAdmissionBlocked, one resting under a
// terminal verdict with ErrNotPermitted, and it is admitted against the host's
// VRAM and RAM like any spec, waiting for room as a request would (bounded by
// the spec's admission wait, and by ctx). Nothing stays in flight afterwards:
// the reservation, not a request, keeps the member from idling out.
func (m *Manager) EnsureMember(ctx context.Context, specID string, reserve time.Duration) error {
	_, release, err := m.ensure(ctx, cmdEnsure{model: specID, member: true, reserve: reserve})
	if err != nil {
		return err
	}
	release()
	return nil
}

// ReleaseMember ends the member's reservation and hold -- the gateway's word
// that the head is not running -- and leaves the process alone: it idles out on
// its own timeout, counted from now, unless a waiting admission evicts it
// first. Releasing a member with neither is a no-op; a spec ID that is not a
// member is ErrModelNotManaged.
func (m *Manager) ReleaseMember(ctx context.Context, specID string) error {
	reply := make(chan error, 1)
	out, err := askOwner(ctx, m, cmdReleaseMember{specID: specID, reply: reply}, reply)
	if err != nil {
		return err
	}
	return out
}

// StopMember stops the member specID with the answers of Manager.Stop, except
// that a member that is held or reserved is refused with ErrMemberInUse unless
// force is set; a forced stop ends the claim along with the process. A spec ID
// that is not a member is ErrModelNotManaged.
func (m *Manager) StopMember(ctx context.Context, specID string, force bool) (StopState, error) {
	reply := make(chan stopOutcome, 1)
	out, err := askOwner(ctx, m, cmdStopMember{specID: specID, force: force, reply: reply}, reply)
	if err != nil {
		return "", err
	}
	return out.state, out.err
}

// SetMemberHold records that the head holder is connected to the member specID
// since since (now when zero), or with holder "" that no head is. It is the
// report of whoever observes the member's connection; a hold reported for a
// member with no process, or for an ID that is not a member, is ignored. It
// does not wait for the owner to apply it, but a later call on the Manager
// from the same goroutine sees it applied.
func (m *Manager) SetMemberHold(specID, holder string, since time.Time) {
	m.postCmd(cmdSetMemberHold{specID: specID, holder: holder, since: since})
}

// PurgeMemberCache removes the member's on-disk tensor cache for its head.
// A held or reserved member keeps it (ErrMemberInUse); a member with no cache
// directory, or none configured, has nothing to purge and succeeds. The owner
// moves the directory aside in the same turn it checks the claim, so a start
// that follows creates a fresh directory rather than losing one being deleted;
// the moved directory is then removed on the caller's goroutine, never the
// owner's, since a cache can hold many gigabytes. A filesystem refusal is
// ErrCachePurgeFailed.
func (m *Manager) PurgeMemberCache(ctx context.Context, specID string) error {
	reply := make(chan purgeOutcome, 1)
	out, err := askOwner(ctx, m, cmdPurgeMemberCache{specID: specID, reply: reply}, reply)
	if err != nil {
		return err
	}
	if out.err != nil || out.dir == "" {
		return out.err
	}
	if err := os.RemoveAll(out.dir); err != nil {
		return fmt.Errorf("%w: %v", ErrCachePurgeFailed, err)
	}
	return nil
}

// memberState returns the state of the rpc_server member specID: instance 0 of
// a usable member spec that is still desired. Members are addressed by spec ID
// only, and nothing else answers to that address.
func (o *owner) memberState(specID string) (*specState, bool) {
	st := o.specs[specID]
	if st == nil || st.removed || st.instance != 0 || !st.spec.IsRPCServer() {
		return nil, false
	}
	return st, true
}

// armReservation gives a running member a reservation until d from now,
// unless one already runs later: two ensures racing for one start must not
// shorten each other's. A model, a member with no process, and d <= 0 arm
// nothing.
func (o *owner) armReservation(st *specState, d time.Duration) {
	if d <= 0 || o.closing || st.proc == nil || !st.spec.IsRPCServer() {
		return
	}
	until := time.Now().Add(d)
	if !until.After(st.claim.reservedUntil) {
		return
	}
	cancelTimer(o.m, &st.claim.reservationTimer)
	st.claim.reservedUntil = until
	specID := st.spec.ID
	st.claim.reservationTimer = o.m.scheduleAfter(d, func() {
		o.m.postCmd(cmdReservationExpired{specID: specID, until: until})
	})
	o.signalTransition()
}

// handleReservationExpired ends the reservation the expired timer armed; a
// newer reservation, or none, is left alone.
func (o *owner) handleReservationExpired(c cmdReservationExpired) {
	st := o.specs[c.specID]
	if st == nil || !st.claim.reservedUntil.Equal(c.until) {
		return
	}
	st.claim.reservationTimer = nil // this IS that timer's own callback; nothing left to Stop()
	st.claim.reservedUntil = time.Time{}
	o.claimEnded(st)
}

// setMemberHold applies a hold report (Manager.SetMemberHold).
func (o *owner) setMemberHold(c cmdSetMemberHold) {
	st, ok := o.memberState(c.specID)
	if !ok {
		return
	}
	if c.holder == "" {
		if st.claim.heldBy == "" {
			return
		}
		st.claim.heldBy, st.claim.heldSince = "", time.Time{}
		o.claimEnded(st)
		return
	}
	if st.proc == nil {
		slog.Debug("runtime: hold ignored; the member has no process", "spec", c.specID, "holder", c.holder)
		return
	}
	if st.claim.heldBy == c.holder {
		return
	}
	since := c.since
	if since.IsZero() {
		since = time.Now()
	}
	st.claim.heldBy, st.claim.heldSince = c.holder, since
	o.signalTransition()
}

// releaseMember ends a member's claim (Manager.ReleaseMember).
func (o *owner) releaseMember(specID string) error {
	st, ok := o.memberState(specID)
	if !ok {
		return ErrModelNotManaged
	}
	if !st.claimed() {
		return nil
	}
	cancelTimer(o.m, &st.claim.reservationTimer)
	st.claim = memberClaim{}
	o.claimEnded(st)
	return nil
}

// claimEnded follows a hold or a reservation ending while the process may run
// on. Once nothing claims the member any more its idle timeout runs from this
// moment (memberClaim.endedAt), and every admission waiting for room is asked
// again, since the member has just become a possible victim.
func (o *owner) claimEnded(st *specState) {
	o.signalTransition()
	if st.claimed() {
		return
	}
	st.claim.endedAt = time.Now()
	o.wakeAdmissionCandidates()
}

// dropClaim ends a member's claim together with its process (an exit, or a
// forced stop). The caller's own path re-asks the waiting admissions: every
// exit wakes them.
func (o *owner) dropClaim(st *specState) {
	if !st.claimed() {
		return
	}
	cancelTimer(o.m, &st.claim.reservationTimer)
	st.claim = memberClaim{}
	o.signalTransition()
}

// stopMember is Manager.StopMember on the owner. A force_stopped member is
// being drained by the push already, so its claim refuses nothing; any other
// claimed member is refused unless the stop is forced. The rest is the
// transient stop of instance 0 (stopResolved).
func (o *owner) stopMember(specID string, force bool) stopOutcome {
	if o.closing {
		return stopOutcome{err: ErrManagerClosed}
	}
	st, ok := o.memberState(specID)
	if !ok {
		return stopOutcome{err: ErrModelNotManaged}
	}
	if st.claimed() && !force && st.spec.AdminState != "force_stopped" {
		return stopOutcome{err: ErrMemberInUse}
	}
	out := o.stopResolved(st, 0)
	if out.err == nil {
		o.dropClaim(st)
	}
	return out
}

// purgeMemberCache is the owner's half of Manager.PurgeMemberCache: refuse a
// claimed member, and otherwise move its cache directory aside to a tombstone
// beside it, which the caller removes. A rename within one directory is a
// single cheap call, so it may run on the owner. A member whose cache
// directory cannot be formed (no config_files_dir, or a head ID that is not a
// plain path element), or does not exist, has nothing to purge.
func (o *owner) purgeMemberCache(specID string) purgeOutcome {
	st, ok := o.memberState(specID)
	if !ok {
		return purgeOutcome{err: ErrModelNotManaged}
	}
	if st.claimed() {
		return purgeOutcome{err: ErrMemberInUse}
	}
	dir, err := memberCacheDir(o.configFilesDir, st.spec.RPC.HeadSpecID)
	if err != nil {
		return purgeOutcome{}
	}
	// The leading dot and the timestamp give the tombstone a name that is no
	// head's cache directory (head IDs are the gateway's spec IDs).
	tomb := filepath.Join(filepath.Dir(dir), ".purge-"+filepath.Base(dir)+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(dir, tomb); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return purgeOutcome{}
		}
		return purgeOutcome{err: fmt.Errorf("%w: %v", ErrCachePurgeFailed, err)}
	}
	return purgeOutcome{dir: tomb}
}

// claimedMemberBlocking names the claimed member an admission that cannot
// proceed now (Admit answered Wait, or the unknown-demand refusal) waits for,
// or "" when no claim is what holds it back. The question is whether the start
// could proceed -- start, or evict what it may -- were the claimed members not
// there: removing them, rather than reading them unpinned, leaves out every
// reason the agent would have to keep them a moment longer once released (the
// eviction grace above all, which a member's start stamps and a claim never
// refreshes), since the gateway, told who blocks, decides that by stopping the
// head. The member named is the lowest spec ID whose absence alone lets the
// start proceed, or the lowest claimed one when only their absence together
// does, so the answer does not depend on map order. A member pinned by its own
// spec is no claim's doing and is left in place.
func (o *owner) claimedMemberBlocking(snap PolicySnapshot, demand Spec) string {
	var claimed []string
	for _, r := range snap.Running {
		if st := o.specs[r.SpecID]; st != nil && st.claimed() && !st.pinnedBySpec() {
			claimed = append(claimed, r.SpecID)
		}
	}
	if len(claimed) == 0 {
		return ""
	}
	slices.Sort(claimed)
	if !admitProceeds(withoutRunning(snap, claimed...), demand) {
		return ""
	}
	for _, id := range claimed {
		if admitProceeds(withoutRunning(snap, id), demand) {
			return id
		}
	}
	return claimed[0]
}

// withoutRunning returns snap with the processes ids taken out of Running, as
// if they had exited; snap itself is left untouched.
func withoutRunning(snap PolicySnapshot, ids ...string) PolicySnapshot {
	snap.Running = slices.DeleteFunc(slices.Clone(snap.Running), func(r RunningProc) bool {
		return slices.Contains(ids, r.SpecID)
	})
	return snap
}

// admitProceeds reports whether Admit lets demand start in snap, at once or by
// evicting: neither a Wait nor a refusal.
func admitProceeds(snap PolicySnapshot, demand Spec) bool {
	dec := Admit(snap, demand)
	return !dec.Wait && dec.Reason == "" && !dec.NoCapacity
}

// setAdmissionBlockedBy records the claimed member st's admission waits for
// ("" for none), ringing the transitions doorbell when that changes: the
// gateway acts on it.
func (o *owner) setAdmissionBlockedBy(st *specState, member string) {
	if st.admissionBlockedBy == member {
		return
	}
	st.admissionBlockedBy = member
	o.signalTransition()
}

// noteWaitEnded drops st's blocking member once nothing wants st up any more:
// its last waiter left, failed or was served, and no protected floor keeps it
// waiting.
func (o *owner) noteWaitEnded(st *specState) {
	if !o.wantsUp(st) {
		o.setAdmissionBlockedBy(st, "")
	}
}
