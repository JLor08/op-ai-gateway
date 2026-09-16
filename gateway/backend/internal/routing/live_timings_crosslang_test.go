// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// portalLiveTimingsPath is the portal's hand-copy of liveTimingsCapableKinds,
// relative to this package's test working directory
// (gateway/backend/internal/routing). It is a sibling tree, reached the same way
// internal/gateway's agent-config golden test reaches ../../../../server-agent.
const portalLiveTimingsPath = "../../../frontend/src/components/shared/liveTimings.ts"

var (
	// portalCapableSetRe captures the body of the portal's
	// `const liveTimingsCapableKinds ... = new Set([ ... ])`. Anchored on the
	// exact variable name and `new Set([`, so a portal refactor that renames or
	// reshapes the literal makes this guard FAIL to find it (and fail the test)
	// rather than pass vacuously against nothing.
	portalCapableSetRe = regexp.MustCompile(`(?s)liveTimingsCapableKinds\b[^=]*=\s*new Set\(\[(.*?)\]\)`)
	portalQuotedRe     = regexp.MustCompile(`['"]([^'"]*)['"]`)
)

// TestLiveTimingsCapableKindsMatchPortalHandCopy is the cross-language guard
// issue #87 asked for. gateway/frontend/src/components/shared/liveTimings.ts
// hand-copies liveTimingsCapableKinds, and until now nothing tied the two: the
// portal's own liveTimings.test.ts pins the copy against the TypeScript unions
// ALONE and cannot see this map, and this set's size pin
// (TestLiveTimingsCapableKindsSizeIsPinned) only counts members and breadcrumbs
// the rest in prose. So a kind added to the GO set with no portal edit shipped a
// silent portal that never offered the switch while the frontend suite stayed
// green.
//
// This reads the portal's Set literal and asserts it is EXACTLY this map, in the
// Go suite, so that gap now fails a real test. It follows the same cross-tree
// pattern as internal/gateway's server-agent config golden test: read a sibling
// tree's file by a relative path and compare it to the Go authority.
func TestLiveTimingsCapableKindsMatchPortalHandCopy(t *testing.T) {
	raw, err := os.ReadFile(portalLiveTimingsPath)
	if err != nil {
		t.Fatalf("read the portal hand-copy %s: %v -- if the file moved, REPOINT this guard, do not delete it", portalLiveTimingsPath, err)
	}
	portal := parsePortalCapableKinds(t, string(raw))

	want := make([]string, 0, len(liveTimingsCapableKinds))
	for k := range liveTimingsCapableKinds {
		want = append(want, k)
	}
	sort.Strings(want)
	sort.Strings(portal)

	if !slices.Equal(want, portal) {
		t.Fatalf("the portal's liveTimingsCapableKinds (%v, in %s) is not routing.liveTimingsCapableKinds (%v).\n"+
			"Edit the two together: a kind added to the Go set must be added to the portal Set, or every operator on that kind is told the feature is llama.cpp-only and gets no checkbox; a kind removed from Go must be removed there too. The size pin (TestLiveTimingsCapableKindsSizeIsPinned) enumerates the OTHER sites -- architecture docs, i18n strings, and type-level comments -- that restate this membership in prose only.",
			portal, portalLiveTimingsPath, want)
	}
}

// parsePortalCapableKinds extracts the members of the portal's
// liveTimingsCapableKinds Set, failing the test if the literal cannot be found
// or parsed -- so a portal refactor can never make the guard above pass
// vacuously.
func parsePortalCapableKinds(t *testing.T, src string) []string {
	t.Helper()
	m := portalCapableSetRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find `const liveTimingsCapableKinds ... = new Set([...])` in %s: the hand-copy was renamed or reshaped, so this guard can no longer see it -- repoint the regex, do not delete the guard", portalLiveTimingsPath)
	}
	body := m[1]
	out := make([]string, 0)
	for _, q := range portalQuotedRe.FindAllStringSubmatch(body, -1) {
		out = append(out, q[1])
	}
	if len(out) == 0 && strings.TrimSpace(body) != "" {
		t.Fatalf("the portal liveTimingsCapableKinds Set body %q holds members this guard could not read as quoted strings", body)
	}
	return out
}
