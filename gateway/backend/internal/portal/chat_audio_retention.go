// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"strings"
)

// The audio answer lifecycle (#281) removes the audio of a stored assistant
// answer -- by the retention knob (ChatAudioSettings.KeepLastAudio), by a manual
// delete, or by a run that was not to store its audio -- and every one of those
// leaves the answer in ONE recognizable state: its audio_url parts replaced by the
// marker below. This file holds the pure JSON half of that: the marker, and the
// rewrite of one message into it together with the chat_blobs ids that rewrite
// leaves unreferenced. It touches no store; the callers (the prune at a run
// commit, the retroactive prune, the per-answer delete) save the rewritten
// document and delete the returned ids in the same transaction.

// removedAudioPartType is the `type` of removedAudioPart, the marker an answer
// carries once its audio is gone. The portal keys the "audio no longer stored"
// bubble on it (a message with no audio and no marker is a failed run, and renders
// its error instead), so the string is a contract with the frontend's content-part
// union.
const removedAudioPartType = "audio_removed"

// removedAudioPart is THE marker content part:
//
//	{"type":"audio_removed"}
//
// It carries nothing else on purpose: no url (there is nothing to play), and no
// reason (pruned, deleted and never stored all render the same placeholder). It
// rides the schema-free chat document, so it needs no migration, and every reader
// that does not know it -- the audio-url extractor, the blob extraction, the run
// history -- already ignores a part with no image_url/audio_url member.
type removedAudioPart struct {
	Type string `json:"type"`
}

// messageHasStoredAudio reports whether the message holds at least one audio_url
// part: the predicate the retention count is made of. An audio part is exactly what
// MessageAudio and the blob extraction call one (an object whose audio_url is an
// object with a string url, members matched without regard to case), so the count
// and the player cannot disagree about which answers hold audio. An answer whose
// audio is gone (it carries the marker instead), a text answer, a plain string
// content and anything that is not a message at all hold none.
//
// It is the PART predicate and deliberately does not look at the role: a
// transcription turn's user message carries an audio_url clip too, and whether such
// a message is an answer is isAssistantMessage's question, which the walk that
// counts answers asks first. The error is the walk's own; no shape of message makes
// it fail today.
func messageHasStoredAudio(raw json.RawMessage) (bool, error) {
	msg, ok := parseJSONObject(raw)
	if !ok {
		return false, nil
	}
	for _, field := range msg {
		if !strings.EqualFold(field.key, "content") {
			continue
		}
		parts, ok := parseJSONArray(field.val)
		if !ok {
			continue
		}
		for _, part := range parts {
			if len(audioPartURLs(part)) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// markMessageAudioRemoved rewrites one assistant message into the not-stored state:
// every audio_url part is removed, one removedAudioPart marker is appended after the
// parts that stay, and the chat_blobs id of each removed part is returned so the
// caller can delete those rows in the same save. Every other part keeps its place
// and every other member of the message (id, status, metrics, ...) stays as it was,
// in its order and byte for byte, the way blobExtraction.message rebuilds a message.
//
// changed is false, and rewritten is raw itself, when there is nothing to do: the
// message is not an assistant message (a user turn's audio is a transcription clip,
// never an answer's), holds no audio part (a text answer, or one already marked), or
// is not a message at all. Marking is therefore idempotent, and a prune that meets
// an answer an earlier one already pruned finds no blob and writes no second marker.
//
// A removed part whose url is not a chat-blob url -- audio that was never extracted
// into chat_blobs -- is removed all the same and adds no id: there is no row to
// delete, and the marker must not sit next to audio that is still playable.
//
// A message holds one marker whatever it arrives with: the marker is appended only
// when the content has none yet.
func markMessageAudioRemoved(raw json.RawMessage) (rewritten json.RawMessage, blobIDs []string, changed bool, err error) {
	if !isAssistantMessage(raw) {
		return raw, nil, false, nil
	}
	msg, ok := parseJSONObject(raw)
	if !ok {
		return raw, nil, false, nil
	}
	// Every member spelled "content" is rewritten, as blobExtraction.message does:
	// the readers of the document decode into tagged structs, which match the name
	// without regard to case, so a spelling this skipped would keep its audio.
	for i := range msg {
		if !strings.EqualFold(msg[i].key, "content") {
			continue
		}
		content, ids, stripped, err := removeAudioParts(msg[i].val)
		if err != nil {
			return nil, nil, false, err
		}
		if stripped {
			msg[i].val, changed = content, true
			blobIDs = append(blobIDs, ids...)
		}
	}
	if !changed {
		return raw, nil, false, nil
	}
	out, err := msg.marshal()
	if err != nil {
		return nil, nil, false, err
	}
	return out, blobIDs, true, nil
}

// removeAudioParts is markMessageAudioRemoved for one content value: an array of
// parts has its audio parts dropped and the marker appended; anything else (a plain
// string, null, an object) holds no parts and comes back as it was. stripped is
// whether an audio part was dropped.
func removeAudioParts(content json.RawMessage) (rewritten json.RawMessage, blobIDs []string, stripped bool, err error) {
	parts, ok := parseJSONArray(content)
	if !ok {
		return content, nil, false, nil
	}
	kept := make([]json.RawMessage, 0, len(parts)+1)
	marked := false
	for _, part := range parts {
		urls := audioPartURLs(part)
		if len(urls) == 0 {
			marked = marked || isRemovedAudioPart(part)
			kept = append(kept, part)
			continue
		}
		stripped = true
		blobIDs = append(blobIDs, chatBlobIDsOf(urls)...)
	}
	if !stripped {
		return content, nil, false, nil
	}
	if !marked {
		marker, err := json.Marshal(removedAudioPart{Type: removedAudioPartType})
		if err != nil {
			return nil, nil, false, err
		}
		kept = append(kept, marker)
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, false, err
	}
	return out, blobIDs, true, nil
}

// chatBlobIDsOf is the chat_blobs id of each url that is a chat-blob url, in order. A url
// that is not one -- audio that was never extracted into chat_blobs -- adds none: there
// is no row to delete.
func chatBlobIDsOf(urls []string) []string {
	var ids []string
	for _, url := range urls {
		if id, ok := blobRefID(url); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// audioPartURLs is the url of each audio member of one content part, in order; none
// means the part is not an audio part. It reads through rewriteBlobPart with the
// audio kind -- the one definition of an audio part MessageAudio, the blob
// extraction and the run resolution share -- so a part is audio here exactly when it
// is audio there.
func audioPartURLs(part json.RawMessage) []string {
	var urls []string
	_, _, _ = rewriteBlobPart(part, audioPartKind, func(_ blobPartKind, url string) (string, json.RawMessage, error) {
		urls = append(urls, url)
		return url, nil, nil
	})
	return urls
}

// isRemovedAudioPart reports whether a content part is the not-stored marker.
func isRemovedAudioPart(part json.RawMessage) bool {
	var p removedAudioPart
	return json.Unmarshal(part, &p) == nil && p.Type == removedAudioPartType
}

// isAssistantMessage reports whether a stored message is an assistant turn: the
// only messages whose audio the lifecycle counts, prunes and deletes. The role is
// read the way the document's other readers read it (a tagged struct, so the member
// name matches without regard to case); anything that is not a message, or has no
// role, is not one.
func isAssistantMessage(raw json.RawMessage) bool {
	var m struct {
		Role string `json:"role"`
	}
	return json.Unmarshal(raw, &m) == nil && m.Role == "assistant"
}

// pruneAudioBeyond is the prune itself: it walks doc.Messages newest to oldest and
// counts the answers that hold audio -- the messages that are BOTH an assistant
// message (isAssistantMessage) AND hold at least one stored audio part
// (messageHasStoredAudio) -- keeps the newest keepLast of them as they are, and
// rewrites every older one in place into the not-stored state
// (markMessageAudioRemoved), returning the chat_blobs ids those rewrites leave
// unreferenced, newest answer first, for the caller to delete in the save that
// writes the document.
//
// What counts is what the retention is about, and it is decided here, in one place:
//
//   - N counts ANSWERS, not blobs: ADR-070 stores one long answer as several blobs,
//     and the whole answer is one slot and goes with all of its blobs together.
//   - Only assistant messages count and are rewritten. A transcription turn's user
//     message carries an audio_url clip too, but that is the user's input, and
//     messageHasStoredAudio deliberately does not look at the role.
//   - An answer that holds no audio takes no slot: a text answer, an errored one,
//     and above all one already in the not-stored state, so an answer pruned earlier
//     never pushes a still-stored older one out of the last N.
//
// keepLast 0 removes every answer's audio (the retroactive prune's "keep none"); a
// negative keepLast is no count and prunes nothing. Pruning is idempotent: the
// answers it has rewritten hold no audio and are skipped, so a second call over the
// same document returns no ids.
//
// The document is rewritten only if the whole walk succeeds: on an error doc is
// exactly as it came in, so a caller that gives up has nothing to undo. The newest
// answer is never special-cased -- the commit-time caller (pruneForRetention) only
// prunes with a freshly appended answer in the newest slot, so keepLast >= 1 always
// keeps it.
func pruneAudioBeyond(doc *chatDoc, keepLast int) (deleteBlobIDs []string, err error) {
	if keepLast < 0 {
		return nil, nil
	}
	type rewrite struct {
		at  int
		raw json.RawMessage
	}
	var rewrites []rewrite
	kept := 0
	for i := len(doc.Messages) - 1; i >= 0; i-- {
		raw := doc.Messages[i]
		if !isAssistantMessage(raw) {
			continue
		}
		has, err := messageHasStoredAudio(raw)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		if kept < keepLast {
			kept++
			continue
		}
		rewritten, ids, changed, err := markMessageAudioRemoved(raw)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		rewrites = append(rewrites, rewrite{at: i, raw: rewritten})
		deleteBlobIDs = append(deleteBlobIDs, ids...)
	}
	for _, r := range rewrites {
		doc.Messages[r.at] = r.raw
	}
	return deleteBlobIDs, nil
}

// pruneForRetention is the lazy, going-forward prune of a run commit: called with the
// document a run is about to save, after its new answer is appended as the newest
// message (and its inline audio moved into chat_blobs), it applies the thread's
// retention to the older answers. It returns the blob ids to delete in the same
// atomic save; doc has had those answers rewritten to the marker.
//
// It acts only for a retention of at least one answer (the thread's
// ChatAudioSettings.KeepLastAudio, read from the stored settings):
//
//   - unset keeps every answer -- what the default and every chat stored before the
//     setting existed read as. That includes a negative count, which PrepareChatRun
//     clamps to unset but which could still reach the schema-free document by
//     another write path; and a value that is not a whole number, or settings that
//     do not parse, which are no retention either.
//   - 0 stores no answer audio at all, which is decided by the run that produces the
//     answer (it never stores the audio there would be to prune), not by pruning the
//     answers before it.
//
// And only when the newest message is an answer that holds audio: the prune is that
// answer making room for itself, so a commit that holds none -- a checkpoint (text
// only, every few seconds), an errored run -- prunes nothing, and the newest answer
// is never the one rewritten.
func pruneForRetention(doc *chatDoc) (deleteBlobIDs []string, err error) {
	keep, ok := storedKeepLastAudio(doc.Settings)
	if !ok || keep < 1 || len(doc.Messages) == 0 {
		return nil, nil
	}
	newest := doc.Messages[len(doc.Messages)-1]
	if !isAssistantMessage(newest) {
		return nil, nil
	}
	if has, err := messageHasStoredAudio(newest); err != nil || !has {
		return nil, err
	}
	return pruneAudioBeyond(doc, keep)
}

// markNewestAnswerNotStored rewrites the just-appended assistant answer (the newest
// message of doc) into the not-stored marker state, for the two speech-answer commits
// that keep the turn WITHOUT its audio (#281): a retention of 0 answers on a run that
// STREAMED -- the audio was synthesized only to bill ADR-068's loud non-zero (the user
// heard it live) and must not be stored -- and a canceled/aborted run, whose empty turn
// becomes a regenerate-able marker rather than a blank bubble.
//
// It is called BEFORE the blob extraction, so a streamed answer's inline audio is never
// moved into chat_blobs at all. An answer that holds audio is rewritten by
// markMessageAudioRemoved -- its audio parts dropped, one marker appended, every other
// member (id, status, metrics) kept in place. An answer that holds none -- a canceled
// run commits an empty turn -- has its content replaced by the lone marker, since there
// is no audio part for markMessageAudioRemoved to strip. Only the newest message is
// touched; a stored older answer is the retention prune's business, never this one's. A
// newest that is not an assistant message is left alone.
func markNewestAnswerNotStored(doc *chatDoc) error {
	if len(doc.Messages) == 0 {
		return nil
	}
	i := len(doc.Messages) - 1
	raw := doc.Messages[i]
	if !isAssistantMessage(raw) {
		return nil
	}
	has, err := messageHasStoredAudio(raw)
	if err != nil {
		return err
	}
	if has {
		rewritten, _, changed, err := markMessageAudioRemoved(raw)
		if err != nil {
			return err
		}
		if changed {
			doc.Messages[i] = rewritten
		}
		return nil
	}
	rewritten, err := setMessageContentToRemovedMarker(raw)
	if err != nil {
		return err
	}
	doc.Messages[i] = rewritten
	return nil
}

// setMessageContentToRemovedMarker replaces (or appends) a message's `content` member
// with the lone not-stored marker array, every other member kept in its order, byte
// for byte. It is how a canceled speech turn -- which commits no audio part at all, so
// markMessageAudioRemoved finds nothing to strip -- is given the not-stored bubble. A
// message spelled in a way parseJSONObject cannot read comes back untouched.
func setMessageContentToRemovedMarker(raw json.RawMessage) (json.RawMessage, error) {
	msg, ok := parseJSONObject(raw)
	if !ok {
		return raw, nil
	}
	marker, err := json.Marshal([]removedAudioPart{{Type: removedAudioPartType}})
	if err != nil {
		return nil, err
	}
	set := false
	for i := range msg {
		if strings.EqualFold(msg[i].key, "content") {
			msg[i].val = marker
			set = true
		}
	}
	if !set {
		msg = append(msg, jsonField{key: "content", val: marker})
	}
	return msg.marshal()
}

// storedKeepLastAudio reads the retention out of a chat document's settings: the
// count and true when one is set, 0 and false for every document that sets none.
// The document is schema-free, so this decodes the one member it needs rather than
// the whole ChatAudioSettings (an unrelated malformed knob must not switch the
// retention off), and a value that is not a non-negative whole number is read as
// unset: a negative count is what PrepareChatRun's clamp already calls unset, and
// anything else is no count at all. A set 0 is returned as (0, true), distinct from
// unset, for the callers that treat "store none" differently.
func storedKeepLastAudio(settings json.RawMessage) (n int, ok bool) {
	if len(settings) == 0 {
		return 0, false
	}
	var s struct {
		Audio *struct {
			KeepLastAudio *int `json:"keep_last_audio"`
		} `json:"audio"`
	}
	if json.Unmarshal(settings, &s) != nil || s.Audio == nil || s.Audio.KeepLastAudio == nil || *s.Audio.KeepLastAudio < 0 {
		return 0, false
	}
	return *s.Audio.KeepLastAudio, true
}
