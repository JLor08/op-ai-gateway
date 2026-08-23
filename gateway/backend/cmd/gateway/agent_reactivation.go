// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/routing"
	"time"
)

// reactivationStore is what the reactivation coordinator reads/writes.
type reactivationStore interface {
	AIServerByID(ctx context.Context, id string) (routing.AIServer, error)
	UpdateServerNetbirdState(ctx context.Context, id, domain, peerID string, connected bool) error
}

// serverReconciler runs the per-server NetBird group/policy reconcile.
type serverReconciler interface {
	ReconcileServerNetbird(ctx context.Context, serverID string)
}

// netbirdSyncFunc is the per-server peer sync (syncServerNetbirdOnce), injected so
// the coordinator's branch logic is testable without a real NetBird endpoint.
type netbirdSyncFunc func(ctx context.Context, ncfg netbird.Config, timeout time.Duration, store netbirdStateStore, server routing.AIServer, onOnline onlineEventFunc) (connected bool, resolved bool)

// reactivationDeps bundles handleAgentReactivation's dependencies (store,
// NetBird settings/reconciler/sync func, and its call timeout) into one value,
// purely to keep the function's own parameter list short; it carries no
// behavior of its own.
type reactivationDeps struct {
	store      reactivationStore
	settings   netbirdSettings
	reconciler serverReconciler
	syncOne    netbirdSyncFunc
	timeout    time.Duration
}

// handleAgentReactivation reacts to a ServerAgent inactive->active edge for one
// server WITHOUT waiting for the periodic loops: if the server has a NetBird peer
// (module configured, NetbirdEnabled, a peer or tracking group), it syncs the peer
// (name/domain/connected) and reconciles its group/policy, then triggers an app
// health check only if the peer is online; a server with no NetBird peer triggers
// the health check directly. Best-effort: every failure is Debug-logged and never
// surfaced; a full trigger channel is dropped (the periodic loops are the backstop).
func handleAgentReactivation(ctx context.Context, serverID string, deps reactivationDeps, healthTrigger chan<- string) {
	server, err := deps.store.AIServerByID(ctx, serverID)
	if err != nil {
		slog.Debug("agent reactivation: server lookup failed", "server_id", serverID, "error", err)
		return
	}
	cfg, ok, cerr := deps.settings.NetbirdConfig(ctx)
	hasPeer := cerr == nil && ok && server.NetbirdEnabled &&
		(server.NetbirdPeerID != "" || server.NetbirdGroupID != "")
	if hasPeer {
		connected, _ := deps.syncOne(ctx, netbird.Config{URL: cfg.URL, Token: cfg.Token}, deps.timeout, deps.store, server, nil)
		deps.reconciler.ReconcileServerNetbird(ctx, serverID)
		if !connected {
			return // offline -> the periodic NetBird + health loops are the backstop
		}
	}
	select {
	case healthTrigger <- serverID:
	default:
		slog.Debug("agent reactivation: app-health trigger full, relying on loop", "server_id", serverID)
	}
}
