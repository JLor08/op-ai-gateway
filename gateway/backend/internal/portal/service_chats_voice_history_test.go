// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// A voice run's LLM history is the stored thread with the prior assistant turns' audio
// stripped (stripVoiceAssistantAudio). A TTS-only speech-run answer in a MIXED thread is
// audio and nothing else, so stripping it leaves no content: the turn must be OMITTED, never
// sent as an empty assistant content (a strict upstream, native Anthropic, rejects that on a
// non-final message) -- and the omission must not leave two same-role turns adjacent, which a
// strict-alternation chat template rejects.

const (
	audioURLPart     = `{"type":"audio_url","audio_url":{"url":"/api/portal/chat-blobs/blb_x"}}`
	audioRemovedPart = `{"type":"audio_removed"}`
)

func voiceHistoryMsg(role, content string) ChatAPIMessage {
	return ChatAPIMessage{Role: role, Content: json.RawMessage(content)}
}

// voiceHistoryRoles is the role sequence of a history, e.g. "user,assistant,user".
func voiceHistoryRoles(history []ChatAPIMessage) string {
	out := make([]string, 0, len(history))
	for _, m := range history {
		out = append(out, m.Role)
	}
	return strings.Join(out, ",")
}

// assertValidVoiceHistory pins the two properties a voice history must have for every
// upstream: no assistant message with empty content, and no two adjacent same-role messages.
func assertValidVoiceHistory(t *testing.T, history []ChatAPIMessage) {
	t.Helper()
	for i, m := range history {
		if m.Role == "assistant" {
			trimmed := strings.TrimSpace(string(m.Content))
			if trimmed == "" || trimmed == `""` || trimmed == "[]" || trimmed == "null" {
				t.Fatalf("history[%d] is an assistant message with empty content %q: %+v", i, m.Content, history)
			}
		}
		if i > 0 && history[i-1].Role == m.Role {
			t.Fatalf("history[%d] and history[%d] are both %q: roles must alternate: %+v", i-1, i, m.Role, history)
		}
	}
}

func TestStripVoiceAssistantAudioOmitsAnAudioOnlyAssistantTurn(t *testing.T) {
	for name, audioOnly := range map[string]string{
		"audio_url only":     `[` + audioURLPart + `]`,
		"audio_removed only": `[` + audioRemovedPart + `]`,
		"both audio markers": `[` + audioURLPart + `,` + audioRemovedPart + `]`,
	} {
		t.Run(name, func(t *testing.T) {
			history := []ChatAPIMessage{
				voiceHistoryMsg("user", `"say hello"`),
				voiceHistoryMsg("assistant", audioOnly),
				voiceHistoryMsg("user", `"and now a question"`),
				voiceHistoryMsg("assistant", `"an answer"`),
				voiceHistoryMsg("user", `"pending"`),
			}
			got := stripVoiceAssistantAudio(history)
			assertValidVoiceHistory(t, got)
			if r := voiceHistoryRoles(got); r != "user,assistant,user" {
				t.Fatalf("roles = %s, want user,assistant,user (the audio-only turn omitted, its user neighbours merged)", r)
			}
			var merged string
			if err := json.Unmarshal(got[0].Content, &merged); err != nil {
				t.Fatalf("merged user content is not a string: %s", got[0].Content)
			}
			if merged != "say hello\n\nand now a question" {
				t.Fatalf("merged user content = %q, want both user turns kept in order", merged)
			}
			if string(got[1].Content) != `"an answer"` {
				t.Fatalf("the plain-string assistant turn changed: %s", got[1].Content)
			}
		})
	}
}

// A normal voice turn -- text AND audio -- keeps its text; only the audio part goes.
func TestStripVoiceAssistantAudioKeepsTheTextOfATextAndAudioTurn(t *testing.T) {
	history := []ChatAPIMessage{
		voiceHistoryMsg("user", `"first"`),
		voiceHistoryMsg("assistant", `[{"type":"text","text":"the answer"},`+audioURLPart+`]`),
		voiceHistoryMsg("user", `"second"`),
	}
	got := stripVoiceAssistantAudio(history)
	assertValidVoiceHistory(t, got)
	if r := voiceHistoryRoles(got); r != "user,assistant,user" {
		t.Fatalf("roles = %s, want the thread untouched", r)
	}
	if string(got[1].Content) != `[{"type":"text","text":"the answer"}]` {
		t.Fatalf("assistant content = %s, want the text part alone", got[1].Content)
	}
	if strings.Contains(string(got[1].Content), "audio") {
		t.Fatalf("an audio part survived: %s", got[1].Content)
	}
}

// The tail case: the thread ends in an audio-only assistant turn, so the pending user text
// (appended last) lands next to the user turn BEFORE it and must be merged into it.
func TestStripVoiceAssistantAudioMergesThePendingTurnAfterATrailingAudioOnlyTurn(t *testing.T) {
	history := []ChatAPIMessage{
		voiceHistoryMsg("user", `"say hello"`),
		voiceHistoryMsg("assistant", `[`+audioURLPart+`]`),
		voiceHistoryMsg("user", `"thanks, now tell me the weather"`),
	}
	got := stripVoiceAssistantAudio(history)
	assertValidVoiceHistory(t, got)
	if r := voiceHistoryRoles(got); r != "user" {
		t.Fatalf("roles = %s, want a single merged user turn", r)
	}
	if !strings.Contains(string(got[0].Content), "say hello") || !strings.Contains(string(got[0].Content), "tell me the weather") {
		t.Fatalf("the merged turn lost a message: %s", got[0].Content)
	}
}

// A leading audio-only assistant turn is simply dropped: nothing precedes it to merge into.
func TestStripVoiceAssistantAudioDropsALeadingAudioOnlyTurn(t *testing.T) {
	got := stripVoiceAssistantAudio([]ChatAPIMessage{
		voiceHistoryMsg("assistant", `[`+audioURLPart+`]`),
		voiceHistoryMsg("user", `"pending"`),
	})
	assertValidVoiceHistory(t, got)
	if r := voiceHistoryRoles(got); r != "user" || string(got[0].Content) != `"pending"` {
		t.Fatalf("got %s %+v, want the pending user turn alone", r, got)
	}
}

// Merging keeps images: a user turn with image parts merges with a text turn as a parts array.
func TestStripVoiceAssistantAudioMergeKeepsImageParts(t *testing.T) {
	image := `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`
	got := stripVoiceAssistantAudio([]ChatAPIMessage{
		voiceHistoryMsg("user", `[{"type":"text","text":"look at this"},`+image+`]`),
		voiceHistoryMsg("assistant", `[`+audioRemovedPart+`]`),
		voiceHistoryMsg("user", `"pending"`),
	})
	assertValidVoiceHistory(t, got)
	if len(got) != 1 {
		t.Fatalf("got %d messages, want one merged user turn: %+v", len(got), got)
	}
	var parts []map[string]any
	if err := json.Unmarshal(got[0].Content, &parts); err != nil {
		t.Fatalf("merged content is not a parts array: %s", got[0].Content)
	}
	if len(parts) != 3 || parts[1]["type"] != "image_url" || parts[2]["text"] != "pending" {
		t.Fatalf("merged parts = %s, want [text, image, pending text]", got[0].Content)
	}
}

// Only turns the omission made adjacent are merged: a thread whose same-role turns were
// ALREADY adjacent when stored is left exactly as stored, and the input is never mutated.
func TestStripVoiceAssistantAudioLeavesStoredAdjacencyAndInputAlone(t *testing.T) {
	history := []ChatAPIMessage{
		voiceHistoryMsg("user", `"one"`),
		voiceHistoryMsg("user", `"two"`),
		voiceHistoryMsg("assistant", `[{"type":"text","text":"a"},`+audioURLPart+`]`),
	}
	before := append([]ChatAPIMessage(nil), history...)
	for i := range before {
		before[i].Content = append(json.RawMessage(nil), history[i].Content...)
	}
	got := stripVoiceAssistantAudio(history)
	if r := voiceHistoryRoles(got); r != "user,user,assistant" {
		t.Fatalf("roles = %s, want the stored adjacency kept (nothing was omitted)", r)
	}
	for i := range history {
		if string(history[i].Content) != string(before[i].Content) {
			t.Fatalf("the input history[%d] was mutated: %s -> %s", i, before[i].Content, history[i].Content)
		}
	}
}

// An un-mergeable pair (content that is neither a string nor a parts array) is kept as two
// turns rather than losing one.
func TestStripVoiceAssistantAudioNeverLosesAnUnmergeableTurn(t *testing.T) {
	got := stripVoiceAssistantAudio([]ChatAPIMessage{
		voiceHistoryMsg("user", `{"weird":true}`),
		voiceHistoryMsg("assistant", `[`+audioURLPart+`]`),
		voiceHistoryMsg("user", `"pending"`),
	})
	if len(got) != 2 {
		t.Fatalf("got %d messages, want both user turns kept: %+v", len(got), got)
	}
}

// End to end through VoiceRunHistory over a MIXED thread (a speech-run turn with audio only,
// then a voice turn with text and audio): the history the LLM leg is handed has no empty
// assistant content, alternates, keeps the voice turn's text and ends in the pending text.
func TestVoiceRunHistoryOmitsAnAudioOnlyAssistantTurnInAMixedThread(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, err := svc.CreateChat(context.Background(), owner, CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[
		{"id":"u1","role":"user","content":"say hello"},
		{"id":"a1","role":"assistant","content":[` + audioRemovedPart + `],"status":"complete"},
		{"id":"u2","role":"user","content":"what is two plus two"},
		{"id":"a2","role":"assistant","content":[{"type":"text","text":"four"},` + audioRemovedPart + `],"status":"complete"}
	]}`)})
	if err != nil {
		t.Fatal(err)
	}

	history, err := svc.VoiceRunHistory(context.Background(), owner, created.ID, "and three plus three")
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	assertValidVoiceHistory(t, history)
	if r := voiceHistoryRoles(history); r != "user,assistant,user" {
		t.Fatalf("roles = %s, want user,assistant,user", r)
	}
	if !strings.Contains(string(history[0].Content), "say hello") || !strings.Contains(string(history[0].Content), "what is two plus two") {
		t.Fatalf("the merged first user turn lost a message: %s", history[0].Content)
	}
	if !strings.Contains(string(history[1].Content), "four") {
		t.Fatalf("the voice turn's text was dropped: %s", history[1].Content)
	}
	if !strings.Contains(string(history[2].Content), "and three plus three") {
		t.Fatalf("the last message must be the pending text: %s", history[2].Content)
	}
	if body, _ := json.Marshal(history); strings.Contains(string(body), "audio") {
		t.Fatalf("an audio part reached the LLM history: %s", body)
	}
}
