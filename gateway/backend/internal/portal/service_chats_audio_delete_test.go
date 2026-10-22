// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/store"
	"slices"
	"testing"
)

// The two manual halves of the audio answer lifecycle (#281): deleting one answer's
// audio, and the retroactive prune of a whole chat to a target count. Both rewrite
// the stored document into the audio_removed marker state and delete the blob rows
// that rewrite leaves unreferenced, in ONE atomic save. They are driven here over a
// real store on the in-memory and SQLite legs, the way the retention tests are;
// the HTTP half (routes, the active-run refusal) is pinned in internal/gateway.

// sameIDs reports whether a and b hold the same ids, in any order: the prune and the
// delete return their ids newest answer first, which no caller depends on.
func sameIDs(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// requireOneSaveDeleting fails unless exactly one save was recorded since before and
// it deleted exactly want: the document rewrite and the removal of every blob row
// travel in a single SaveChatWithBlobs, which is what keeps a failed delete from
// leaving the document pointing at a blob that is gone.
func (e *retentionEnv) requireOneSaveDeleting(t *testing.T, before int, want []string) {
	t.Helper()
	if len(e.rec.saves) != before+1 {
		t.Fatalf("saves = %d (was %d), want exactly one atomic save", len(e.rec.saves), before)
	}
	if got := e.rec.deletes[before]; !sameIDs(got, want) {
		t.Fatalf("the save deleted %v, want exactly %v", got, want)
	}
	if blobs := e.rec.saves[before]; len(blobs) != 0 {
		t.Fatalf("the save inserted %d blobs; a delete/prune stores nothing", len(blobs))
	}
}

// Deleting one answer's audio removes EVERY blob row of that answer. ADR-070 stores a
// long answer as several ordered audio_url parts, one chat_blobs row each, so a delete
// that stopped at the first part would leave paid-for bytes behind that nothing
// references any more. The answer stays, as the marker; its neighbour is untouched;
// and the removed rows 404 cleanly when the player (or anyone) asks for them. [Review
// Focus]
func TestDeleteMessageAudioRemovesAllBlobs(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		ctx := context.Background()
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)

		e.ask(t, chatID, "read the whole chapter", nil)
		long := e.answerWith(t, chatID, taggedWAV(1), taggedWAV(2), taggedWAV(3))
		if len(long) != 3 {
			t.Fatalf("the long answer stored %d blobs, want 3 (one per part)", len(long))
		}
		other := e.turn(t, chatID, nil, 4)

		answers := e.answers(t, chatID)
		saves := len(e.rec.saves)
		if err := e.svc.DeleteMessageAudio(ctx, e.owner, chatID, answers[0].ID); err != nil {
			t.Fatalf("DeleteMessageAudio: %v", err)
		}

		e.requireOneSaveDeleting(t, saves, long)
		after := e.answers(t, chatID)
		if len(after) != 2 {
			t.Fatalf("answers = %d after the delete, want both kept", len(after))
		}
		e.requirePruned(t, "deleted answer", after[0], long)
		e.requireStored(t, "neighbouring answer", after[1], other)
		for _, id := range long {
			if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
				t.Fatalf("ChatBlobBytes(%s) = %v, want ErrChatBlobNotFound after the delete", id, err)
			}
		}
		for _, id := range other {
			if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, id); err != nil {
				t.Fatalf("ChatBlobBytes(%s) = %v, want the neighbour's audio still served", id, err)
			}
		}
	})
}

// A message with no stored audio of its own is a successful no-op, not an error and
// not a write: a text answer, an answer an earlier delete or prune already turned
// into the marker (so deleting twice is safe, and the second delete adds no second
// marker), and a user turn -- whose audio_url is a transcription clip the user
// supplied, never an answer's audio.
func TestDeleteMessageAudioWithoutStoredAudioIsNoOp(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		ctx := context.Background()
		chatID := e.newChat(t, `{"settings":{},"messages":[`+
			`{"id":"msg_clip","role":"user","content":[{"type":"audio_url","audio_url":{"url":"/api/portal/chat-blobs/blob_clip"}}]},`+
			`{"id":"msg_text","role":"assistant","status":"complete","content":"only words"},`+
			`{"id":"msg_gone","role":"assistant","status":"complete","content":[{"type":"audio_removed"}]}]}`)
		before, err := e.svc.GetChat(ctx, e.owner, chatID)
		if err != nil {
			t.Fatalf("GetChat: %v", err)
		}

		for _, id := range []string{"msg_clip", "msg_text", "msg_gone"} {
			if err := e.svc.DeleteMessageAudio(ctx, e.owner, chatID, id); err != nil {
				t.Fatalf("DeleteMessageAudio(%s) = %v, want a no-op success", id, err)
			}
		}

		if len(e.rec.saves) != 0 {
			t.Fatalf("saves = %d, want none: there was nothing to delete", len(e.rec.saves))
		}
		after, err := e.svc.GetChat(ctx, e.owner, chatID)
		if err != nil {
			t.Fatalf("GetChat after: %v", err)
		}
		if string(after.Content) != string(before.Content) {
			t.Fatalf("the document changed:\n before %s\n after  %s", before.Content, after.Content)
		}
	})
}

// An unknown chat or an unknown message is store.ErrNotFound, and nothing is written.
// An empty message id never matches: a stored message without an id member must not be
// reachable through the empty string.
func TestDeleteMessageAudioUnknownIsNotFound(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		ctx := context.Background()
		chatID := e.newChat(t, `{"settings":{},"messages":[`+
			`{"role":"assistant","status":"complete","content":[{"type":"audio_url","audio_url":{"url":"/api/portal/chat-blobs/blob_noid"}}]}]}`)
		before, err := e.svc.GetChat(ctx, e.owner, chatID)
		if err != nil {
			t.Fatalf("GetChat: %v", err)
		}

		for name, call := range map[string]func() error{
			"unknown chat":    func() error { return e.svc.DeleteMessageAudio(ctx, e.owner, "chat_nope", "msg_1") },
			"unknown message": func() error { return e.svc.DeleteMessageAudio(ctx, e.owner, chatID, "msg_nope") },
			"empty id":        func() error { return e.svc.DeleteMessageAudio(ctx, e.owner, chatID, "") },
		} {
			if err := call(); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s: err = %v, want store.ErrNotFound", name, err)
			}
		}

		if len(e.rec.saves) != 0 {
			t.Fatalf("saves = %d, want none after a not-found", len(e.rec.saves))
		}
		after, err := e.svc.GetChat(ctx, e.owner, chatID)
		if err != nil {
			t.Fatalf("GetChat after: %v", err)
		}
		if string(after.Content) != string(before.Content) {
			t.Fatalf("the document changed:\n before %s\n after  %s", before.Content, after.Content)
		}
	})
}

// Another owner's chat reads as not found for both mutations (no existence leak,
// like every chat read), and neither the document nor a blob row of the real owner
// is touched.
func TestAudioMutationsOnAnotherOwnersChatAreNotFound(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		ctx := context.Background()
		stranger := chatToken("usr_b")
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		first := e.turn(t, chatID, nil, 1)
		second := e.turn(t, chatID, nil, 2)
		answers := e.answers(t, chatID)
		saves := len(e.rec.saves)

		if err := e.svc.DeleteMessageAudio(ctx, stranger, chatID, answers[0].ID); !errors.Is(err, ErrChatNotFound) {
			t.Fatalf("DeleteMessageAudio as another owner = %v, want ErrChatNotFound", err)
		}
		if err := e.svc.PruneChatAudio(ctx, stranger, chatID, 0); !errors.Is(err, ErrChatNotFound) {
			t.Fatalf("PruneChatAudio as another owner = %v, want ErrChatNotFound", err)
		}

		if len(e.rec.saves) != saves {
			t.Fatalf("saves = %d (was %d): a refused foreign call wrote", len(e.rec.saves), saves)
		}
		after := e.answers(t, chatID)
		e.requireStored(t, "first answer", after[0], first)
		e.requireStored(t, "second answer", after[1], second)
	})
}

// The retroactive prune keeps the newest keepLast answers that hold audio and turns
// every older one into the marker, all of its blobs with it, in one atomic save --
// the same walk the commit-time prune makes, computed from the stored document and
// not from anything the client says. The middle answer is a multi-blob one: an
// answer is one slot however many blobs it spans. Pruning again to the same count, or
// to a count the chat is already under, finds nothing and writes nothing. [Review
// Focus]
func TestPruneChatAudioToKeepLast(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		ctx := context.Background()
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		a1 := e.turn(t, chatID, nil, 1)
		e.ask(t, chatID, "a long one", nil)
		a2 := e.answerWith(t, chatID, taggedWAV(2), taggedWAV(22))
		a3 := e.turn(t, chatID, nil, 3)
		a4 := e.turn(t, chatID, nil, 4)
		before := len(e.messages(t, chatID))
		settings := e.settingsOf(t, chatID)
		saves := len(e.rec.saves)

		if err := e.svc.PruneChatAudio(ctx, e.owner, chatID, 2); err != nil {
			t.Fatalf("PruneChatAudio(2): %v", err)
		}

		e.requireOneSaveDeleting(t, saves, append(slices.Clone(a1), a2...))
		answers := e.answers(t, chatID)
		e.requirePruned(t, "oldest answer", answers[0], a1)
		e.requirePruned(t, "second answer (two blobs)", answers[1], a2)
		e.requireStored(t, "third answer", answers[2], a3)
		e.requireStored(t, "newest answer", answers[3], a4)
		if got := len(e.messages(t, chatID)); got != before {
			t.Fatalf("messages = %d, want the %d it had: a prune rewrites answers, it never removes a turn", got, before)
		}
		// Not the prune's business to touch: the thread's settings ride along as they were.
		if got := e.settingsOf(t, chatID); got != settings {
			t.Fatalf("settings = %s, want %s unchanged by the prune", got, settings)
		}

		for _, keep := range []int{2, 5} {
			saves = len(e.rec.saves)
			if err := e.svc.PruneChatAudio(ctx, e.owner, chatID, keep); err != nil {
				t.Fatalf("PruneChatAudio(%d) again: %v", keep, err)
			}
			if len(e.rec.saves) != saves {
				t.Fatalf("PruneChatAudio(%d) with nothing to prune saved again (%d -> %d)", keep, saves, len(e.rec.saves))
			}
		}
	})
}

// keepLast 0 is "keep none": every stored answer's audio goes, and every blob row
// with it. The user turns and the answers themselves stay.
func TestPruneChatAudioZeroRemovesAll(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		ctx := context.Background()
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		a1 := e.turn(t, chatID, nil, 1)
		e.ask(t, chatID, "a long one", nil)
		a2 := e.answerWith(t, chatID, taggedWAV(2), taggedWAV(22), taggedWAV(222))
		a3 := e.turn(t, chatID, nil, 3)
		before := len(e.messages(t, chatID))
		saves := len(e.rec.saves)

		if err := e.svc.PruneChatAudio(ctx, e.owner, chatID, 0); err != nil {
			t.Fatalf("PruneChatAudio(0): %v", err)
		}

		e.requireOneSaveDeleting(t, saves, slices.Concat(a1, a2, a3))
		answers := e.answers(t, chatID)
		e.requirePruned(t, "first answer", answers[0], a1)
		e.requirePruned(t, "long answer", answers[1], a2)
		e.requirePruned(t, "last answer", answers[2], a3)
		if got := len(e.messages(t, chatID)); got != before {
			t.Fatalf("messages = %d, want the %d it had", got, before)
		}
		for _, id := range slices.Concat(a1, a2, a3) {
			if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
				t.Fatalf("ChatBlobBytes(%s) = %v, want ErrChatBlobNotFound", id, err)
			}
		}

		saves = len(e.rec.saves)
		if err := e.svc.PruneChatAudio(ctx, e.owner, chatID, 0); err != nil {
			t.Fatalf("PruneChatAudio(0) again: %v", err)
		}
		if len(e.rec.saves) != saves {
			t.Fatalf("a second keep-none prune saved again (%d -> %d): nothing was left to remove", saves, len(e.rec.saves))
		}
	})
}

// A chat from before audio moved into chat_blobs holds an answer's audio INLINE, as a
// data: url in the document, with no row behind it. Deleting or pruning such an answer
// still has to take the audio out of the document -- the marker must not sit next to
// audio that is still playable -- even though there is no blob to delete, so the save
// is made on the strength of the rewritten document and not of the ids it returns.
func TestAudioMutationsRewriteInlineAudioThatHasNoBlob(t *testing.T) {
	inline := func(id string) string {
		return `{"id":"` + id + `","role":"assistant","status":"complete","content":[{"type":"audio_url","audio_url":{"url":` +
			`"` + dataURL("audio/wav", taggedWAV(7)) + `"}}]}`
	}
	t.Run("delete", func(t *testing.T) {
		forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
			chatID := e.newChat(t, `{"settings":{},"messages":[`+inline("msg_old")+`]}`)

			if err := e.svc.DeleteMessageAudio(context.Background(), e.owner, chatID, "msg_old"); err != nil {
				t.Fatalf("DeleteMessageAudio: %v", err)
			}

			e.requireOneSaveDeleting(t, 0, nil)
			e.requirePruned(t, "inline answer", e.answers(t, chatID)[0], nil)
		})
	})
	t.Run("prune", func(t *testing.T) {
		forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
			chatID := e.newChat(t, `{"settings":{},"messages":[`+inline("msg_old")+`]}`)

			if err := e.svc.PruneChatAudio(context.Background(), e.owner, chatID, 0); err != nil {
				t.Fatalf("PruneChatAudio(0): %v", err)
			}

			e.requireOneSaveDeleting(t, 0, nil)
			e.requirePruned(t, "inline answer", e.answers(t, chatID)[0], nil)
		})
	})
}

// A negative count is not a count: it is refused as invalid rather than read as "no
// retention" (pruneAudioBeyond prunes nothing for it, which would answer success to a
// request that asked for something), and nothing is written.
func TestPruneChatAudioNegativeIsInvalid(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		ids := e.turn(t, chatID, nil, 1)
		saves := len(e.rec.saves)

		if err := e.svc.PruneChatAudio(context.Background(), e.owner, chatID, -1); !errors.Is(err, ErrChatAudioKeepLastInvalid) {
			t.Fatalf("PruneChatAudio(-1) = %v, want ErrChatAudioKeepLastInvalid", err)
		}

		if len(e.rec.saves) != saves {
			t.Fatalf("saves = %d (was %d): a refused prune wrote", len(e.rec.saves), saves)
		}
		e.requireStored(t, "the answer", e.answers(t, chatID)[0], ids)
	})
}

// settingsOf is the stored document's settings member, as stored.
func (e *retentionEnv) settingsOf(t *testing.T, chatID string) string {
	t.Helper()
	got, err := e.svc.GetChat(context.Background(), e.owner, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	var doc struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(got.Content, &doc); err != nil {
		t.Fatalf("stored document: %v", err)
	}
	return string(doc.Settings)
}
