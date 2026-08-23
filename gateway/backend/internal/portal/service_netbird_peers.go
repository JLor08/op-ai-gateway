// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// netbirdTrackingGroupPrefix names the per-server NetBird tracking group
// ("op-gw-"+server.ID) that generateNetbirdSetupKey finds-or-creates.
const netbirdTrackingGroupPrefix = "op-gw-"

// generateNetbirdSetupKey creates (or reuses) the per-server tracking group,
// resolves the optional module group, generates a one-off setup key whose
// auto-groups are both, and records the setup-key id + tracking-group id on the
// server (a targeted UPDATE). The tracking group is resolved idempotently: an
// explicit trackingGroupID wins, else the server's stored NetbirdGroupID, else a
// find-or-create by name ("op-gw-"+server.ID) — so a regenerate or a create-hook
// retry never creates a duplicate same-named group. It returns the generated key
// (display-once). Every step is timeout-bounded; any error is returned to the
// caller (who decides whether to fail).
func (s *Service) generateNetbirdSetupKey(ctx context.Context, cfg NetbirdConfig, server routing.AIServer, trackingGroupID string) (netbird.SetupKey, error) {
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	autoGroups := make([]string, 0, len(cfg.Groups)+1)
	seenGroup := make(map[string]bool, len(cfg.Groups))
	for _, name := range cfg.Groups {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		moduleGroupID, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, name)
		if err != nil {
			return netbird.SetupKey{}, err
		}
		if moduleGroupID == "" || seenGroup[moduleGroupID] {
			continue
		}
		seenGroup[moduleGroupID] = true
		autoGroups = append(autoGroups, moduleGroupID)
	}
	// Prefer an already-stored tracking-group id; only resolve by name when none is
	// known. ResolveGroupID is find-or-create, so a regenerate/create-hook retry
	// reuses the existing "op-gw-<id>" group instead of creating a duplicate.
	if trackingGroupID == "" {
		trackingGroupID = server.NetbirdGroupID
	}
	if trackingGroupID == "" {
		resolved, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdTrackingGroupPrefix+server.ID)
		if err != nil {
			return netbird.SetupKey{}, err
		}
		trackingGroupID = resolved
	}
	if trackingGroupID != "" {
		autoGroups = append(autoGroups, trackingGroupID)
	}
	sk, err := netbird.CreateSetupKey(ctx, ncfg, netbirdCallTimeout, netbird.SetupKeyParams{
		Name:       server.Name,
		AutoGroups: autoGroups,
	})
	if err != nil {
		return netbird.SetupKey{}, err
	}
	// Persist with enabled=true so the create hook keeps the flag on AND an
	// enroll on a previously non-NetBird server flips netbird_enabled true.
	if err := s.routes.UpdateServerNetbirdKey(ctx, server.ID, true, sk.ID, trackingGroupID); err != nil {
		return netbird.SetupKey{}, err
	}
	// Provenance: this peer originates from a gateway-generated setup key
	// (portal-created) → mark it managed. Best-effort: a store error here must NOT
	// abort a successful key creation (the key was already generated).
	if err := s.routes.UpdateServerNetbirdPeerManaged(ctx, server.ID, true); err != nil {
		slog.Debug("netbird: mark peer managed on setup-key generation failed", "server_id", server.ID, "error", err)
	}
	return sk, nil
}

// RegenerateNetbirdKey is the enroll-or-regenerate setup-key endpoint: it
// generates a fresh setup key for a server (creating/reusing its per-server
// tracking group) and returns the key (display-once). On a server that is NOT
// yet NetBird-enabled it ENROLLS it — generateNetbirdSetupKey persists
// enabled=true, so the flag flips on and the tracking group is created; on an
// already-NetBird server it regenerates (the old one-off key is superseded).
// Requires admin-or-owner (no-leak 404) and the module enabled.
// It returns the setup-key value AND the ready-to-paste `netbird up` console
// command (display-once; the command contains the key, never the admin token).
//
// Before generating, it GATES on peer provenance: a server with an existing peer
// that is NOT gateway-managed (a manually-linked / externally-used peer) is
// rejected with ErrNetbirdPeerNotManaged (409) — the only blocked case, so the
// proactive deletion below can never delete a foreign peer. A fresh server
// (peer_id == "") or a managed server is always allowed. It then, best-effort,
// PROACTIVELY DELETES the server's existing peer(s) so the re-enrollment yields a
// single peer: fetch the tracking group + DeletePeer each member, DeletePeer the
// stored peer id (defensive; normally already in the group), then clear the local
// linkage (peer_id="", connected=false; domain unchanged — never-clear-domain). A
// NetBird error there is Debug-logged and never aborts key generation.
func (s *Service) RegenerateNetbirdKey(ctx context.Context, principal auth.Token, serverID string) (key string, command string, err error) {
	server, err := s.authorizeServer(ctx, principal, serverID)
	if err != nil {
		return "", "", err
	}
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", ErrNetbirdModuleDisabled
	}
	// Gate: block ONLY a server with an existing NON-managed peer (a manually-linked
	// / externally-used peer). A fresh server (no peer) or a gateway-managed peer is
	// allowed — so the proactive deletion never touches a foreign peer.
	if server.NetbirdPeerID != "" && !server.NetbirdPeerManaged {
		return "", "", ErrNetbirdPeerNotManaged
	}
	// Proactive one-peer cleanup: only when there is something to clean (a fresh
	// server / first enrollment skips this entirely). Best-effort throughout — a
	// NetBird error is Debug-logged and never aborts key generation.
	if server.NetbirdPeerID != "" || server.NetbirdGroupID != "" {
		ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
		if server.NetbirdGroupID != "" {
			if g, gErr := netbird.GetGroup(ctx, ncfg, netbirdCallTimeout, server.NetbirdGroupID); gErr == nil {
				for _, p := range g.Peers {
					if dErr := netbird.DeletePeer(ctx, ncfg, netbirdCallTimeout, p.ID); dErr != nil {
						slog.Debug("netbird: delete existing peer on regenerate failed", "server_id", server.ID, "peer_id", p.ID, "error", dErr)
					}
				}
			} else {
				slog.Debug("netbird: get tracking group on regenerate failed", "server_id", server.ID, "group_id", server.NetbirdGroupID, "error", gErr)
			}
		}
		if server.NetbirdPeerID != "" {
			// Defensive: normally the peer is already deleted via the group above; a
			// 404 (already gone) is nil anyway.
			if dErr := netbird.DeletePeer(ctx, ncfg, netbirdCallTimeout, server.NetbirdPeerID); dErr != nil {
				slog.Debug("netbird: delete stored peer on regenerate failed", "server_id", server.ID, "peer_id", server.NetbirdPeerID, "error", dErr)
			}
		}
		// Clear the local linkage so the sync doesn't keep resolving the deleted peer;
		// the domain is preserved (never-clear-domain).
		if uErr := s.routes.UpdateServerNetbirdState(ctx, server.ID, server.Domain, "", false); uErr != nil {
			slog.Debug("netbird: clear peer linkage on regenerate failed", "server_id", server.ID, "error", uErr)
		}
	}
	sk, err := s.generateNetbirdSetupKey(ctx, cfg, server, server.NetbirdGroupID)
	if err != nil {
		return "", "", err
	}
	return sk.Key, netbirdSetupCommand(cfg.URL, sk.Key), nil
}

// netbirdSetupCommand composes the ready-to-paste `netbird up` console command
// from the configured module URL (assumed to be the NetBird management URL) and
// the freshly-generated setup key. Display-once, never persisted.
func netbirdSetupCommand(url, key string) string {
	return "netbird up --management-url " + url + " --setup-key " + key
}

// resolveNetbirdPeerForMirror resolves a server's NetBird peer for the group mirror:
// by the stored peer id when known, else the first peer of the server's tracking
// group. A cached peer id that no longer resolves falls back to group re-resolution
// (adopting a re-enrolled peer). ok=false on ANY error / empty group, so the caller
// skips the server WITHOUT writing (never clearing a good mirror). Faithful port of
// cmd/gateway resolveNetbirdPeer (the T6 refactor removes the cmd copy).
func (s *Service) resolveNetbirdPeerForMirror(ctx context.Context, ncfg netbird.Config, server routing.AIServer) (netbird.Peer, bool) {
	if server.NetbirdPeerID != "" {
		peer, err := netbird.GetPeer(ctx, ncfg, netbirdCallTimeout, server.NetbirdPeerID)
		if err == nil {
			return peer, true
		}
		slog.Debug("group mirror: get peer failed, re-resolving via group", "server_id", server.ID, "peer_id", server.NetbirdPeerID, "error", err)
	}
	group, err := netbird.GetGroup(ctx, ncfg, netbirdCallTimeout, server.NetbirdGroupID)
	if err != nil {
		slog.Debug("group mirror: get group failed", "server_id", server.ID, "group_id", server.NetbirdGroupID, "error", err)
		return netbird.Peer{}, false
	}
	if len(group.Peers) == 0 {
		return netbird.Peer{}, false // setup key not yet used -> not enrolled
	}
	peer, err := netbird.GetPeer(ctx, ncfg, netbirdCallTimeout, group.Peers[0].ID)
	if err != nil {
		slog.Debug("group mirror: get first group peer failed", "server_id", server.ID, "peer_id", group.Peers[0].ID, "error", err)
		return netbird.Peer{}, false
	}
	return peer, true
}

// mirrorServerGroups mirrors a peer's authoritative NetBird POLICY group membership
// (tracking group excluded) into the portal's local netbird_group_ids column, in the
// SAME canonical form pushNetbirdGroups + the sync loop use (byte-identical → no
// oscillation). Best-effort/void: it only writes on a real diff, and skips silently
// on any resolve/marshal/store error (never clearing a good mirror). It is a no-op
// for a non-NetBird / peerless+groupless server.
func (s *Service) mirrorServerGroups(ctx context.Context, ncfg netbird.Config, server routing.AIServer) {
	if !server.NetbirdEnabled || (server.NetbirdGroupID == "" && server.NetbirdPeerID == "") {
		return
	}
	peer, ok := s.resolveNetbirdPeerForMirror(ctx, ncfg, server)
	if !ok {
		return
	}
	refs := netbirdPolicyGroups(peer.Groups, server.NetbirdGroupID)
	js, err := netbird.CanonicalGroupsJSON(refs)
	if err != nil {
		slog.Debug("group mirror: canonical json", "server_id", server.ID, "error", err)
		return
	}
	if js != server.NetbirdGroupIDs {
		if err := s.routes.UpdateServerNetbirdGroups(ctx, server.ID, js); err != nil {
			slog.Debug("group mirror: update", "server_id", server.ID, "error", err)
		}
	}
}

// ReconcileServerNetbird is the combined per-server reconcile (group mirror + access
// policy) for ONE server, used by the event hooks (T5). Best-effort/void: it gates on
// the module, mirrors the peer's groups, then reconciles the access policy (which
// gates on policy management internally). No caller yet in this task (T5/T6 wire it).
func (s *Service) ReconcileServerNetbird(ctx context.Context, serverID string) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	server, err := s.routes.AIServerByID(ctx, serverID)
	if err != nil {
		slog.Debug("reconcile server: load", "server_id", serverID, "error", err)
		return
	}
	s.mirrorServerGroups(ctx, ncfg, server)
	s.reconcileServerPolicy(ctx, serverID)
}

// ReconcileAllServerNetbird is the fleet pass (group mirror for every NetBird server
// + one access-policy diff per server against a SINGLE ListPolicies) used by the sync
// loop (T6). Best-effort/void: it gates on the module, lists policies once (only when
// policy management is on), mirrors + reconciles each NetBird server, and finally
// enforces deny-by-default when configured.
//
// Concurrency: the group mirror is idempotent last-write-wins on
// UpdateServerNetbirdGroups (no policy hazard), so it runs OUTSIDE netbirdPolicyMu. The
// policy section — ListPolicies + the per-server reconcileServerPolicyWith loop — is
// held under netbirdPolicyMu as ONE atomic critical section (the SAME lock
// reconcileServerPolicy/reconcileAllServerPolicies use for the app-CRUD and
// settings-PUT triggers): listing outside the lock and creating inside would still let
// two concurrent passes both see "no policy" and both CreatePolicy it.
// reconcileServerPolicyWith itself stays UNLOCKED — it is called here WHILE the lock is
// held, so it must never try to acquire netbirdPolicyMu itself (non-reentrant mutex,
// would deadlock). Deny-by-default enforcement takes its OWN separate, sequential
// netbirdPolicyMu acquisition inside applyDenyByDefault (via enforceDenyByDefault),
// run AFTER the policy-loop lock has been released — never nested inside it.
func (s *Service) ReconcileAllServerNetbird(ctx context.Context) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	set := s.NetbirdPolicySettings(ctx, 0)
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Debug("reconcile all: list servers", "error", err)
		return
	}
	for _, server := range servers {
		if !server.NetbirdEnabled {
			continue
		}
		s.mirrorServerGroups(ctx, ncfg, server)
	}
	if set.ManagePolicies {
		s.netbird.policyMu.Lock()
		// listOK distinguishes a successful list (even of an EMPTY account → a
		// managed server with active apps must still get a CreatePolicy) from a
		// list ERROR (skip the policy diff entirely this tick — an empty
		// `policies` slice on error must not look like "no policies exist" and
		// mass-create). A nil-vs-non-nil guard can't tell those apart; the
		// explicit flag can.
		if policies, lErr := netbird.ListPolicies(ctx, ncfg, netbirdCallTimeout); lErr != nil {
			slog.Debug("reconcile all: list policies", "error", lErr)
		} else {
			for _, server := range servers {
				if !server.NetbirdEnabled {
					continue
				}
				s.reconcileServerPolicyWith(ctx, ncfg, set, server, policies)
			}
			// Fleet-wide agent-ingest policy: reuse the SAME policy snapshot, still
			// under netbirdPolicyMu (atomic with the per-server reconcile above).
			s.reconcileAgentIngestPolicyWith(ctx, ncfg, servers, policies)
			// Fleet-wide agent-ingest-TLS policy (separate mode only; same snapshot,
			// same lock).
			s.reconcileAgentIngestTLSPolicyWith(ctx, ncfg, servers, policies)
			// Fleet-wide ICMP ping-allow policies (same snapshot, same lock).
			s.reconcilePingGatewayPolicyWith(ctx, ncfg, set, servers, policies)
			s.reconcilePingServersPolicyWith(ctx, ncfg, set, servers, policies)
		}
		s.netbird.policyMu.Unlock()
	}
	if set.DenyByDefault && set.DenyByDefaultEnforce {
		s.enforceDenyByDefault(ctx, ncfg)
	}
}

// CreateGatewaySetupKey mints a one-off NetBird setup key for the GATEWAY's own
// peer (so an operator no longer has to create it by hand in the NetBird
// dashboard). It resolve-or-creates the gateway group (netbirdGatewayGroupName)
// and generates a one-off, single-use, 30-day setup key whose enrolling peer
// joins that group, returning the key AND the ready-to-paste `netbird up` console
// command. Both are display-once: the key is NEVER persisted, and the admin token
// is NEVER logged or returned. ErrNetbirdModuleDisabled when the module is off /
// not configured.
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) CreateGatewaySetupKey(ctx context.Context, principal auth.Token) (key string, command string, err error) {
	if !isSystem(principal) {
		return "", "", ErrPrincipalForbidden
	}
	return s.mintGatewaySetupKey(ctx)
}

// mintGatewaySetupKey is the core mint path shared by CreateGatewaySetupKey (the
// display-once endpoint) and EnrollGatewaySidecar (which ALSO writes the key to a
// shared-volume file). It gates on the module being configured, resolve-or-creates
// the gateway group (netbirdGatewayGroupName), and generates a one-off, single-use,
// 30-day setup key whose enrolling peer joins that group; it returns the key AND the
// ready-to-paste `netbird up` console command. The admin token is NEVER logged or
// returned. ErrNetbirdModuleDisabled when the module is off / not configured.
func (s *Service) mintGatewaySetupKey(ctx context.Context) (key string, command string, err error) {
	cfg, ok, cErr := s.NetbirdConfig(ctx)
	if cErr != nil {
		return "", "", cErr
	}
	if !ok {
		return "", "", ErrNetbirdModuleDisabled
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	gid, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil {
		return "", "", err
	}
	var autoGroups []string
	if gid != "" {
		autoGroups = []string{gid}
	}
	sk, err := netbird.CreateSetupKey(ctx, ncfg, netbirdCallTimeout, netbird.SetupKeyParams{
		Name:       netbirdGatewaySetupKeyName,
		AutoGroups: autoGroups,
	})
	if err != nil {
		return "", "", err
	}
	return sk.Key, netbirdSetupCommand(cfg.URL, sk.Key), nil
}

// EnrollGatewaySidecar mints a one-off NetBird gateway setup key (like
// CreateGatewaySetupKey) and ALSO writes the raw key to the configured shared
// key file (netbirdKeyFile / OP_AI_GATEWAY_NETBIRD_KEY_FILE) so a waiting NetBird
// sidecar can read it and self-`netbird up` — no operator copy-paste. It returns
// the key + the `netbird up` command as a display-once fallback (the operator can
// also copy it). The key is written ATOMICALLY with 0600 perms (temp file in the
// same dir + rename) so a concurrent sidecar reader never sees a partial key.
// ErrNetbirdKeyFileNotConfigured when no key file is configured (feature off);
// ErrNetbirdModuleDisabled when the module is off. The key/token are NEVER logged.
// NetbirdKeyFileConfigured reports whether a shared setup-key file path is
// configured (OP_AI_GATEWAY_NETBIRD_KEY_FILE). It is the deployment signal that an
// autonomous-enroll NetBird sidecar is wired (the gateway writes the minted key to
// this path for the waiting sidecar); the frontend gates the "Sidecar enrollen"
// action on it so the button only shows when a sidecar can actually consume the key.
func (s *Service) NetbirdKeyFileConfigured() bool { return s.netbird.keyFile != "" }

// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate.
func (s *Service) EnrollGatewaySidecar(ctx context.Context, principal auth.Token) (key string, command string, err error) {
	if !isSystem(principal) {
		return "", "", ErrPrincipalForbidden
	}
	if s.netbird.keyFile == "" {
		return "", "", ErrNetbirdKeyFileNotConfigured
	}
	key, command, err = s.mintGatewaySetupKey(ctx)
	if err != nil {
		return "", "", err
	}
	// Hand the sidecar the management URL from system settings too, so the autonomous
	// `netbird up` targets the SAME server the key was minted on (no separate
	// NB_MANAGEMENT_URL needed). Written BEFORE the key so the key file stays the
	// "ready" signal the wrapper waits on. Best-effort; the URL is not secret.
	if cfg, ok, cErr := s.NetbirdConfig(ctx); cErr == nil && ok && cfg.URL != "" {
		if wErr := writeKeyFileAtomic(s.netbird.keyFile+".mgmt-url", cfg.URL); wErr != nil {
			slog.Debug("netbird sidecar enroll: writing the management-url file failed", "error", wErr)
		}
	}
	if err := writeKeyFileAtomic(s.netbird.keyFile, key); err != nil {
		// The key was minted but could not be delivered to the shared volume. Log at
		// Debug (the error is an os error over the path, never the key) and surface it
		// to the caller.
		slog.Debug("netbird sidecar enroll: writing the setup key file failed", "path", s.netbird.keyFile, "error", err)
		return "", "", err
	}
	return key, command, nil
}

// writeKeyFileAtomic writes key to path with 0600 permissions atomically: it writes
// to a temp file in the SAME directory, then renames it over the target, so a
// concurrent reader (the NetBird sidecar) never observes a partial key. The key is
// never logged. A failure before the rename leaves the target untouched (the temp
// file is best-effort removed).
func writeKeyFileAtomic(path, key string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".netbird-key-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: a no-op once the rename has consumed the temp file.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(key); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// SetServerNetbird is the system-admin NetBird linkage editor: set the enabled
// flag + peer id + the peer's POLICY groups (e.g. to link a manually-created
// NetBird peer to an existing server and choose which policy groups it belongs
// to). The route is system-scoped, so there is no owner check; an unknown id
// returns the store's ErrNotFound (mapped to a no-leak 404 at the handler).
// Disabling NetBird on a domainless (never-synced) server is rejected with
// ErrServerDomainRequired (a plain server must have a domain).
//
// A non-empty peer id must not already be linked to ANOTHER server —
// ErrNetbirdPeerInUse (409). The current server is excluded so a same-server
// re-link is allowed.
//
// When a peer is linked (enabled + peer id) it runs a BEST-EFFORT, SYNCHRONOUS
// reconcile: fetch the peer, rename it to the gateway server name, set the
// server's domain to the peer's NetBird DNS (dns_label) + connected NOW, ensure
// the per-server tracking group exists (creating "op-gw-<id>" on the first link
// if missing) and that the peer is in it, and push the desired POLICY groups
// (add/remove delta; the tracking group is NEVER removed) — then mirror the
// desired set to the portal in the SAME canonical form the sync loop stores (no
// oscillation). Any NetBird error leaves the saved link untouched and returns no
// error (the sync loop reconciles later); the never-clear-domain invariant holds
// because the domain defaults to the current server domain and is only
// overwritten by a non-empty dns_label.
//
// When enabled == false the per-server tracking group is DELETED (best-effort,
// module permitting) and the stored linkage — group id + policy-group mirror — is
// cleared alongside the disable link write.
//
// The peerManaged flag is written verbatim on the enable path (a system-admin may
// declare a manually-linked peer as gateway-managed, which also governs whether a
// key regenerate is allowed). The disable path returns early and does not touch it.
//
// The allowPing flag is persisted best-effort on BOTH branches (a store error is
// Debug-logged, never fails the linkage write); on the enable path it is written
// BEFORE the policy reconcile so the fleet-wide op-gw-ping-servers destination set
// sees the fresh value.
func (s *Service) SetServerNetbird(ctx context.Context, principal auth.Token, id string, enabled bool, peerID string, groupIDs []string, peerManaged bool, policyOverride string, allowPing bool, pingExclude bool) (ServerDTO, error) {
	if !isSystem(principal) {
		return ServerDTO{}, ErrPrincipalForbidden
	}
	server, err := s.routes.AIServerByID(ctx, id)
	if err != nil {
		return ServerDTO{}, err // store.ErrNotFound -> 404 no-leak at the handler
	}
	peerID = strings.TrimSpace(peerID)
	policyOverride = normalizeNetbirdPolicyOverride(policyOverride)
	// The two ping flags are mutually exclusive (clean 3-state: default/always/never).
	// A both-true request resolves to "never" (exclude wins).
	if pingExclude {
		allowPing = false
	}
	if !enabled && strings.TrimSpace(server.Domain) == "" {
		return ServerDTO{}, ErrServerDomainRequired
	}
	// Uniqueness: a peer id must not be linked to another server.
	if peerID != "" {
		servers, err := s.routes.AIServers(ctx)
		if err != nil {
			return ServerDTO{}, err
		}
		for _, other := range servers {
			if other.ID != id && other.NetbirdPeerID == peerID {
				return ServerDTO{}, ErrNetbirdPeerInUse
			}
		}
	}
	// Disable: delete the per-server tracking group (best-effort, only when one was
	// stored + the module is configured) and ALWAYS clear the local linkage (group
	// id + policy-group mirror) alongside the disable link write (which also resets
	// connected=0). The local clears run unconditionally so a stale mirror can't
	// survive a disable when NetbirdGroupID happens to be empty. The domain-guard
	// ran first.
	if !enabled {
		// Best-effort NetBird cleanup (module permitting): delete the tracking group
		// (only when one was stored) AND the server's access policy. A NetBird error
		// never fails the disable.
		if cfg, ok, cErr := s.NetbirdConfig(ctx); ok && cErr == nil {
			ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
			if server.NetbirdGroupID != "" {
				_ = netbird.DeleteGroup(ctx, ncfg, netbirdCallTimeout, server.NetbirdGroupID)
			}
			s.deleteServerPolicy(ctx, ncfg, id)
		}
		_ = s.routes.UpdateServerNetbirdKey(ctx, id, false, server.NetbirdSetupKeyID, "")
		_ = s.routes.UpdateServerNetbirdGroups(ctx, id, "")
		if pErr := s.routes.UpdateServerNetbirdPolicyOverride(ctx, id, policyOverride); pErr != nil {
			slog.Debug("netbird: persist policy override on disable failed", "server_id", id, "error", pErr)
		}
		if pErr := s.routes.UpdateServerNetbirdAllowPing(ctx, id, allowPing); pErr != nil {
			slog.Debug("netbird: persist allow-ping on disable failed", "server_id", id, "error", pErr)
		}
		if pErr := s.routes.UpdateServerNetbirdPingExclude(ctx, id, pingExclude); pErr != nil {
			slog.Debug("netbird: persist ping-exclude on disable failed", "server_id", id, "error", pErr)
		}
		if err := s.routes.UpdateServerNetbirdLink(ctx, id, false, peerID); err != nil {
			return ServerDTO{}, err
		}
		server, err = s.routes.AIServerByID(ctx, id)
		if err != nil {
			return ServerDTO{}, err
		}
		return s.serverDTO(ctx, server)
	}
	if err := s.routes.UpdateServerNetbirdLink(ctx, id, enabled, peerID); err != nil {
		return ServerDTO{}, err
	}
	// Provenance: the linkage editor writes the PASSED managed flag — a system-admin
	// can declare a manually-linked peer as gateway-managed (or leave it unmanaged).
	// This also lets a re-save of a managed server preserve its managed provenance.
	// The surrounding code returns on store errors, so propagate rather than corrupt
	// provenance.
	if err := s.routes.UpdateServerNetbirdPeerManaged(ctx, id, peerManaged); err != nil {
		return ServerDTO{}, err
	}
	// Persist the per-server policy override BEFORE the policy reconcile below reads
	// the server back (so serverManaged sees the fresh value). Best-effort — a store
	// error here must not corrupt the linkage already written above.
	if pErr := s.routes.UpdateServerNetbirdPolicyOverride(ctx, id, policyOverride); pErr != nil {
		slog.Debug("netbird: persist policy override on link failed", "server_id", id, "error", pErr)
	}
	// Persist the per-server allow-ping flag BEFORE the reconcile below so the
	// fleet-wide op-gw-ping-servers destination set sees the fresh value. Best-effort.
	if pErr := s.routes.UpdateServerNetbirdAllowPing(ctx, id, allowPing); pErr != nil {
		slog.Debug("netbird: persist allow-ping on link failed", "server_id", id, "error", pErr)
	}
	if pErr := s.routes.UpdateServerNetbirdPingExclude(ctx, id, pingExclude); pErr != nil {
		slog.Debug("netbird: persist ping-exclude on link failed", "server_id", id, "error", pErr)
	}
	// Best-effort synchronous reconcile + group push: when a peer is linked, fetch
	// it, rename it to the server name, set the domain to its NetBird DNS +
	// connected NOW, and reconcile the tracking + policy groups. Any NetBird error
	// leaves the link saved; the sync loop reconciles later.
	if peerID != "" {
		if cfg, ok, cErr := s.NetbirdConfig(ctx); ok && cErr == nil {
			ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
			if peer, gErr := netbird.GetPeer(ctx, ncfg, netbirdCallTimeout, peerID); gErr == nil {
				if peer.Name != server.Name {
					if renamed, rErr := netbird.UpdatePeerName(ctx, ncfg, netbirdCallTimeout, peer, server.Name); rErr == nil {
						peer = renamed
					}
				}
				domain := server.Domain
				if peer.DNSLabel != "" {
					domain = peer.DNSLabel
				}
				_ = s.routes.UpdateServerNetbirdState(ctx, id, domain, peer.ID, peer.Connected)
				// Tracking group create-on-link: reuse the stored id, else find-or-create
				// "op-gw-<id>" and persist it (keeping the setup-key id).
				trackingID := server.NetbirdGroupID
				if trackingID == "" {
					trackingID, _ = netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdTrackingGroupPrefix+id)
					if trackingID != "" {
						_ = s.routes.UpdateServerNetbirdKey(ctx, id, true, server.NetbirdSetupKeyID, trackingID)
					}
				}
				s.pushNetbirdGroups(ctx, ncfg, id, peer, trackingID, groupIDs)
			}
		}
	}
	// Reconcile the server's access policy (best-effort; gates internally on the
	// module + policy management). Runs on every enable, independent of whether a
	// peer id was supplied — the policy is driven by the server's active app ports.
	s.reconcileServerPolicy(ctx, id)
	// Refresh the account-wide policies (incl. op-gw-ping-servers) so this server's
	// allow-ping flag change reflects promptly in the fleet-wide destination set.
	// Async (context.Background()) — the request ctx may be cancelled after the
	// response; Loop B is the backstop. Tracked by policySideEffectWG (mirrors the
	// settings-PUT side-effect pattern) for clean shutdown + test determinism.
	s.netbird.policySideEffectWG.Add(1)
	go func() {
		defer s.netbird.policySideEffectWG.Done()
		s.reconcileAllServerPolicies(context.Background())
	}()
	server, err = s.routes.AIServerByID(ctx, id)
	if err != nil {
		return ServerDTO{}, err
	}
	return s.serverDTO(ctx, server)
}

// pushNetbirdGroups reconciles a linked peer's NetBird POLICY group membership to
// the desired set (best-effort) and mirrors the desired set to the portal in the
// SAME canonical form the sync loop stores (byte-identical canonical structure —
// no oscillation). The desired set is groupIDs trimmed/deduped with the tracking
// group removed; the delta is add = desired∖current, remove = current∖desired
// (both computed with the tracking group excluded from current, so the tracking
// group is NEVER in remove). The peer is ADDED to the tracking group when it is
// not already a member. Mirror names come from the peer's own group list where
// known, else the id — the next sync tick overwrites with authoritative names.
func (s *Service) pushNetbirdGroups(ctx context.Context, ncfg netbird.Config, serverID string, peer netbird.Peer, trackingID string, groupIDs []string) {
	current := map[string]bool{}
	names := map[string]string{}
	peerInTracking := false
	for _, g := range peer.Groups {
		if g.ID == "" {
			continue
		}
		names[g.ID] = g.Name
		if trackingID != "" && g.ID == trackingID {
			peerInTracking = true
			continue // tracking group is excluded from the policy delta
		}
		current[g.ID] = true
	}
	desired := map[string]bool{}
	desiredOrder := make([]string, 0, len(groupIDs))
	for _, raw := range groupIDs {
		gid := strings.TrimSpace(raw)
		if gid == "" || gid == trackingID || desired[gid] {
			continue
		}
		desired[gid] = true
		desiredOrder = append(desiredOrder, gid)
	}
	add := make([]string, 0)
	remove := make([]string, 0)
	for gid := range desired {
		if !current[gid] {
			add = append(add, gid)
		}
	}
	for gid := range current {
		if !desired[gid] {
			remove = append(remove, gid) // tracking never here (excluded from current)
		}
	}
	// Ensure the peer is in the tracking group (added, NEVER removed).
	if trackingID != "" && !peerInTracking {
		add = append(add, trackingID)
	}
	if len(add) > 0 || len(remove) > 0 {
		_ = netbird.SetPeerGroups(ctx, ncfg, netbirdCallTimeout, peer.ID, add, remove)
	}
	// Mirror the desired policy set (tracking excluded) — canonical, byte-identical
	// to the sync loop's writer so the two never oscillate.
	refs := make([]netbird.GroupRef, 0, len(desiredOrder))
	for _, gid := range desiredOrder {
		name := names[gid]
		if name == "" {
			name = gid
		}
		refs = append(refs, netbird.GroupRef{ID: gid, Name: name})
	}
	js, err := netbird.CanonicalGroupsJSON(refs)
	if err != nil {
		return
	}
	_ = s.routes.UpdateServerNetbirdGroups(ctx, serverID, js)
}

// NetbirdGroups lists the groups configured in the NetBird account (for the
// settings group picker). ErrNetbirdModuleDisabled when the module is off / not
// configured; otherwise the netbird.ListGroups result (or netbird.ErrAuth / a
// transport error). The admin token is never returned — only the group id/name.
func (s *Service) NetbirdGroups(ctx context.Context) ([]netbird.Group, error) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNetbirdModuleDisabled
	}
	all, err := netbird.ListGroups(ctx, netbird.Config{URL: cfg.URL, Token: cfg.Token}, netbirdCallTimeout)
	if err != nil {
		return nil, err
	}
	// Hide the internal per-server tracking groups ("op-gw-<serverID>") from the
	// pickers — they are managed automatically by the gateway and must never be
	// selectable in the system-settings or linkage-editor group multiselect.
	filtered := make([]netbird.Group, 0, len(all))
	for _, g := range all {
		if strings.HasPrefix(g.Name, netbirdTrackingGroupPrefix) {
			continue
		}
		filtered = append(filtered, g)
	}
	return filtered, nil
}

// NetbirdPeers lists the peers in the NetBird account (for the linkage-editor
// peer picker). ErrNetbirdModuleDisabled when the module is off / not configured;
// otherwise the netbird.ListPeers result (or netbird.ErrAuth / a transport
// error). The admin token is never returned.
func (s *Service) NetbirdPeers(ctx context.Context) ([]netbird.Peer, error) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNetbirdModuleDisabled
	}
	return netbird.ListPeers(ctx, netbird.Config{URL: cfg.URL, Token: cfg.Token}, netbirdCallTimeout)
}

// ResolveGatewayPeerIP resolves the NetBird IP (the 100.x address) of the peer
// the operator selected as the gateway's own peer (netbird_gateway_peer_id). It
// is called once at startup to bind the agent listener. When no peer is selected
// it returns ("", nil) so the caller keeps the single-listener behavior; when a
// peer is selected but the module is off it returns ErrNetbirdModuleDisabled;
// otherwise it propagates the GetPeer result. The admin token is never returned
// or logged.
func (s *Service) ResolveGatewayPeerIP(ctx context.Context) (string, error) {
	peerID := s.NetbirdGatewayPeerID(ctx)
	if peerID == "" {
		return "", nil
	}
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNetbirdModuleDisabled
	}
	peer, err := netbird.GetPeer(ctx, netbird.Config{URL: cfg.URL, Token: cfg.Token}, netbirdCallTimeout, peerID)
	if err != nil {
		return "", err
	}
	return peer.IP, nil
}

// ResolveGatewayPeerDNS returns the selected gateway peer's NetBird DNS name
// (DNSLabel), or "" if no peer is selected / the module is off / on any error.
// Best-effort; callers fall back to the listener IP:port.
func (s *Service) ResolveGatewayPeerDNS(ctx context.Context) (string, error) {
	peerID := s.NetbirdGatewayPeerID(ctx)
	if peerID == "" {
		return "", nil
	}
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil || !ok {
		return "", err
	}
	peer, err := netbird.GetPeer(ctx, netbird.Config{URL: cfg.URL, Token: cfg.Token}, netbirdCallTimeout, peerID)
	if err != nil {
		return "", err
	}
	return peer.DNSLabel, nil
}

// ReconcileGatewayPeer keeps the gateway's own-peer System Settings in sync with
// NetBird in one group/peer fetch: (1) auto-selects netbird_gateway_peer_id from the
// op-gw-portal group's live winner peer when the stored id is empty or stale (no
// longer a member), and (2) renames that peer to netbird_gateway_peer_name when set
// and differing. Best-effort: module off / not-enrolled / a NetBird error is a no-op
// or a swallowed rename; never crashes. Returns the effective peer id + whether the
// selection changed. Called on an interval by the reconcile loop.
func (s *Service) ReconcileGatewayPeer(ctx context.Context) (string, bool, error) {
	cfg, ok, err := s.NetbirdConfig(ctx)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	gid, err := netbird.ResolveGroupID(ctx, ncfg, netbirdCallTimeout, netbirdGatewayGroupName)
	if err != nil || gid == "" {
		return "", false, err
	}
	group, err := netbird.GetGroup(ctx, ncfg, netbirdCallTimeout, gid)
	if err != nil {
		return "", false, err
	}
	if len(group.Peers) == 0 {
		return "", false, nil
	}
	members := make([]netbird.Peer, 0, len(group.Peers))
	for _, gp := range group.Peers {
		p, gErr := netbird.GetPeer(ctx, ncfg, netbirdCallTimeout, gp.ID)
		if gErr != nil {
			slog.Debug("netbird gateway reconcile: get peer failed", "peer_id", gp.ID, "error", gErr)
			continue
		}
		members = append(members, p)
	}
	if len(members) == 0 {
		return "", false, nil
	}
	winner := pickGatewayPeer(members)

	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return "", false, err
	}
	storedID := NetbirdGatewayPeerID(values)
	desiredName := NetbirdGatewayPeerName(values)

	effectiveID := storedID
	changed := false
	if storedID == "" || !groupHasPeer(group.Peers, storedID) {
		if wErr := s.settings.SetSystemSetting(ctx, netbirdGatewayPeerIDKey, winner.ID, s.clock()); wErr != nil {
			return storedID, false, wErr
		}
		effectiveID = winner.ID
		changed = true
	}

	if desiredName != "" {
		if target := findPeer(members, effectiveID); target != nil && target.Name != desiredName {
			if _, rErr := netbird.UpdatePeerName(ctx, ncfg, netbirdCallTimeout, *target, desiredName); rErr != nil {
				slog.Debug("netbird gateway reconcile: rename failed", "peer_id", target.ID, "error", rErr)
			}
		}
	}
	return effectiveID, changed, nil
}

// groupHasPeer reports whether the group's authoritative membership list contains
// peer id (independent of whether that peer's GetPeer succeeded this tick, so a
// transient GetPeer blip on the stored peer never makes a valid member look stale).
func groupHasPeer(peers []netbird.GroupPeer, id string) bool {
	for _, p := range peers {
		if p.ID == id {
			return true
		}
	}
	return false
}

func findPeer(members []netbird.Peer, id string) *netbird.Peer {
	for i := range members {
		if members[i].ID == id {
			return &members[i]
		}
	}
	return nil
}

// pickGatewayPeer: connected beats disconnected; then latest last_seen (RFC3339,
// tolerant); ties broken by the lexicographically largest id. members non-empty.
func pickGatewayPeer(members []netbird.Peer) netbird.Peer {
	winner := members[0]
	for _, p := range members[1:] {
		if betterGatewayPeer(p, winner) {
			winner = p
		}
	}
	return winner
}

func betterGatewayPeer(a, b netbird.Peer) bool {
	if a.Connected != b.Connected {
		return a.Connected
	}
	as, bs := parseNetbirdLastSeen(a.LastSeen), parseNetbirdLastSeen(b.LastSeen)
	if !as.Equal(bs) {
		return as.After(bs)
	}
	return a.ID > b.ID
}

func parseNetbirdLastSeen(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// NetbirdTestOverride optionally overrides the stored url/token for a
// connection test so the operator can test unsaved credentials before saving
// them. A nil field falls back to the stored value. The token is used only for
// the ping — it is never persisted or logged.
type NetbirdTestOverride struct {
	URL   *string
	Token *string
}

// TestNetbird verifies the NetBird URL + token by pinging the admin API.
// Without an override it uses the SAVED settings (save first); with an
// override it pings the given url/token instead (each field falls back to the
// stored value when nil), so the operator can test UNSAVED credentials before
// saving them — including the fresh-setup case where the module checkbox is on
// but no url/token has been saved yet (NetbirdConfig's completeness check would
// otherwise always report "not configured" and defeat the override entirely).
// The gate is therefore the RAW module-enabled checkbox (NetbirdModuleChecked),
// NOT the stored config's completeness — completeness is checked only AFTER
// the override has been applied. ErrNetbirdNotConfigured when the module
// checkbox is off, or when neither the stored nor the override values yield a
// non-empty url+token; otherwise the netbird.Ping result (nil, netbird.ErrAuth,
// or another error).
//
// The route is system-scoped (requireWebScope("system") at the handler); as of
// PT-2 Part 2b this also checks isSystem(principal) itself (ErrPrincipalForbidden
// otherwise) as defense-in-depth against a future internal caller that bypasses
// the HTTP gate. The handler surfaces every error uniformly as {ok:false,
// error} at HTTP 200 (never a distinct status per error), so
// ErrPrincipalForbidden is reported the same way as any other TestNetbird
// failure -- it never reaches an internal caller that skips the HTTP gate.
func (s *Service) TestNetbird(ctx context.Context, principal auth.Token, override *NetbirdTestOverride) error {
	if !isSystem(principal) {
		return ErrPrincipalForbidden
	}
	if !s.NetbirdModuleChecked(ctx) {
		return ErrNetbirdNotConfigured
	}
	// ok is ignored here: completeness (url+token non-empty) is re-checked below
	// AFTER the override is applied, since the stored values alone may be
	// incomplete by design (fresh setup) while the override supplies the rest.
	cfg, _, err := s.NetbirdConfig(ctx)
	if err != nil {
		return err
	}
	ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
	if override != nil {
		if override.URL != nil {
			ncfg.URL = *override.URL
		}
		if override.Token != nil {
			ncfg.Token = *override.Token
		}
	}
	if ncfg.URL == "" || ncfg.Token == "" {
		return ErrNetbirdNotConfigured
	}
	return netbird.Ping(ctx, ncfg, netbirdCallTimeout)
}
