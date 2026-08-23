// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import "testing"

// TestRecorderExactFilters covers the in-memory matchUsage exact (case-insensitive
// equality) filters ServerExact / SessionIDExact / ModelExact: each pins a single
// value and must NOT match a longer sibling that merely has it as a prefix
// (e.g. "prod" must not match "prod-eu"). The SQL store is exercised separately in
// internal/store; this pins the memory Recorder path directly.
func TestRecorderExactFilters(t *testing.T) {
	rec := NewRecorder()

	a := recorderEvent("req_a", "usr_1")
	a.ServerName = "prod"
	a.SessionID = "sess_a"
	a.Model = "gpt"
	b := recorderEvent("req_b", "usr_1")
	b.ServerName = "prod-eu"
	b.SessionID = "sess_ab"
	b.Model = "gpt-4" // hyphen
	// c: an underscore model the same length as b's hyphen model. EqualFold already
	// treats `_` literally; assert it for symmetry with the SQL store, where a LIKE
	// `_`-wildcard would over-match "gpt-4".
	c := recorderEvent("req_c", "usr_1")
	c.ServerName = "prod-us"
	c.SessionID = "sess_c"
	c.Model = "gpt_4" // underscore
	rec.Record(a)
	rec.Record(b)
	rec.Record(c)

	// ServerExact "prod" matches only the exact "prod" row, not "prod-eu".
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ServerExact: "prod"})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("ServerExact=prod ids = %v, want [req_a]", got)
	}

	// SessionIDExact "sess_a" matches only the exact "sess_a" row, not "sess_ab".
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", SessionIDExact: "sess_a"})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("SessionIDExact=sess_a ids = %v, want [req_a]", got)
	}

	// ModelExact "gpt" matches only the exact "gpt" row, not "gpt-4".
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ModelExact: "gpt"})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("ModelExact=gpt ids = %v, want [req_a]", got)
	}

	// ModelExact "gpt_4" (underscore) matches only req_c, never the hyphen "gpt-4".
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ModelExact: "gpt_4"})); len(got) != 1 || got[0] != "req_c" {
		t.Fatalf("ModelExact=gpt_4 ids = %v, want [req_c] (underscore literal, not a wildcard)", got)
	}
}

// TestRecorderExactFiltersMatchEmpty covers the empty-key group expansion: with the
// Has*Exact presence flag set and an EMPTY value, matchUsage must return ONLY the
// rows whose value is empty (the "(no value)" bucket), NOT every in-scope row.
// Mutation: dropping the flag from the gate (so an empty value fires no filter) makes
// this return both rows and the len==1 assertions fail.
func TestRecorderExactFiltersMatchEmpty(t *testing.T) {
	rec := NewRecorder()

	empty := recorderEvent("req_empty", "usr_1")
	empty.SessionID = ""
	empty.ServerName = ""
	empty.Model = ""
	set := recorderEvent("req_set", "usr_1")
	set.SessionID = "sess_x"
	set.ServerName = "prod"
	set.Model = "gpt"
	rec.Record(empty)
	rec.Record(set)

	// HasSessionIDExact with an empty value matches only the empty-session row.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasSessionIDExact: true, SessionIDExact: ""})); len(got) != 1 || got[0] != "req_empty" {
		t.Fatalf("HasSessionIDExact empty ids = %v, want [req_empty]", got)
	}
	// Same for server and model.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasServerExact: true, ServerExact: ""})); len(got) != 1 || got[0] != "req_empty" {
		t.Fatalf("HasServerExact empty ids = %v, want [req_empty]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasModelExact: true, ModelExact: ""})); len(got) != 1 || got[0] != "req_empty" {
		t.Fatalf("HasModelExact empty ids = %v, want [req_empty]", got)
	}

	// WITHOUT the presence flag an empty value fires no filter -> both rows (the
	// pre-fix behavior; guards the flag's meaning).
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", SessionIDExact: ""})); len(got) != 2 {
		t.Fatalf("no-flag empty SessionIDExact ids = %v, want both rows", got)
	}
}
