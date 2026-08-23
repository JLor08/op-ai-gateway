// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "strings"

// FlatGroup is one group in the flattening graph: its traversal strategy, ordered
// (priority) members (each a bare name that may be a model or a subgroup), and
// whether the group is active. Keyed by lowercased gateway_model_name in the graph.
type FlatGroup struct {
	Traversal string // "depth" | "breadth" | "round_robin"
	Members   []GroupMember
	Active    bool
}

// FlattenGroup returns the ordered, de-duplicated leaf-MODEL member names reachable
// from the group `name`, expanding each subgroup by ITS OWN traversal strategy. A
// member present in graph is a subgroup (expanded); one absent is a leaf model
// (emitted). An inactive subgroup and a cyclic back-edge contribute nothing. De-dup
// keeps a model's FIRST occurrence. graph is keyed by lowercased name.
func FlattenGroup(name string, graph map[string]FlatGroup) []string {
	raw := flattenGroup(strings.ToLower(strings.TrimSpace(name)), graph, map[string]struct{}{})
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		key := strings.ToLower(strings.TrimSpace(m))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	return out
}

func flattenGroup(lname string, graph map[string]FlatGroup, visiting map[string]struct{}) []string {
	g, ok := graph[lname]
	if !ok || !g.Active {
		return nil
	}
	if _, onPath := visiting[lname]; onPath {
		return nil // cycle back-edge
	}
	visiting[lname] = struct{}{}
	defer delete(visiting, lname)

	isGroup := func(memberName string) bool {
		_, present := graph[strings.ToLower(strings.TrimSpace(memberName))]
		return present
	}
	// subKey normalizes a member name to its graph key the SAME way isGroup does,
	// so the recursive lookup can never disagree with the isGroup check (a
	// whitespace-padded subgroup name must expand, not silently drop).
	subKey := func(memberName string) string {
		return strings.ToLower(strings.TrimSpace(memberName))
	}

	switch g.Traversal {
	case "breadth":
		out := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if !isGroup(m.MemberGatewayName) {
				out = append(out, m.MemberGatewayName)
			}
		}
		for _, m := range g.Members {
			if isGroup(m.MemberGatewayName) {
				out = append(out, flattenGroup(subKey(m.MemberGatewayName), graph, visiting)...)
			}
		}
		return out
	case "round_robin":
		type stream struct {
			isModel bool
			name    string
			items   []string
		}
		streams := make([]stream, 0, len(g.Members))
		maxLen := 0
		for _, m := range g.Members {
			if isGroup(m.MemberGatewayName) {
				items := flattenGroup(subKey(m.MemberGatewayName), graph, visiting)
				streams = append(streams, stream{items: items})
				if len(items) > maxLen {
					maxLen = len(items)
				}
			} else {
				streams = append(streams, stream{isModel: true, name: m.MemberGatewayName})
			}
		}
		passes := maxLen
		if passes == 0 {
			passes = 1 // still emit the direct models on pass 1
		}
		out := make([]string, 0)
		for k := 0; k < passes; k++ {
			for _, st := range streams {
				if st.isModel {
					if k == 0 {
						out = append(out, st.name)
					}
					continue
				}
				if k < len(st.items) {
					out = append(out, st.items[k])
				}
			}
		}
		return out
	default: // "depth" and any unknown, defensively
		out := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if isGroup(m.MemberGatewayName) {
				out = append(out, flattenGroup(subKey(m.MemberGatewayName), graph, visiting)...)
			} else {
				out = append(out, m.MemberGatewayName)
			}
		}
		return out
	}
}
