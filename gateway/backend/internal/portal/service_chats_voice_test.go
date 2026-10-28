// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/store"
	"testing"
)

// A voice answer opens with a short spoken summary (the <speak> part); the thread must record
// WHICH text that was, so the portal can re-render it as the collapsed block on reload. The
// summary rides the assistant message as a spoken_summary key of the schema-free chat document,
// additional metadata beside the unchanged full answer in `content`.

const voiceFullAnswer = "Sum. Detail."

// voiceAnswerTurn is the assistant turn a voice run commits: the FULL answer text and one
// synthesized audio part, as [text, audio], with summary as the recorded spoken summary.
func voiceAnswerTurn(summary string) AssistantTurn {
	text, _ := json.Marshal(map[string]string{"type": "text", "text": voiceFullAnswer})
	audio, _ := json.Marshal(map[string]any{
		"type":      "audio_url",
		"audio_url": map[string]string{"url": dataURL("audio/wav", wavLike(64))},
	})
	return AssistantTurn{
		Content:       voiceFullAnswer,
		ContentParts:  json.RawMessage("[" + string(text) + "," + string(audio) + "]"),
		AudioAnswer:   true,
		AudioStreamed: true,
		SpokenSummary: summary,
	}
}

// commitVoiceAnswer commits one voice turn to a fresh in-memory thread and returns the stored
// assistant message as a raw key->value map, read back through GetChat the way a reload does.
func commitVoiceAnswer(t *testing.T, summary string) map[string]json.RawMessage {
	t.Helper()
	return commitVoiceAnswerTurn(t, voiceAnswerTurn(summary))
}

// commitVoiceAnswerTurn is commitVoiceAnswer for a caller-built turn, so a test can set fields
// (SpokenSummaryStart) the summary-only helper does not expose.
func commitVoiceAnswerTurn(t *testing.T, turn AssistantTurn) map[string]json.RawMessage {
	t.Helper()
	ctx := context.Background()
	owner := chatToken("usr_a")
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	created, err := svc.CreateChat(ctx, owner, CreateChatRequest{Content: json.RawMessage(`{"messages":[]}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := svc.CommitVoiceTurn(ctx, owner, created.ID, "what is the weather", turn, "complete"); err != nil {
		t.Fatalf("CommitVoiceTurn: %v", err)
	}
	got, err := svc.GetChat(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	doc := parseChatDoc(got.Content)
	if len(doc.Messages) != 2 {
		t.Fatalf("stored %d messages, want the user turn and the assistant turn", len(doc.Messages))
	}
	var assistant map[string]json.RawMessage
	if err := json.Unmarshal(doc.Messages[1], &assistant); err != nil {
		t.Fatalf("assistant message: %v", err)
	}
	return assistant
}

// textPartOf is the text of the first {"type":"text"} part of a stored message's content.
func textPartOf(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		t.Fatalf("content is not a parts array: %s", content)
	}
	for _, p := range parts {
		if p.Type == "text" {
			return p.Text
		}
	}
	t.Fatalf("content has no text part: %s", content)
	return ""
}

func TestCommitVoiceTurn_PersistsSpokenSummary(t *testing.T) {
	msg := commitVoiceAnswer(t, "Sum.")

	raw, ok := msg["spoken_summary"]
	if !ok {
		stored, _ := json.Marshal(msg)
		t.Fatalf("assistant message has no spoken_summary key: %s", stored)
	}
	var summary string
	if err := json.Unmarshal(raw, &summary); err != nil || summary != "Sum." {
		t.Fatalf("spoken_summary = %s, want %q", raw, "Sum.")
	}
	// The summary is ADDITIONAL metadata: the text content stays the full clean answer,
	// summary and detail both.
	if got := textPartOf(t, msg["content"]); got != voiceFullAnswer {
		t.Fatalf("text content = %q, want the full answer %q", got, voiceFullAnswer)
	}
}

func TestCommitVoiceTurn_EmptySummaryOmitted(t *testing.T) {
	msg := commitVoiceAnswer(t, "")

	if raw, ok := msg["spoken_summary"]; ok {
		t.Fatalf("spoken_summary = %s, want the key omitted for a no-tag turn (the phase-1 shape)", raw)
	}
	if got := textPartOf(t, msg["content"]); got != voiceFullAnswer {
		t.Fatalf("text content = %q, want %q", got, voiceFullAnswer)
	}
}

// The spoken summary's rune offset rides the turn under spoken_summary_start, so the portal can
// slice the summary out of the full answer by position rather than re-finding it by value (#322).
func TestCommitVoiceTurn_PersistsSpokenSummaryStart(t *testing.T) {
	turn := voiceAnswerTurn("Sum.")
	turn.SpokenSummaryStart = 7
	msg := commitVoiceAnswerTurn(t, turn)

	raw, ok := msg["spoken_summary_start"]
	if !ok {
		stored, _ := json.Marshal(msg)
		t.Fatalf("assistant message has no spoken_summary_start key: %s", stored)
	}
	var start int
	if err := json.Unmarshal(raw, &start); err != nil || start != 7 {
		t.Fatalf("spoken_summary_start = %s, want 7", raw)
	}
}

// An offset of 0 is the common case (the summary IS the leading text), so it must be written,
// not dropped as a zero value -- the presence of a summary is what gates the key, not the offset.
func TestCommitVoiceTurn_PersistsZeroSpokenSummaryStart(t *testing.T) {
	turn := voiceAnswerTurn("Sum.")
	turn.SpokenSummaryStart = 0
	msg := commitVoiceAnswerTurn(t, turn)

	raw, ok := msg["spoken_summary_start"]
	if !ok {
		stored, _ := json.Marshal(msg)
		t.Fatalf("a zero offset must still be persisted beside the summary: %s", stored)
	}
	var start int
	if err := json.Unmarshal(raw, &start); err != nil || start != 0 {
		t.Fatalf("spoken_summary_start = %s, want 0", raw)
	}
}

// No summary, no offset: a no-tag turn carries neither key, exactly as the phase-1 shape.
func TestCommitVoiceTurn_OmitsSummaryStartWithoutSummary(t *testing.T) {
	turn := voiceAnswerTurn("")
	turn.SpokenSummaryStart = 5 // set but must be dropped: there is no summary to anchor it
	msg := commitVoiceAnswerTurn(t, turn)

	if raw, ok := msg["spoken_summary_start"]; ok {
		t.Fatalf("spoken_summary_start = %s, want the key omitted when there is no summary", raw)
	}
}
