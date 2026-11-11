// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/portal"
	"slices"
	"strings"
	"time"
)

// rpcMemberNamesTTL is how long one status-stream subscriber reuses the
// member names it resolved while the set of members on its server stays the
// same: a renamed model or server shows within it, and a stream costs at most
// one resolution per TTL however often frames arrive.
const rpcMemberNamesTTL = 15 * time.Second

// rpcMemberNamer adds the display names of RPC member rows to one
// subscriber's status frames. It belongs to one stream (one reader, one
// server) and is not safe for concurrent use.
type rpcMemberNamer struct {
	// resolve names the given member spec ids for the reader.
	resolve func(ctx context.Context, specIDs []string) (map[string]string, error)
	now     func() time.Time
	key     string
	at      time.Time
	names   map[string]string
}

// apply returns rows with each RPC member row's DisplayName set. The rows of
// a published frame are shared by every subscriber, so a frame with a member
// is copied before it is named. A resolution that fails keeps the names
// already known; a member without one is shown by its id.
func (n *rpcMemberNamer) apply(ctx context.Context, rows []RuntimeStatusDTO) []RuntimeStatusDTO {
	var ids []string
	for _, r := range rows {
		if r.Kind == portal.AgentSpecKindRPCServer {
			ids = append(ids, r.SpecID)
		}
	}
	if len(ids) == 0 {
		return rows
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	key := strings.Join(ids, "\x00")
	now := n.now()
	if key != n.key || now.Sub(n.at) >= rpcMemberNamesTTL {
		if names, err := n.resolve(ctx, ids); err == nil {
			n.key, n.at, n.names = key, now, names
		}
	}
	out := slices.Clone(rows)
	for i := range out {
		if out[i].Kind == portal.AgentSpecKindRPCServer {
			out[i].DisplayName = n.names[out[i].SpecID]
		}
	}
	return out
}
