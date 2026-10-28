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
// then a voice turn with text and audio): the history the LLM leg is handed opens with the
// voice system prompt, then has no empty assistant content, alternates, keeps the voice turn's
// text and ends in the pending text.
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

	full, err := svc.VoiceRunHistory(context.Background(), owner, created.ID, "and three plus three")
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	assertSoleSystemMessage(t, full, defaultVoiceSystemPrompt)
	history := full[1:] // the conversation after the voice system prompt
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

// A voice run carries ONE system message: the voice system prompt the user stored in
// settings.voice_chat.system_prompt (or the built-in default when none is stored, or the stored
// value is blank), led by the thread's own system prompt (settings.system_prompt) when that is
// non-blank. The voice run reads both from the stored chat document, never from the ticket, so a
// client cannot plant one per request.

func TestVoiceSystemPrompt_DefaultWhenAbsent(t *testing.T) {
	for name, settings := range map[string]json.RawMessage{
		"nil settings":          nil,
		"empty object":          json.RawMessage(`{}`),
		"null":                  json.RawMessage(`null`),
		"voice_chat absent":     json.RawMessage(`{"model":"llama"}`),
		"voice_chat null":       json.RawMessage(`{"voice_chat":null}`),
		"voice_chat empty":      json.RawMessage(`{"voice_chat":{}}`),
		"prompt blank":          json.RawMessage(`{"voice_chat":{"system_prompt":"  "}}`),
		"prompt empty":          json.RawMessage(`{"voice_chat":{"system_prompt":""}}`),
		"prompt not a string":   json.RawMessage(`{"voice_chat":{"system_prompt":42}}`),
		"malformed settings":    json.RawMessage(`{"voice_chat":`),
		"picks only, no prompt": json.RawMessage(`{"voice_chat":` + storedVoiceChatPicks + `}`),
	} {
		if got := voiceSystemPrompt(settings); got != defaultVoiceSystemPrompt {
			t.Errorf("%s: voiceSystemPrompt = %q, want the default %q", name, got, defaultVoiceSystemPrompt)
		}
	}
}

func TestVoiceSystemPrompt_Override(t *testing.T) {
	for name, tc := range map[string]struct{ settings, want string }{
		"plain":                  {`{"voice_chat":{"system_prompt":"Be terse."}}`, "Be terse."},
		"trimmed":                {"{\"voice_chat\":{\"system_prompt\":\"  Be terse.\\n\"}}", "Be terse."},
		"multi-line is kept":     {`{"voice_chat":{"system_prompt":"Be terse.\nNo lists."}}`, "Be terse.\nNo lists."},
		"beside the model picks": {`{"model":"llama","voice_chat":{"asr_model":"asr-b","system_prompt":"Be terse."}}`, "Be terse."},
	} {
		if got := voiceSystemPrompt(json.RawMessage(tc.settings)); got != tc.want {
			t.Errorf("%s: voiceSystemPrompt = %q, want %q", name, got, tc.want)
		}
	}
}

// The built-in voice prompt describes the phase-3 behaviour the scanner relies on: spoken parts in
// their own <speak lang="..."> tags -- one for a short answer, several for a long one -- with the
// detail left outside the tags.
func TestDefaultVoiceSystemPromptAsksForSeveralSpokenSections(t *testing.T) {
	for _, want := range []string{
		"`<speak lang=\"…\">…</speak>`",
		"its own `<speak",
		"ONE section",
		"use several",
		"two-letter code",
		"outside the tags",
		"shown but not spoken",
	} {
		if !strings.Contains(defaultVoiceSystemPrompt, want) {
			t.Errorf("defaultVoiceSystemPrompt does not contain %q", want)
		}
	}
}

// The thread's own system prompt (settings.system_prompt: the persona/context) leads the voice
// prompt (the form), separated by one blank line, as the ONE system message. A blank, absent or
// mistyped thread prompt leaves the voice prompt alone, and a malformed top-level knob must not
// switch the voice prompt off.
func TestVoiceSystemPrompt_ComposesThreadAndVoicePrompts(t *testing.T) {
	for name, tc := range map[string]struct{ settings, want string }{
		"thread and voice":            {`{"system_prompt":"Persona.","voice_chat":{"system_prompt":"Voice."}}`, "Persona.\n\nVoice."},
		"thread only: voice default":  {`{"system_prompt":"Persona."}`, "Persona.\n\n" + defaultVoiceSystemPrompt},
		"thread beside picks only":    {`{"system_prompt":"Persona.","voice_chat":` + storedVoiceChatPicks + `}`, "Persona.\n\n" + defaultVoiceSystemPrompt},
		"thread, blank voice":         {`{"system_prompt":"Persona.","voice_chat":{"system_prompt":"  "}}`, "Persona.\n\n" + defaultVoiceSystemPrompt},
		"thread, mistyped voice":      {`{"system_prompt":"Persona.","voice_chat":{"system_prompt":42}}`, "Persona.\n\n" + defaultVoiceSystemPrompt},
		"both are trimmed":            {"{\"system_prompt\":\"  Persona.\\n\",\"voice_chat\":{\"system_prompt\":\"\\nVoice.  \"}}", "Persona.\n\nVoice."},
		"multi-line thread is kept":   {`{"system_prompt":"You are Ada.\nBe kind.","voice_chat":{"system_prompt":"Voice."}}`, "You are Ada.\nBe kind.\n\nVoice."},
		"voice only":                  {`{"voice_chat":{"system_prompt":"Voice."}}`, "Voice."},
		"neither":                     {`{"model":"llama"}`, defaultVoiceSystemPrompt},
		"blank thread, voice":         {`{"system_prompt":"   ","voice_chat":{"system_prompt":"Voice."}}`, "Voice."},
		"empty thread, no voice":      {`{"system_prompt":""}`, defaultVoiceSystemPrompt},
		"null thread":                 {`{"system_prompt":null,"voice_chat":{"system_prompt":"Voice."}}`, "Voice."},
		"mistyped thread, voice kept": {`{"system_prompt":42,"voice_chat":{"system_prompt":"Voice."}}`, "Voice."},
		"mistyped thread, no voice":   {`{"system_prompt":["x"]}`, defaultVoiceSystemPrompt},
	} {
		if got := voiceSystemPrompt(json.RawMessage(tc.settings)); got != tc.want {
			t.Errorf("%s: voiceSystemPrompt = %q, want %q", name, got, tc.want)
		}
	}
}

// voiceHistoryThread creates a chat whose stored settings are the given raw JSON and whose
// messages are a one-question exchange, the way the client creates one, by content.
func voiceHistoryThread(t *testing.T, svc *Service, settings string) string {
	t.Helper()
	created, err := svc.CreateChat(context.Background(), chatToken("usr_a"), CreateChatRequest{Content: json.RawMessage(`{"settings":` + settings + `,"messages":[
		{"id":"u1","role":"user","content":"what is two plus two"},
		{"id":"a1","role":"assistant","content":"four","status":"complete"}
	]}`)})
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// assertSoleSystemMessage pins that history opens with the one system message, whose string
// content is want, and that no other message in it has the system role.
func assertSoleSystemMessage(t *testing.T, history []ChatAPIMessage, want string) {
	t.Helper()
	if len(history) == 0 || history[0].Role != "system" {
		t.Fatalf("history[0] is not the system message: %+v", history)
	}
	var got string
	if err := json.Unmarshal(history[0].Content, &got); err != nil {
		t.Fatalf("the system message content is not a string: %s", history[0].Content)
	}
	if got != want {
		t.Errorf("system message = %q, want %q", got, want)
	}
	for i, m := range history[1:] {
		if m.Role == "system" {
			t.Fatalf("history[%d] is a second system message: %+v", i+1, history)
		}
	}
}

func TestVoiceRunHistory_InjectsSystemMessage(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	chatID := voiceHistoryThread(t, svc, `{"voice_chat":{"system_prompt":"Be terse."}}`)

	history, err := svc.VoiceRunHistory(context.Background(), chatToken("usr_a"), chatID, "and three plus three")
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	assertSoleSystemMessage(t, history, "Be terse.")
	if r := voiceHistoryRoles(history); r != "system,user,assistant,user" {
		t.Fatalf("roles = %s, want system,user,assistant,user", r)
	}
	if !strings.Contains(string(history[len(history)-1].Content), "and three plus three") {
		t.Fatalf("the last message must be the pending text: %s", history[len(history)-1].Content)
	}
}

func TestVoiceRunHistory_DefaultSystemMessage(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	for name, settings := range map[string]string{
		"no voice_chat":    `{}`,
		"blank prompt":     `{"voice_chat":{"system_prompt":"   "}}`,
		"picks, no prompt": `{"voice_chat":` + storedVoiceChatPicks + `}`,
	} {
		chatID := voiceHistoryThread(t, svc, settings)
		history, err := svc.VoiceRunHistory(context.Background(), chatToken("usr_a"), chatID, "and three plus three")
		if err != nil {
			t.Fatalf("%s: VoiceRunHistory: %v", name, err)
		}
		assertSoleSystemMessage(t, history, defaultVoiceSystemPrompt)
	}
}

// The thread's own (typed-chat) system prompt, stored in settings.system_prompt, is the PERSONA
// layer of a voice run: the run's one system message is that prompt, a blank line, then the voice
// prompt (the stored override, else the built-in default). It is COMBINED into that one message,
// never emitted as a second system message, and a thread without one is unchanged.
func TestVoiceRunHistory_ThreadSystemPromptLeadsTheVoicePrompt(t *testing.T) {
	const threadPrompt = "THREAD-ONLY: answer like a pirate."
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})

	for name, tc := range map[string]struct{ settings, want string }{
		"voice override beside the thread prompt": {
			`{"system_prompt":"` + threadPrompt + `","voice_chat":{"system_prompt":"Be terse."}}`, threadPrompt + "\n\nBe terse.",
		},
		"voice default (picks only) beside the thread prompt": {
			`{"system_prompt":"` + threadPrompt + `","voice_chat":` + storedVoiceChatPicks + `}`, threadPrompt + "\n\n" + defaultVoiceSystemPrompt,
		},
		"voice default (blank override) beside the thread prompt": {
			`{"system_prompt":"` + threadPrompt + `","voice_chat":{"system_prompt":"  "}}`, threadPrompt + "\n\n" + defaultVoiceSystemPrompt,
		},
		"thread prompt alone, no voice_chat at all": {
			`{"system_prompt":"` + threadPrompt + `"}`, threadPrompt + "\n\n" + defaultVoiceSystemPrompt,
		},
		"blank thread prompt beside a voice override": {
			`{"system_prompt":"   ","voice_chat":{"system_prompt":"Be terse."}}`, "Be terse.",
		},
		"no thread prompt: the voice prompt alone": {
			`{"voice_chat":{"system_prompt":"Be terse."}}`, "Be terse.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			chatID := voiceHistoryThread(t, svc, tc.settings)
			history, err := svc.VoiceRunHistory(context.Background(), chatToken("usr_a"), chatID, "and three plus three")
			if err != nil {
				t.Fatalf("VoiceRunHistory: %v", err)
			}
			// ONE system message, the composed text (and no second one anywhere in the history).
			assertSoleSystemMessage(t, history, tc.want)
			// Combined into that message, not folded into any other message of the run.
			for i, m := range history[1:] {
				if strings.Contains(string(m.Content), threadPrompt) {
					t.Fatalf("history[%d] carries the thread prompt outside the system message: %s", i+1, m.Content)
				}
			}
		})
	}

	// Typed runs are unchanged: the same stored prompt is a typed run's ONE system message ON ITS
	// OWN (no voice prompt joins it), so the combination above is the voice run's alone and the
	// assertions are not vacuously true of a prompt no run ever reads.
	t.Run("control: a typed run carries the thread prompt alone", func(t *testing.T) {
		created, err := svc.CreateChat(context.Background(), chatToken("usr_a"), CreateChatRequest{Content: json.RawMessage(`{"settings":{"system_prompt":"` + threadPrompt + `"},"messages":[]}`)})
		if err != nil {
			t.Fatal(err)
		}
		history, _, err := svc.PrepareChatRun(context.Background(), chatToken("usr_a"), created.ID, PrepareRunRequest{
			UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hello"}`),
			Settings:    ChatRunSettings{Model: "m", SystemPrompt: threadPrompt},
		})
		if err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
		assertSoleSystemMessage(t, history, threadPrompt)
	})
}
