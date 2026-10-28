// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/store"
	"testing"
)

// A voice answer carries one or more spoken sections (the <speak> regions the user hears); the
// thread must record WHERE each one sits in the full clean answer, so the portal can re-render it
// as its own collapsed block on reload. They ride the assistant message as an ordered
// `spoken_sections` array of the schema-free chat document, additional metadata beside the
// unchanged full answer in `content`. The phase-2 scalar spoken_summary / spoken_summary_start
// keys are no longer written.

const voiceFullAnswer = "Sum. Detail."

// voiceAnswerTurn is the assistant turn a voice run commits: the FULL answer text and one
// synthesized audio part, as [text, audio], with sections as the recorded spoken sections.
func voiceAnswerTurn(sections ...SpokenSection) AssistantTurn {
	text, _ := json.Marshal(map[string]string{"type": "text", "text": voiceFullAnswer})
	audio, _ := json.Marshal(map[string]any{
		"type":      "audio_url",
		"audio_url": map[string]string{"url": dataURL("audio/wav", wavLike(64))},
	})
	return AssistantTurn{
		Content:        voiceFullAnswer,
		ContentParts:   json.RawMessage("[" + string(text) + "," + string(audio) + "]"),
		AudioAnswer:    true,
		AudioStreamed:  true,
		SpokenSections: sections,
	}
}

// commitVoiceAnswerTurn commits one voice turn to a fresh in-memory thread and returns the stored
// assistant message as a raw key->value map, read back through GetChat the way a reload does.
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

// assertNoScalarSummaryKeys fails when the message carries either phase-2 scalar key: the writer
// emits only spoken_sections now, even for a single section.
func assertNoScalarSummaryKeys(t *testing.T, msg map[string]json.RawMessage) {
	t.Helper()
	for _, key := range []string{"spoken_summary", "spoken_summary_start"} {
		if raw, ok := msg[key]; ok {
			t.Fatalf("%s = %s, want the phase-2 scalar key no longer written", key, raw)
		}
	}
}

// Two sections persist as an ordered array of {text,start,len} spans (rune offsets into the full
// clean answer), in document order -- and the scalar phase-2 keys are not written.
func TestCommitVoiceTurn_PersistsSpokenSections(t *testing.T) {
	msg := commitVoiceAnswerTurn(t, voiceAnswerTurn(
		SpokenSection{Text: "Sum.", Start: 0, Len: 4},
		SpokenSection{Text: "Detail.", Start: 5, Len: 7},
	))

	raw, ok := msg["spoken_sections"]
	if !ok {
		stored, _ := json.Marshal(msg)
		t.Fatalf("assistant message has no spoken_sections key: %s", stored)
	}
	// The exact persisted shape, which the renderer and the run executor depend on.
	const want = `[{"text":"Sum.","start":0,"len":4},{"text":"Detail.","start":5,"len":7}]`
	if string(raw) != want {
		t.Fatalf("spoken_sections = %s, want %s", raw, want)
	}
	var got []SpokenSection
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("spoken_sections does not decode as []SpokenSection: %v", err)
	}
	if len(got) != 2 || got[0] != (SpokenSection{Text: "Sum.", Start: 0, Len: 4}) || got[1] != (SpokenSection{Text: "Detail.", Start: 5, Len: 7}) {
		t.Fatalf("spoken_sections = %+v, want the two sections in order", got)
	}
	assertNoScalarSummaryKeys(t, msg)
	// The sections are ADDITIONAL metadata: the text content stays the full clean answer.
	if text := textPartOf(t, msg["content"]); text != voiceFullAnswer {
		t.Fatalf("text content = %q, want the full answer %q", text, voiceFullAnswer)
	}
	// And the spans are exact: the runes at [start, start+len) are the section's text.
	runes := []rune(voiceFullAnswer)
	for i, sec := range got {
		if sec.Start+sec.Len > len(runes) || string(runes[sec.Start:sec.Start+sec.Len]) != sec.Text {
			t.Fatalf("section %d span [%d,%d) of %q does not equal its text %q", i, sec.Start, sec.Start+sec.Len, voiceFullAnswer, sec.Text)
		}
	}
}

// A single section is the N=1 case: a ONE-element spoken_sections array, never the phase-2
// scalar shape.
func TestCommitVoiceTurn_OneSectionIsAOneElementArray(t *testing.T) {
	msg := commitVoiceAnswerTurn(t, voiceAnswerTurn(SpokenSection{Text: "Sum.", Start: 0, Len: 4}))

	raw, ok := msg["spoken_sections"]
	if !ok {
		stored, _ := json.Marshal(msg)
		t.Fatalf("assistant message has no spoken_sections key: %s", stored)
	}
	if want := `[{"text":"Sum.","start":0,"len":4}]`; string(raw) != want {
		t.Fatalf("spoken_sections = %s, want %s", raw, want)
	}
	assertNoScalarSummaryKeys(t, msg)
}

// No sections (a no-tag fallback, a speech-failed turn): neither the array nor either scalar
// key is written, exactly the phase-1 shape.
func TestCommitVoiceTurn_NoSectionsOmitsEveryKey(t *testing.T) {
	msg := commitVoiceAnswerTurn(t, voiceAnswerTurn())

	if raw, ok := msg["spoken_sections"]; ok {
		t.Fatalf("spoken_sections = %s, want the key omitted for a no-section turn (the phase-1 shape)", raw)
	}
	assertNoScalarSummaryKeys(t, msg)
	if text := textPartOf(t, msg["content"]); text != voiceFullAnswer {
		t.Fatalf("text content = %q, want %q", text, voiceFullAnswer)
	}
}

// A start of 0 is the common case (the section IS the leading text), so it must be written,
// not dropped as a zero value -- the span is only meaningful as a pair.
func TestCommitVoiceTurn_PersistsZeroSectionStart(t *testing.T) {
	msg := commitVoiceAnswerTurn(t, voiceAnswerTurn(SpokenSection{Text: "Sum.", Start: 0, Len: 4}))

	var got []map[string]json.RawMessage
	if err := json.Unmarshal(msg["spoken_sections"], &got); err != nil || len(got) != 1 {
		t.Fatalf("spoken_sections = %s, want one element", msg["spoken_sections"])
	}
	if start, ok := got[0]["start"]; !ok || string(start) != "0" {
		t.Fatalf("start = %s (present %v), want an explicit 0", start, ok)
	}
}

// The text is optional beside the authoritative span: an empty text is dropped from the element
// (start and len stay).
func TestCommitVoiceTurn_SectionTextIsOptional(t *testing.T) {
	msg := commitVoiceAnswerTurn(t, voiceAnswerTurn(SpokenSection{Start: 5, Len: 7}))

	if want := `[{"start":5,"len":7}]`; string(msg["spoken_sections"]) != want {
		t.Fatalf("spoken_sections = %s, want %s", msg["spoken_sections"], want)
	}
}

// A turn committed before phase 3 carries the scalar spoken_summary + spoken_summary_start keys.
// Those are never rewritten: a later commit on the same thread leaves the old turn byte for byte
// as stored, so the renderer's fallback still finds them.
func TestCommitVoiceTurn_LeavesPhaseTwoTurnsReadable(t *testing.T) {
	ctx := context.Background()
	owner := chatToken("usr_a")
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	const oldTurn = `{"id":"msg_old","role":"assistant","status":"complete","content":[{"type":"text","text":"Sum. Detail."}],"spoken_summary":"Sum.","spoken_summary_start":0}`
	created, err := svc.CreateChat(ctx, owner, CreateChatRequest{Content: json.RawMessage(
		`{"messages":[{"id":"msg_u","role":"user","content":"hi"},` + oldTurn + `]}`)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	turn := voiceAnswerTurn(SpokenSection{Text: "Sum.", Start: 0, Len: 4})
	if err := svc.CommitVoiceTurn(ctx, owner, created.ID, "and now?", turn, "complete"); err != nil {
		t.Fatalf("CommitVoiceTurn: %v", err)
	}
	got, err := svc.GetChat(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	doc := parseChatDoc(got.Content)
	if len(doc.Messages) != 4 {
		t.Fatalf("stored %d messages, want the old pair and the new pair", len(doc.Messages))
	}
	var old, fresh map[string]json.RawMessage
	if err := json.Unmarshal(doc.Messages[1], &old); err != nil {
		t.Fatalf("old assistant message: %v", err)
	}
	if string(old["spoken_summary"]) != `"Sum."` || string(old["spoken_summary_start"]) != "0" {
		t.Fatalf("old turn lost its phase-2 keys: %s", doc.Messages[1])
	}
	if _, ok := old["spoken_sections"]; ok {
		t.Fatalf("old turn gained spoken_sections: %s", doc.Messages[1])
	}
	if err := json.Unmarshal(doc.Messages[3], &fresh); err != nil {
		t.Fatalf("new assistant message: %v", err)
	}
	if _, ok := fresh["spoken_sections"]; !ok {
		t.Fatalf("new turn has no spoken_sections: %s", doc.Messages[3])
	}
	assertNoScalarSummaryKeys(t, fresh)
}
