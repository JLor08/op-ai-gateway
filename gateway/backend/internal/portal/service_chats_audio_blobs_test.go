// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"testing"
)

// A speech turn's reply is one audio_url part carrying the WAV inline (the
// contract of gateway/chat_runs_audio.go). It takes the road a generated image
// takes: the part's data: url is moved into chat_blobs at the commit and
// rewritten to the gateway path, so the sealed document never carries the bytes.

// wavLike is bytes with a RIFF/WAVE signature and a body that is not valid UTF-8
// text, so a payload that round-tripped through a string or JSON encoder would
// not compare equal.
func wavLike(n int) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	for i := len(b); i < n; i++ {
		b = append(b, byte(i*29+7))
	}
	return b
}

// audioContent is a speech turn's content: the one audio_url part, spelled the way
// the executor emits it.
func audioContent(url string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`[{"type":"audio_url","audio_url":{"url":%q}}]`, url))
}

// storedMemberURLs returns the url of every part carrying member (image_url or
// audio_url) in a stored document, in order.
func storedMemberURLs(t *testing.T, content json.RawMessage, member string) []string {
	t.Helper()
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("stored document is not JSON: %v", err)
	}
	var urls []string
	for _, m := range doc.Messages {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			var target struct {
				URL string `json:"url"`
			}
			if raw, ok := p[member]; ok && json.Unmarshal(raw, &target) == nil {
				urls = append(urls, target.URL)
			}
		}
	}
	return urls
}

// startSpeechThread pins a fresh chat to the speech kind with one prompt.
func (e *blobTestEnv) startSpeechThread(t *testing.T, prompt string) string {
	t.Helper()
	chatID := e.emptyChat(t)
	if _, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf("%q", prompt)),
		Settings:    ChatRunSettings{Model: "m", Kind: chatKindSpeech},
	}); err != nil {
		t.Fatalf("PrepareChatRun(speech): %v", err)
	}
	return chatID
}

// The core behaviour: the WAV leaves the document for chat_blobs (sealed when a
// cipher is configured), the part keeps its shape with the url rewritten, and the
// blob is served back as the very bytes, typed audio/wav.
func TestCommitAssistantExtractsASpeechReplyIntoChatBlobs(t *testing.T) {
	ctx := context.Background()
	for _, withCipher := range []bool{true, false} {
		t.Run(fmt.Sprintf("cipher=%v", withCipher), func(t *testing.T) {
			e := newBlobTestEnv(t, withCipher, 0)
			chatID := e.startSpeechThread(t, "say hello")
			raw := wavLike(5 << 20) // ~6.7 MiB of base64: over the 4 MiB document cap

			if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: audioContent(dataURL("audio/wav", raw))}, "complete"); err != nil {
				t.Fatalf("CommitAssistant: %v", err)
			}

			doc := string(e.stored(t, chatID))
			if strings.Contains(doc, "data:") || strings.Contains(doc, "base64") || !strings.Contains(doc, `"status":"complete"`) {
				t.Fatalf("the reply was not committed as a reference: %.300s", doc)
			}
			urls := storedMemberURLs(t, json.RawMessage(doc), "audio_url")
			if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) || !blobIDPattern.MatchString(strings.TrimPrefix(urls[0], gatewayBlobPrefix)) {
				t.Fatalf("stored audio urls = %v, want one gateway url", urls)
			}
			// The part keeps the documented shape, "type" first.
			want := fmt.Sprintf(`"content":[{"type":"audio_url","audio_url":{"url":%q}}]`, urls[0])
			if !strings.Contains(doc, want) {
				t.Fatalf("stored part is not the rewritten audio_url part:\n got  %.400s\n want ...%s", doc, want)
			}

			id := strings.TrimPrefix(urls[0], gatewayBlobPrefix)
			row, err := e.mem.ChatBlobByID(ctx, id)
			if err != nil {
				t.Fatalf("ChatBlobByID: %v", err)
			}
			if row.ChatID != chatID || row.ContentType != "audio/wav" || row.ByteLen != int64(len(raw)) || !bytes.Equal(e.blobBytes(t, row), raw) {
				t.Fatalf("blob row = chat %s type %s, %d bytes: not the WAV", row.ChatID, row.ContentType, row.ByteLen)
			}
			if wantVersion := map[bool]int{true: 1, false: 0}[withCipher]; row.KeyVersion != wantVersion {
				t.Fatalf("KeyVersion = %d, want %d (sealed iff a cipher is configured)", row.KeyVersion, wantVersion)
			}

			served, mediaType, err := e.svc.ChatBlobBytes(ctx, e.owner, id)
			if err != nil {
				t.Fatalf("ChatBlobBytes: %v", err)
			}
			if mediaType != "audio/wav" || !bytes.Equal(served, raw) {
				t.Fatalf("served %d bytes as %s, want the %d stored bytes as audio/wav", len(served), mediaType, len(raw))
			}
		})
	}
}

// The audio part is just another part of a message: beside text and an image it is
// extracted in the same pass, each blob under its own type, and the text and the
// image's handling are exactly what they were.
func TestCommitAssistantExtractsAudioBesideTextAndImages(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)
	chatID := e.startSpeechThread(t, "mixed")
	img, wav := blobTestBytes(300, 3), wavLike(400)
	parts := json.RawMessage(fmt.Sprintf(
		`[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":%q}},{"type":"audio_url","audio_url":{"url":%q}},{"type":"audio_url","audio_url":{"url":"https://example.com/a.wav"}}]`,
		dataURL("image/png", img), dataURL("audio/wav", wav)))

	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: parts}, "complete"); err != nil {
		t.Fatalf("CommitAssistant: %v", err)
	}
	doc := e.stored(t, chatID)
	images, audios := storedMemberURLs(t, doc, "image_url"), storedMemberURLs(t, doc, "audio_url")
	if len(images) != 1 || !strings.HasPrefix(images[0], gatewayBlobPrefix) || len(audios) != 2 || !strings.HasPrefix(audios[0], gatewayBlobPrefix) || audios[1] != "https://example.com/a.wav" {
		t.Fatalf("image urls %v, audio urls %v: want both inline parts rewritten, the external url left alone", images, audios)
	}
	for _, tc := range []struct {
		url, mediaType string
		raw            []byte
	}{{images[0], "image/png", img}, {audios[0], "audio/wav", wav}} {
		row, err := e.mem.ChatBlobByID(ctx, strings.TrimPrefix(tc.url, gatewayBlobPrefix))
		if err != nil {
			t.Fatalf("ChatBlobByID(%s): %v", tc.url, err)
		}
		if row.ContentType != tc.mediaType || !bytes.Equal(e.blobBytes(t, row), tc.raw) {
			t.Fatalf("blob %s = type %s, %d bytes; want %s with the original bytes", tc.url, row.ContentType, row.ByteLen, tc.mediaType)
		}
	}
	if !strings.Contains(string(doc), `{"type":"text","text":"hello"}`) {
		t.Fatalf("the text part changed: %.300s", doc)
	}
	if e.rec.storedBlobs() != 2 {
		t.Fatalf("blobs stored = %d, want 2", e.rec.storedBlobs())
	}
}

// The allow-list governs an audio part exactly as it governs an image part, and
// each part kind takes only its own media family: a refusal leaves the transcript
// and the blob table as they were.
func TestCommitAssistantRefusesBadSpeechAudio(t *testing.T) {
	payload := dataURL("audio/wav", wavLike(64))[len("data:audio/wav;base64,"):]
	for _, tc := range []struct {
		name   string
		member string
		url    string
		want   error
	}{
		// Only the types the portal's consumers need are listed: the speech reply's
		// WAV and the transcription composer's input formats (webm, mpeg, mp4, ogg,
		// wav). Any other audio subtype stays refused.
		{"flac", "audio_url", dataURL("audio/flac", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"aac", "audio_url", dataURL("audio/aac", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		// The executor normalises the aliases to audio/wav before it commits, so the
		// store has no need to know them.
		{"audio/x-wav alias", "audio_url", dataURL("audio/x-wav", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"audio/wave alias", "audio_url", dataURL("audio/wave", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"html in an audio part", "audio_url", dataURL("text/html", []byte("<script>alert(1)</script>")), ErrChatBlobTypeUnsupported},
		{"octet-stream", "audio_url", dataURL("application/octet-stream", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"a codec parameter", "audio_url", "data:audio/wav;codecs=1;base64," + payload, ErrChatBlobTypeUnsupported},
		{"not base64", "audio_url", "data:audio/wav,AAEC", ErrChatBlobTypeUnsupported},
		{"unpadded", "audio_url", "data:audio/wav;base64,AQ", ErrChatBlobTypeUnsupported},
		{"empty payload", "audio_url", "data:audio/wav;base64,", ErrChatBlobTypeUnsupported},
		{"an image in an audio part", "audio_url", dataURL("image/png", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"audio in an image part", "image_url", dataURL("audio/wav", wavLike(64)), ErrChatBlobTypeUnsupported},
		// The input formats join the audio family only: an image part still cannot
		// take them.
		{"webm in an image part", "image_url", dataURL("audio/webm", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"mpeg in an image part", "image_url", dataURL("audio/mpeg", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"mp4 in an image part", "image_url", dataURL("audio/mp4", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"ogg in an image part", "image_url", dataURL("audio/ogg", []byte{1, 2, 3}), ErrChatBlobTypeUnsupported},
		{"too large", "audio_url", dataURL("audio/wav", wavLike(1001)), ErrChatBlobTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBlobTestEnv(t, false, 1000)
			chatID := e.startSpeechThread(t, "say hello")
			before := string(e.stored(t, chatID))
			saves := len(e.rec.saves)

			parts := json.RawMessage(fmt.Sprintf(`[{"type":%q,%q:{"url":%q}}]`, tc.member, tc.member, tc.url))
			if err := e.svc.CommitAssistant(context.Background(), e.owner, chatID, AssistantTurn{ContentParts: parts}, "complete"); !errors.Is(err, tc.want) {
				t.Fatalf("CommitAssistant = %v, want %v", err, tc.want)
			}
			if string(e.stored(t, chatID)) != before || len(e.rec.saves) != saves || e.rec.storedBlobs() != 0 {
				t.Fatalf("a refused commit changed state")
			}
		})
	}
}

// A client-supplied audio part is held to the same rules as an image part, which
// matters because extraction is not limited to the assistant's turn: a gateway url
// must be one THIS chat holds, and the member names are read the way the document's
// other readers read them, without regard to case.
func TestPrepareChatRunVerifiesAndNormalisesSubmittedAudioParts(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)
	mine := e.startSpeechThread(t, "mine")
	if err := e.svc.CommitAssistant(ctx, e.owner, mine, AssistantTurn{ContentParts: audioContent(dataURL("audio/wav", wavLike(128)))}, "complete"); err != nil {
		t.Fatal(err)
	}
	myBlob := strings.TrimPrefix(storedMemberURLs(t, e.stored(t, mine), "audio_url")[0], gatewayBlobPrefix)
	sibling := e.emptyChat(t)

	edit := func(chat, content string) error {
		_, _, err := e.svc.PrepareChatRun(ctx, e.owner, chat, PrepareRunRequest{
			EditedHistory: []json.RawMessage{json.RawMessage(content)},
			Settings:      ChatRunSettings{Model: "m"},
		})
		return err
	}

	t.Run("another chat's audio blob is refused", func(t *testing.T) {
		err := edit(sibling, fmt.Sprintf(`{"id":"a","role":"assistant","content":[{"type":"audio_url","audio_url":{"url":%q}}]}`, gatewayBlobPrefix+myBlob))
		if !errors.Is(err, ErrChatBlobNotOwned) {
			t.Fatalf("PrepareChatRun = %v, want ErrChatBlobNotOwned", err)
		}
	})
	t.Run("an id that does not exist is refused", func(t *testing.T) {
		err := edit(mine, fmt.Sprintf(`{"id":"a","role":"assistant","content":[{"type":"audio_url","audio_url":{"url":%q}}]}`, gatewayBlobPrefix+"blob_"+strings.Repeat("0", 32)))
		if !errors.Is(err, ErrChatBlobNotOwned) {
			t.Fatalf("PrepareChatRun = %v, want ErrChatBlobNotOwned", err)
		}
	})
	t.Run("the chat's own blob is kept", func(t *testing.T) {
		if err := edit(mine, fmt.Sprintf(`{"id":"a","role":"assistant","content":[{"type":"audio_url","audio_url":{"url":%q}}]}`, gatewayBlobPrefix+myBlob)); err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
		if got := storedMemberURLs(t, e.stored(t, mine), "audio_url"); len(got) != 1 || got[0] != gatewayBlobPrefix+myBlob {
			t.Fatalf("stored audio urls = %v, want the owned url unchanged", got)
		}
	})
	t.Run("member spelling does not hide a part", func(t *testing.T) {
		err := edit(sibling, `{"id":"a","role":"assistant","CONTENT":[{"type":"audio_url","Audio_Url":{"URL":"data:text/html;base64,PGI+"}}]}`)
		if !errors.Is(err, ErrChatBlobTypeUnsupported) {
			t.Fatalf("PrepareChatRun = %v, want ErrChatBlobTypeUnsupported", err)
		}
	})
	t.Run("an inline wav in an edited history is stored", func(t *testing.T) {
		raw := wavLike(200)
		if err := edit(sibling, fmt.Sprintf(`{"id":"a","role":"assistant","content":[{"type":"audio_url","audio_url":{"url":%q}}]}`, dataURL("audio/wav", raw))); err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
		urls := storedMemberURLs(t, e.stored(t, sibling), "audio_url")
		if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) {
			t.Fatalf("stored audio urls = %v", urls)
		}
		row, err := e.mem.ChatBlobByID(ctx, strings.TrimPrefix(urls[0], gatewayBlobPrefix))
		if err != nil || row.ContentType != "audio/wav" || !bytes.Equal(e.blobBytes(t, row), raw) {
			t.Fatalf("blob row type %q, err %v: not the WAV", row.ContentType, err)
		}
	})
}

// The audio part's decode is the image decode's sibling, not a loosening of it:
// DecodeImageDataURL stays image-only (the image-edit executor sends what it
// returns to /v1/images/edits as a source image), and an audio part takes only the
// audio allow-list -- no image type, no other audio subtype.
func TestDataURLDecodersKeepToTheirOwnMediaFamily(t *testing.T) {
	raw := wavLike(97)
	wav := dataURL("audio/wav", raw)
	if mediaType, got, err := decodeDataURL(wav, audioPartKind); err != nil || mediaType != "audio/wav" || !bytes.Equal(got, raw) {
		t.Fatalf("decodeDataURL(wav, audio) = %q, %d bytes, %v", mediaType, len(got), err)
	}
	upper := "DATA:AUDIO/WAV;BASE64," + wav[len("data:audio/wav;base64,"):]
	if mediaType, _, err := decodeDataURL(upper, audioPartKind); err != nil || mediaType != "audio/wav" {
		t.Fatalf("decodeDataURL(upper case, audio) = %q, %v: want the canonical lower-case type", mediaType, err)
	}
	for _, url := range []string{
		dataURL("image/png", raw), dataURL("audio/flac", raw), dataURL("audio/x-wav", raw), dataURL("text/html", raw),
		gatewayBlobPrefix + "blob_" + strings.Repeat("a", 32), "https://example.com/a.wav", "",
	} {
		if mediaType, _, err := decodeDataURL(url, audioPartKind); !errors.Is(err, ErrChatBlobTypeUnsupported) {
			t.Errorf("decodeDataURL(%.40q, audio) = %q, %v; want ErrChatBlobTypeUnsupported", url, mediaType, err)
		}
	}
	for _, url := range []string{wav, dataURL("audio/mpeg", raw), dataURL("audio/webm", raw)} {
		if mediaType, _, err := DecodeImageDataURL(url); !errors.Is(err, ErrChatBlobTypeUnsupported) {
			t.Errorf("DecodeImageDataURL(%.40q) = %q, %v; want ErrChatBlobTypeUnsupported: audio is not a source image", url, mediaType, err)
		}
	}
}

// The carry check. A speech run takes the text branch of PrepareChatRun, so its
// history goes through resolveChatBlobRefs, which resolves IMAGE references only.
// A stored WAV must not be pulled into that history (an upstream would never be
// handed it, and a speech thread's earlier turns are all audio) and must not
// count against the run-size bound: a thread of many replies would otherwise be
// refused for bytes the run never sends.
func TestSpeechRunHistoryDoesNotPullAnAudioBlob(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 1000) // bound 1000 bytes; each reply below is 5000
	chatID := e.emptyChat(t)
	send := func(prompt string) ([]ChatAPIMessage, error) {
		return e.sendKind(chatID, chatKindSpeech, json.RawMessage(fmt.Sprintf("%q", prompt)))
	}
	if _, err := send("first"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: audioContent(dataURL("audio/wav", wavLike(5000)))}, "complete"); err != nil {
			t.Fatal(err)
		}
		if _, err := send("again"); err != nil {
			t.Fatalf("a speech thread whose stored replies exceed the run bound must keep running: %v", err)
		}
	}
	e.byIDs.calls = nil

	history, err := send("and once more")
	if err != nil {
		t.Fatalf("PrepareChatRun(speech): %v", err)
	}
	if len(e.byIDs.calls) != 0 {
		t.Fatalf("a speech run asked the blob store for %d batches (%v), want none: it has no image to resolve", len(e.byIDs.calls), e.byIDs.calls)
	}
	var audioRefs int
	for _, m := range history {
		if strings.Contains(string(m.Content), "data:") {
			t.Fatalf("the run history carries inline bytes: %.200s", m.Content)
		}
		audioRefs += len(storedMemberURLs(t, json.RawMessage(fmt.Sprintf(`{"messages":[{"content":%s}]}`, m.Content)), "audio_url"))
	}
	if audioRefs != 2 {
		t.Fatalf("history carries %d audio references, want the two stored replies left as references", audioRefs)
	}
	if got := MessageText(history[len(history)-1].Content); got != "and once more" {
		t.Fatalf("the last prompt = %q: the speech run still reads its text", got)
	}
}

// In a history that mixes the two, resolution takes the image and leaves the
// audio: the batched lookup names only the image's id, and only the image's bytes
// count against the bound.
func TestRunHistoryResolutionIgnoresAudioParts(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 1000)
	chatID := e.emptyChat(t)
	imageID := e.sendImage(t, chatID, blobTestBytes(600, 1))
	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: audioContent(dataURL("audio/wav", wavLike(5000)))}, "complete"); err != nil {
		t.Fatal(err)
	}
	audioID := strings.TrimPrefix(storedMemberURLs(t, e.stored(t, chatID), "audio_url")[0], gatewayBlobPrefix)
	e.byIDs.calls = nil

	history, err := e.send(chatID, json.RawMessage(`"go"`))
	if err != nil {
		t.Fatalf("PrepareChatRun: %v (the 5000 audio bytes must not count against the 1000 bound)", err)
	}
	if len(e.byIDs.calls) != 1 || len(e.byIDs.calls[0].ids) != 1 || e.byIDs.calls[0].ids[0] != imageID {
		t.Fatalf("ChatBlobsByIDs calls = %+v, want one batch naming only the image %s (not the audio %s)", e.byIDs.calls, imageID, audioID)
	}
	if urls := historyImageURLs(history); len(urls) != 1 || !strings.HasPrefix(urls[0], "data:image/png;base64,") {
		t.Fatalf("the image was not resolved: %.80v", urls)
	}
	var sawAudioRef bool
	for _, m := range history {
		if strings.Contains(string(m.Content), "data:audio") {
			t.Fatalf("an audio part was resolved into the history: %.200s", m.Content)
		}
		sawAudioRef = sawAudioRef || strings.Contains(string(m.Content), gatewayBlobPrefix+audioID)
	}
	if !sawAudioRef {
		t.Fatal("the audio reference was dropped from the history instead of being left alone")
	}
}

// collectBlobRefIDs is where the walk is decided; pin it directly too. The walk is
// told which family it reads: a text, image or speech run's history is read as
// images (the images only, a stored audio reference is never listed, so never
// fetched), a transcription run's source as audio.
func TestCollectBlobRefIDsReadsTheKindItIsAskedFor(t *testing.T) {
	ref := func(id string) string { return gatewayBlobPrefix + "blob_" + strings.Repeat(id, 32) }
	history := []ChatAPIMessage{
		{Role: "user", Content: imageContent("see", ref("a"))},
		{Role: "assistant", Content: audioContent(ref("b"))},
		{Role: "user", Content: json.RawMessage(fmt.Sprintf(
			`[{"type":"image_url","image_url":{"url":%q}},{"type":"audio_url","audio_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q}},{"type":"audio_url","audio_url":{"url":%q}}]`, ref("a"), ref("c"), ref("a"), ref("c")))},
	}
	idOf := func(c string) string { return "blob_" + strings.Repeat(c, 32) }

	ids, occurrences, err := collectBlobRefIDs(history, imagePartKind)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != idOf("a") || occurrences[ids[0]] != 3 {
		t.Fatalf("image walk: ids = %v, occurrences = %v; want only the image blob, three times", ids, occurrences)
	}

	ids, occurrences, err = collectBlobRefIDs(history, audioPartKind)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{idOf("b"), idOf("c")}) || occurrences[idOf("b")] != 1 || occurrences[idOf("c")] != 2 {
		t.Fatalf("audio walk: ids = %v, occurrences = %v; want only the audio blobs, in order first seen", ids, occurrences)
	}
}

// A row the store hands back for an IMAGE reference must itself be an image: a
// chat can carry an image_url part that points at one of its own WAV blobs (the
// ownership check passes, the blob is the chat's), and resolving it would put
// data:audio/wav into an image part of the request -- bytes the bound counts and
// an upstream is told are a picture.
func TestPrepareChatRunNeverResolvesAnImageReferenceToAnAudioBlob(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 0)
	chatID := e.startSpeechThread(t, "say hello")
	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: audioContent(dataURL("audio/wav", wavLike(300)))}, "complete"); err != nil {
		t.Fatal(err)
	}
	audioID := strings.TrimPrefix(storedMemberURLs(t, e.stored(t, chatID), "audio_url")[0], gatewayBlobPrefix)

	history, _, err := e.svc.PrepareChatRun(ctx, e.owner, chatID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"id":"u1","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}`, gatewayBlobPrefix+audioID))},
		Settings:      ChatRunSettings{Model: "m"},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if urls := historyImageURLs(history); len(urls) != 0 {
		t.Fatalf("an audio blob was resolved as an image: %.100v", urls)
	}
	if len(history) != 1 || !strings.Contains(string(history[0].Content), unavailableImageText) || strings.Contains(string(history[0].Content), "data:") {
		t.Fatalf("history = %.200v, want the image placeholder and no inline bytes", history)
	}
}

// The store carries any content type; the allow-list lives in the portal. The
// set is pinned so a later edit cannot admit another type by accident -- extend it
// deliberately, together with the consumer that needs the type. The audio side is
// the speech reply's WAV plus the transcription composer's input formats (what the
// browser's recorder and an attached file produce).
func TestChatBlobAllowListHoldsExactlyTheImageTypesAndTheAudioFormats(t *testing.T) {
	want := map[string]struct{}{
		"image/png": {}, "image/jpeg": {}, "image/webp": {}, "image/gif": {},
		"audio/wav": {}, "audio/webm": {}, "audio/mpeg": {}, "audio/mp4": {}, "audio/ogg": {},
	}
	if len(chatBlobMediaTypes) != len(want) {
		t.Fatalf("chatBlobMediaTypes = %v, want exactly %v", chatBlobMediaTypes, want)
	}
	for mediaType := range want {
		if _, ok := chatBlobMediaTypes[mediaType]; !ok {
			t.Fatalf("%s is missing from chatBlobMediaTypes", mediaType)
		}
	}
}

// The widened allow-list keeps the two families apart: every audio input format
// is accepted by an audio part and refused by an image part, and every image type
// is still refused by an audio part -- the family test (blobPartKind.accepts) is
// the prefix AND the allow-list, so a new audio type joins the audio family only.
func TestAudioInputTypesJoinTheAudioFamilyOnly(t *testing.T) {
	for _, mediaType := range []string{"audio/wav", "audio/webm", "audio/mpeg", "audio/mp4", "audio/ogg"} {
		if !audioPartKind.accepts(mediaType) {
			t.Errorf("audioPartKind.accepts(%q) = false, want true", mediaType)
		}
		if imagePartKind.accepts(mediaType) {
			t.Errorf("imagePartKind.accepts(%q) = true, want false: audio is never filed under an image part", mediaType)
		}
	}
	for _, mediaType := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		if audioPartKind.accepts(mediaType) {
			t.Errorf("audioPartKind.accepts(%q) = true, want false: an image is never filed under an audio part", mediaType)
		}
		if !imagePartKind.accepts(mediaType) {
			t.Errorf("imagePartKind.accepts(%q) = false, want true", mediaType)
		}
	}
	for _, mediaType := range []string{"audio/flac", "audio/aac", "audio/x-wav", "audio/webm;codecs=opus", "video/webm", "text/html", ""} {
		if audioPartKind.accepts(mediaType) || imagePartKind.accepts(mediaType) {
			t.Errorf("%q is accepted by a part kind, want it refused by both", mediaType)
		}
	}
}

// DecodeAudioDataURL is the ONE definition of an inline audio url, the audio twin
// of DecodeImageDataURL: the run commit that stores an attached or recorded clip
// and the transcription executor that sends it parse the same text, so the
// executor can never accept a spelling the store refused.
func TestDecodeAudioDataURL(t *testing.T) {
	raw := blobTestBytes(131, 17)
	payload := base64.StdEncoding.EncodeToString(raw)
	for _, mediaType := range []string{"audio/webm", "audio/mpeg", "audio/mp4", "audio/ogg", "audio/wav"} {
		got, bytesOut, err := DecodeAudioDataURL(dataURL(mediaType, raw))
		if err != nil || got != mediaType || !bytes.Equal(bytesOut, raw) {
			t.Errorf("DecodeAudioDataURL(%s) = %q, %d bytes, %v; want the type and the %d bytes", mediaType, got, len(bytesOut), err, len(raw))
		}
	}
	// The scheme, the base64 flag and the type are matched without regard to case,
	// and the type comes back canonical.
	if got, _, err := DecodeAudioDataURL("DATA:AUDIO/WEBM;BASE64," + payload); err != nil || got != "audio/webm" {
		t.Errorf("DecodeAudioDataURL(upper case) = %q, %v; want the canonical lower-case type", got, err)
	}
	for name, url := range map[string]string{
		"an image type":         dataURL("image/png", raw),
		"an unlisted audio":     dataURL("audio/flac", raw),
		"a non-audio webm":      dataURL("video/webm", raw),
		"html":                  dataURL("text/html", raw),
		"a codec parameter":     "data:audio/webm;codecs=opus;base64," + payload,
		"not base64":            "data:audio/webm,AAEC",
		"unpadded":              "data:audio/webm;base64,AQ",
		"the url-safe alphabet": "data:audio/webm;base64," + strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe})),
		"an empty payload":      "data:audio/webm;base64,",
		"a gateway url":         gatewayBlobPrefix + "blob_" + strings.Repeat("a", 32),
		"an https url":          "https://example.com/a.webm",
		"empty":                 "",
	} {
		if mediaType, got, err := DecodeAudioDataURL(url); !errors.Is(err, ErrChatBlobTypeUnsupported) || mediaType != "" || got != nil {
			t.Errorf("DecodeAudioDataURL(%s: %.50q) = %q, %d bytes, %v; want ErrChatBlobTypeUnsupported", name, url, mediaType, len(got), err)
		}
	}
	// It and DecodeImageDataURL are siblings, not a loosening of each other: the
	// image decoder still refuses every audio input format.
	for _, mediaType := range []string{"audio/webm", "audio/mpeg", "audio/mp4", "audio/ogg"} {
		if _, _, err := DecodeImageDataURL(dataURL(mediaType, raw)); !errors.Is(err, ErrChatBlobTypeUnsupported) {
			t.Errorf("DecodeImageDataURL(%s) = %v; want ErrChatBlobTypeUnsupported", mediaType, err)
		}
	}
}

// MessageAudio reads the audio parts of a message the way the extraction and the
// resolution do (through rewriteBlobPart with the audio kind): an object whose
// audio_url is an object with a string url, members matched without regard to
// case, in order. An image part is not an audio part, and neither is a content
// that is not an array.
func TestMessageAudio(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []string
	}{
		{"a plain string", `"hello"`, nil},
		{"null", `null`, nil},
		{"no audio part", `[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]`, nil},
		{"one clip", `[{"type":"audio_url","audio_url":{"url":"data:audio/webm;base64,AAEC"}}]`, []string{"data:audio/webm;base64,AAEC"}},
		{"beside text and an image", `[{"type":"text","text":"transcribe"},{"type":"image_url","image_url":{"url":"/img"}},{"type":"audio_url","audio_url":{"url":"/api/portal/chat-blobs/blob_x"}}]`, []string{"/api/portal/chat-blobs/blob_x"}},
		{"two clips, in order", `[{"type":"audio_url","audio_url":{"url":"/a"}},{"type":"text","text":"t"},{"type":"audio_url","audio_url":{"url":"/b"}}]`, []string{"/a", "/b"}},
		{"member spelling", `[{"type":"audio_url","Audio_Url":{"URL":"/c"}}]`, []string{"/c"}},
		{"audio_url is not an object", `[{"type":"audio_url","audio_url":"/d"}]`, nil},
		{"the url is not a string", `[{"type":"audio_url","audio_url":{"url":7}}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MessageAudio(json.RawMessage(tc.content))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("MessageAudio(%s) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// A transcription thread's user turn carries the attached or recorded clip inline.
// The run commit moves it into chat_blobs under its own type (sealed when a cipher
// is configured) and rewrites the part's url, so the sealed document never holds
// the bytes, and the blob is served back as the very bytes under the same type.
func TestPrepareChatRunStoresTheTranscriptionInputAudio(t *testing.T) {
	ctx := context.Background()
	for _, mediaType := range []string{"audio/webm", "audio/mpeg", "audio/mp4", "audio/ogg", "audio/wav"} {
		for _, withCipher := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cipher=%v", mediaType, withCipher), func(t *testing.T) {
				e := newBlobTestEnv(t, withCipher, 0)
				chatID := e.emptyChat(t)
				raw := blobTestBytes(2500, 5)

				if _, _, err := e.svc.PrepareChatRun(ctx, e.owner, chatID, PrepareRunRequest{
					UserMessage: audioContent(dataURL(mediaType, raw)),
					Settings:    ChatRunSettings{Model: "whisper", Kind: chatKindTranscription},
				}); err != nil {
					t.Fatalf("PrepareChatRun: %v", err)
				}
				doc := string(e.stored(t, chatID))
				if strings.Contains(doc, "data:") || strings.Contains(doc, "base64") {
					t.Fatalf("the clip was not stored as a reference: %.300s", doc)
				}
				urls := storedMemberURLs(t, json.RawMessage(doc), "audio_url")
				if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) || !blobIDPattern.MatchString(strings.TrimPrefix(urls[0], gatewayBlobPrefix)) {
					t.Fatalf("stored audio urls = %v, want one gateway url", urls)
				}
				id := strings.TrimPrefix(urls[0], gatewayBlobPrefix)
				row, err := e.mem.ChatBlobByID(ctx, id)
				if err != nil {
					t.Fatalf("ChatBlobByID: %v", err)
				}
				if row.ChatID != chatID || row.ContentType != mediaType || row.ByteLen != int64(len(raw)) || !bytes.Equal(e.blobBytes(t, row), raw) {
					t.Fatalf("blob row = chat %s type %s, %d bytes: not the clip", row.ChatID, row.ContentType, row.ByteLen)
				}
				served, servedType, err := e.svc.ChatBlobBytes(ctx, e.owner, id)
				if err != nil || servedType != mediaType || !bytes.Equal(served, raw) {
					t.Fatalf("served %d bytes as %q, err %v; want the %d stored bytes as %s", len(served), servedType, err, len(raw), mediaType)
				}
			})
		}
	}
}

// Unlisted audio rows are never served, whatever the row says.
func TestChatBlobBytesNeverServesAnUnlistedAudioType(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, false, 0)
	chatID := e.emptyChat(t)
	for i, mediaType := range []string{"audio/flac", "audio/x-wav", "audio/wav; codecs=1", "AUDIO/WAV", "audio/webm; codecs=opus", "AUDIO/WEBM"} {
		id := fmt.Sprintf("blob_audio_%d", i)
		if err := e.mem.PutChatBlob(ctx, store.ChatBlob{ID: id, ChatID: chatID, Blob: wavLike(32), ContentType: mediaType, ByteLen: 32}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
			t.Errorf("%s: ChatBlobBytes = %v, want ErrChatBlobNotFound", mediaType, err)
		}
	}
}
