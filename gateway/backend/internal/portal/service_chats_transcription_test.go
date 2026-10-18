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

// transcriptionSettings is the transcription settings a transcription-thread
// test submits: every field set, so a persistence/round-trip assertion exercises
// the whole carrier.
func transcriptionSettings() *ChatTranscriptionSettings {
	temperature := 0.2
	return &ChatTranscriptionSettings{
		Language:    "de",
		Prompt:      "Names: Jürgen, Kubernetes",
		Temperature: &temperature,
		Passthrough: map[string]json.RawMessage{"timestamp_granularities": json.RawMessage(`["word"]`)},
	}
}

// The first send establishes the transcription kind and it is persisted,
// together with the thread's transcription settings (a transcription thread's
// analog of the speech kind).
func TestPrepareChatRunPinsTheTranscriptionKindOnFirstSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"transcribe this"}`),
		Settings:    ChatRunSettings{Model: "whisper", Kind: chatKindTranscription, Transcription: transcriptionSettings()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != chatKindTranscription {
		t.Fatalf("Kind = %q, want transcription", settings.Kind)
	}
	if settings.Transcription == nil || settings.Transcription.Language != "de" ||
		settings.Transcription.Temperature == nil || *settings.Transcription.Temperature != 0.2 {
		t.Fatalf("transcription settings not returned: %+v", settings.Transcription)
	}
	if got := string(settings.Transcription.Passthrough["timestamp_granularities"]); got != `["word"]` {
		t.Fatalf("passthrough not returned: %q", got)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"transcription"`) {
		t.Fatalf("kind not persisted: %s", got.Content)
	}
	for _, want := range []string{`"transcription":{`, `"language":"de"`, `"temperature":0.2`, `"passthrough":{"timestamp_granularities":["word"]}`} {
		if !strings.Contains(string(got.Content), want) {
			t.Fatalf("transcription settings not persisted (missing %s): %s", want, got.Content)
		}
	}
}

// A later send cannot re-pin a transcription thread, however the client submits
// the kind (ChatRunSettings is the POST body verbatim), and its transcription
// settings stay per-send: the later send replaces them, like the audio and image
// knobs do.
func TestPrepareChatRunKeepsTheTranscriptionPinAndUpdatesSettingsOnALaterSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hello"}`),
		Settings:    ChatRunSettings{Model: "whisper", Kind: chatKindTranscription, Transcription: transcriptionSettings()},
	}); err != nil {
		t.Fatal(err)
	}

	// Second send submits an empty kind (a client that dropped it) and new
	// transcription settings; the stored pin must win, the settings must replace.
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"again"}`),
		Settings:    ChatRunSettings{Model: "whisper", Kind: "", Transcription: &ChatTranscriptionSettings{Language: "en"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != chatKindTranscription {
		t.Fatalf("Kind = %q, want the pinned transcription -- a later send must not unpin it", settings.Kind)
	}
	if settings.Transcription == nil || settings.Transcription.Language != "en" ||
		settings.Transcription.Prompt != "" || settings.Transcription.Temperature != nil || len(settings.Transcription.Passthrough) != 0 {
		t.Fatalf("transcription settings not replaced on the later send: %+v", settings.Transcription)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"transcription"`) {
		t.Fatalf("the pin was lost from the document: %s", got.Content)
	}
	if !strings.Contains(string(got.Content), `"language":"en"`) || strings.Contains(string(got.Content), `"language":"de"`) ||
		strings.Contains(string(got.Content), "Kubernetes") || strings.Contains(string(got.Content), "timestamp_granularities") {
		t.Fatalf("the later send's transcription settings must replace the first's: %s", got.Content)
	}
}

// A text-pinned thread must not be flippable to transcription by a later send,
// and a transcription-pinned thread must not be flippable to any other kind (nor
// the speech and image kinds to transcription): the pin is forced from the
// stored kind whenever the thread has prior messages.
func TestPrepareChatRunRefusesToRepinAcrossTranscription(t *testing.T) {
	cases := []struct {
		name      string
		firstKind string
		laterKind string
		wantKind  string
	}{
		{"text to transcription", "", chatKindTranscription, ""},
		{"transcription to text", chatKindTranscription, "", chatKindTranscription},
		{"transcription to speech", chatKindTranscription, chatKindSpeech, chatKindTranscription},
		{"speech to transcription", chatKindSpeech, chatKindTranscription, chatKindSpeech},
		{"transcription to image", chatKindTranscription, chatKindImage, chatKindTranscription},
		{"image to transcription", chatKindImage, chatKindTranscription, chatKindImage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
			owner := chatToken("usr_a")
			created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
				Content: json.RawMessage(`{"settings":{},"messages":[]}`),
			})
			if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
				UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"first"}`),
				Settings:    ChatRunSettings{Model: "m", Kind: tc.firstKind},
			}); err != nil {
				t.Fatal(err)
			}
			_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
				UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"second"}`),
				Settings:    ChatRunSettings{Model: "m", Kind: tc.laterKind, Transcription: transcriptionSettings()},
			})
			if err != nil {
				t.Fatal(err)
			}
			if settings.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want the stored pin %q", settings.Kind, tc.wantKind)
			}
			// The pin decides which settings survive: a thread that is not
			// transcription must drop the transcription settings the raw client
			// submitted.
			if (settings.Transcription != nil) != (tc.wantKind == chatKindTranscription) {
				t.Fatalf("Transcription = %+v with kind %q", settings.Transcription, settings.Kind)
			}
		})
	}
}

// A text, an image and a speech thread must carry no "transcription" key -- the
// byte-identical guarantee the kind, image and audio tests pin, now for the
// transcription settings. Even a raw client that submits transcription settings
// on such a thread must not pollute the stored document: PrepareChatRun clears
// them for any non-transcription kind. For each kind the persisted settings are
// compared to those of a thread that never submitted any, byte for byte.
func TestPrepareChatRunOmitsTranscriptionSettingsForOtherKinds(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")

	cases := []struct {
		name string
		kind string
		// carrier is the kind's own settings carrier, so the thread under test
		// is a realistic one rather than an empty shell.
		carrier func(*ChatRunSettings)
		// forbidden are keys that must be absent from the stored settings.
		forbidden []string
	}{
		{"text", "", func(*ChatRunSettings) {}, []string{"kind", "audio", "image"}},
		{"image", chatKindImage, func(s *ChatRunSettings) { s.Image = &ChatImageSettings{Size: "512x512"} }, []string{"audio"}},
		{"speech", chatKindSpeech, func(s *ChatRunSettings) { s.Audio = speechAudio() }, []string{"image"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			send := func(transcription *ChatTranscriptionSettings) (ChatRunSettings, string) {
				t.Helper()
				created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
					Content: json.RawMessage(`{"settings":{},"messages":[]}`),
				})
				settings := ChatRunSettings{Model: "m", Kind: tc.kind, Transcription: transcription}
				tc.carrier(&settings)
				_, got, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
					UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
					Settings:    settings,
				})
				if err != nil {
					t.Fatal(err)
				}
				stored, _ := svc.GetChat(ctx, owner, created.ID)
				return got, storedSettings(t, stored.Content)
			}

			settings, polluted := send(transcriptionSettings())
			if settings.Transcription != nil {
				t.Fatalf("transcription settings must be cleared on a %s thread, got %+v", tc.name, settings.Transcription)
			}
			_, clean := send(nil)
			if polluted != clean {
				t.Fatalf("a %s thread's settings must be byte-identical:\n with transcription: %s\n without:           %s", tc.name, polluted, clean)
			}
			for _, key := range append([]string{"transcription", "language"}, tc.forbidden...) {
				if strings.Contains(clean, `"`+key+`"`) {
					t.Fatalf("a %s thread must carry no %q key at all: %s", tc.name, key, clean)
				}
			}
		})
	}
}

// A transcription thread carries no audio or image settings: each kind's carrier
// is cleared for every other kind, so a raw client cannot park the wrong knobs in
// a thread's document.
func TestPrepareChatRunTranscriptionClearsTheOtherKindsSettings(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
		Settings: ChatRunSettings{
			Model: "whisper", Kind: chatKindTranscription,
			Transcription: transcriptionSettings(), Audio: speechAudio(), Image: &ChatImageSettings{Size: "512x512"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Audio != nil {
		t.Fatalf("audio settings must be cleared on a transcription thread, got %+v", settings.Audio)
	}
	if settings.Image != nil {
		t.Fatalf("image settings must be cleared on a transcription thread, got %+v", settings.Image)
	}
	if settings.Transcription == nil {
		t.Fatal("transcription settings must survive on a transcription thread")
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	stored := storedSettings(t, got.Content)
	for _, key := range []string{"audio", "image"} {
		if strings.Contains(stored, `"`+key+`"`) {
			t.Fatalf("a transcription thread must carry no %s key: %s", key, stored)
		}
	}
}

// ChatTranscriptionSettings' wire shape: only the set knobs are emitted, an
// explicit temperature of 0 survives (the field is a pointer, so 0 is told apart
// from unset), the passthrough round-trips its raw JSON values untouched, and the
// carrier is absent from a ChatRunSettings that has none -- which is what keeps
// text/image/speech settings byte-identical.
func TestChatTranscriptionSettingsWireShape(t *testing.T) {
	b, err := json.Marshal(transcriptionSettings())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"language":"de","prompt":"Names: Jürgen, Kubernetes","temperature":0.2,"passthrough":{"timestamp_granularities":["word"]}}`; string(b) != want {
		t.Fatalf("marshal = %s, want %s", b, want)
	}

	zero := 0.0
	if explicit, _ := json.Marshal(ChatTranscriptionSettings{Temperature: &zero}); string(explicit) != `{"temperature":0}` {
		t.Fatalf("an explicit temperature of 0 must be emitted, got %s", explicit)
	}

	var back ChatTranscriptionSettings
	if err := json.Unmarshal([]byte(`{"language":"fr","passthrough":{"n":3,"o":{"a":[1,2]},"s":"x"}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Language != "fr" || back.Prompt != "" || back.Temperature != nil {
		t.Fatalf("unmarshal = %+v", back)
	}
	for k, want := range map[string]string{"n": `3`, "o": `{"a":[1,2]}`, "s": `"x"`} {
		if got := string(back.Passthrough[k]); got != want {
			t.Fatalf("passthrough[%s] = %s, want %s", k, got, want)
		}
	}

	if empty, _ := json.Marshal(ChatTranscriptionSettings{}); string(empty) != `{}` {
		t.Fatalf("an unset ChatTranscriptionSettings must marshal to {}, got %s", empty)
	}
	if none, _ := json.Marshal(ChatRunSettings{Model: "m"}); strings.Contains(string(none), "transcription") {
		t.Fatalf("a ChatRunSettings without transcription must carry no transcription key: %s", none)
	}
}
