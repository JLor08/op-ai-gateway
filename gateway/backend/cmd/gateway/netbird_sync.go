// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"time"
)

// netbirdCallTimeout bounds every NetBird admin-API call the sync loop makes so a
// wedged NetBird portal can never stall a sync cycle. It is never the timeout-less
// http.DefaultClient.
const netbirdCallTimeout = 15 * time.Second

// netbirdSyncMinInterval is the fallback cadence used ONLY if the settings reader
// ever yields a non-positive interval (NetbirdPolicySettings normally floors both
// the peer and reconcile intervals at 10s). It guards time.NewTicker against a
// zero/negative duration; it is not a lower bound on the operator-configured
// cadence (the reader is the authority there, so a UI value as low as 10s is
// honored).
const netbirdSyncMinInterval = 30 * time.Second

// serverStore is the store surface the peer-sync loop needs: enumerate all
// servers and write the peer-synced server state. *store.SQLiteStore,
// *store.PostgresStore, and *routing.MemoryStore all satisfy it. The group mirror
// (UpdateServerNetbirdGroups) moved into portal.Service (Loop B), so this
// interface no longer needs it.
type serverStore interface {
	AIServers(ctx context.Context) ([]routing.AIServer, error)
	UpdateServerNetbirdState(ctx context.Context, id, domain, peerID string, connected bool) error
}

// netbirdSettings is the settings source the peer loop reads the resolved NetBird
// config from each cycle. *portal.Service satisfies it.
type netbirdSettings interface {
	NetbirdConfig(ctx context.Context) (portal.NetbirdConfig, bool, error)
}

// onlineEventFunc is called when a server transitions connected false->true. The
// peer loop invokes it (best-effort) after a successful state write; production
// wraps the call in a goroutine so a burst of transitions never blocks the loop.
type onlineEventFunc func(serverID string)

// netbirdStateStore is the write side syncServerNetbirdOnce needs — a subset of
// serverStore, so any serverStore satisfies it.
type netbirdStateStore interface {
	UpdateServerNetbirdState(ctx context.Context, id, domain, peerID string, connected bool) error
}

// netbirdReconciler is the group+policy loop's (Loop B) dependency: one fleet pass
// of group mirror + access-policy reconcile + deny-enforce, plus the auto-rotation
// threshold check for the NetBird admin API token. *portal.Service satisfies it
// via ReconcileAllServerNetbird + MaybeRotateNetbirdToken.
type netbirdReconciler interface {
	ReconcileAllServerNetbird(ctx context.Context)
	MaybeRotateNetbirdToken(ctx context.Context)
}

// intervalReader supplies both loop cadences from the live system settings.
// *portal.Service satisfies it via NetbirdPolicySettings, which floors both
// intervals at 10s and clamps the reconcile interval >= the peer interval (A<=B).
// peerEnvFallback (seconds) is the legacy env default used only when the peer KV
// is blank.
type intervalReader interface {
	NetbirdPolicySettings(ctx context.Context, peerEnvFallback int) portal.NetbirdPolicySettings
}

// runNetbirdSyncOnce runs one NetBird reconcile pass: for every NetBird-enabled
// server with a tracking group, it resolves the enrolled peer (by the stored peer
// id when known, else the first peer of the per-server tracking group), keeps the
// peer's NetBird name synced to the gateway server name, and — only on a real
// change — writes the peer's DNS name to the server domain plus the peer id +
// connected status.
//
// It is a no-op when the module is off (NetbirdConfig ok=false). It is best-effort
// throughout: any per-server resolve/rename/write error is logged at Debug and the
// server skipped, so one server's failure never aborts the pass — and, critically,
// a resolve or rename error CONTINUES WITHOUT WRITING, so a transient NetBird error
// can never clear a server's good domain. The NetBird token is never logged.
//
// On a confirmed false->true connection transition it fires onOnline(server.ID)
// AFTER a successful state write, so a freshly-connected peer's group/policy state
// is reconciled promptly (Loop B otherwise only converges it on the reconcile
// cadence). onOnline may be nil.
//
// now is injected for parity with the app-health loop's testable form; the
// reconcile itself is stateless in wall-clock time.
func runNetbirdSyncOnce(ctx context.Context, store serverStore, settings netbirdSettings, timeout time.Duration, now func() time.Time, onOnline onlineEventFunc) {
	cfg, ok, err := settings.NetbirdConfig(ctx)
	if err != nil {
		slog.Debug("netbird sync: config error", "error", err) // never log the token
		return
	}
	if !ok {
		return // module off / not configured
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	servers, err := store.AIServers(ctx)
	if err != nil {
		slog.Debug("netbird sync: list servers failed", "error", err)
		return
	}
	for _, server := range servers {
		syncServerNetbirdOnce(ctx, ncfg, timeout, store, server, onOnline)
	}
}

// syncServerNetbirdOnce reconciles a single NetBird server's peer state — name,
// domain, connected — exactly as one iteration of runNetbirdSyncOnce's loop does,
// and returns its connection state. resolved is false when the server is not a
// NetBird peer, the peer could not be resolved, or a rename/state write failed; in
// every not-resolved path it writes nothing (a good domain is never cleared), so
// connected is only authoritative when resolved is true. onOnline (may be nil)
// fires only on a genuine false->true transition after a successful state write.
func syncServerNetbirdOnce(ctx context.Context, ncfg netbird.Config, timeout time.Duration, store netbirdStateStore, server routing.AIServer, onOnline onlineEventFunc) (connected bool, resolved bool) {
	if !server.NetbirdEnabled || (server.NetbirdGroupID == "" && server.NetbirdPeerID == "") {
		return false, false
	}
	peer, ok := resolveNetbirdPeer(ctx, ncfg, timeout, server)
	if !ok {
		return false, false // not enrolled or a resolve error -> never write (don't clear a good domain)
	}
	// One-peer backstop: enforce at most one peer in the server's OWN
	// tracking group, deleting stale duplicates and adopting the surviving
	// winner. Conservative + best-effort: it returns the resolved peer
	// unchanged (no delete, no behavior change) whenever the server has no
	// tracking group, the group can't be fetched, or the group holds <= 1
	// peer — so the resolve/skip invariants above still hold.
	peer = dedupTrackingGroup(ctx, ncfg, timeout, server, peer)
	if peer.Name != server.Name {
		renamed, err := netbird.UpdatePeerName(ctx, ncfg, timeout, peer, server.Name)
		if err != nil {
			slog.Debug("netbird sync: rename peer failed", "server_id", server.ID, "error", err)
			return false, false // rename error -> never write (don't clear a good domain)
		}
		peer = renamed
	}
	newDomain := server.Domain
	if peer.DNSLabel != "" {
		newDomain = peer.DNSLabel
	}
	// Capture the pre-write connection state so the online event fires only on a
	// genuine false->true transition (not on a mere domain/peer-id change while
	// already connected).
	wasConnected := server.NetbirdConnected
	if peer.ID != server.NetbirdPeerID || newDomain != server.Domain || peer.Connected != server.NetbirdConnected {
		if err := store.UpdateServerNetbirdState(ctx, server.ID, newDomain, peer.ID, peer.Connected); err != nil {
			slog.Debug("netbird sync: update server state failed", "server_id", server.ID, "error", err)
			return false, false
		}
		// Online event: only after the state write actually succeeded, and only
		// on a confirmed false->true connection transition. Best-effort — the
		// production callback wraps the reconcile in a goroutine so it never
		// blocks the loop.
		if !wasConnected && peer.Connected && onOnline != nil {
			onOnline(server.ID)
		}
	}
	return peer.Connected, true
}

// resolveNetbirdPeer resolves the NetBird peer for a server: by the stored peer id
// when known, else the first peer of the server's tracking group. If the stored
// peer id no longer resolves (e.g. the peer was deleted + re-enrolled, so the
// tracking group now holds a NEW peer id), it FALLS BACK to group re-resolution so
// the fresh peer is adopted rather than the server being skipped forever. It returns
// ok=false only when nothing can be resolved — a group error/empty group (a
// transient error or a not-yet-enrolled server) — so the caller skips the server
// WITHOUT writing (never clearing a good domain).
func resolveNetbirdPeer(ctx context.Context, ncfg netbird.Config, timeout time.Duration, server routing.AIServer) (netbird.Peer, bool) {
	if server.NetbirdPeerID != "" {
		peer, err := netbird.GetPeer(ctx, ncfg, timeout, server.NetbirdPeerID)
		if err == nil {
			return peer, true
		}
		// Cached peer id gone — re-resolve via the tracking group to adopt a
		// re-enrolled peer's new id (falls through to the group path below).
		slog.Debug("netbird sync: get peer failed, re-resolving via group", "server_id", server.ID, "peer_id", server.NetbirdPeerID, "error", err)
	}
	group, err := netbird.GetGroup(ctx, ncfg, timeout, server.NetbirdGroupID)
	if err != nil {
		slog.Debug("netbird sync: get group failed", "server_id", server.ID, "group_id", server.NetbirdGroupID, "error", err)
		return netbird.Peer{}, false
	}
	if len(group.Peers) == 0 {
		return netbird.Peer{}, false // setup key not yet used -> not enrolled
	}
	peer, err := netbird.GetPeer(ctx, ncfg, timeout, group.Peers[0].ID)
	if err != nil {
		slog.Debug("netbird sync: get first group peer failed", "server_id", server.ID, "peer_id", group.Peers[0].ID, "error", err)
		return netbird.Peer{}, false
	}
	return peer, true
}

// dedupTrackingGroup is the sync's one-peer backstop: it enforces at most one
// peer in a server's OWN tracking group (server.NetbirdGroupID), deleting stale
// duplicates and returning the surviving winner. It is CONSERVATIVE and
// best-effort throughout — it NEVER deletes on partial or error information and
// NEVER deletes the winner:
//
//   - No tracking group id -> return resolved (nothing to dedup; also keeps the
//     manually-linked peer-id-only path from ever touching a group).
//   - GetGroup errors -> Debug-log + return resolved (no dedup this tick; a
//     transient error can never clear a good domain, since the caller only
//     writes on a real diff of the returned peer).
//   - <= 1 peer -> return resolved (byte-identical to the pre-backstop behavior:
//     no new writes/deletes).
//   - > 1 peer -> GetPeer EVERY member for full info; if ANY GetPeer errors,
//     Debug-log + return resolved (need complete info; never delete on a partial
//     view). Otherwise pick the winner (latest last_seen; ties broken by the
//     lexicographically largest peer id — deterministic), best-effort DeletePeer
//     every OTHER member, and return the winner so the caller's existing
//     rename/domain/connected/group-mirror logic adopts it (the peer.ID !=
//     server.NetbirdPeerID diff adopts a new winner id).
//
// Every NetBird call is bounded by the passed timeout. The NetBird token is
// never logged.
func dedupTrackingGroup(ctx context.Context, ncfg netbird.Config, timeout time.Duration, server routing.AIServer, resolved netbird.Peer) netbird.Peer {
	if server.NetbirdGroupID == "" {
		return resolved // no tracking group -> nothing to dedup
	}
	group, err := netbird.GetGroup(ctx, ncfg, timeout, server.NetbirdGroupID)
	if err != nil {
		slog.Debug("netbird sync: dedup get group failed", "server_id", server.ID, "group_id", server.NetbirdGroupID, "error", err)
		return resolved // no dedup this tick (never clear a good domain)
	}
	if len(group.Peers) <= 1 {
		return resolved // 0 or 1 peer -> unchanged behavior
	}
	// > 1 peer: fetch FULL info for every member. Any error aborts the dedup for
	// this tick (need complete info; never delete on a partial view).
	members := make([]netbird.Peer, 0, len(group.Peers))
	for _, gp := range group.Peers {
		p, err := netbird.GetPeer(ctx, ncfg, timeout, gp.ID)
		if err != nil {
			slog.Debug("netbird sync: dedup get member peer failed, skipping dedup", "server_id", server.ID, "peer_id", gp.ID, "error", err)
			return resolved
		}
		members = append(members, p)
	}
	winner := pickDedupWinner(members)
	for _, p := range members {
		if p.ID == winner.ID {
			continue // never delete the winner
		}
		if err := netbird.DeletePeer(ctx, ncfg, timeout, p.ID); err != nil {
			slog.Debug("netbird sync: dedup delete extra peer failed", "server_id", server.ID, "peer_id", p.ID, "error", err)
		}
	}
	return winner
}

// pickDedupWinner selects the peer to KEEP among a tracking group's members: the
// one with the latest last_seen (parsed tolerantly as RFC3339; empty/unparseable
// -> the zero time, so it never beats a peer with a real timestamp), ties broken
// by the lexicographically largest peer id so the choice is deterministic.
// members must be non-empty (dedupTrackingGroup only calls it for a > 1-peer
// group).
func pickDedupWinner(members []netbird.Peer) netbird.Peer {
	winner := members[0]
	winnerSeen := parseLastSeen(winner.LastSeen)
	for _, p := range members[1:] {
		seen := parseLastSeen(p.LastSeen)
		if seen.After(winnerSeen) || (seen.Equal(winnerSeen) && p.ID > winner.ID) {
			winner = p
			winnerSeen = seen
		}
	}
	return winner
}

// parseLastSeen parses a NetBird last_seen timestamp as RFC3339, returning the
// zero time on an empty or unparseable value.
func parseLastSeen(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// netbirdPeerInterval reads the peer-sync cadence from the live settings each
// cycle. The reader floors it at 10s; a non-positive value (unreachable in
// production, possible in a test fake) falls back to netbirdSyncMinInterval so
// time.NewTicker never panics.
func netbirdPeerInterval(ctx context.Context, intervals intervalReader, peerEnvFallback int) time.Duration {
	d := intervals.NetbirdPolicySettings(ctx, peerEnvFallback).PeerSyncInterval
	if d <= 0 {
		return netbirdSyncMinInterval
	}
	return d
}

// netbirdReconcileInterval reads the group+policy reconcile cadence from the live
// settings each cycle (floored at 10s by the reader, and >= the peer interval).
func netbirdReconcileInterval(ctx context.Context, intervals intervalReader, peerEnvFallback int) time.Duration {
	d := intervals.NetbirdPolicySettings(ctx, peerEnvFallback).ReconcileInterval
	if d <= 0 {
		return netbirdSyncMinInterval
	}
	return d
}

// netbirdSyncDeps bundles runNetbirdSyncLoop's dependencies (store, settings,
// interval reader + its env fallback, call timeout, clock, and the online-
// transition callback) into one value, purely to keep the function's own
// parameter list short; it carries no behavior of its own.
type netbirdSyncDeps struct {
	store           serverStore
	settings        netbirdSettings
	intervals       intervalReader
	peerEnvFallback int
	timeout         time.Duration
	now             func() time.Time
	onOnline        onlineEventFunc
}

// runNetbirdSyncLoop is Loop A: the NetBird PEER-sync + online-detector loop. It
// reconciles each peer's name/domain/connected state once immediately, then on
// the peer-sync interval re-read from settings each cycle (Ticker+Reset, mirroring
// runAppHealthLoop). It fires onOnline on a false->true connection transition. A
// send on trigger runs an EXTRA pass immediately, independent of the ticker
// cadence — used to reconcile promptly after a NetBird account dns_domain change
// (every peer's dns_label changes with it, so stored server domains would
// otherwise go stale until the next tick). trigger may be nil (never fires). It
// returns when ctx is cancelled.
func runNetbirdSyncLoop(ctx context.Context, deps netbirdSyncDeps, trigger <-chan struct{}) {
	runLoop(ctx, loopOpts{
		Immediate: true,
		Interval:  func() time.Duration { return netbirdPeerInterval(ctx, deps.intervals, deps.peerEnvFallback) },
		Trigger:   trigger,
		Pass: func(ctx context.Context) {
			runNetbirdSyncOnce(ctx, deps.store, deps.settings, deps.timeout, deps.now, deps.onOnline)
		},
	})
}

// startNetbirdSyncLoop launches runNetbirdSyncLoop (Loop A) in a goroutine and
// returns its cancel func. It is a package var so a test can substitute a fake
// store + fake settings + fake interval reader and observe the goroutine
// start/stop, mirroring startAppHealthLoop. trigger, when non-nil, lets a caller
// force an immediate extra pass (e.g. on a NetBird dns_domain change); pass nil
// when no such trigger is needed.
var startNetbirdSyncLoop = func(store serverStore, settings netbirdSettings, onOnline onlineEventFunc, intervals intervalReader, peerEnvFallback int, timeout time.Duration, trigger <-chan struct{}) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go runNetbirdSyncLoop(ctx, netbirdSyncDeps{
		store: store, settings: settings, intervals: intervals, peerEnvFallback: peerEnvFallback,
		timeout: timeout, now: func() time.Time { return time.Now().UTC() }, onOnline: onOnline,
	}, trigger)
	return cancel
}

// runNetbirdReconcileOnce runs one group+policy fleet pass (Loop B's unit of
// work), then the auto-rotation threshold+cooldown check for the NetBird admin
// API token (best-effort; MaybeRotateNetbirdToken never surfaces an error here).
func runNetbirdReconcileOnce(ctx context.Context, reconciler netbirdReconciler) {
	reconciler.ReconcileAllServerNetbird(ctx)
	reconciler.MaybeRotateNetbirdToken(ctx)
}

// runNetbirdReconcileLoop is Loop B: the NetBird group-mirror + access-policy
// reconcile loop. It runs one fleet pass immediately, then on the reconcile
// interval re-read from settings each cycle (Ticker+Reset, mirroring
// runAppHealthLoop). It returns when ctx is cancelled.
func runNetbirdReconcileLoop(ctx context.Context, reconciler netbirdReconciler, intervals intervalReader, peerEnvFallback int) {
	runLoop(ctx, loopOpts{
		Immediate: true,
		Interval:  func() time.Duration { return netbirdReconcileInterval(ctx, intervals, peerEnvFallback) },
		Pass:      func(ctx context.Context) { runNetbirdReconcileOnce(ctx, reconciler) },
	})
}

// startNetbirdReconcileLoop launches runNetbirdReconcileLoop (Loop B) in a
// goroutine and returns its cancel func. It is a package var so a test can
// substitute a fake reconciler + fake interval reader and observe the goroutine
// start/stop, mirroring startNetbirdSyncLoop.
var startNetbirdReconcileLoop = func(reconciler netbirdReconciler, intervals intervalReader, peerEnvFallback int) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go runNetbirdReconcileLoop(ctx, reconciler, intervals, peerEnvFallback)
	return cancel
}
