// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
)

// unavailableImageText stands in, in a run's history, for an image the chat has
// no stored bytes for (see resolveChatBlobRefs). The model is told rather than
// left to guess, and the message keeps a part: an upstream rejects an empty one.
const unavailableImageText = "[image unavailable]"

var unavailableImagePart = json.RawMessage(`{"type":"text","text":"` + unavailableImageText + `"}`)

// resolveChatBlobRefs turns the stored-image references of a run's history into
// the images themselves: every /api/portal/chat-blobs/{id} url becomes a
// data:<type>;base64,<bytes> url of the blob's real bytes.
//
// A run's history goes to an upstream that cannot reach this gateway's urls --
// above all a remote provider -- and the adapters drop an image whose url is not
// a data: or http(s) one without an error, so a reference left in place would
// silently vanish from the request and the model would answer without seeing the
// picture. Resolution therefore happens here, where the history is built, for
// every text run (see PrepareChatRun for why an image run resolves only the
// source of its turn, resolveImageRunSource).
//
// What it relies on, and what it does not:
//
//   - One batched ChatBlobsByIDs for all the distinct ids, scoped to chatID, so
//     the store returns only blobs this chat holds. An id that is another chat's,
//     another user's or never stored is simply absent. A transcript can
//     carry any id (create and a bare autosave store what they are given), so
//     this scoping is what stops a grafted reference from becoming someone
//     else's bytes. It is not the only guard: a row of another chat, or of a type
//     that is not an allow-listed image type, is refused here too. An audio blob
//     (a speech reply's WAV) is not an image type: a run's history is resolved
//     through the image walk alone, so a stored WAV is never fetched, unsealed or
//     counted by this resolution, and an image reference that happens to name one
//     is refused like any other row that is not an image.
//   - An absent image is replaced by unavailableImagePart and the run goes on.
//     The alternatives are worse: leaving the reference sends an upstream a
//     relative url it rejects, or (Anthropic) drops silently; refusing the run
//     would strand a chat whose transcript carries one foreign or missing id
//     (a chat evicted from the RAM store is gone with its blobs and never gets
//     here: the run is refused at the chat lookup). The stored
//     document is not touched; only the history of this run is.
//   - The bytes carried are bounded by maxRunHistoryBytes, counted per
//     occurrence (the request carries an image once per occurrence). The
//     declared sizes are checked first, so a refusal costs no decryption; the
//     bytes actually unsealed are counted as well, because byte_len is metadata
//     the store keeps beside the payload and an understated one must not let a
//     larger payload through.
//   - A store or cipher failure is returned as the failure it is, never as an
//     absent image.
//
// history is not modified; a history with no reference comes back as it is, with
// no store call.
func (s *Service) resolveChatBlobRefs(ctx context.Context, chatID string, history []ChatAPIMessage) ([]ChatAPIMessage, error) {
	out, unavailable, err := s.resolveBlobRefs(ctx, chatID, history, imagePartKind)
	if err != nil {
		return nil, err
	}
	if unavailable > 0 {
		log.Printf("portal: chat %s: %d image reference(s) in the run history have no stored image for this chat and were left out", chatID, unavailable)
	}
	return out, nil
}

// resolveBlobRefs is resolveChatBlobRefs for the parts of one family, kind, also
// reporting how many references had no stored blob for the chat (each is now an
// unavailableImagePart). The caller decides what an absent blob means for its run:
// a text run goes on without an absent image, an edit cannot (see
// resolveImageRunSource), and neither can a transcription without its clip (see
// resolveTranscriptionRunSource) -- which matters because the placeholder is worded
// for an image, so an audio resolution must refuse on a non-zero count rather than
// hand its history on.
//
// kind is the whole of what is read: the walk lists, fetches and substitutes only
// the parts of that family (a text, image or speech run's history is an
// imagePartKind resolution, so a stored audio reference of the same history is
// never fetched, unsealed or counted), and a stored row is usable only if its type
// is one the family accepts.
func (s *Service) resolveBlobRefs(ctx context.Context, chatID string, history []ChatAPIMessage, kind blobPartKind) ([]ChatAPIMessage, int, error) {
	ids, occurrences, err := collectBlobRefIDs(history, kind)
	if err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 {
		return history, 0, nil
	}

	rows, err := s.chats.ChatBlobsByIDs(ctx, chatID, ids)
	if err != nil {
		return nil, 0, err
	}
	limit := int64(s.maxRunHistoryBytes)
	usable, err := usableChatBlobs(chatID, ids, occurrences, rows, limit, kind)
	if err != nil {
		return nil, 0, err
	}
	urls, err := s.blobDataURLs(ids, occurrences, usable, limit)
	if err != nil {
		return nil, 0, err
	}
	return substituteBlobRefs(history, urls, kind)
}

// resolveImageRunSource gives an image run the source image of its turn. An image
// thread's turn is an EDIT when its last user message carries an image: the
// executor sends that image's bytes to /v1/images/edits. It reads nothing else of
// the history (an image thread's older images -- earlier sources, its own
// generated results -- are never sent anywhere), so only the LAST user message is
// resolved, with the very resolution a text run's whole history gets: the images
// are fetched scoped to chatID (an id that is another chat's, another user's or
// never stored is simply absent), the declared and the unsealed sizes are bounded,
// and the stored document is not touched. A message with no stored reference (a
// fresh upload arrives inline, a plain prompt carries no image) costs no store
// call.
//
// What it does NOT share with a text run is the tolerance. There an absent image
// becomes a placeholder and the run carries on without it, which is right for a
// model that can answer without seeing a picture. An edit cannot: without its
// source it is a different request, and the executor would generate an unrelated
// image from the prompt alone. A source this chat does not hold therefore refuses
// the run with ErrChatBlobNotOwned -- the error a foreign image in a new turn or
// an edited_history already gets at the commit (extractChatBlobs), here also
// covering the one message that check never sees: a stored transcript's own last
// message, whose references nothing vetted at this request. It is raised before
// anything is stored, so a retry is not a duplicate turn.
//
// history is not modified. A history with no user message comes back as it is.
func (s *Service) resolveImageRunSource(ctx context.Context, chatID string, history []ChatAPIMessage) ([]ChatAPIMessage, error) {
	return s.resolveLastUserSource(ctx, chatID, history, imagePartKind, "the image to edit has no stored image for this chat")
}

// resolveTranscriptionRunSource is resolveImageRunSource's audio twin: it gives a
// transcription run the clip of its turn. A transcription thread's turn is a run
// over the audio its last user message carries: the executor sends that clip's
// bytes to /v1/audio/transcriptions. It reads nothing else of the history (the
// thread's older clips and transcripts are never sent anywhere), so only the LAST
// user message is resolved, with the very resolution an image edit's source gets,
// read as audio parts (audioPartKind) instead of images: the clips are fetched
// scoped to chatID (an id that is another chat's, another user's or never stored is
// simply absent), a row that is not an allow-listed audio type is not a clip, the
// declared and the unsealed sizes are bounded, and the stored document is not
// touched. A message with no stored reference (a fresh attachment or recording
// arrives inline, a plain prompt carries no audio) costs no store call.
//
// The tolerance is the edit's, not the text run's: a clip this chat does not hold
// refuses the run with ErrChatBlobNotOwned. A transcription cannot proceed without
// its source -- and the image placeholder the shared walk would put in an absent
// clip's place is no input to transcribe -- so the run stops before anything is
// stored (the error a foreign blob in a new turn or an edited_history already gets
// at the commit, extractChatBlobs, here also covering the one message that check
// never sees: a stored transcript's own last message), and a retry is not a
// duplicate turn.
//
// The other kinds of run never come here: a text, image or speech run resolves
// images only, so a stored clip in such a thread's history is left as a reference.
//
// history is not modified. A history with no user message comes back as it is.
func (s *Service) resolveTranscriptionRunSource(ctx context.Context, chatID string, history []ChatAPIMessage) ([]ChatAPIMessage, error) {
	return s.resolveLastUserSource(ctx, chatID, history, audioPartKind, "the audio to transcribe has no stored clip for this chat")
}

// resolveLastUserSource is the one resolution of a run's source: the blobs of
// family kind that the LAST user message of history references are resolved to
// data: urls, every other message is left as it is, and a reference the chat holds
// no usable blob for refuses the run with ErrChatBlobNotOwned (absent says, in the
// log line, what was missing). resolveImageRunSource and
// resolveTranscriptionRunSource are this for the image and the audio family.
func (s *Service) resolveLastUserSource(ctx context.Context, chatID string, history []ChatAPIMessage, kind blobPartKind, absent string) ([]ChatAPIMessage, error) {
	last := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return history, nil
	}
	resolved, unavailable, err := s.resolveBlobRefs(ctx, chatID, history[last:last+1], kind)
	if err != nil {
		return nil, err
	}
	if unavailable > 0 {
		log.Printf("portal: chat %s: %s; refusing the run", chatID, absent)
		return nil, ErrChatBlobNotOwned
	}
	out := slices.Clone(history)
	out[last] = resolved[0]
	return out, nil
}

// collectBlobRefIDs lists the distinct blob ids the history references from parts
// of family kind, in the order first seen, and how many times each occurs. The
// history is only read.
func collectBlobRefIDs(history []ChatAPIMessage, kind blobPartKind) ([]string, map[string]int, error) {
	var ids []string
	occurrences := map[string]int{}
	count := func(_ blobPartKind, url string) (string, json.RawMessage, error) {
		if id, ok := blobRefID(url); ok {
			if occurrences[id] == 0 {
				ids = append(ids, id)
			}
			occurrences[id]++
		}
		return url, nil, nil
	}
	for _, m := range history {
		if _, _, err := rewriteBlobParts(m.Content, kind, count); err != nil {
			return nil, nil, err
		}
	}
	return ids, occurrences, nil
}

// usableChatBlobs keeps, of the rows the store returned for ids, those this run
// may send: a row of another chat or of a type the family kind does not accept
// (outside the allow-list, or of the other family) is left out like an absent
// one. It bounds the declared sizes -- byte_len per occurrence -- by limit, so a
// refusal costs no decryption.
func usableChatBlobs(chatID string, ids []string, occurrences map[string]int, rows map[string]store.ChatBlob, limit int64, kind blobPartKind) (map[string]store.ChatBlob, error) {
	usable := make(map[string]store.ChatBlob, len(rows))
	var declared int64
	for _, id := range ids {
		row, ok := rows[id]
		if !ok || row.ChatID != chatID {
			continue
		}
		if !kind.accepts(row.ContentType) {
			log.Printf("portal: chat blob %s has a content type that is not sendable as the part that references it; leaving it out of the run history", id)
			continue
		}
		usable[id] = row
		declared += int64(occurrences[id]) * row.ByteLen
		if declared > limit {
			return nil, ErrChatRunHistoryTooLarge
		}
	}
	return usable, nil
}

// blobDataURLs unseals and encodes each usable image once; the url is shared by
// every occurrence. The encoding is the standard padded alphabet, which is what
// the extraction decodes and what the providers' data-url parsing expects. The
// bytes actually unsealed are bounded by limit as well, per occurrence, because
// the declared sizes (see usableChatBlobs) are only metadata.
func (s *Service) blobDataURLs(ids []string, occurrences map[string]int, usable map[string]store.ChatBlob, limit int64) (map[string]string, error) {
	urls := make(map[string]string, len(usable))
	var actual int64
	for _, id := range ids {
		row, ok := usable[id]
		if !ok {
			continue
		}
		plain, err := s.openChatBlob(row)
		if err != nil {
			return nil, err
		}
		actual += int64(occurrences[id]) * int64(len(plain))
		if actual > limit {
			return nil, ErrChatRunHistoryTooLarge
		}
		urls[id] = "data:" + row.ContentType + ";base64," + base64.StdEncoding.EncodeToString(plain)
	}
	return urls, nil
}

// substituteBlobRefs returns a copy of history in which every stored-blob url of
// a part of family kind is replaced by its entry in urls, or -- when it has none --
// the whole part by unavailableImagePart. It also returns how many parts were
// replaced that way. history is not modified.
func substituteBlobRefs(history []ChatAPIMessage, urls map[string]string, kind blobPartKind) ([]ChatAPIMessage, int, error) {
	out := make([]ChatAPIMessage, len(history))
	copy(out, history)
	unavailable := 0
	resolve := func(_ blobPartKind, url string) (string, json.RawMessage, error) {
		id, ok := blobRefID(url)
		if !ok {
			return url, nil, nil
		}
		if resolved, ok := urls[id]; ok {
			return resolved, nil, nil
		}
		unavailable++
		return url, unavailableImagePart, nil
	}
	for i, m := range history {
		content, changed, err := rewriteBlobParts(m.Content, kind, resolve)
		if err != nil {
			return nil, 0, err
		}
		if changed {
			out[i].Content = content
		}
	}
	return out, unavailable, nil
}

// blobRefID is the blob id of a stored-blob url, and whether url is one.
func blobRefID(url string) (string, bool) {
	return strings.CutPrefix(url, chatBlobURLPrefix)
}
