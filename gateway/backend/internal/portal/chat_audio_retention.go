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
		for _, url := range urls {
			if id, ok := blobRefID(url); ok && id != "" {
				blobIDs = append(blobIDs, id)
			}
		}
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
