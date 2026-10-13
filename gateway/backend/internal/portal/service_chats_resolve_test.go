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
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"testing"
)

// byIDsCall is one ChatBlobsByIDs call the service made.
type byIDsCall struct {
	chatID string
	ids    []string
}

// byIDsStore wraps a chat store and records every ChatBlobsByIDs call: the chat
// it was scoped to and the ids it asked for. err makes the call fail; rewrite
// lets a test make the store misreport what it holds (a row of another chat, a
// lying byte_len, a tampered type), which is how the tests below pin that the
// service does not trust the store's scoping alone.
type byIDsStore struct {
	ChatStore
	calls   []byIDsCall
	err     error
	rewrite func(got map[string]store.ChatBlob)
}

func (b *byIDsStore) ChatBlobsByIDs(ctx context.Context, chatID string, ids []string) (map[string]store.ChatBlob, error) {
	b.calls = append(b.calls, byIDsCall{chatID: chatID, ids: slices.Clone(ids)})
	if b.err != nil {
		return nil, b.err
	}
	got, err := b.ChatStore.ChatBlobsByIDs(ctx, chatID, ids)
	if err == nil && b.rewrite != nil {
		b.rewrite(got)
	}
	return got, err
}

type resolveEnv struct {
	*blobTestEnv
	byIDs *byIDsStore
}

// newResolveEnv is a Service over a memory chat store seen through the
// recording wrapper (saves, owner lookups) and the batch-call recorder.
// maxRunBytes 0 leaves the service default in place.
func newResolveEnv(t *testing.T, withCipher bool, maxRunBytes int) *resolveEnv {
	t.Helper()
	mem := store.NewMemoryChatStore(0)
	rec := &recordingChatStore{ChatStore: mem}
	byIDs := &byIDsStore{ChatStore: rec}
	deps := ServiceDeps{Chats: byIDs, MaxRunHistoryBytes: maxRunBytes}
	var cipher *capture.Cipher
	if withCipher {
		var err error
		cipher, err = capture.New(captureTestKey)
		if err != nil {
			t.Fatalf("capture.New: %v", err)
		}
		deps.Cipher = cipher
	}
	return &resolveEnv{
		blobTestEnv: &blobTestEnv{svc: NewService(deps), rec: rec, mem: mem, cipher: cipher, owner: chatToken("usr_a")},
		byIDs:       byIDs,
	}
}

// sendKind runs one turn of the given thread kind ("" is text) and returns the
// history the run would be handed.
func (e *resolveEnv) sendKind(chatID, kind string, content json.RawMessage) ([]ChatAPIMessage, error) {
	history, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		UserMessage: content,
		Settings:    ChatRunSettings{Model: "m", Kind: kind},
	})
	return history, err
}

// storedBlobIDs is the id of every blob the stored document references, in order.
func (e *resolveEnv) storedBlobIDs(t *testing.T, chatID string) []string {
	t.Helper()
	var ids []string
	for _, u := range storedImageURLs(t, e.stored(t, chatID)) {
		ids = append(ids, strings.TrimPrefix(u, gatewayBlobPrefix))
	}
	return ids
}

// historyImageURLs is every image_url.url in a run history, in order.
func historyImageURLs(history []ChatAPIMessage) []string {
	var urls []string
	for _, m := range history {
		var parts []struct {
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.ImageURL != nil {
				urls = append(urls, p.ImageURL.URL)
			}
		}
	}
	return urls
}

// decodeResolvedURL splits a resolved url into its media type and bytes, and
// insists it is exactly the form the remote providers accept: data:<type>;base64
// with the STANDARD padded alphabet, canonically encoded.
func decodeResolvedURL(t *testing.T, url string) (string, []byte) {
	t.Helper()
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		t.Fatalf("url %.80q is not a data: url", url)
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		t.Fatalf("data url %.80q has no payload", url)
	}
	mediaType, flag, ok := strings.Cut(meta, ";")
	if !ok || flag != "base64" {
		t.Fatalf("data url header %q, want <type>;base64", meta)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not standard padded base64: %v", err)
	}
	if base64.StdEncoding.EncodeToString(raw) != payload {
		t.Fatalf("payload is not canonical standard base64")
	}
	return mediaType, raw
}

func requireNoGatewayURL(t *testing.T, history []ChatAPIMessage) {
	t.Helper()
	for i, m := range history {
		if strings.Contains(string(m.Content), "chat-blobs") {
			t.Fatalf("history message %d still carries a gateway url: %.200s", i, m.Content)
		}
	}
}

// A later vision turn over a thread whose stored history references images by
// url is handed those images as data: urls with the real bytes, whichever
// content type each one has and whether or not the store seals them. The stored
// document is not touched by that: it keeps the small references.
func TestPrepareChatRunResolvesStoredBlobRefsToDataURLs(t *testing.T) {
	for _, withCipher := range []bool{true, false} {
		t.Run(fmt.Sprintf("cipher=%v", withCipher), func(t *testing.T) {
			e := newResolveEnv(t, withCipher, 0)
			chatID := e.emptyChat(t)
			images := []struct {
				mediaType string
				raw       []byte
			}{
				{"image/png", blobTestBytes(4096, 1)},
				{"image/jpeg", blobTestBytes(5001, 2)},
				{"image/webp", blobTestBytes(77, 3)},
				{"image/gif", blobTestBytes(3, 4)},
			}
			for _, img := range images {
				if _, err := e.send(chatID, imageContent("look", dataURL(img.mediaType, img.raw))); err != nil {
					t.Fatalf("PrepareChatRun: %v", err)
				}
			}
			history, err := e.send(chatID, json.RawMessage(`"and now?"`))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}
			urls := historyImageURLs(history)
			if len(urls) != len(images) {
				t.Fatalf("history carries %d image urls, want %d", len(urls), len(images))
			}
			for i, img := range images {
				mediaType, raw := decodeResolvedURL(t, urls[i])
				if mediaType != img.mediaType || !bytes.Equal(raw, img.raw) {
					t.Fatalf("image %d resolved to %s with %d bytes, want %s with the %d uploaded bytes", i, mediaType, len(raw), img.mediaType, len(img.raw))
				}
			}
			requireNoGatewayURL(t, history)
			if got := MessageText(history[0].Content); got != "look" {
				t.Fatalf("the text part next to the image became %q, want it kept", got)
			}

			// What was stored is the reference, still: resolution is for the run only.
			storedAfter := e.stored(t, chatID)
			if strings.Contains(string(storedAfter), "data:") {
				t.Fatalf("the stored document gained inline bytes: %.200s", storedAfter)
			}
			if got := storedImageURLs(t, storedAfter); len(got) != len(images) || !strings.HasPrefix(got[0], gatewayBlobPrefix) {
				t.Fatalf("stored urls = %v, want the %d gateway references", got, len(images))
			}
		})
	}
}

// The resolved url is standard, padded base64 whatever the payload length: the
// three residues of length mod 3 produce no padding, "=" and "==", and what
// comes back decodes to exactly the stored bytes.
func TestPrepareChatRunResolvedBase64IsStandardAndPadded(t *testing.T) {
	e := newResolveEnv(t, true, 0)
	chatID := e.emptyChat(t)
	sizes := []int{1, 2, 3, 4, 5, 255, 256, 257}
	for i, n := range sizes {
		if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(n, byte(i))))); err != nil {
			t.Fatalf("PrepareChatRun: %v", err)
		}
	}
	history, err := e.send(chatID, json.RawMessage(`"go"`))
	if err != nil {
		t.Fatal(err)
	}
	urls := historyImageURLs(history)
	if len(urls) != len(sizes) {
		t.Fatalf("history has %d images, want %d", len(urls), len(sizes))
	}
	padded := map[string]bool{}
	for i, n := range sizes {
		_, raw := decodeResolvedURL(t, urls[i])
		if !bytes.Equal(raw, blobTestBytes(n, byte(i))) {
			t.Fatalf("image %d (%d bytes) did not round trip", i, n)
		}
		switch {
		case strings.HasSuffix(urls[i], "=="):
			padded["=="] = true
		case strings.HasSuffix(urls[i], "="):
			padded["="] = true
		default:
			padded[""] = true
		}
	}
	if len(padded) != 3 {
		t.Fatalf("the sizes must cover all three padding shapes, got %v", padded)
	}
}

// N references are one store call, not N, and it is scoped to the run's chat.
// The call asks for the older references only: the turn being sent carries its
// bytes inline already (they are in hand; nothing to fetch), and is handed to the
// run exactly as it was submitted.
func TestPrepareChatRunResolvesAllRefsInOneBatchedCall(t *testing.T) {
	e := newResolveEnv(t, true, 0)
	chatID := e.emptyChat(t)
	for i := 0; i < 5; i++ {
		if _, err := e.send(chatID, imageContent("look", dataURL("image/png", blobTestBytes(300+i, byte(i))))); err != nil {
			t.Fatal(err)
		}
	}
	want := e.storedBlobIDs(t, chatID)
	e.byIDs.calls = nil

	newURL := dataURL("image/jpeg", blobTestBytes(900, 9))
	history, err := e.send(chatID, imageContent("this one is new", newURL))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.byIDs.calls) != 1 {
		t.Fatalf("resolving 5 references took %d ChatBlobsByIDs calls, want exactly 1", len(e.byIDs.calls))
	}
	call := e.byIDs.calls[0]
	if call.chatID != chatID {
		t.Fatalf("the batch was scoped to chat %q, want %q", call.chatID, chatID)
	}
	got := slices.Clone(call.ids)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the batch asked for %v, want the 5 older ids %v (and not the new turn's inline image)", got, want)
	}

	urls := historyImageURLs(history)
	if len(urls) != 6 {
		t.Fatalf("history has %d image urls, want 6", len(urls))
	}
	if urls[5] != newURL {
		t.Fatalf("the new turn's url changed: the run must be handed the bytes as submitted")
	}
	for i := 0; i < 5; i++ {
		if _, raw := decodeResolvedURL(t, urls[i]); !bytes.Equal(raw, blobTestBytes(300+i, byte(i))) {
			t.Fatalf("older image %d did not resolve to its bytes", i)
		}
	}
}

// An image referenced more than once is asked for once and resolved everywhere
// it appears. The same holds for an edited history, which also hands the run its
// own inline images untouched.
func TestPrepareChatRunEditedHistoryResolvesRefsOnceAndKeepsInlineParts(t *testing.T) {
	e := newResolveEnv(t, false, 0)
	chatID := e.emptyChat(t)
	raw := blobTestBytes(600, 7)
	id := e.sendImage(t, chatID, raw)
	e.byIDs.calls = nil

	inline := dataURL("image/gif", blobTestBytes(40, 1))
	msg := func(role, url string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"id":"m","role":%q,"content":[{"type":"image_url","image_url":{"url":%q}}]}`, role, url))
	}
	history, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{
			msg("user", gatewayBlobPrefix+id),
			msg("assistant", inline),
			msg("user", gatewayBlobPrefix+id),
		},
		Settings: ChatRunSettings{Model: "m"},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if len(e.byIDs.calls) != 1 || len(e.byIDs.calls[0].ids) != 1 || e.byIDs.calls[0].ids[0] != id {
		t.Fatalf("calls = %+v, want one call for the single distinct id %s", e.byIDs.calls, id)
	}
	urls := historyImageURLs(history)
	if len(urls) != 3 || urls[1] != inline {
		t.Fatalf("urls = %.60v, want the inline part in the middle untouched", urls)
	}
	for _, i := range []int{0, 2} {
		if _, got := decodeResolvedURL(t, urls[i]); !bytes.Equal(got, raw) {
			t.Fatalf("occurrence %d did not resolve to the stored bytes", i)
		}
	}
}

// A history with nothing to resolve makes no store call at all: a thread of
// text, a first turn with an inline upload, and a legacy thread whose images are
// still inline.
func TestPrepareChatRunWithNoRefsDoesNotTouchTheBlobStore(t *testing.T) {
	e := newResolveEnv(t, true, 0)

	text := e.emptyChat(t)
	if _, err := e.send(text, json.RawMessage(`"hello"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.send(text, json.RawMessage(`"again"`)); err != nil {
		t.Fatal(err)
	}

	upload := e.emptyChat(t)
	if _, err := e.send(upload, imageContent("look", dataURL("image/png", blobTestBytes(64, 1)))); err != nil {
		t.Fatal(err)
	}

	legacy := e.newChat(t, fmt.Sprintf(`{"settings":{},"messages":[{"id":"u1","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}]}`, dataURL("image/png", blobTestBytes(64, 2))))
	history, err := e.send(legacy, json.RawMessage(`"what is this?"`))
	if err != nil {
		t.Fatal(err)
	}
	if urls := historyImageURLs(history); len(urls) != 1 || !strings.HasPrefix(urls[0], "data:image/png;base64,") {
		t.Fatalf("a legacy inline image must reach the run as it always did: %.80v", urls)
	}
	if len(e.byIDs.calls) != 0 {
		t.Fatalf("%d ChatBlobsByIDs calls for histories with no gateway reference, want 0", len(e.byIDs.calls))
	}
}

// An image thread's run reads only the last prompt's text, so it resolves
// nothing: no store call, and the history keeps its references. The same
// references in a text thread are resolved, so it is the kind alone that decides.
func TestPrepareChatRunImageKindDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	generated := func() json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, dataURL("image/png", blobTestBytes(2048, 5))))
	}

	t.Run("image thread", func(t *testing.T) {
		e := newResolveEnv(t, true, 0)
		chatID := e.emptyChat(t)
		if _, err := e.sendKind(chatID, "image", json.RawMessage(`"a cat"`)); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: generated()}, "complete"); err != nil {
			t.Fatal(err)
		}
		e.byIDs.calls = nil

		history, err := e.sendKind(chatID, "image", json.RawMessage(`"make it blue"`))
		if err != nil {
			t.Fatal(err)
		}
		if len(e.byIDs.calls) != 0 {
			t.Fatalf("an image run issued %d ChatBlobsByIDs calls, want 0", len(e.byIDs.calls))
		}
		urls := historyImageURLs(history)
		if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) {
			t.Fatalf("image history urls = %.80v, want the stored reference left as it is", urls)
		}
		if got := MessageText(history[len(history)-1].Content); got != "make it blue" {
			t.Fatalf("the last prompt = %q: an image run still reads its text", got)
		}
	})

	t.Run("same thread as text", func(t *testing.T) {
		e := newResolveEnv(t, true, 0)
		chatID := e.emptyChat(t)
		if _, err := e.send(chatID, json.RawMessage(`"a cat"`)); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: generated()}, "complete"); err != nil {
			t.Fatal(err)
		}
		e.byIDs.calls = nil

		history, err := e.send(chatID, json.RawMessage(`"describe it"`))
		if err != nil {
			t.Fatal(err)
		}
		if len(e.byIDs.calls) != 1 {
			t.Fatalf("a text run issued %d ChatBlobsByIDs calls, want 1", len(e.byIDs.calls))
		}
		urls := historyImageURLs(history)
		if len(urls) != 1 {
			t.Fatalf("urls = %v", urls)
		}
		if _, raw := decodeResolvedURL(t, urls[0]); !bytes.Equal(raw, blobTestBytes(2048, 5)) {
			t.Fatalf("the generated image did not resolve to its bytes")
		}
	})
}

// The bound is on the bytes the history carries, exactly: a history that reaches
// it runs, one byte more is refused with its own error, and the refused turn
// stores nothing -- the document and the blob table are as the run found them,
// so a retry is not a duplicate.
func TestPrepareChatRunRefusesAHistoryOverTheRunBound(t *testing.T) {
	e := newResolveEnv(t, false, 1000)
	chatID := e.emptyChat(t)
	for _, n := range []int{400, 400} {
		if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(n, 1)))); err != nil {
			t.Fatalf("under the bound: %v", err)
		}
	}
	if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(200, 2)))); err != nil {
		t.Fatalf("a history of 800 resolved bytes (bound 1000): %v", err)
	}
	// 400 + 400 + 200 = 1000: exactly the bound is accepted. This turn adds the
	// image that makes the NEXT run's history 1001.
	if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(1, 3)))); err != nil {
		t.Fatalf("a history of exactly the bound (1000): %v", err)
	}
	before := string(e.stored(t, chatID))
	saves, blobs := len(e.rec.saves), e.rec.storedBlobs()

	history, err := e.send(chatID, imageContent("one too many", dataURL("image/png", blobTestBytes(5, 4))))
	if !errors.Is(err, ErrChatRunHistoryTooLarge) {
		t.Fatalf("PrepareChatRun = %v (history %d messages), want ErrChatRunHistoryTooLarge", err, len(history))
	}
	if history != nil {
		t.Fatalf("a refused run returned a history")
	}
	if string(e.stored(t, chatID)) != before || len(e.rec.saves) != saves || e.rec.storedBlobs() != blobs {
		t.Fatalf("a refused run stored something: %d saves, %d blobs", len(e.rec.saves)-saves, e.rec.storedBlobs()-blobs)
	}
}

// The bound counts per occurrence, because the request carries an image once per
// occurrence: one stored image referenced twice counts twice.
func TestPrepareChatRunBoundCountsEveryOccurrence(t *testing.T) {
	e := newResolveEnv(t, false, 1000)
	chatID := e.emptyChat(t)
	id := e.sendImage(t, chatID, blobTestBytes(600, 1))
	part := func() json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"id":"m","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}`, gatewayBlobPrefix+id))
	}
	edit := func(msgs ...json.RawMessage) error {
		_, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
			EditedHistory: msgs, Settings: ChatRunSettings{Model: "m"},
		})
		return err
	}
	if err := edit(part()); err != nil {
		t.Fatalf("one reference (600 of 1000 bytes): %v", err)
	}
	if err := edit(part(), part()); !errors.Is(err, ErrChatRunHistoryTooLarge) {
		t.Fatalf("two references (1200 of 1000 bytes): err = %v, want ErrChatRunHistoryTooLarge", err)
	}
}

// byte_len is metadata the store keeps next to the payload; a row that
// understates it must not let a larger payload past the bound, so the bytes
// actually unsealed are counted as well.
func TestPrepareChatRunBoundAlsoCountsTheBytesActuallyUnsealed(t *testing.T) {
	e := newResolveEnv(t, true, 100)
	chatID := e.emptyChat(t)
	if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(90, 1)))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(90, 2)))); err != nil {
		t.Fatalf("a history of 90 bytes (bound 100): %v", err)
	}
	// Two images of 90 bytes are 180 > 100; the store claims 1 byte each.
	e.byIDs.rewrite = func(got map[string]store.ChatBlob) {
		for id, row := range got {
			row.ByteLen = 1
			got[id] = row
		}
	}
	if _, err := e.send(chatID, json.RawMessage(`"go"`)); !errors.Is(err, ErrChatRunHistoryTooLarge) {
		t.Fatalf("PrepareChatRun = %v, want ErrChatRunHistoryTooLarge despite the understated byte_len", err)
	}
}

func TestNewServiceRunHistoryBoundDefaultsTo32MiB(t *testing.T) {
	for _, configured := range []int{0, -1} {
		if got := NewService(ServiceDeps{MaxRunHistoryBytes: configured}).maxRunHistoryBytes; got != 32<<20 {
			t.Fatalf("MaxRunHistoryBytes %d -> %d, want the 32 MiB default", configured, got)
		}
	}
	if got := NewService(ServiceDeps{MaxRunHistoryBytes: 4096}).maxRunHistoryBytes; got != 4096 {
		t.Fatalf("an explicit MaxRunHistoryBytes must be honoured, got %d", got)
	}
}

// A reference this chat does not hold is never resolved, whoever's blob it is:
// create and a bare autosave store whatever urls they are given, so a transcript
// can carry another user's (or another chat's) blob id, and the run must not turn
// that into bytes. The part is replaced by a short text, the run goes on, and the
// stored document is left alone (the stored reference grants nothing; it is the
// run's history that is made safe).
func TestPrepareChatRunNeverResolvesABlobTheChatDoesNotHold(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 0)
	secret := blobTestBytes(2048, 77)
	secretID := e.sendImage(t, e.emptyChat(t), secret) // user A's chat holds this blob
	secretPrefix := base64.StdEncoding.EncodeToString(secret[:48])

	graft := func(url string) string {
		return fmt.Sprintf(`{"settings":{},"messages":[{"id":"u1","role":"user","content":[{"type":"text","text":"mine"},{"type":"image_url","image_url":{"url":%q}}]},{"id":"a1","role":"assistant","content":"ok"}]}`, url)
	}
	userB := chatToken("usr_b")
	userBChat, err := e.svc.CreateChat(ctx, userB, CreateChatRequest{Content: json.RawMessage(graft(gatewayBlobPrefix + secretID))})
	if err != nil {
		t.Fatal(err)
	}
	sameUserChat := e.newChat(t, graft(gatewayBlobPrefix+secretID)) // A's other chat

	for _, tc := range []struct {
		name   string
		owner  auth.Token
		chatID string
	}{
		{"another user's chat", userB, userBChat.ID},
		{"another chat of the owner", e.owner, sameUserChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.byIDs.calls = nil
			before := docMessages(t, mustContent(t, e.svc, tc.owner, tc.chatID))

			history, _, err := e.svc.PrepareChatRun(ctx, tc.owner, tc.chatID, PrepareRunRequest{
				UserMessage: json.RawMessage(`"and?"`), Settings: ChatRunSettings{Model: "m"},
			})
			if err != nil {
				t.Fatalf("PrepareChatRun: %v (a foreign reference must not fail the run)", err)
			}
			if len(e.byIDs.calls) != 1 || e.byIDs.calls[0].chatID != tc.chatID {
				t.Fatalf("calls = %+v, want one batch scoped to chat %s", e.byIDs.calls, tc.chatID)
			}
			for i, m := range history {
				s := string(m.Content)
				if strings.Contains(s, "data:") || strings.Contains(s, secretPrefix) || strings.Contains(s, "chat-blobs") {
					t.Fatalf("history message %d carries the foreign image or its url: %.200s", i, s)
				}
			}
			if got := MessageText(history[0].Content); got != "mine" {
				t.Fatalf("the text next to the dropped image = %q, want it kept", got)
			}
			if !strings.Contains(string(history[0].Content), unavailableImageText) {
				t.Fatalf("the dropped image left no trace for the model: %s", history[0].Content)
			}
			after := docMessages(t, mustContent(t, e.svc, tc.owner, tc.chatID))
			if len(after) != len(before)+1 || !slices.Equal(after[:len(before)], before) {
				t.Fatalf("the stored messages were rewritten:\n before %v\n after  %v", before, after)
			}
		})
	}

	// Next to an image the chat does hold, only the foreign one is dropped.
	mixed := e.emptyChat(t)
	ownRaw := blobTestBytes(512, 5)
	e.sendImage(t, mixed, ownRaw)
	doc := docMessages(t, e.stored(t, mixed))
	doc = append(doc, fmt.Sprintf(`{"id":"u9","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}`, gatewayBlobPrefix+secretID))
	content := json.RawMessage(`{"settings":{},"messages":[` + strings.Join(doc, ",") + `]}`)
	if _, err := e.svc.SaveChat(ctx, e.owner, mixed, UpdateChatRequest{Content: &content}); err != nil {
		t.Fatal(err)
	}
	out, err := e.send(mixed, json.RawMessage(`"go"`))
	if err != nil {
		t.Fatal(err)
	}
	urls := historyImageURLs(out)
	if len(urls) != 1 {
		t.Fatalf("urls = %.80v, want only the chat's own image to resolve", urls)
	}
	if _, got := decodeResolvedURL(t, urls[0]); !bytes.Equal(got, ownRaw) {
		t.Fatalf("the chat's own image did not resolve to its bytes")
	}
	for i, m := range out {
		if strings.Contains(string(m.Content), secretPrefix) {
			t.Fatalf("history message %d carries the foreign image", i)
		}
	}
}

// docMessages is each message of a stored document, as the raw JSON string.
func docMessages(t *testing.T, content json.RawMessage) []string {
	t.Helper()
	var doc chatDoc
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(doc.Messages))
	for i, m := range doc.Messages {
		out[i] = string(m)
	}
	return out
}

// The service does not rely on the store's chat scoping alone. If the store
// answers with a row that belongs to another chat, or with a type that is not an
// image type, the part is not resolved: no other chat's bytes, and no
// data:text/html, ever reach an upstream.
func TestPrepareChatRunDoesNotTrustAStoreThatMisreports(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(row *store.ChatBlob)
	}{
		{"row of another chat", func(row *store.ChatBlob) { row.ChatID = "chat_someone_else" }},
		{"unlisted type", func(row *store.ChatBlob) { row.ContentType = "text/html" }},
		{"svg", func(row *store.ChatBlob) { row.ContentType = "image/svg+xml" }},
		{"type with parameters", func(row *store.ChatBlob) { row.ContentType = "image/png; charset=utf-8" }},
		{"empty type", func(row *store.ChatBlob) { row.ContentType = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newResolveEnv(t, true, 0)
			chatID := e.emptyChat(t)
			raw := blobTestBytes(300, 8)
			if _, err := e.send(chatID, imageContent("look", dataURL("image/png", raw))); err != nil {
				t.Fatal(err)
			}
			e.byIDs.rewrite = func(got map[string]store.ChatBlob) {
				for id, row := range got {
					tc.mutate(&row)
					got[id] = row
				}
			}
			history, err := e.send(chatID, json.RawMessage(`"go"`))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}
			requireNoGatewayURL(t, history)
			if urls := historyImageURLs(history); len(urls) != 0 {
				t.Fatalf("a misreported row was resolved: %.100v", urls)
			}
			if !strings.Contains(string(history[0].Content), unavailableImageText) {
				t.Fatalf("no placeholder for the dropped image: %s", history[0].Content)
			}
		})
	}
}

// A reference whose blob is gone -- a RAM store evicted it to stay inside its
// budget, or it never existed -- must not stop the conversation: the part is
// replaced by a short text and the rest of the history is sent.
func TestPrepareChatRunReplacesAnUnavailableImageAndKeepsTheRunGoing(t *testing.T) {
	e := newResolveEnv(t, false, 0)
	chatID := e.newChat(t, fmt.Sprintf(`{"settings":{},"messages":[{"id":"u1","role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":%q,"detail":"high"}}]},{"id":"a1","role":"assistant","content":"a cat"}]}`, gatewayBlobPrefix+"blob_"+strings.Repeat("0", 32)))

	history, err := e.send(chatID, json.RawMessage(`"and a dog?"`))
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d messages, want 3", len(history))
	}
	want := `[{"type":"text","text":"see"},{"type":"text","text":"` + unavailableImageText + `"}]`
	if got := string(history[0].Content); got != want {
		t.Fatalf("message 0 = %s, want %s", got, want)
	}
}

// A store failure is a failure of the run, not an empty image: the batch's error
// is returned, and nothing of the turn is stored. A payload that does not open is
// the same.
func TestPrepareChatRunSurfacesBlobReadFailures(t *testing.T) {
	setup := func(t *testing.T) (*resolveEnv, string) {
		e := newResolveEnv(t, true, 0)
		chatID := e.emptyChat(t)
		if _, err := e.send(chatID, imageContent("x", dataURL("image/png", blobTestBytes(100, 1)))); err != nil {
			t.Fatal(err)
		}
		return e, chatID
	}
	t.Run("store error", func(t *testing.T) {
		e, chatID := setup(t)
		boom := errors.New("db down")
		e.byIDs.err = boom
		before := string(e.stored(t, chatID))
		saves := len(e.rec.saves)
		if _, err := e.send(chatID, json.RawMessage(`"go"`)); !errors.Is(err, boom) {
			t.Fatalf("PrepareChatRun = %v, want the store error", err)
		}
		if string(e.stored(t, chatID)) != before || len(e.rec.saves) != saves {
			t.Fatalf("a failed resolution stored the turn")
		}
	})
	t.Run("payload that does not open", func(t *testing.T) {
		e, chatID := setup(t)
		e.byIDs.rewrite = func(got map[string]store.ChatBlob) {
			for id, row := range got {
				row.Blob = []byte("not a sealed payload at all, nonce missing or tag wrong")
				got[id] = row
			}
		}
		before := string(e.stored(t, chatID))
		history, err := e.send(chatID, json.RawMessage(`"go"`))
		if err == nil || history != nil {
			t.Fatalf("PrepareChatRun = %v with %d messages, want the unseal error", err, len(history))
		}
		if errors.Is(err, ErrChatRunHistoryTooLarge) || errors.Is(err, ErrChatBlobNotFound) {
			t.Fatalf("err = %v: a broken payload must not look like a verdict", err)
		}
		if string(e.stored(t, chatID)) != before {
			t.Fatalf("a failed resolution stored the turn")
		}
	})
	t.Run("sealed payload with no cipher", func(t *testing.T) {
		e, chatID := setup(t)
		// A service with no cipher cannot open the sealed chat either, so the
		// sealed payload is exercised on the resolver directly.
		bare := NewService(ServiceDeps{Chats: e.byIDs})
		history := []ChatAPIMessage{{Role: "user", Content: imageContent("x", gatewayBlobPrefix+e.storedBlobIDs(t, chatID)[0])}}
		if _, err := bare.resolveChatBlobRefs(context.Background(), chatID, history); !errors.Is(err, ErrChatCipherMissing) {
			t.Fatalf("resolveChatBlobRefs = %v, want ErrChatCipherMissing", err)
		}
	})
}

// Only the image url of an image part is touched. The rest of a part, the other
// parts of a message, string contents and the system prompt come through as they
// were, with keys in their original order and spelling (an upstream reads keys
// case-insensitively, so the resolver must find an image part spelled any way).
func TestPrepareChatRunResolutionLeavesTheRestOfTheHistoryAlone(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, false, 0)
	chatID := e.emptyChat(t)
	raw := blobTestBytes(150, 4)
	id := e.sendImage(t, chatID, raw)
	want := dataURL("image/png", raw)

	history, _, err := e.svc.PrepareChatRun(ctx, e.owner, chatID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{
			json.RawMessage(`{"id":"1","role":"user","content":"plain text"}`),
			json.RawMessage(fmt.Sprintf(`{"id":"2","role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":%q,"detail":"high"}},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}`, gatewayBlobPrefix+id)),
			json.RawMessage(fmt.Sprintf(`{"id":"3","role":"user","CONTENT":[{"type":"image_url","Image_Url":{"URL":%q,"detail":"low"}}]}`, gatewayBlobPrefix+id)),
			json.RawMessage(fmt.Sprintf(`{"id":"4","role":"user","content":[{"type":"image_url","image_url":{"url":"\/api\/portal\/chat-blobs\/%s"}}]}`, id)),
		},
		Settings: ChatRunSettings{Model: "m", SystemPrompt: "be brief"},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if len(history) != 5 {
		t.Fatalf("history has %d messages, want the system prompt and 4", len(history))
	}
	for i, tc := range []struct{ role, content string }{
		{"system", `"be brief"`},
		{"user", `"plain text"`},
		{"user", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"` + want + `","detail":"high"}},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]`},
		{"user", `[{"type":"image_url","Image_Url":{"URL":"` + want + `","detail":"low"}}]`},
		{"user", `[{"type":"image_url","image_url":{"url":"` + want + `"}}]`},
	} {
		if history[i].Role != tc.role || string(history[i].Content) != tc.content {
			t.Fatalf("history[%d] = %s %.160s\n want      %s %.160s", i, history[i].Role, history[i].Content, tc.role, tc.content)
		}
	}
}
