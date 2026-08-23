// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package inference

import (
	"encoding/json"
	"testing"
)

func TestMessageTextIgnoresImageParts(t *testing.T) {
	m := Message{Role: RoleUser, Content: []ContentPart{
		{Type: ContentText, Text: "hello"},
		{Type: ContentImage, ImageURL: "data:image/png;base64,AAAA"},
	}}
	if got := m.Text(); got != "hello" {
		t.Fatalf("Text() = %q, want hello", got)
	}
}

func TestStreamEventReasoningRoundTrips(t *testing.T) {
	ev := StreamEvent{Type: StreamEventTextDelta, Reasoning: "thinking"}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back StreamEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Reasoning != "thinking" {
		t.Fatalf("reasoning = %q, want thinking", back.Reasoning)
	}
}
