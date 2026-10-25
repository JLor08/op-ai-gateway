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

// The portal's Voice Chat model picks (issue #317) live in settings.voice_chat of the schema-free
// chat document, written by the client's own save (PUT). PrepareChatRun rewrites the document's
// settings from the typed ChatRunSettings on every user-turn commit, which has no place for a key
// it does not know -- so unless it carries the stored key across, a text send wipes the picks, and
// the client can only write them back if the thread is still the one it has open when the run ends.
// The backend does not read the picks (the ticket carries the models and the co-load gate
// validates them); it only preserves the raw key, the way it lifts the pinned kind.

const storedVoiceChatPicks = `{"asr_model":"asr-b","tts_model":"tts-a"}`

// voiceChatThread is a text thread whose stored settings carry the given voice_chat value (raw
// JSON; "" for none), created the way the client does, by content.
func voiceChatThread(t *testing.T, svc *Service, voiceChat string) string {
	t.Helper()
	settings := `{}`
	if voiceChat != "" {
		settings = `{"model":"llama","voice_chat":` + voiceChat + `}`
	}
	created, err := svc.CreateChat(context.Background(), chatToken("usr_a"), CreateChatRequest{
		Content: json.RawMessage(`{"settings":` + settings + `,"messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func sendText(t *testing.T, svc *Service, chatID, id string, settings ChatRunSettings) ChatRunSettings {
	t.Helper()
	_, got, err := svc.PrepareChatRun(context.Background(), chatToken("usr_a"), chatID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"` + id + `","role":"user","content":"hi"}`),
		Settings:    settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func storedVoiceChat(t *testing.T, svc *Service, chatID string) string {
	t.Helper()
	got, err := svc.GetChat(context.Background(), chatToken("usr_a"), chatID)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		VoiceChat json.RawMessage `json:"voice_chat"`
	}
	if err := json.Unmarshal([]byte(storedSettings(t, got.Content)), &settings); err != nil {
		t.Fatal(err)
	}
	return string(settings.VoiceChat)
}

// THE POINT: a text send keeps the picks the thread had stored, whatever the run body carried.
func TestPrepareChatRunPreservesTheStoredVoiceChatPicks(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	chatID := voiceChatThread(t, svc, storedVoiceChatPicks)

	sendText(t, svc, chatID, "u1", ChatRunSettings{Model: "llama"})

	if got := storedVoiceChat(t, svc, chatID); got != storedVoiceChatPicks {
		t.Fatalf("voice_chat = %s, want the stored picks %s", got, storedVoiceChatPicks)
	}
}

// The backend does not read the picks, so it must not reshape them either: a key this build does
// not know, or a newer client's extra pick, survives byte for byte.
func TestPrepareChatRunPreservesTheVoiceChatPicksVerbatim(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	raw := `{"tts_model":"tts-a","a_newer_pick":{"x":[1,2]},"asr_model":"asr-b"}`
	chatID := voiceChatThread(t, svc, raw)

	sendText(t, svc, chatID, "u1", ChatRunSettings{Model: "llama"})

	if got := storedVoiceChat(t, svc, chatID); got != raw {
		t.Fatalf("voice_chat = %s, want it verbatim: %s", got, raw)
	}
}

// Every later send keeps them too, and so does the replay of an edited history (an edit or a
// regenerate), which rewrites the settings the same way.
func TestPrepareChatRunKeepsTheVoiceChatPicksAcrossSendsAndReplays(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	chatID := voiceChatThread(t, svc, storedVoiceChatPicks)

	sendText(t, svc, chatID, "u1", ChatRunSettings{Model: "llama"})
	sendText(t, svc, chatID, "u2", ChatRunSettings{Model: "llama"})
	if got := storedVoiceChat(t, svc, chatID); got != storedVoiceChatPicks {
		t.Fatalf("after the second send voice_chat = %s, want %s", got, storedVoiceChatPicks)
	}

	if _, _, err := svc.PrepareChatRun(context.Background(), chatToken("usr_a"), chatID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{json.RawMessage(`{"id":"u1","role":"user","content":"edited"}`)},
		Settings:      ChatRunSettings{Model: "llama"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := storedVoiceChat(t, svc, chatID); got != storedVoiceChatPicks {
		t.Fatalf("after an edited-history replay voice_chat = %s, want %s", got, storedVoiceChatPicks)
	}
}

// The picks are the stored document's, written by the client's own save: a run body (the POST
// body is ChatRunSettings verbatim, so this is reachable from the API) can neither replace them
// nor plant some on a thread that has none.
func TestPrepareChatRunTakesNoVoiceChatPicksFromTheRunBody(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	submitted := json.RawMessage(`{"asr_model":"evil","tts_model":"evil"}`)

	t.Run("does not replace the stored picks", func(t *testing.T) {
		chatID := voiceChatThread(t, svc, storedVoiceChatPicks)
		got := sendText(t, svc, chatID, "u1", ChatRunSettings{Model: "llama", VoiceChat: submitted})
		if string(got.VoiceChat) != storedVoiceChatPicks {
			t.Fatalf("returned VoiceChat = %s, want the stored picks", got.VoiceChat)
		}
		if stored := storedVoiceChat(t, svc, chatID); stored != storedVoiceChatPicks {
			t.Fatalf("voice_chat = %s, want the stored picks %s", stored, storedVoiceChatPicks)
		}
	})

	t.Run("does not plant picks on a thread that has none", func(t *testing.T) {
		chatID := voiceChatThread(t, svc, "")
		got := sendText(t, svc, chatID, "u1", ChatRunSettings{Model: "llama", VoiceChat: submitted})
		if len(got.VoiceChat) != 0 {
			t.Fatalf("returned VoiceChat = %s, want none", got.VoiceChat)
		}
		if stored := storedVoiceChat(t, svc, chatID); stored != "" {
			t.Fatalf("voice_chat = %s, want none", stored)
		}
	})
}

// A thread that never picked stays byte-identical: no voice_chat key appears -- not for an absent
// key, and not for a stored JSON null, which is not a pick either.
func TestPrepareChatRunOmitsVoiceChatWhenThereAreNoPicks(t *testing.T) {
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	for name, voiceChat := range map[string]string{"absent": "", "null": "null"} {
		t.Run(name, func(t *testing.T) {
			chatID := voiceChatThread(t, svc, voiceChat)
			sendText(t, svc, chatID, "u1", ChatRunSettings{Model: "llama"})
			got, err := svc.GetChat(context.Background(), chatToken("usr_a"), chatID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(storedSettings(t, got.Content), "voice_chat") {
				t.Fatalf("a thread with no picks must carry no voice_chat key at all: %s", storedSettings(t, got.Content))
			}
		})
	}
}
