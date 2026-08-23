// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
)

// filterByAllowedServers is the generic core of the per-principal
// server-visibility filter — "dedupe server ids -> AllowedServerIDs -> keep
// allowed rows" — that used to be written out three times (XC-1): here, in
// filterAllowedModelServerRows, and in what is now
// FilterAllowedGroupModelServerRows below. rows is filtered down to the
// entries whose serverID(row) is allowed for token, deduping ids into a
// single AllowedServerIDs call no matter how many rows share a server (no
// N+1). An empty rows returns immediately with no store call, mirroring
// every original loop's early-out.
//
// allowedServerIDs is a bound AllowedServerIDs method value — e.g.
// s.AllowedServerIDs or an API value's — rather than an interface parameter,
// so this same generic works whether the caller holds a *Service (the two
// portal-internal sites below) or only a portal.API (package gateway, which
// cannot reference this unexported function directly; see
// FilterAllowedGroupModelServerRows).
//
// failOpen selects the error behavior on an AllowedServerIDs failure, and
// must reproduce each call site's own documented, pre-existing choice
// exactly:
//   - false (visibleMappingViews, filterAllowedModelServerRows): propagate
//     the error unchanged.
//   - true (FilterAllowedGroupModelServerRows): return rows UNFILTERED.
//     AllowedServerIDs already fails safe per its own documented direction,
//     and that site's rows are already filtered once upstream (per group
//     member, via ModelServers), so a transient error here must not
//     double-reject a request that layer already vetted.
func filterByAllowedServers[T any](
	ctx context.Context,
	allowedServerIDs func(context.Context, auth.Token, []string) (map[string]bool, error),
	token auth.Token,
	rows []T,
	serverID func(T) string,
	failOpen bool,
) ([]T, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		id := serverID(r)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	allowed, err := allowedServerIDs(ctx, token, ids)
	if err != nil {
		if failOpen {
			return rows, nil
		}
		return nil, err
	}
	out := make([]T, 0, len(rows))
	for _, r := range rows {
		if allowed[serverID(r)] {
			out = append(out, r)
		}
	}
	return out, nil
}

// FilterAllowedGroupModelServerRows drops any row whose ServerID the given
// principal is not allowed to USE under resource-group provisioning
// (Resource Groups Phase 2 — Task 4), via filterByAllowedServers's generic
// core configured to FAIL OPEN on an AllowedServerIDs error. This is the one
// caller of filterByAllowedServers OUTSIDE package portal: gateway's
// handlePortalModelGroupServers runs it as a defense-in-depth re-check over
// rows that already came from Service.ModelServers per group member (which
// itself filters by AllowedServerIDs — see filterAllowedModelServerRows), so
// a transient store error here must not double-reject a request that layer
// already vetted.
//
// Exported (unlike filterByAllowedServers itself) only because a generic
// function's type parameter cannot be dispatched through a portal.API
// interface method — Go methods cannot be generic — so package gateway needs
// a concrete, non-generic entry point. allower is typically s.Portal (a
// portal.API), never a *Service directly, from package gateway's side.
func FilterAllowedGroupModelServerRows(ctx context.Context, allower API, token auth.Token, rows []GroupModelServerDTO) []GroupModelServerDTO {
	out, _ := filterByAllowedServers(ctx, allower.AllowedServerIDs, token, rows, func(r GroupModelServerDTO) string { return r.ServerID }, true)
	return out
}
