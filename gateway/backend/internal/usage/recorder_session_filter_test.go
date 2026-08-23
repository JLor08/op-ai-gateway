// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import "testing"

// TestRecorderQuerySessionProvenanceFilters covers the in-memory matchUsage
// substring filters for the protocol-aware session provenance columns:
// SessionSource, AgentID, and SessionID. The SQL store is exercised via the
// conformance suite; this pins the memory Recorder path directly.
func TestRecorderQuerySessionProvenanceFilters(t *testing.T) {
	rec := NewRecorder()

	a := recorderEvent("req_a", "usr_1") // Codex request, subagent agent_a
	a.SessionSource = "codex"
	a.SessionID = "sess_a"
	a.AgentID = "agent_a"
	b := recorderEvent("req_b", "usr_1") // Claude Code request, no subagent
	b.SessionSource = "claude-code"
	b.SessionID = "sess_b"
	rec.Record(a)
	rec.Record(b)

	// session_source substring isolates each protocol.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", SessionSource: "codex"})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("SessionSource=codex ids = %v, want [req_a]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", SessionSource: "claude-code"})); len(got) != 1 || got[0] != "req_b" {
		t.Fatalf("SessionSource=claude-code ids = %v, want [req_b]", got)
	}
	if got, err := rec.Query(Query{UserID: "usr_1", SessionSource: "nonexistent"}); err != nil || got.Total != 0 || len(got.Data) != 0 {
		t.Fatalf("SessionSource=nonexistent total/len = %d/%d, want 0/0", got.Total, len(got.Data))
	}

	// agent_id substring: matches A only (B has an empty agent_id, so the
	// "agent_a" needle does not match it).
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", AgentID: "agent_a"})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("AgentID=agent_a ids = %v, want [req_a]", got)
	}

	// session_id substring: matches B only.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", SessionID: "sess_b"})); len(got) != 1 || got[0] != "req_b" {
		t.Fatalf("SessionID=sess_b ids = %v, want [req_b]", got)
	}
}
