// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/store"
	"strconv"
	"strings"
	"testing"
)

// speechAudio is the audio settings a speech-thread test submits: every field
// set, so a persistence/round-trip assertion exercises the whole carrier.
func speechAudio() *ChatAudioSettings {
	speed := 1.25
	return &ChatAudioSettings{
		Voice:       "af_bella",
		Speed:       &speed,
		Passthrough: map[string]json.RawMessage{"instructions": json.RawMessage(`"calm and slow"`)},
	}
}

// storedSettings returns the raw "settings" object of a chat's stored document.
func storedSettings(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var doc struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	return string(doc.Settings)
}

// The first send establishes the speech kind and it is persisted, together with
// the thread's audio settings (a speech thread's analog of the image kind).
func TestPrepareChatRunPinsTheSpeechKindOnFirstSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"read this aloud"}`),
		Settings:    ChatRunSettings{Model: "kokoro", Kind: chatKindSpeech, Audio: speechAudio()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != chatKindSpeech {
		t.Fatalf("Kind = %q, want speech", settings.Kind)
	}
	if settings.Audio == nil || settings.Audio.Voice != "af_bella" || settings.Audio.Speed == nil || *settings.Audio.Speed != 1.25 {
		t.Fatalf("audio settings not returned: %+v", settings.Audio)
	}
	if got := string(settings.Audio.Passthrough["instructions"]); got != `"calm and slow"` {
		t.Fatalf("passthrough not returned: %q", got)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"speech"`) {
		t.Fatalf("kind not persisted: %s", got.Content)
	}
	for _, want := range []string{`"audio":{`, `"voice":"af_bella"`, `"speed":1.25`, `"passthrough":{"instructions":"calm and slow"}`} {
		if !strings.Contains(string(got.Content), want) {
			t.Fatalf("audio settings not persisted (missing %s): %s", want, got.Content)
		}
	}
}

// A later send cannot re-pin a speech thread, however the client submits the
// kind (ChatRunSettings is the POST body verbatim), and its audio settings stay
// per-send: the later send replaces them, like the image knobs do.
func TestPrepareChatRunKeepsTheSpeechPinAndUpdatesAudioOnALaterSend(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
		Content: json.RawMessage(`{"settings":{},"messages":[]}`),
	})

	if _, _, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hello"}`),
		Settings:    ChatRunSettings{Model: "kokoro", Kind: chatKindSpeech, Audio: speechAudio()},
	}); err != nil {
		t.Fatal(err)
	}

	// Second send submits an empty kind (a client that dropped it) and new audio
	// settings; the stored speech pin must win, the audio settings must replace.
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"again"}`),
		Settings:    ChatRunSettings{Model: "kokoro", Kind: "", Audio: &ChatAudioSettings{Voice: "am_adam"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Kind != chatKindSpeech {
		t.Fatalf("Kind = %q, want the pinned speech -- a later send must not unpin it", settings.Kind)
	}
	if settings.Audio == nil || settings.Audio.Voice != "am_adam" || settings.Audio.Speed != nil || len(settings.Audio.Passthrough) != 0 {
		t.Fatalf("audio settings not replaced on the later send: %+v", settings.Audio)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"kind":"speech"`) {
		t.Fatalf("the pin was lost from the document: %s", got.Content)
	}
	if !strings.Contains(string(got.Content), `"am_adam"`) || strings.Contains(string(got.Content), "af_bella") || strings.Contains(string(got.Content), "calm and slow") {
		t.Fatalf("the later send's audio settings must replace the first's: %s", got.Content)
	}
}

// A text-pinned thread must not be flippable to speech by a later send, and a
// speech-pinned or image-pinned thread must not be flippable to any other kind:
// the pin is forced from the stored kind whenever the thread has prior messages.
func TestPrepareChatRunRefusesToRepinAcrossSpeech(t *testing.T) {
	cases := []struct {
		name      string
		firstKind string
		laterKind string
		wantKind  string
	}{
		{"text to speech", "", chatKindSpeech, ""},
		{"speech to text", chatKindSpeech, "", chatKindSpeech},
		{"speech to image", chatKindSpeech, chatKindImage, chatKindSpeech},
		{"image to speech", chatKindImage, chatKindSpeech, chatKindImage},
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
				Settings:    ChatRunSettings{Model: "m", Kind: tc.laterKind, Audio: speechAudio()},
			})
			if err != nil {
				t.Fatal(err)
			}
			if settings.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want the stored pin %q", settings.Kind, tc.wantKind)
			}
			// The pin decides which settings survive: a thread that is not speech
			// must drop the audio settings the raw client submitted.
			if (settings.Audio != nil) != (tc.wantKind == chatKindSpeech) {
				t.Fatalf("Audio = %+v with kind %q", settings.Audio, settings.Kind)
			}
		})
	}
}

// A text thread must carry no "audio" key -- the byte-identical guarantee the
// kind and image tests pin, now for the audio settings. Even a raw client that
// submits audio settings on a text thread must not pollute the stored document:
// PrepareChatRun clears them for any non-speech kind. The persisted settings are
// compared to those of a thread that never submitted any, byte for byte.
func TestPrepareChatRunOmitsAudioSettingsForATextThread(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	send := func(audio *ChatAudioSettings) (ChatRunSettings, string) {
		t.Helper()
		created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
			Content: json.RawMessage(`{"settings":{},"messages":[]}`),
		})
		_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
			UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
			Settings:    ChatRunSettings{Model: "llama", Audio: audio},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := svc.GetChat(ctx, owner, created.ID)
		return settings, storedSettings(t, got.Content)
	}

	settings, polluted := send(speechAudio())
	if settings.Audio != nil {
		t.Fatalf("audio settings must be cleared on a text thread, got %+v", settings.Audio)
	}
	_, clean := send(nil)
	if polluted != clean {
		t.Fatalf("a text thread's settings must be byte-identical:\n with audio: %s\n without:    %s", polluted, clean)
	}
	for _, key := range []string{"audio", "kind", "voice", "speed", "passthrough"} {
		if strings.Contains(clean, `"`+key+`"`) {
			t.Fatalf("a text thread must carry no %q key at all: %s", key, clean)
		}
	}
}

// A speech thread carries no image settings (and an image thread no audio
// settings): each kind's carrier is cleared for every other kind, so a raw client
// cannot park the wrong knobs in a thread's document.
func TestPrepareChatRunClearsTheOtherKindsSettings(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")

	t.Run("speech clears image", func(t *testing.T) {
		created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
			Content: json.RawMessage(`{"settings":{},"messages":[]}`),
		})
		_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
			UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"hi"}`),
			Settings: ChatRunSettings{
				Model: "kokoro", Kind: chatKindSpeech,
				Audio: speechAudio(), Image: &ChatImageSettings{Size: "512x512"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if settings.Image != nil {
			t.Fatalf("image settings must be cleared on a speech thread, got %+v", settings.Image)
		}
		if settings.Audio == nil {
			t.Fatal("audio settings must survive on a speech thread")
		}
		got, _ := svc.GetChat(ctx, owner, created.ID)
		if strings.Contains(storedSettings(t, got.Content), `"image"`) {
			t.Fatalf("a speech thread must carry no image key: %s", got.Content)
		}
	})

	t.Run("image clears audio", func(t *testing.T) {
		created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{
			Content: json.RawMessage(`{"settings":{},"messages":[]}`),
		})
		_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
			UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"a cat"}`),
			Settings: ChatRunSettings{
				Model: "sd-turbo", Kind: chatKindImage,
				Audio: speechAudio(), Image: &ChatImageSettings{Size: "512x512"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if settings.Audio != nil {
			t.Fatalf("audio settings must be cleared on an image thread, got %+v", settings.Audio)
		}
		if settings.Image == nil {
			t.Fatal("image settings must survive on an image thread")
		}
		got, _ := svc.GetChat(ctx, owner, created.ID)
		if strings.Contains(storedSettings(t, got.Content), `"audio"`) {
			t.Fatalf("an image thread must carry no audio key: %s", got.Content)
		}
	})
}

// ChatAudioSettings' wire shape: only the set knobs are emitted, the passthrough
// round-trips its raw JSON values untouched, and the carrier is absent from a
// ChatRunSettings that has none -- which is what keeps text/image settings
// byte-identical.
func TestChatAudioSettingsWireShape(t *testing.T) {
	b, err := json.Marshal(speechAudio())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"voice":"af_bella","speed":1.25,"passthrough":{"instructions":"calm and slow"}}`; string(b) != want {
		t.Fatalf("marshal = %s, want %s", b, want)
	}

	var back ChatAudioSettings
	if err := json.Unmarshal([]byte(`{"voice":"v","passthrough":{"n":3,"o":{"a":[1,2]},"s":"x"}}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Voice != "v" || back.Speed != nil {
		t.Fatalf("unmarshal = %+v", back)
	}
	for k, want := range map[string]string{"n": `3`, "o": `{"a":[1,2]}`, "s": `"x"`} {
		if got := string(back.Passthrough[k]); got != want {
			t.Fatalf("passthrough[%s] = %s, want %s", k, got, want)
		}
	}

	if empty, _ := json.Marshal(ChatAudioSettings{}); string(empty) != `{}` {
		t.Fatalf("an unset ChatAudioSettings must marshal to {}, got %s", empty)
	}
	if zero, _ := json.Marshal(ChatRunSettings{Model: "m"}); strings.Contains(string(zero), "audio") {
		t.Fatalf("a ChatRunSettings without audio must carry no audio key: %s", zero)
	}
}

// The speech language is declared LAST, after KeepLastAudio: a chat stored before the
// field existed decodes with Language "" and re-encodes to the very same bytes, and a set
// language is emitted after every earlier knob, so no stored document's bytes move.
func TestChatAudioSettingsLanguageWireShape(t *testing.T) {
	const stored = `{"voice":"af_bella","speed":1.25,"passthrough":{"instructions":"calm"},"stream":false,"keep_last_audio":3}`
	var back ChatAudioSettings
	if err := json.Unmarshal([]byte(stored), &back); err != nil {
		t.Fatal(err)
	}
	if back.Language != "" {
		t.Fatalf("a document without language must read as unset, got %q", back.Language)
	}
	if again, _ := json.Marshal(back); string(again) != stored {
		t.Fatalf("re-marshal = %s, want the stored bytes %s", again, stored)
	}

	back.Language = "de"
	withLang, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(stored, "}") + `,"language":"de"}`; string(withLang) != want {
		t.Fatalf("marshal = %s, want language appended last: %s", withLang, want)
	}
	if b, _ := json.Marshal(ChatAudioSettings{Language: "de"}); string(b) != `{"language":"de"}` {
		t.Fatalf("a lone language must marshal as {\"language\":\"de\"}, got %s", b)
	}
}

// The speech-streaming choice (ADR-068): unset stays off the wire so an existing thread's
// persisted settings are byte-identical, while an explicit false -- the opt-out -- is
// emitted and survives the round trip (a plain bool with omitempty would lose it).
func TestChatAudioSettingsSpeechStreamWireShape(t *testing.T) {
	off, on := false, true
	if b, _ := json.Marshal(ChatAudioSettings{Voice: "v"}); string(b) != `{"voice":"v"}` {
		t.Fatalf("an unset stream must be omitted, got %s", b)
	}
	b, err := json.Marshal(ChatAudioSettings{Stream: &off})
	if err != nil || string(b) != `{"stream":false}` {
		t.Fatalf("an explicit false must be emitted, got %s (%v)", b, err)
	}
	if b, _ := json.Marshal(ChatAudioSettings{Stream: &on}); string(b) != `{"stream":true}` {
		t.Fatalf("an explicit true is emitted too, got %s", b)
	}
	var back ChatAudioSettings
	if err := json.Unmarshal([]byte(`{"stream":false}`), &back); err != nil || back.Stream == nil || *back.Stream {
		t.Fatalf("unmarshal = %+v (%v), want a set false", back, err)
	}
}

// The thread's speech-streaming choice is persisted with its other audio settings and handed
// to the run, which is what lets the composer's toggle outlive the page; a thread that never
// touched it stores nothing for it.
func TestPrepareChatRunKeepsTheSpeechStreamChoice(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")
	created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})

	off := false
	_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"read this aloud"}`),
		Settings:    ChatRunSettings{Model: "kokoro", Kind: chatKindSpeech, Audio: &ChatAudioSettings{Voice: "af_bella", Stream: &off}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Audio == nil || settings.Audio.Stream == nil || *settings.Audio.Stream {
		t.Fatalf("the choice was not handed to the run: %+v", settings.Audio)
	}
	got, _ := svc.GetChat(ctx, owner, created.ID)
	if !strings.Contains(string(got.Content), `"stream":false`) {
		t.Fatalf("the choice was not persisted: %s", got.Content)
	}

	// A later send that leaves it unset stores nothing for it: the default is the absence.
	_, settings, err = svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
		UserMessage: json.RawMessage(`{"id":"u2","role":"user","content":"again"}`),
		Settings:    ChatRunSettings{Model: "kokoro", Kind: chatKindSpeech, Audio: &ChatAudioSettings{Voice: "af_bella"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Audio.Stream != nil {
		t.Fatalf("an unset choice came back as %v", *settings.Audio.Stream)
	}
	got, _ = svc.GetChat(ctx, owner, created.ID)
	if strings.Contains(string(got.Content), `"stream"`) {
		t.Fatalf("an unset choice was persisted: %s", got.Content)
	}
}

// keep_last_audio is the per-chat retention of a speech thread's answers (#281):
// nil keeps every answer, 0 stores none and N >= 1 keeps the last N. A negative
// value is not a count at all, so PrepareChatRun reads it as unset (keep all) and
// persists nothing for it, while 0 and a positive N survive untouched -- 0 above
// all, which a plain int with omitempty would lose. A non-speech thread still has
// its whole audio carrier cleared, the retention knob included.
func TestPrepareChatRunClampsKeepLastAudio(t *testing.T) {
	ctx := context.Background()
	svc := NewService(ServiceDeps{Chats: store.NewMemoryChatStore(0)})
	owner := chatToken("usr_a")

	send := func(kind string, keep int) (ChatRunSettings, string) {
		t.Helper()
		created, _ := svc.CreateChat(ctx, owner, CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
		model := "kokoro"
		if kind != chatKindSpeech {
			model = "llama"
		}
		_, settings, err := svc.PrepareChatRun(ctx, owner, created.ID, PrepareRunRequest{
			UserMessage: json.RawMessage(`{"id":"u1","role":"user","content":"read this aloud"}`),
			Settings:    ChatRunSettings{Model: model, Kind: kind, Audio: &ChatAudioSettings{Voice: "af_bella", KeepLastAudio: &keep}},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := svc.GetChat(ctx, owner, created.ID)
		return settings, storedSettings(t, got.Content)
	}

	// A negative value is read as unset: nothing handed to the run, nothing stored.
	settings, stored := send(chatKindSpeech, -3)
	if settings.Audio == nil || settings.Audio.KeepLastAudio != nil {
		t.Fatalf("a negative keep_last_audio must come back unset, got %+v", settings.Audio)
	}
	if strings.Contains(stored, "keep_last_audio") {
		t.Fatalf("a negative keep_last_audio must not be persisted: %s", stored)
	}

	// 0 (do not store) and a positive N (keep the last N) survive, and are stored.
	for _, keep := range []int{0, 2} {
		settings, stored := send(chatKindSpeech, keep)
		if settings.Audio == nil || settings.Audio.KeepLastAudio == nil || *settings.Audio.KeepLastAudio != keep {
			t.Fatalf("keep_last_audio %d did not survive: %+v", keep, settings.Audio)
		}
		if want := `"keep_last_audio":` + strconv.Itoa(keep); !strings.Contains(stored, want) {
			t.Fatalf("keep_last_audio %d was not persisted (want %s): %s", keep, want, stored)
		}
	}

	// A non-speech thread keeps nothing of the audio carrier.
	settings, stored = send("", 2)
	if settings.Audio != nil {
		t.Fatalf("audio settings must be cleared on a text thread, got %+v", settings.Audio)
	}
	if strings.Contains(stored, "keep_last_audio") || strings.Contains(stored, `"audio"`) {
		t.Fatalf("a text thread must carry no audio key at all: %s", stored)
	}
}
