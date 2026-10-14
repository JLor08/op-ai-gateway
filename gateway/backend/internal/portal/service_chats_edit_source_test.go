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

// An image thread's turn whose LAST user message carries an image is an edit, and
// the executor has to send that image's bytes. The image run's history is not
// resolved wholesale (the executor reads nothing of it but the last prompt and
// this source), so PrepareChatRun resolves the last user message alone -- through
// the same chat-scoped, bounded resolution a text run uses -- and the executor
// finds a data: url there whether the source was just uploaded or is a stored
// reference.
func TestPrepareChatRunImageKindResolvesTheEditSource(t *testing.T) {
	ctx := context.Background()
	for _, withCipher := range []bool{true, false} {
		t.Run(fmt.Sprintf("cipher=%v", withCipher), func(t *testing.T) {
			e := newResolveEnv(t, withCipher, 0)
			chatID := e.emptyChat(t)
			source := blobTestBytes(3000, 11)

			// A fresh upload arrives inline and is handed to the run as it was sent:
			// no round trip through the store for bytes already in hand.
			history, err := e.sendKind(chatID, "image", imageContent("make it blue", dataURL("image/png", source)))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}
			if len(e.byIDs.calls) != 0 {
				t.Fatalf("a fresh upload issued %d ChatBlobsByIDs calls, want 0", len(e.byIDs.calls))
			}
			urls := historyImageURLs(history)
			if len(urls) != 1 {
				t.Fatalf("history carries %d image urls, want the one source", len(urls))
			}
			if mediaType, raw := decodeResolvedURL(t, urls[0]); mediaType != "image/png" || !bytes.Equal(raw, source) {
				t.Fatalf("the fresh source arrived as %s with %d bytes, want the %d uploaded bytes", mediaType, len(raw), len(source))
			}

			// The stored copy is a reference. A later turn that names it as its source
			// (the shape a refine takes, and a replayed edit) gets the bytes back.
			ids := e.storedBlobIDs(t, chatID)
			if len(ids) != 1 {
				t.Fatalf("stored blob ids = %v, want one", ids)
			}
			if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: imageContent("", dataURL("image/png", blobTestBytes(64, 3)))}, "complete"); err != nil {
				t.Fatal(err)
			}
			e.byIDs.calls = nil

			history, err = e.sendKind(chatID, "image", imageContent("now greener", gatewayBlobPrefix+ids[0]))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}
			if len(e.byIDs.calls) != 1 || e.byIDs.calls[0].chatID != chatID || !slices.Equal(e.byIDs.calls[0].ids, ids) {
				t.Fatalf("calls = %+v, want ONE batch scoped to chat %s for exactly the source %v (the older images are not fetched)", e.byIDs.calls, chatID, ids)
			}
			last := history[len(history)-1]
			if got := MessageText(last.Content); got != "now greener" {
				t.Fatalf("the prompt next to the source = %q, want it kept", got)
			}
			lastURLs := messageImageURLs(last.Content)
			if len(lastURLs) != 1 {
				t.Fatalf("the last user message carries %d image urls, want 1", len(lastURLs))
			}
			if mediaType, raw := decodeResolvedURL(t, lastURLs[0]); mediaType != "image/png" || !bytes.Equal(raw, source) {
				t.Fatalf("the stored source resolved to %s with %d bytes, want the %d uploaded bytes", mediaType, len(raw), len(source))
			}
			// The earlier turns keep their references: only the source is fetched.
			for i, m := range history[:len(history)-1] {
				for _, u := range messageImageURLs(m.Content) {
					if !strings.HasPrefix(u, gatewayBlobPrefix) {
						t.Fatalf("history message %d was resolved (%.60s), want only the last user message resolved", i, u)
					}
				}
			}

			// And the stored document is untouched by it: resolution is for the run.
			if strings.Contains(string(e.stored(t, chatID)), "data:") {
				t.Fatal("the stored document gained inline bytes")
			}
		})
	}
}

// A regenerate or an edit replays the history the browser holds, whose images are
// the references of the saved transcript. The last user message's source resolves
// the same way.
func TestPrepareChatRunImageKindResolvesTheEditSourceOfAReplayedHistory(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 0)
	chatID := e.emptyChat(t)
	source := blobTestBytes(1200, 21)
	if _, err := e.sendKind(chatID, "image", imageContent("edit me", dataURL("image/jpeg", source))); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: imageContent("", dataURL("image/png", blobTestBytes(80, 4)))}, "complete"); err != nil {
		t.Fatal(err)
	}
	stored := docMessages(t, e.stored(t, chatID))
	if len(stored) != 2 {
		t.Fatalf("stored %d messages, want the user turn and the assistant turn", len(stored))
	}
	e.byIDs.calls = nil

	// Regenerate: the history up to the user turn, exactly as the browser holds it.
	history, _, err := e.svc.PrepareChatRun(ctx, e.owner, chatID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{json.RawMessage(stored[0])},
		Settings:      ChatRunSettings{Model: "m", Kind: "image"},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	urls := historyImageURLs(history)
	if len(urls) != 1 {
		t.Fatalf("urls = %.80v, want the one source", urls)
	}
	if mediaType, raw := decodeResolvedURL(t, urls[0]); mediaType != "image/jpeg" || !bytes.Equal(raw, source) {
		t.Fatalf("the replayed source resolved to %s with %d bytes, want the %d uploaded bytes", mediaType, len(raw), len(source))
	}
	requireNoGatewayURL(t, history)
}

// An image thread's older messages are never resolved, a source they carried
// included: only the LAST user message is the turn's source. A turn whose last
// user message is plain text is a generation and touches the blob store not at all.
func TestPrepareChatRunImageKindLeavesAnOlderSourceAlone(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 0)
	chatID := e.emptyChat(t)
	if _, err := e.sendKind(chatID, "image", imageContent("edit me", dataURL("image/png", blobTestBytes(500, 5)))); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: imageContent("", dataURL("image/png", blobTestBytes(80, 6)))}, "complete"); err != nil {
		t.Fatal(err)
	}
	e.byIDs.calls = nil

	history, err := e.sendKind(chatID, "image", json.RawMessage(`"a fresh wish"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.byIDs.calls) != 0 {
		t.Fatalf("a generation turn issued %d ChatBlobsByIDs calls, want 0", len(e.byIDs.calls))
	}
	for i, m := range history {
		for _, u := range messageImageURLs(m.Content) {
			if !strings.HasPrefix(u, gatewayBlobPrefix) {
				t.Fatalf("history message %d was resolved (%.60s), want the older images left as references", i, u)
			}
		}
	}
	if got := MessageText(history[len(history)-1].Content); got != "a fresh wish" {
		t.Fatalf("the last prompt = %q", got)
	}
}

// A source the chat does not hold is never turned into bytes, whoever's it is --
// and, unlike an older image of a text thread (which becomes a placeholder and the
// run goes on), it fails the run: an edit whose source silently vanished would
// become a plain generation of an unrelated image. The refusal is the error a
// foreign reference already is at run start, before anything is stored.
func TestPrepareChatRunImageKindRefusesAnEditSourceTheChatDoesNotHold(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 0)
	secret := blobTestBytes(2048, 77)
	secretID := e.sendImage(t, e.emptyChat(t), secret) // this chat's owner holds the blob in ANOTHER chat

	graft := func(url string) string {
		return fmt.Sprintf(`{"settings":{"kind":"image"},"messages":[{"id":"u1","role":"user","content":[{"type":"text","text":"mine"},{"type":"image_url","image_url":{"url":%q}}]}]}`, url)
	}
	foreignRef := gatewayBlobPrefix + secretID
	missingRef := gatewayBlobPrefix + "blob_" + strings.Repeat("0", 32)

	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"another chat's blob", foreignRef},
		{"a blob that never existed", missingRef},
	} {
		t.Run("stored last message: "+tc.name, func(t *testing.T) {
			// A transcript can carry any id (create and a bare autosave store whatever
			// they are given), so this is the shape a run that supplies no message of
			// its own sees: the stored last user message, not vetted at this request.
			chatID := e.newChat(t, graft(tc.ref))
			before := docMessages(t, mustContent(t, e.svc, e.owner, chatID))
			history, _, err := e.svc.PrepareChatRun(ctx, e.owner, chatID, PrepareRunRequest{Settings: ChatRunSettings{Model: "m", Kind: "image"}})
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("err = %v, want ErrChatBlobNotOwned (and no history)", err)
			}
			if history != nil {
				t.Fatalf("a refused source still returned a history: %.200s", history)
			}
			after := docMessages(t, mustContent(t, e.svc, e.owner, chatID))
			if !slices.Equal(before, after) {
				t.Fatalf("a refused run changed the stored document:\n before %v\n after  %v", before, after)
			}
		})
		t.Run("new message: "+tc.name, func(t *testing.T) {
			chatID := e.emptyChat(t)
			_, err := e.sendKind(chatID, "image", imageContent("mine", tc.ref))
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("err = %v, want ErrChatBlobNotOwned", err)
			}
		})
	}

	// The service does not trust the store's chat scoping alone either: a row of
	// another chat, or of a type that is not an allow-listed image type, is not a
	// source -- the refusal, not a placeholder the run would carry on with.
	for _, tc := range []struct {
		name   string
		mutate func(row *store.ChatBlob)
	}{
		{"row of another chat", func(row *store.ChatBlob) { row.ChatID = "chat_someone_else" }},
		{"unlisted type", func(row *store.ChatBlob) { row.ContentType = "text/html" }},
	} {
		t.Run("misreporting store: "+tc.name, func(t *testing.T) {
			own := newResolveEnv(t, true, 0)
			chatID := own.emptyChat(t)
			// The thread is pinned "image" by its first send: a text thread would be
			// resolved with the tolerance of a text run.
			if _, err := own.sendKind(chatID, "image", imageContent("first", dataURL("image/png", blobTestBytes(300, 8)))); err != nil {
				t.Fatal(err)
			}
			id := own.storedBlobIDs(t, chatID)[0]
			own.byIDs.rewrite = func(got map[string]store.ChatBlob) {
				for k, row := range got {
					tc.mutate(&row)
					got[k] = row
				}
			}
			_, _, err := own.svc.PrepareChatRun(ctx, own.owner, chatID, PrepareRunRequest{
				UserMessage: imageContent("again", gatewayBlobPrefix+id),
				Settings:    ChatRunSettings{Model: "m", Kind: "image"},
			})
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("err = %v, want ErrChatBlobNotOwned", err)
			}
		})
	}
}

// DecodeImageDataURL is the ONE definition of an inline image url: the run commit
// that stores an upload and the executor that sends it as an edit's source parse
// the same text, so the executor can never accept a spelling the store refused.
func TestDecodeImageDataURL(t *testing.T) {
	raw := blobTestBytes(97, 9)
	payload := base64.StdEncoding.EncodeToString(raw)
	for _, tc := range []struct {
		name     string
		url      string
		wantType string
		wantErr  bool
	}{
		{"png", dataURL("image/png", raw), "image/png", false},
		{"jpeg", dataURL("image/jpeg", raw), "image/jpeg", false},
		{"webp", dataURL("image/webp", raw), "image/webp", false},
		{"gif", dataURL("image/gif", raw), "image/gif", false},
		{"upper-case scheme and type", "DATA:IMAGE/PNG;BASE64," + payload, "image/png", false},
		{"a gateway reference", gatewayBlobPrefix + "blob_" + strings.Repeat("a", 32), "", true},
		{"an https url", "https://example.com/cat.png", "", true},
		{"no payload separator", "data:image/png;base64", "", true},
		{"not base64", "data:image/png,rawtext", "", true},
		{"charset parameter", "data:image/png;charset=utf-8;base64," + payload, "", true},
		{"text/html", dataURL("text/html", raw), "", true},
		{"svg", dataURL("image/svg+xml", raw), "", true},
		{"url-safe alphabet", "data:image/png;base64,_-_-", "", true},
		{"empty payload", "data:image/png;base64,", "", true},
		{"empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mediaType, got, err := DecodeImageDataURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %.60q as %s with %d bytes", tc.url, mediaType, len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeImageDataURL: %v", err)
			}
			if mediaType != tc.wantType || !bytes.Equal(got, raw) {
				t.Fatalf("got %s with %d bytes, want %s with the %d original bytes", mediaType, len(got), tc.wantType, len(raw))
			}
		})
	}
}

// messageImageURLs is the url of every image part of a content, mask included, in
// order (how MessageImages reads a message is TestMessageImages' business).
func messageImageURLs(content json.RawMessage) []string {
	var urls []string
	for _, img := range MessageImages(content) {
		urls = append(urls, img.URL)
	}
	return urls
}
