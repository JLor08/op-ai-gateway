// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"reflect"
	"testing"
)

func fg(traversal string, members ...string) FlatGroup {
	ms := make([]GroupMember, len(members))
	for i, m := range members {
		ms[i] = GroupMember{MemberGatewayName: m, Priority: i}
	}
	return FlatGroup{Traversal: traversal, Members: ms, Active: true}
}

func TestFlattenGroupStrategies(t *testing.T) {
	// G=[A,H,B,I], H=[C,D], I=[E,F,J]; H,I default depth.
	base := map[string]FlatGroup{
		"h": fg("depth", "C", "D"),
		"i": fg("depth", "E", "F", "J"),
	}
	cases := []struct {
		traversal string
		want      []string
	}{
		{"depth", []string{"A", "C", "D", "B", "E", "F", "J"}},
		{"breadth", []string{"A", "B", "C", "D", "E", "F", "J"}},
		{"round_robin", []string{"A", "C", "B", "E", "D", "F", "J"}},
	}
	for _, c := range cases {
		graph := map[string]FlatGroup{"h": base["h"], "i": base["i"]}
		graph["g"] = fg(c.traversal, "A", "H", "B", "I")
		got := FlattenGroup("G", graph)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %v want %v", c.traversal, got, c.want)
		}
	}
}

func TestFlattenGroupDedupFirstWins(t *testing.T) {
	graph := map[string]FlatGroup{
		"g": fg("depth", "A", "H", "A"),
		"h": fg("depth", "A", "B"),
	}
	// A appears at G[0], inside H, and G[2] — only the first survives.
	got := FlattenGroup("G", graph)
	if !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("dedup got %v want [A B]", got)
	}
}

func TestFlattenGroupCycleDropped(t *testing.T) {
	// G -> H -> G (indirect) and G -> G (self) must not hang and must drop back-edges.
	graph := map[string]FlatGroup{
		"g": fg("depth", "A", "H", "G"),
		"h": fg("depth", "B", "G"),
	}
	got := FlattenGroup("G", graph)
	if !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("cycle got %v want [A B]", got)
	}
}

func TestFlattenGroupInactiveSubgroupSkipped(t *testing.T) {
	h := fg("depth", "C", "D")
	h.Active = false
	graph := map[string]FlatGroup{"g": fg("depth", "A", "H", "B"), "h": h}
	got := FlattenGroup("G", graph)
	if !reflect.DeepEqual(got, []string{"A", "B"}) { // H contributes nothing, not emitted as a model
		t.Fatalf("inactive got %v want [A B]", got)
	}
}

func TestFlattenGroupOwnStrategyPerSubgroup(t *testing.T) {
	// G depth over [A, H]; H breadth over [B, J, K] where J=[C,D].
	graph := map[string]FlatGroup{
		"g": fg("depth", "A", "H"),
		"h": fg("breadth", "B", "J", "K"),
		"j": fg("depth", "C", "D"),
	}
	// H(breadth): direct B,K first, then J → B,K,C,D. G(depth): A then H → A,B,K,C,D.
	got := FlattenGroup("G", graph)
	if !reflect.DeepEqual(got, []string{"A", "B", "K", "C", "D"}) {
		t.Fatalf("own-strategy got %v", got)
	}
}

func TestFlattenGroupTrimsPaddedMemberNames(t *testing.T) {
	// A subgroup member name with surrounding whitespace must still expand — the
	// recursion key must be normalized the same way isGroup is (regression: the
	// two disagreed, silently dropping the padded subgroup's contents).
	graph := map[string]FlatGroup{
		"g": fg("depth", "A", " H "),
		"h": fg("depth", "C", "D"),
	}
	got := FlattenGroup("G", graph)
	if !reflect.DeepEqual(got, []string{"A", "C", "D"}) {
		t.Fatalf("padded-name got %v want [A C D]", got)
	}
}
