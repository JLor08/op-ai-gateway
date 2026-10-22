// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"slices"
)

// The two manual mutations of the audio answer lifecycle (#281): deleting one
// answer's audio, and the retroactive prune of a chat's stored audio down to a
// target count (what the portal asks for when the user tightens the retention
// setting on a chat that already holds more). Both are the same move the run
// commit makes -- rewrite the stored document into the audio_removed marker state
// (chat_audio_retention.go) and delete the chat_blobs rows that leave unreferenced
// -- and both hand the rewritten document and the ids to commitChatRun, so the save
// is ONE atomic store step: the document never points at a row this call removed,
// and a failed save removes neither.
//
// Both rewrite the WHOLE document and hard-delete rows, exactly like PUT /chats
// (SaveChat), so they must not run while a run is live for the chat: the run owns
// the transcript tail and a rewrite racing it would overwrite its checkpoint or its
// terminal commit. That refusal is the HTTP layer's (the run registry lives there;
// see handlePortalChatSingle's PUT case, handleDeleteMessageAudio and
// handlePruneChatAudio); this service has no notion of an active run.

// ErrChatAudioKeepLastInvalid is returned by PruneChatAudio for a negative count: a
// keep-last of less than zero is no count, and answering success to it would tell
// the caller a prune it asked for had happened. Mapped to 400.
var ErrChatAudioKeepLastInvalid = errors.New("portal.chat_audio_keep_last_invalid")

// DeleteMessageAudio removes the stored audio of one answer of an owned chat: every
// audio part of the message goes -- all the ordered parts of a long ADR-070 answer,
// not only the first -- the message keeps its place and everything else it carries,
// and it gains the audio_removed marker, with the chat_blobs rows of those parts
// deleted in the same atomic save.
//
// store.ErrNotFound is returned for a chat that does not exist and for a message id
// the chat does not hold (an empty id never matches, so a stored message without an
// id is not reachable through it); another owner's chat is ErrChatNotFound, as for
// every chat read. Both map to the same 404, so neither leaks whether the chat
// exists.
//
// A message with no stored audio of its own is a no-op that succeeds and writes
// nothing: a text answer, an answer whose audio is already gone (deleting twice is
// safe and adds no second marker), and a user turn, whose audio is a transcription
// clip the user supplied and never an answer's audio.
//
// An id that more than one stored message carries -- ids are generated and unique,
// so this cannot come from the portal -- is applied to each of them, so no audio of
// that id is left behind.
func (s *Service) DeleteMessageAudio(ctx context.Context, owner auth.Token, chatID, messageID string) error {
	row, doc, err := s.openOwnedChatDoc(ctx, owner, chatID)
	if err != nil {
		return err
	}
	if messageID == "" {
		return store.ErrNotFound
	}
	var (
		found         bool
		rewrote       bool
		deleteBlobIDs []string
	)
	for i, raw := range doc.Messages {
		if storedMessageID(raw) != messageID {
			continue
		}
		found = true
		rewritten, ids, changed, err := markMessageAudioRemoved(raw)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		doc.Messages[i] = rewritten
		rewrote = true
		deleteBlobIDs = append(deleteBlobIDs, ids...)
	}
	if !found {
		return store.ErrNotFound
	}
	if !rewrote {
		return nil
	}
	return s.commitChatRun(ctx, row, row.Title, doc, nil, deleteBlobIDs)
}

// PruneChatAudio prunes an owned chat's stored audio down to keepLast answers: the
// newest keepLast answers that hold audio keep it, every older one becomes the
// marker with all of its blobs deleted, in one atomic save. keepLast 0 removes all
// stored audio. It is the same walk the commit-time prune makes (pruneAudioBeyond),
// computed here from the stored document -- the caller states only the target count,
// never which messages or blobs go, so a stale or hostile client cannot name another
// answer's audio.
//
// It does not change the thread's stored retention setting (the portal saves that
// itself); it only brings what is stored in line with a count. A chat already within
// the count, or already pruned to it, is a successful no-op that writes nothing.
//
// A negative keepLast is ErrChatAudioKeepLastInvalid. Another owner's chat is
// ErrChatNotFound and a missing one store.ErrNotFound, as for DeleteMessageAudio.
func (s *Service) PruneChatAudio(ctx context.Context, owner auth.Token, chatID string, keepLast int) error {
	if keepLast < 0 {
		return ErrChatAudioKeepLastInvalid
	}
	row, doc, err := s.openOwnedChatDoc(ctx, owner, chatID)
	if err != nil {
		return err
	}
	before := slices.Clone(doc.Messages)
	deleteBlobIDs, err := pruneAudioBeyond(&doc, keepLast)
	if err != nil {
		return err
	}
	// Whether anything was rewritten is read off the document, not off the ids: an
	// answer of a chat from before audio moved into chat_blobs holds its audio inline,
	// so pruning it rewrites the document yet leaves no row to delete.
	if slices.EqualFunc(before, doc.Messages, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) }) {
		return nil
	}
	return s.commitChatRun(ctx, row, row.Title, doc, nil, deleteBlobIDs)
}

// openOwnedChatDoc loads an owned chat and opens its document: the load every
// rewriting call of the chat service starts with (writeAssistant, PrepareChatRun). A
// missing chat is store.ErrNotFound and another owner's is ErrChatNotFound.
func (s *Service) openOwnedChatDoc(ctx context.Context, owner auth.Token, chatID string) (store.ChatRow, chatDoc, error) {
	row, err := s.chats.ChatByID(ctx, chatID)
	if err != nil {
		return store.ChatRow{}, chatDoc{}, err
	}
	if row.UserID != owner.UserID {
		return store.ChatRow{}, chatDoc{}, ErrChatNotFound
	}
	content, err := s.openChat(row)
	if err != nil {
		return store.ChatRow{}, chatDoc{}, err
	}
	return row, parseChatDoc(content), nil
}

// storedMessageID is the id member of a stored message ("" when it has none or is
// not a message), read the way the document's other readers read it: a tagged struct,
// so the member name matches without regard to case.
func storedMessageID(raw json.RawMessage) string {
	var m struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m.ID
}
