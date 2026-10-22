// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The pure JSON helpers of the audio answer lifecycle (#281): a stored assistant
// audio answer becomes a "not stored" marker, and the chat_blobs ids it referenced
// are handed back for the caller to delete in the same save. No store is involved
// here, so every case is a message in and a message out.

// retentionAudioPart is a stored speech part: the audio_url part of
// gateway/chat_runs_audio.go whose url already points into chat_blobs, the way a
// committed document holds it.
func retentionAudioPart(id string) string {
	return fmt.Sprintf(`{"type":"audio_url","audio_url":{"url":%q}}`, chatBlobURLPrefix+id)
}

const retentionTextPart = `{"type":"text","text":"Here is the answer."}`

// retentionMessage is a stored message of the given role whose content is the
// given array of parts, with the members a real one carries after it so the
// tests can see them survive a rewrite byte for byte.
func retentionMessage(role string, parts ...string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"id":"msg_1","role":%q,"status":"complete","content":[%s],"tps":12.5}`,
		role, strings.Join(parts, ",")))
}

func TestRemovedAudioPartMarshalsToTheMinimalMarker(t *testing.T) {
	got, err := json.Marshal(removedAudioPart{Type: removedAudioPartType})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != `{"type":"audio_removed"}` {
		t.Fatalf("marker = %s, want {\"type\":\"audio_removed\"} and nothing else", got)
	}
	if removedAudioPartType != "audio_removed" {
		t.Fatalf("removedAudioPartType = %q, want audio_removed", removedAudioPartType)
	}
}

func TestMarkMessageAudioRemoved(t *testing.T) {
	const marker = `{"type":"audio_removed"}`
	for _, tc := range []struct {
		name        string
		in          json.RawMessage
		wantOut     json.RawMessage // nil: the input comes back byte for byte
		wantIDs     []string
		wantChanged bool
	}{
		{
			// ADR-070: one long answer is several ordered blobs, one audio_url part
			// each. All of them go, and the message keeps ONE marker.
			name: "multi-part answer: every blob id collected, one marker",
			in: retentionMessage("assistant",
				retentionAudioPart("blob_a"), retentionAudioPart("blob_b"), retentionAudioPart("blob_c")),
			wantOut:     retentionMessage("assistant", marker),
			wantIDs:     []string{"blob_a", "blob_b", "blob_c"},
			wantChanged: true,
		},
		{
			name:        "single audio part",
			in:          retentionMessage("assistant", retentionAudioPart("blob_only")),
			wantOut:     retentionMessage("assistant", marker),
			wantIDs:     []string{"blob_only"},
			wantChanged: true,
		},
		{
			// Only the audio parts leave; the text and its place in the order stay,
			// and the marker is appended after them.
			name: "mixed text and audio: text kept in order, audio replaced by the marker",
			in: retentionMessage("assistant",
				retentionTextPart, retentionAudioPart("blob_a"), `{"type":"text","text":"tail"}`, retentionAudioPart("blob_b")),
			wantOut:     retentionMessage("assistant", retentionTextPart, `{"type":"text","text":"tail"}`, marker),
			wantIDs:     []string{"blob_a", "blob_b"},
			wantChanged: true,
		},
		{
			name: "an image part next to the audio is not touched",
			in: retentionMessage("assistant",
				`{"type":"image_url","image_url":{"url":"/api/portal/chat-blobs/blob_img"}}`, retentionAudioPart("blob_a")),
			wantOut: retentionMessage("assistant",
				`{"type":"image_url","image_url":{"url":"/api/portal/chat-blobs/blob_img"}}`, marker),
			wantIDs:     []string{"blob_a"},
			wantChanged: true,
		},
		{
			name: "a text-only answer has nothing to remove",
			in:   retentionMessage("assistant", retentionTextPart),
		},
		{
			name: "a plain string content has nothing to remove",
			in:   json.RawMessage(`{"id":"msg_1","role":"assistant","content":"just text"}`),
		},
		{
			name: "an already-marked answer is left alone",
			in:   retentionMessage("assistant", marker),
		},
		{
			name: "an already-marked mixed answer is left alone",
			in:   retentionMessage("assistant", retentionTextPart, marker),
		},
		{
			// Not a state a run produces, but the single-marker invariant is the
			// rewrite's, so it holds for a document that arrives this way too.
			name:        "stale audio next to an existing marker: stripped, still one marker",
			in:          retentionMessage("assistant", marker, retentionAudioPart("blob_late")),
			wantOut:     retentionMessage("assistant", marker),
			wantIDs:     []string{"blob_late"},
			wantChanged: true,
		},
		{
			// A user message with audio is a transcription turn's input clip: another
			// thread kind, never pruned or deleted by the answer lifecycle.
			name: "a user message's audio clip is never removed",
			in:   retentionMessage("user", retentionAudioPart("blob_clip")),
		},
		{
			name: "a message without a role is not an assistant answer",
			in:   json.RawMessage(`{"content":[` + retentionAudioPart("blob_x") + `]}`),
		},
		{
			// A part not yet extracted into chat_blobs (inline data) has no blob to
			// delete, but it is audio all the same and must not survive the marker.
			name:        "an inline audio part is stripped without an id",
			in:          retentionMessage("assistant", `{"type":"audio_url","audio_url":{"url":"data:audio/wav;base64,AAAA"}}`),
			wantOut:     retentionMessage("assistant", marker),
			wantChanged: true,
		},
		{
			// The documents' readers match member names without regard to case, so
			// the rewrite must too, or this spelling would keep its audio.
			name: "member names are matched without regard to case",
			in: json.RawMessage(`{"id":"msg_1","Role":"assistant","CONTENT":[` +
				`{"type":"audio_url","Audio_URL":{"URL":"/api/portal/chat-blobs/blob_ci"}}]}`),
			wantOut:     json.RawMessage(`{"id":"msg_1","Role":"assistant","CONTENT":[` + marker + `]}`),
			wantIDs:     []string{"blob_ci"},
			wantChanged: true,
		},
		{
			name: "an audio_url part without a string url is not an audio part",
			in:   retentionMessage("assistant", `{"type":"audio_url","audio_url":{"url":5}}`, `{"type":"audio_url","audio_url":"x"}`),
		},
		{
			name: "null is not a message",
			in:   json.RawMessage(`null`),
		},
		{
			name: "a number is not a message",
			in:   json.RawMessage(`5`),
		},
		{
			name: "malformed JSON is left to whatever else reads it",
			in:   json.RawMessage(`{"role":"assistant","content":[`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ids, changed, err := markMessageAudioRemoved(tc.in)
			if err != nil {
				t.Fatalf("markMessageAudioRemoved: %v", err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("blob ids = %v, want %v", ids, tc.wantIDs)
			}
			want := tc.wantOut
			if want == nil {
				want = tc.in
			}
			if string(out) != string(want) {
				t.Fatalf("rewritten message =\n%s\nwant\n%s", out, want)
			}
		})
	}
}

// The rewrite is idempotent: marking a message that was just marked changes
// nothing and finds no blob to delete, so a retroactive prune that meets an
// already-pruned answer is a no-op rather than a second marker.
func TestMarkMessageAudioRemovedIsIdempotent(t *testing.T) {
	in := retentionMessage("assistant", retentionAudioPart("blob_a"), retentionAudioPart("blob_b"))
	once, ids, changed, err := markMessageAudioRemoved(in)
	if err != nil || !changed || len(ids) != 2 {
		t.Fatalf("first mark = (%s, %v, %v, %v), want changed with two ids", once, ids, changed, err)
	}
	twice, ids, changed, err := markMessageAudioRemoved(once)
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if changed || len(ids) != 0 || string(twice) != string(once) {
		t.Fatalf("second mark = (%s, %v, %v), want the same message, no ids, unchanged", twice, ids, changed)
	}
}

// What the marker is FOR: the message it leaves has no playable audio for any
// reader of the document -- the audio-url extractor included -- yet is still a
// real, non-empty assistant turn that keeps its id and its other members.
func TestMarkMessageAudioRemovedLeavesNoPlayableAudio(t *testing.T) {
	in := retentionMessage("assistant", retentionAudioPart("blob_a"), retentionAudioPart("blob_b"))
	out, _, changed, err := markMessageAudioRemoved(in)
	if err != nil || !changed {
		t.Fatalf("markMessageAudioRemoved = (%s, %v, %v)", out, changed, err)
	}
	var msg struct {
		ID      string          `json:"id"`
		Status  string          `json:"status"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("rewritten message is not JSON: %v", err)
	}
	if msg.ID != "msg_1" || msg.Status != "complete" {
		t.Fatalf("id/status = %q/%q, want msg_1/complete kept", msg.ID, msg.Status)
	}
	if urls := MessageAudio(msg.Content); len(urls) != 0 {
		t.Fatalf("MessageAudio after the rewrite = %v, want none", urls)
	}
	if has, err := messageHasStoredAudio(out); err != nil || has {
		t.Fatalf("messageHasStoredAudio after the rewrite = (%v, %v), want (false, nil)", has, err)
	}
}

func TestMessageHasStoredAudio(t *testing.T) {
	const marker = `{"type":"audio_removed"}`
	for _, tc := range []struct {
		name string
		in   json.RawMessage
		want bool
	}{
		{"multi-part audio answer", retentionMessage("assistant", retentionAudioPart("blob_a"), retentionAudioPart("blob_b")), true},
		{"single audio part", retentionMessage("assistant", retentionAudioPart("blob_a")), true},
		{"mixed text and audio counts as one audio answer", retentionMessage("assistant", retentionTextPart, retentionAudioPart("blob_a")), true},
		{"text-only answer", retentionMessage("assistant", retentionTextPart), false},
		{"plain string content", json.RawMessage(`{"role":"assistant","content":"hi"}`), false},
		{"not-stored answer (marker only)", retentionMessage("assistant", marker), false},
		{"not-stored mixed answer", retentionMessage("assistant", retentionTextPart, marker), false},
		{"no content at all", json.RawMessage(`{"role":"assistant"}`), false},
		{"an audio_url part without a string url", retentionMessage("assistant", `{"type":"audio_url","audio_url":{}}`), false},
		{"case-insensitive member names", json.RawMessage(`{"CONTENT":[{"Audio_URL":{"url":"/api/portal/chat-blobs/blob_x"}}]}`), true},
		{"null", json.RawMessage(`null`), false},
		{"malformed JSON", json.RawMessage(`{"content":[`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := messageHasStoredAudio(tc.in)
			if err != nil {
				t.Fatalf("messageHasStoredAudio: %v", err)
			}
			if got != tc.want {
				t.Fatalf("messageHasStoredAudio = %v, want %v", got, tc.want)
			}
		})
	}
}

// messageHasStoredAudio is the PART predicate and does not look at the role: the
// walk that counts answers against N asks isAssistantMessage first, so a
// transcription turn's input clip never takes a slot. Pinned so the two stay two
// separate questions.
func TestMessageHasStoredAudioDoesNotLookAtTheRole(t *testing.T) {
	clip := retentionMessage("user", retentionAudioPart("blob_clip"))
	if has, err := messageHasStoredAudio(clip); err != nil || !has {
		t.Fatalf("messageHasStoredAudio(user clip) = (%v, %v), want (true, nil)", has, err)
	}
	if isAssistantMessage(clip) {
		t.Fatal("isAssistantMessage(user clip) = true, want false")
	}
	if !isAssistantMessage(retentionMessage("assistant", retentionTextPart)) {
		t.Fatal("isAssistantMessage(assistant) = false, want true")
	}
	if isAssistantMessage(json.RawMessage(`null`)) || isAssistantMessage(json.RawMessage(`{"role":`)) {
		t.Fatal("isAssistantMessage of a non-message = true, want false")
	}
}

// docOf is a chat document holding the given stored messages, oldest first.
func docOf(messages ...json.RawMessage) *chatDoc {
	return &chatDoc{Messages: messages}
}

// pruneIDs is the blob ids a prune is expected to return, as one slice.
func pruneIDs(ids ...string) []string { return ids }

// pruneAudioBeyond keeps the newest N ANSWERS holding audio and rewrites every older
// one in place into the marker, returning the blob ids the rewrites free. What counts
// as an answer, and as holding audio, is the part this table pins: the role gate
// (a transcription turn's clip is a user message), the part gate (a text answer holds
// none), the marker (an answer already pruned takes no slot), and the unit (one
// answer is one slot whatever its blob count).
func TestPruneAudioBeyond(t *testing.T) {
	const marker = `{"type":"audio_removed"}`
	a := func(ids ...string) json.RawMessage {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = retentionAudioPart(id)
		}
		return retentionMessage("assistant", parts...)
	}
	user := json.RawMessage(`{"id":"u","role":"user","content":"hello"}`)
	clip := retentionMessage("user", retentionAudioPart("blob_clip"))
	text := retentionMessage("assistant", retentionTextPart)
	// gone is an answer in the not-stored state: what a pruned answer becomes and what an
	// already-pruned one is, byte for byte (the rewrite keeps every other member).
	gone := retentionMessage("assistant", marker)

	for _, tc := range []struct {
		name     string
		messages []json.RawMessage
		keepLast int
		wantIDs  []string
		want     []json.RawMessage // the messages after the prune
	}{
		{
			name:     "keep 1 of 3: the two older answers go, the newest stays",
			messages: []json.RawMessage{user, a("blob_1"), user, a("blob_2"), user, a("blob_3")},
			keepLast: 1,
			wantIDs:  pruneIDs("blob_2", "blob_1"), // newest to oldest: the walk's order
			want:     []json.RawMessage{user, gone, user, gone, user, a("blob_3")},
		},
		{
			name:     "keep 2 of 3: only the oldest goes",
			messages: []json.RawMessage{user, a("blob_1"), user, a("blob_2"), user, a("blob_3")},
			keepLast: 2,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{user, gone, user, a("blob_2"), user, a("blob_3")},
		},
		{
			name:     "within N: nothing to do",
			messages: []json.RawMessage{user, a("blob_1"), user, a("blob_2")},
			keepLast: 2,
			want:     []json.RawMessage{user, a("blob_1"), user, a("blob_2")},
		},
		{
			name:     "keep 0 removes every answer's audio (the retroactive prune's zero)",
			messages: []json.RawMessage{user, a("blob_1"), user, a("blob_2")},
			keepLast: 0,
			wantIDs:  pruneIDs("blob_2", "blob_1"),
			want:     []json.RawMessage{user, gone, user, gone},
		},
		{
			name:     "a negative count is no count: nothing is pruned",
			messages: []json.RawMessage{user, a("blob_1"), user, a("blob_2")},
			keepLast: -1,
			want:     []json.RawMessage{user, a("blob_1"), user, a("blob_2")},
		},
		{
			name:     "a multi-blob answer is one slot, and goes with all of its blobs",
			messages: []json.RawMessage{user, a("blob_1a", "blob_1b", "blob_1c"), user, a("blob_2a"), user, a("blob_3a", "blob_3b")},
			keepLast: 2,
			wantIDs:  pruneIDs("blob_1a", "blob_1b", "blob_1c"),
			want:     []json.RawMessage{user, gone, user, a("blob_2a"), user, a("blob_3a", "blob_3b")},
		},
		{
			// The clip of a transcription turn is audio, but not an answer: it takes no slot
			// and is never rewritten, whatever N is.
			name:     "a user message's audio clip is neither counted nor pruned",
			messages: []json.RawMessage{clip, a("blob_1"), clip, a("blob_2")},
			keepLast: 1,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{clip, gone, clip, a("blob_2")},
		},
		{
			// The gate that stops a clip taking a slot: were it counted, the newest
			// answer would be pushed out of the last N by a message that is no answer.
			name:     "a newer user clip takes no slot from the answers before it",
			messages: []json.RawMessage{user, a("blob_1"), user, a("blob_2"), clip},
			keepLast: 1,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{user, gone, user, a("blob_2"), clip},
		},
		{
			name:     "keep 0 still leaves a user's clip alone",
			messages: []json.RawMessage{clip, a("blob_1")},
			keepLast: 0,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{clip, gone},
		},
		{
			name:     "a text answer holds no audio: no slot, never rewritten",
			messages: []json.RawMessage{user, a("blob_1"), user, text, user, a("blob_2")},
			keepLast: 1,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{user, gone, user, text, user, a("blob_2")},
		},
		{
			// An answer whose audio is already gone must not push a still-stored older
			// answer out of the last N.
			name:     "a marked answer takes no slot",
			messages: []json.RawMessage{user, a("blob_1"), user, gone, user, a("blob_3")},
			keepLast: 2,
			want:     []json.RawMessage{user, a("blob_1"), user, gone, user, a("blob_3")},
		},
		{
			name:     "a marked answer in between is skipped, not rewritten twice",
			messages: []json.RawMessage{user, a("blob_1"), user, gone, user, a("blob_3")},
			keepLast: 1,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{user, gone, user, gone, user, a("blob_3")},
		},
		{
			name: "a mixed text+audio answer is one audio answer; its text stays",
			messages: []json.RawMessage{
				retentionMessage("assistant", retentionTextPart, retentionAudioPart("blob_1")), user, a("blob_2"),
			},
			keepLast: 1,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{retentionMessage("assistant", retentionTextPart, marker), user, a("blob_2")},
		},
		{
			name:     "a stored element that is not a message is skipped",
			messages: []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`"x"`), a("blob_1"), json.RawMessage(`{"role":`)},
			keepLast: 0,
			wantIDs:  pruneIDs("blob_1"),
			want:     []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`"x"`), gone, json.RawMessage(`{"role":`)},
		},
		{
			name:     "an empty document",
			messages: nil,
			keepLast: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := docOf(append([]json.RawMessage(nil), tc.messages...)...)
			ids, err := pruneAudioBeyond(doc, tc.keepLast)
			if err != nil {
				t.Fatalf("pruneAudioBeyond: %v", err)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
			if len(doc.Messages) != len(tc.want) {
				t.Fatalf("%d messages after the prune, want %d: pruning rewrites an answer, it never drops one", len(doc.Messages), len(tc.want))
			}
			for i := range tc.want {
				if string(doc.Messages[i]) != string(tc.want[i]) {
					t.Fatalf("message %d = %s, want %s", i, doc.Messages[i], tc.want[i])
				}
			}
		})
	}
}

// A second prune of the same document finds nothing: the answers it rewrote hold no
// audio now and take no slot, so repeating the call (a retry, two commits in a row)
// returns no ids and writes no second marker.
func TestPruneAudioBeyondIsIdempotent(t *testing.T) {
	doc := docOf(
		retentionMessage("assistant", retentionAudioPart("blob_1")),
		retentionMessage("assistant", retentionAudioPart("blob_2")),
		retentionMessage("assistant", retentionAudioPart("blob_3")),
	)
	if ids, err := pruneAudioBeyond(doc, 1); err != nil || !slices.Equal(ids, []string{"blob_2", "blob_1"}) {
		t.Fatalf("first prune = %v, %v, want blob_2 and blob_1", ids, err)
	}
	before := slices.Clone(doc.Messages)
	ids, err := pruneAudioBeyond(doc, 1)
	if err != nil || len(ids) != 0 {
		t.Fatalf("second prune = %v, %v, want none", ids, err)
	}
	for i := range before {
		if string(before[i]) != string(doc.Messages[i]) {
			t.Fatalf("second prune rewrote message %d: %s -> %s", i, before[i], doc.Messages[i])
		}
	}
}

// The commit-time prune reads the thread's retention from the stored settings, and
// acts only when that is a count of at least one. Everything else leaves the
// document and the blob rows alone: unset (keep all, what every legacy chat reads
// as), a negative count (not a count; PrepareChatRun clamps it, but the document is
// schema-free), a malformed value, and 0 -- "store none" is the N=0 run's business
// (it never stores the answer to prune), not this prune's.
func TestPruneForRetention(t *testing.T) {
	three := func() *chatDoc {
		return docOf(
			retentionMessage("assistant", retentionAudioPart("blob_1")),
			retentionMessage("user", retentionTextPart),
			retentionMessage("assistant", retentionAudioPart("blob_2")),
			retentionMessage("user", retentionTextPart),
			retentionMessage("assistant", retentionAudioPart("blob_3")),
		)
	}
	for _, tc := range []struct {
		name     string
		settings string
		wantIDs  []string
	}{
		{"no settings at all", ``, nil},
		{"null settings", `null`, nil},
		{"empty settings", `{}`, nil},
		{"audio settings without the field", `{"kind":"speech","audio":{"voice":"alloy"}}`, nil},
		{"explicit null", `{"audio":{"keep_last_audio":null}}`, nil},
		{"zero is the N=0 run's, not this prune's", `{"audio":{"keep_last_audio":0}}`, nil},
		{"negative reads as unset", `{"audio":{"keep_last_audio":-3}}`, nil},
		{"a string is not a count", `{"audio":{"keep_last_audio":"2"}}`, nil},
		{"a fraction is not a count", `{"audio":{"keep_last_audio":1.5}}`, nil},
		{"malformed settings", `{"audio":`, nil},
		{"settings that are not an object", `[1]`, nil},
		{"keep 1", `{"kind":"speech","audio":{"keep_last_audio":1}}`, []string{"blob_2", "blob_1"}},
		{"keep 2", `{"kind":"speech","audio":{"keep_last_audio":2}}`, []string{"blob_1"}},
		{"keep more than there are", `{"audio":{"keep_last_audio":9}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := three()
			doc.Settings = json.RawMessage(tc.settings)
			before := slices.Clone(doc.Messages)
			ids, err := pruneForRetention(doc)
			if err != nil {
				t.Fatalf("pruneForRetention: %v", err)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
			if tc.wantIDs == nil {
				for i := range before {
					if string(before[i]) != string(doc.Messages[i]) {
						t.Fatalf("no prune was due, yet message %d changed: %s -> %s", i, before[i], doc.Messages[i])
					}
				}
			}
		})
	}
}

// The prune is the commit of an audio answer making room for itself, so it runs only
// when the newest message IS an answer holding audio. A commit whose newest message
// holds none -- a checkpoint (text only, every few seconds), an errored run, a text
// answer -- prunes nothing, however many answers the chat already holds, and the
// newest answer is never itself the one rewritten.
func TestPruneForRetentionOnlyWhenTheNewestAnswerHoldsAudio(t *testing.T) {
	settings := json.RawMessage(`{"audio":{"keep_last_audio":1}}`)
	for _, tc := range []struct {
		name    string
		newest  json.RawMessage
		wantIDs []string
	}{
		{"a text-only newest answer (a checkpoint)", retentionMessage("assistant", retentionTextPart), nil},
		{"an errored newest answer with no content parts", json.RawMessage(`{"id":"m","role":"assistant","status":"error","content":""}`), nil},
		{"a marker newest answer", retentionMessage("assistant", `{"type":"audio_removed"}`), nil},
		{"a user message as the newest", retentionMessage("user", retentionAudioPart("blob_clip")), nil},
		{"an audio answer", retentionMessage("assistant", retentionAudioPart("blob_new")), []string{"blob_2", "blob_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := docOf(
				retentionMessage("assistant", retentionAudioPart("blob_1")),
				retentionMessage("assistant", retentionAudioPart("blob_2")),
				tc.newest,
			)
			doc.Settings = settings
			ids, err := pruneForRetention(doc)
			if err != nil {
				t.Fatalf("pruneForRetention: %v", err)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
			if string(doc.Messages[2]) != string(tc.newest) {
				t.Fatalf("the newest message was rewritten: %s -> %s", tc.newest, doc.Messages[2])
			}
			if tc.wantIDs == nil && strings.Contains(string(doc.Messages[0])+string(doc.Messages[1]), "audio_removed") {
				t.Fatalf("no prune was due, yet an older answer was marked: %s %s", doc.Messages[0], doc.Messages[1])
			}
		})
	}
}
