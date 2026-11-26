// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "strings"

// ProcessTree is the per-spec choice of HOW the agent supervises a native
// runtime's process tree on a Windows host: "direct" (today's behaviour and the
// default of every pre-feature row: the agent signals and kills the child it
// started) or "job" (the agent puts the child in a Windows Job Object, so stop
// and kill reach every descendant). Inert on every other OS, where a process
// group already scopes the tree. Serialized as its lowercase string, stored
// text (migration 113; mirrors ExecutionMode).
type ProcessTree string

const (
	ProcessTreeDirect ProcessTree = "direct"
	ProcessTreeJob    ProcessTree = "job"
)

// NormalizeProcessTree resolves a process_tree value as an operator or a
// stored row spells it: "" (omitted, or a row never written with the column)
// reads as direct; "direct" and "job" are accepted after trimming; anything
// else -- a different case included -- is not a mode (ok false).
func NormalizeProcessTree(v string) (ProcessTree, bool) {
	switch p := ProcessTree(strings.TrimSpace(v)); p {
	case "", ProcessTreeDirect:
		return ProcessTreeDirect, true
	case ProcessTreeJob:
		return p, true
	default:
		return "", false
	}
}
