// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"testing"
	"time"
)

// The requested (pre-token-override) model is a first-class event field: it
// round-trips through the memory recorder, is reachable by the free-text q
// search, and is an accepted sort key (issue #7).
func TestRequestedModelRoundTripSearchAndSort(t *testing.T) {
	r := NewRecorder()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "req_a", UserID: "u1", Model: "qwen-coder", RequestedModel: "gpt-oss-20b", CreatedAt: base},
		{ID: "req_b", UserID: "u1", Model: "qwen-coder", RequestedModel: "claude-sonnet", CreatedAt: base.Add(time.Minute)},
	}
	for _, e := range events {
		if err := r.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// Round-trip: the field survives storage.
	page, err := r.Query(Query{UserID: "u1", Limit: 25})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Data) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Data))
	}

	// Free-text q matches requested_model.
	page, err = r.Query(Query{UserID: "u1", Q: "gpt-oss", Limit: 25})
	if err != nil {
		t.Fatalf("Query(q): %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "req_a" {
		t.Fatalf("q=gpt-oss matched %#v, want exactly req_a", page.Data)
	}

	// requested_model is a whitelisted sort key and sorts ascending.
	if got := NormalizeSort("requested_model"); got != "requested_model" {
		t.Fatalf("NormalizeSort(requested_model) = %q, want it whitelisted", got)
	}
	page, err = r.Query(Query{UserID: "u1", Sort: "requested_model", Order: "asc", Limit: 25})
	if err != nil {
		t.Fatalf("Query(sort): %v", err)
	}
	if page.Data[0].RequestedModel != "claude-sonnet" || page.Data[1].RequestedModel != "gpt-oss-20b" {
		t.Fatalf("sort order = [%s %s], want [claude-sonnet gpt-oss-20b]",
			page.Data[0].RequestedModel, page.Data[1].RequestedModel)
	}
}
