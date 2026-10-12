// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package geminiwire

import (
	"encoding/base64"
	"testing"
)

// TestParseResponseParallelSameNameCallsDistinctIDs pins that two parallel calls
// to the SAME function with no (or an identical) thought signature get distinct
// neutral ToolCall.IDs. Gemini correlates functionResponses by name and order,
// but a strict downstream client keys on tool_call_id, so duplicate ids on one
// message would make it reject or merge the pair. Each id must still decode back
// to the function name so the round-trip through render stays correct.
func TestParseResponseParallelSameNameCallsDistinctIDs(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"get_weather","args":{"city":"NYC"}}},
		{"functionCall":{"name":"get_weather","args":{"city":"LA"}}}]},
		"finishReason":"STOP"}]}`)
	r, err := ParseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(r.ToolCalls))
	}
	if r.ToolCalls[0].ID == r.ToolCalls[1].ID {
		t.Fatalf("parallel same-name calls got identical ids: %q", r.ToolCalls[0].ID)
	}
	n0, _, ok0 := decodeToolCallID(r.ToolCalls[0].ID)
	n1, _, ok1 := decodeToolCallID(r.ToolCalls[1].ID)
	if !ok0 || !ok1 || n0 != "get_weather" || n1 != "get_weather" {
		t.Fatalf("both ids must decode to the function name, got %q/%v and %q/%v", n0, ok0, n1, ok1)
	}
}

// TestParseStreamChunkToolCallIndexIsStreamGlobal pins that two parallel calls to
// the same function arriving on SEPARATE stream chunks still get distinct ids:
// the caller passes its running tool-call count as the offset, exactly as the
// client's stream aggregator does.
func TestParseStreamChunkToolCallIndexIsStreamGlobal(t *testing.T) {
	c0, _, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	c1, _, err := ParseStreamChunk([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}]}`), len(c0.ToolCalls))
	if err != nil {
		t.Fatal(err)
	}
	if len(c0.ToolCalls) != 1 || len(c1.ToolCalls) != 1 {
		t.Fatalf("want one call per chunk, got %d and %d", len(c0.ToolCalls), len(c1.ToolCalls))
	}
	if c0.ToolCalls[0].ID == c1.ToolCalls[0].ID {
		t.Fatalf("same-name calls across chunks got identical ids: %q", c0.ToolCalls[0].ID)
	}
}

// TestEncodeToolCallIDIndexInPayload pins the id format: index 0 omits x (so it
// stays the bare encoding), a non-zero index is carried as "x", and the index
// never changes what the id decodes to.
func TestEncodeToolCallIDIndexInPayload(t *testing.T) {
	id0 := encodeToolCallID("f", "", 0)
	id1 := encodeToolCallID("f", "", 1)
	if id0 == id1 {
		t.Fatalf("index 0 and 1 must produce different ids, both %q", id0)
	}
	if want := toolCallIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"n":"f"}`)); id0 != want {
		t.Fatalf("id0 = %q, want %q (index 0 omitted)", id0, want)
	}
	if want := toolCallIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"n":"f","x":1}`)); id1 != want {
		t.Fatalf("id1 = %q, want %q", id1, want)
	}
	if n, _, ok := decodeToolCallID(id1); !ok || n != "f" {
		t.Fatalf("id1 must still decode to the function name, got %q/%v", n, ok)
	}
}
