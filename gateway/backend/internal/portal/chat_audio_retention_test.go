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
