// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/portal"
	"slices"
	"strings"
	"time"
)

// rpcMemberNamesTTL is how long one status-stream subscriber reuses the
// member names it resolved while the set of members on its server stays the
// same: the first frame 15 s or more after the last lookup looks them up
// again, so a renamed model or server shows on it, and a stream costs at most
// one lookup per TTL however often frames arrive.
const rpcMemberNamesTTL = 15 * time.Second

// rpcMemberNamesRetry is how long a stream waits after a failed lookup before
// it tries again, so a lookup that keeps failing (a store outage) does not
// run on every frame.
const rpcMemberNamesRetry = 5 * time.Second

// rpcMemberNamer adds the display names of RPC member rows to one
// subscriber's status frames. It belongs to one stream (one reader, one
// server) and is not safe for concurrent use.
type rpcMemberNamer struct {
	// resolve names the given member spec ids for the reader.
	resolve func(ctx context.Context, specIDs []string) (map[string]portal.RPCMemberName, error)
	now     func() time.Time
	// key is the member set of the last lookup, tried at tried; names and
	// resolvedAt are from the last successful one.
	key        string
	tried      time.Time
	failed     bool
	names      map[string]portal.RPCMemberName
	resolvedAt time.Time
}

// apply returns rows with each RPC member row's DisplayName, HeadModel and
// HeadServer set. The rows of a published frame are shared by every
// subscriber, so a frame with a member is copied before it is named. A member without a name is shown by its id.
func (n *rpcMemberNamer) apply(ctx context.Context, rows []RuntimeStatusDTO) []RuntimeStatusDTO {
	ids := rpcMemberIDs(rows)
	if len(ids) == 0 {
		return rows
	}
	n.refresh(ctx, ids)
	out := slices.Clone(rows)
	for i := range out {
		if out[i].Kind == portal.AgentSpecKindRPCServer {
			name := n.names[out[i].SpecID]
			out[i].DisplayName, out[i].HeadModel, out[i].HeadServer = name.DisplayName, name.HeadModel, name.HeadServer
		}
	}
	return out
}

// refresh looks the names up again when the member set changed, when the
// last successful lookup is rpcMemberNamesTTL old, or rpcMemberNamesRetry
// after a failed one.
//
// A failed lookup keeps the names already known, but never past the TTL: a
// name is at most rpcMemberNamesTTL older than the last successful lookup.
// A refusal of the server (the reader lost its rights) drops them at once.
func (n *rpcMemberNamer) refresh(ctx context.Context, ids []string) {
	key := strings.Join(ids, "\x00")
	now := n.now()
	if key != n.key || n.due(now) {
		n.lookup(ctx, key, ids, now)
	}
	if n.failed && now.Sub(n.resolvedAt) >= rpcMemberNamesTTL {
		n.names = nil
	}
}

// lookup resolves the names for the member set key.
func (n *rpcMemberNamer) lookup(ctx context.Context, key string, ids []string, now time.Time) {
	names, err := n.resolve(ctx, ids)
	n.key, n.tried, n.failed = key, now, err != nil
	switch {
	case err == nil:
		n.names, n.resolvedAt = names, now
	case errors.Is(err, portal.ErrServerNotFound):
		n.names = nil
	}
}

// due reports whether the names for the current member set are looked up
// again at now.
func (n *rpcMemberNamer) due(now time.Time) bool {
	if n.failed {
		return now.Sub(n.tried) >= rpcMemberNamesRetry
	}
	return now.Sub(n.resolvedAt) >= rpcMemberNamesTTL
}

// rpcMemberIDs is the sorted, deduplicated member spec ids of rows.
func rpcMemberIDs(rows []RuntimeStatusDTO) []string {
	var ids []string
	for _, r := range rows {
		if r.Kind == portal.AgentSpecKindRPCServer {
			ids = append(ids, r.SpecID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
