// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"testing"
)

// A masked edit (inpainting) is the source image plus a MASK in the last user
// message of an image thread. The mask is an ordinary image part -- so it is stored
// as a chat blob, resolved back to its bytes and owner-checked by exactly the code
// that handles the source -- carrying a tag (ImagePurposeMask) that tells the
// executor to send it as the `mask` form field instead of as a second source.

// maskedContent is a user message: a prompt, a source image and the mask.
func maskedContent(text, sourceURL, maskURL string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`[{"type":"text","text":%q},{"type":"image_url","image_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q},"purpose":"mask"}]`,
		text, sourceURL, maskURL))
}

// TestMessageImages reads the images of a message the way the extraction and the
// resolution do -- the same shape (an object whose image_url is an object with a
// string url), members matched without regard to case, plain-string content and
// non-image parts yielding nothing -- and says which of them is tagged as a mask.
func TestMessageImages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    []MessageImage
	}{
		{"plain string", `"hello"`, nil},
		{"text parts only", `[{"type":"text","text":"hi"}]`, nil},
		{"one image", `[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"u1"}}]`, []MessageImage{{URL: "u1"}}},
		{"two images in order", `[{"type":"image_url","image_url":{"url":"u1"}},{"type":"image_url","image_url":{"url":"u2"}}]`, []MessageImage{{URL: "u1"}, {URL: "u2"}}},
		{"member case is ignored", `[{"type":"image_url","Image_Url":{"URL":"u1"}}]`, []MessageImage{{URL: "u1"}}},
		{"image_url that is not an object", `[{"type":"image_url","image_url":"u1"}]`, nil},
		{"an image_url without a string url", `[{"type":"image_url","image_url":{"url":3}}]`, nil},
		{"not JSON", `nope`, nil},
		{
			"a tagged part is the mask", `[{"type":"image_url","image_url":{"url":"u1"}},{"type":"image_url","image_url":{"url":"m"},"purpose":"mask"}]`,
			[]MessageImage{{URL: "u1"}, {URL: "m", Mask: true}},
		},
		{"the tag's key case is ignored too", `[{"type":"image_url","image_url":{"url":"m"},"Purpose":"mask"}]`, []MessageImage{{URL: "m", Mask: true}}},
		{"the tag's place in the part does not matter", `[{"purpose":"mask","image_url":{"url":"m"},"type":"image_url"}]`, []MessageImage{{URL: "m", Mask: true}}},
		{"another purpose is not a mask", `[{"type":"image_url","image_url":{"url":"u1"},"purpose":"reference"}]`, []MessageImage{{URL: "u1"}}},
		{"a purpose that is not a string is not a mask", `[{"type":"image_url","image_url":{"url":"u1"},"purpose":true}]`, []MessageImage{{URL: "u1"}}},
		{"the tag's value is exact", `[{"type":"image_url","image_url":{"url":"u1"},"purpose":"MASK"}]`, []MessageImage{{URL: "u1"}}},
		{"a tag on a part that is not an image is nothing", `[{"type":"text","text":"x","purpose":"mask"}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MessageImages(json.RawMessage(tc.content))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("MessageImages(%s) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// The mask is stored and resolved exactly like the source: a fresh upload is a
// data: url that becomes a chat blob at the run commit (the tag stays on the part,
// the bytes leave the transcript) and is handed to the run as the uploaded bytes;
// a stored mask named in a later turn comes back as bytes through the SAME
// chat-scoped batch lookup the source goes through -- one call, for both ids.
func TestPrepareChatRunImageKindResolvesTheMaskLikeTheSource(t *testing.T) {
	ctx := context.Background()
	for _, withCipher := range []bool{true, false} {
		t.Run(fmt.Sprintf("cipher=%v", withCipher), func(t *testing.T) {
			e := newResolveEnv(t, withCipher, 0)
			chatID := e.emptyChat(t)
			source, mask := blobTestBytes(3000, 11), blobTestBytes(900, 23)

			// A fresh upload arrives inline and is handed to the run as it was sent.
			history, err := e.sendKind(chatID, "image", maskedContent("fill the sky", dataURL("image/png", source), dataURL("image/png", mask)))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}
			if len(e.byIDs.calls) != 0 {
				t.Fatalf("a fresh upload issued %d ChatBlobsByIDs calls, want 0", len(e.byIDs.calls))
			}
			requireMaskedPair(t, history[len(history)-1].Content, source, mask)

			// What was stored: two blobs (the source and the mask), the transcript
			// holds their references, the mask part keeps its tag, and no bytes.
			doc := string(e.stored(t, chatID))
			if strings.Contains(doc, "data:") {
				t.Fatal("the stored document gained inline bytes")
			}
			if n := strings.Count(doc, `"purpose":"mask"`); n != 1 {
				t.Fatalf("the stored document carries the mask tag %d times, want once: %s", n, doc)
			}
			ids := e.storedBlobIDs(t, chatID)
			if len(ids) != 2 || ids[0] == ids[1] {
				t.Fatalf("stored blob ids = %v, want two distinct ones (the source and the mask)", ids)
			}

			// A later turn names both stored copies (a replayed edit, or the same mask
			// over a new prompt): one scoped lookup returns them, and each keeps its role.
			if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: imageContent("", dataURL("image/png", blobTestBytes(64, 3)))}, "complete"); err != nil {
				t.Fatal(err)
			}
			e.byIDs.calls = nil
			history, err = e.sendKind(chatID, "image", maskedContent("now the sea", gatewayBlobPrefix+ids[0], gatewayBlobPrefix+ids[1]))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}
			if len(e.byIDs.calls) != 1 || e.byIDs.calls[0].chatID != chatID || !slices.Equal(e.byIDs.calls[0].ids, ids) {
				t.Fatalf("calls = %+v, want ONE batch scoped to chat %s for exactly the source and the mask %v", e.byIDs.calls, chatID, ids)
			}
			requireMaskedPair(t, history[len(history)-1].Content, source, mask)
			// The earlier turns keep their references: only the last user message is resolved.
			for i, m := range history[:len(history)-1] {
				for _, img := range MessageImages(m.Content) {
					if !strings.HasPrefix(img.URL, gatewayBlobPrefix) {
						t.Fatalf("history message %d was resolved (%.60s), want only the last user message resolved", i, img.URL)
					}
				}
			}
			if strings.Contains(string(e.stored(t, chatID)), "data:") {
				t.Fatal("the stored document gained inline bytes")
			}
		})
	}
}

// requireMaskedPair insists a run history's last user message holds the source and
// then the mask, as data: urls of exactly these bytes, the mask still tagged and the
// source not.
func requireMaskedPair(t *testing.T, content json.RawMessage, source, mask []byte) {
	t.Helper()
	images := MessageImages(content)
	if len(images) != 2 {
		t.Fatalf("the last user message carries %d images, want the source and the mask: %.300s", len(images), content)
	}
	for i, want := range []struct {
		raw  []byte
		mask bool
	}{{source, false}, {mask, true}} {
		if images[i].Mask != want.mask {
			t.Fatalf("image %d is mask=%v, want %v", i, images[i].Mask, want.mask)
		}
		if mediaType, raw := decodeResolvedURL(t, images[i].URL); mediaType != "image/png" || !bytes.Equal(raw, want.raw) {
			t.Fatalf("image %d arrived as %s with %d bytes, want the %d original bytes", i, mediaType, len(raw), len(want.raw))
		}
	}
}

// A mask the chat does not hold is refused exactly like a source it does not hold
// (TestPrepareChatRunImageKindRefusesAnEditSourceTheChatDoesNotHold): the run is not
// started with the foreign bytes, not started without the mask (a masked edit
// silently becoming a whole-image one would repaint what the user meant to keep),
// and nothing is stored.
func TestPrepareChatRunImageKindRefusesAMaskTheChatDoesNotHold(t *testing.T) {
	ctx := context.Background()
	e := newResolveEnv(t, true, 0)
	secretID := e.sendImage(t, e.emptyChat(t), blobTestBytes(2048, 77)) // the owner holds it in ANOTHER chat
	foreignRef := gatewayBlobPrefix + secretID
	missingRef := gatewayBlobPrefix + "blob_" + strings.Repeat("0", 32)
	// A source this chat DOES hold, so that it is the mask alone that is refused.
	ownChat := e.emptyChat(t)
	if _, err := e.sendKind(ownChat, "image", imageContent("first", dataURL("image/png", blobTestBytes(400, 8)))); err != nil {
		t.Fatal(err)
	}
	ownRef := gatewayBlobPrefix + e.storedBlobIDs(t, ownChat)[0]

	graft := func(maskRef string) string {
		return fmt.Sprintf(`{"settings":{"kind":"image"},"messages":[{"id":"u1","role":"user","content":%s}]}`, maskedContent("mine", ownRef, maskRef))
	}
	for _, tc := range []struct{ name, ref string }{
		{"another chat's blob", foreignRef},
		{"a blob that never existed", missingRef},
	} {
		t.Run("stored last message: "+tc.name, func(t *testing.T) {
			chatID := e.newChat(t, graft(tc.ref))
			before := docMessages(t, mustContent(t, e.svc, e.owner, chatID))
			history, _, err := e.svc.PrepareChatRun(ctx, e.owner, chatID, PrepareRunRequest{Settings: ChatRunSettings{Model: "m", Kind: "image"}})
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("err = %v, want ErrChatBlobNotOwned", err)
			}
			if history != nil {
				t.Fatalf("a refused mask still returned a history: %.200s", history)
			}
			if after := docMessages(t, mustContent(t, e.svc, e.owner, chatID)); !slices.Equal(before, after) {
				t.Fatalf("a refused run changed the stored document:\n before %v\n after  %v", before, after)
			}
		})
		t.Run("new message: "+tc.name, func(t *testing.T) {
			chatID := e.emptyChat(t)
			_, err := e.sendKind(chatID, "image", maskedContent("mine", dataURL("image/png", blobTestBytes(100, 1)), tc.ref))
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("err = %v, want ErrChatBlobNotOwned", err)
			}
		})
	}

	// The service does not trust the store's scoping alone for the mask either: a
	// row of another chat, or of an unlisted type, is not a mask.
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
			if _, err := own.sendKind(chatID, "image", maskedContent("first", dataURL("image/png", blobTestBytes(300, 8)), dataURL("image/png", blobTestBytes(120, 9)))); err != nil {
				t.Fatal(err)
			}
			ids := own.storedBlobIDs(t, chatID)
			if len(ids) != 2 {
				t.Fatalf("stored ids = %v, want the source and the mask", ids)
			}
			// Only the MASK's row is misreported: the source resolves, the mask must not.
			own.byIDs.rewrite = func(got map[string]store.ChatBlob) {
				if row, ok := got[ids[1]]; ok {
					tc.mutate(&row)
					got[ids[1]] = row
				}
			}
			_, _, err := own.svc.PrepareChatRun(ctx, own.owner, chatID, PrepareRunRequest{
				UserMessage: maskedContent("again", gatewayBlobPrefix+ids[0], gatewayBlobPrefix+ids[1]),
				Settings:    ChatRunSettings{Model: "m", Kind: "image"},
			})
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("err = %v, want ErrChatBlobNotOwned", err)
			}
		})
	}
}
