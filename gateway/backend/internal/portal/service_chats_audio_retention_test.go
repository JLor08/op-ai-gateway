// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The lazy, going-forward half of the audio answer lifecycle (#281): when a speech
// answer commits, the thread's retention (ChatAudioSettings.KeepLastAudio) decides
// whether older answers lose their audio in the SAME atomic save. These tests drive
// it the way a run does -- PrepareChatRun for the user turn, CommitAssistant for the
// answer -- over a real store, and read the outcome back from the stored document
// and the blob rows.

// retentionEnv is a Service over one chat-store driver, wrapped so every save's
// blobs and deletes are on record.
type retentionEnv struct {
	svc   *Service
	rec   *recordingChatStore
	owner auth.Token
}

// forEachRetentionLeg runs run once per driver the portal package can reach in every
// run: the in-memory store and a freshly migrated SQLite file, whose blob rows have
// real foreign keys to the chat. There is deliberately no PostgreSQL leg here: the
// store package owns the shared test database (every subtest of it drops and
// re-migrates its public schema), so a second package opening THAT database would
// race it, and the atomic delete the prune relies on is pinned on all three drivers
// there (TestSaveChatWithBlobsDeletes* in internal/store). The composed prune on
// PostgreSQL is covered by TestPruneChatAudioToKeepLastOnPostgres, which brings its
// own database (service_chats_audio_postgres_test.go).
func forEachRetentionLeg(t *testing.T, run func(t *testing.T, e *retentionEnv)) {
	t.Helper()
	owner := chatToken("usr_a")
	t.Run("memory", func(t *testing.T) {
		rec := &recordingChatStore{ChatStore: store.NewMemoryChatStore(0)}
		run(t, &retentionEnv{svc: NewService(ServiceDeps{Chats: rec}), rec: rec, owner: owner})
	})
	t.Run("sqlite", func(t *testing.T) {
		ctx := context.Background()
		st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		seedRetentionOwner(t, st, owner)
		rec := &recordingChatStore{ChatStore: st}
		run(t, &retentionEnv{svc: NewService(ServiceDeps{Chats: rec}), rec: rec, owner: owner})
	})
}

// seedRetentionOwner creates the user the chats of a SQL leg belong to (chats.user_id
// is a foreign key).
func seedRetentionOwner(t *testing.T, st *store.SQLStore, owner auth.Token) {
	t.Helper()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	if err := st.CreateUser(context.Background(), store.User{
		ID: owner.UserID, Email: "usr_a@example.test", DisplayName: "A", Role: "user",
		Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func (e *retentionEnv) newChat(t *testing.T, content string) string {
	t.Helper()
	created, err := e.svc.CreateChat(context.Background(), e.owner, CreateChatRequest{Content: json.RawMessage(content)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return created.ID
}

// ask sends one prompt to a speech thread, with audio as the thread's settings.
func (e *retentionEnv) ask(t *testing.T, chatID, prompt string, audio *ChatAudioSettings) {
	t.Helper()
	if _, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		UserMessage: json.RawMessage(fmt.Sprintf("%q", prompt)),
		Settings:    ChatRunSettings{Model: "m", Kind: chatKindSpeech, Audio: audio},
	}); err != nil {
		t.Fatalf("PrepareChatRun(%q): %v", prompt, err)
	}
}

// answerWith commits one complete speech answer whose audio is wavs, one audio_url
// part (hence one chat_blobs row) each, and returns the ids of those blobs.
func (e *retentionEnv) answerWith(t *testing.T, chatID string, wavs ...[]byte) []string {
	t.Helper()
	parts := make([]string, len(wavs))
	for i, wav := range wavs {
		parts[i] = fmt.Sprintf(`{"type":"audio_url","audio_url":{"url":%q}}`, dataURL("audio/wav", wav))
	}
	if err := e.svc.CommitAssistant(context.Background(), e.owner, chatID, AssistantTurn{ContentParts: json.RawMessage("[" + strings.Join(parts, ",") + "]")}, "complete"); err != nil {
		t.Fatalf("CommitAssistant: %v", err)
	}
	msgs := e.messages(t, chatID)
	return blobIDsOf(MessageAudio(msgs[len(msgs)-1].Content))
}

// turn is ask followed by answerWith, with a one-blob answer tagged by its own byte.
func (e *retentionEnv) turn(t *testing.T, chatID string, audio *ChatAudioSettings, tag byte) []string {
	t.Helper()
	e.ask(t, chatID, fmt.Sprintf("prompt %d", tag), audio)
	return e.answerWith(t, chatID, taggedWAV(tag))
}

// taggedWAV is a small WAV-like payload ending in tag, so two answers never share bytes.
func taggedWAV(tag byte) []byte {
	w := wavLike(64)
	w[len(w)-1] = tag
	return w
}

func blobIDsOf(urls []string) []string {
	ids := make([]string, len(urls))
	for i, u := range urls {
		ids[i] = strings.TrimPrefix(u, gatewayBlobPrefix)
	}
	return ids
}

type storedMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Status  string          `json:"status"`
	Content json.RawMessage `json:"content"`
}

func (e *retentionEnv) messages(t *testing.T, chatID string) []storedMessage {
	t.Helper()
	got, err := e.svc.GetChat(context.Background(), e.owner, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	var doc struct {
		Messages []storedMessage `json:"messages"`
	}
	if err := json.Unmarshal(got.Content, &doc); err != nil {
		t.Fatalf("stored document: %v", err)
	}
	return doc.Messages
}

// answers is the stored assistant messages, oldest first.
func (e *retentionEnv) answers(t *testing.T, chatID string) []storedMessage {
	t.Helper()
	var out []storedMessage
	for _, m := range e.messages(t, chatID) {
		if m.Role == "assistant" {
			out = append(out, m)
		}
	}
	return out
}

// requireStored fails unless msg still holds exactly the audio of ids, each a blob
// row the store serves, and no marker.
func (e *retentionEnv) requireStored(t *testing.T, label string, msg storedMessage, ids []string) {
	t.Helper()
	if got := blobIDsOf(MessageAudio(msg.Content)); !slices.Equal(got, ids) {
		t.Fatalf("%s: audio = %v, want the stored %v (content %s)", label, got, ids, msg.Content)
	}
	if strings.Contains(string(msg.Content), removedAudioPartType) {
		t.Fatalf("%s: carries the %s marker though its audio is kept: %s", label, removedAudioPartType, msg.Content)
	}
	for _, id := range ids {
		if _, err := e.rec.ChatBlobByID(context.Background(), id); err != nil {
			t.Fatalf("%s: blob %s: %v, want it stored", label, id, err)
		}
	}
}

// requirePruned fails unless msg is an answer whose audio is gone -- the marker
// instead of the audio parts -- and every blob in ids has left the store. The
// message itself stays: its id, role and status are the answer's own.
func (e *retentionEnv) requirePruned(t *testing.T, label string, msg storedMessage, ids []string) {
	t.Helper()
	if msg.Content == nil || string(msg.Content) != `[{"type":"audio_removed"}]` {
		t.Fatalf("%s: content = %s, want exactly the audio_removed marker", label, msg.Content)
	}
	if msg.ID == "" || msg.Role != "assistant" || msg.Status != "complete" {
		t.Fatalf("%s: message = %+v, want the same assistant answer, id and status kept", label, msg)
	}
	for _, id := range ids {
		if _, err := e.rec.ChatBlobByID(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: blob %s: %v, want it deleted (store.ErrNotFound)", label, id, err)
		}
	}
}

func keepLast(n int) *ChatAudioSettings { return &ChatAudioSettings{KeepLastAudio: &n} }

// commitAudioAnswer commits one speech answer the way the run executor does: a
// speech-run turn (AudioAnswer) that either actually streamed (AudioStreamed) or fell
// back to the buffered one-shot, with one audio_url part per wav. It returns the blob
// ids the answer stored -- none when it is committed as the not-stored marker.
func (e *retentionEnv) commitAudioAnswer(t *testing.T, chatID string, streamed bool, wavs ...[]byte) []string {
	t.Helper()
	parts := make([]string, len(wavs))
	for i, wav := range wavs {
		parts[i] = fmt.Sprintf(`{"type":"audio_url","audio_url":{"url":%q}}`, dataURL("audio/wav", wav))
	}
	turn := AssistantTurn{
		ContentParts:  json.RawMessage("[" + strings.Join(parts, ",") + "]"),
		AudioAnswer:   true,
		AudioStreamed: streamed,
	}
	if err := e.svc.CommitAssistant(context.Background(), e.owner, chatID, turn, "complete"); err != nil {
		t.Fatalf("CommitAssistant: %v", err)
	}
	msgs := e.messages(t, chatID)
	return blobIDsOf(MessageAudio(msgs[len(msgs)-1].Content))
}

// With keep_last_audio = 0 a speech answer the run ACTUALLY STREAMED is committed as
// the not-stored marker and stores no blob at all: the audio was synthesized only to
// bill ADR-068's loud non-zero (the user heard it live), and "store none" means the
// stored transcript holds the marker, never the bytes. The turn is still a complete,
// regenerate-able assistant answer with its own id.
func TestN0StreamedCommitsMarker(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		settings := keepLast(0)

		e.ask(t, chatID, "say hi", settings)
		if ids := e.commitAudioAnswer(t, chatID, true /*streamed*/, taggedWAV(1)); len(ids) != 0 {
			t.Fatalf("a streamed N=0 answer stored %d blobs, want 0 (it is the marker)", len(ids))
		}

		answers := e.answers(t, chatID)
		if len(answers) != 1 {
			t.Fatalf("stored answers = %d, want 1", len(answers))
		}
		e.requirePruned(t, "streamed N=0 answer", answers[0], nil)
		if e.rec.storedBlobs() != 0 {
			t.Fatalf("the store was handed %d blobs, want 0: a streamed N=0 answer stores none", e.rec.storedBlobs())
		}
		for i, d := range e.rec.deletes {
			if len(d) != 0 {
				t.Fatalf("save %d deleted %v; a streamed N=0 answer that stored nothing deletes nothing", i, d)
			}
		}
	})
}

// With keep_last_audio = 0 an answer that fell back to the BUFFERED one-shot is not
// lost: the user never heard it live, so it is STORED. But the thread must not
// accumulate on a model that only sometimes falls back, so each buffered-fallback
// answer keeps only itself -- every older stored answer becomes a marker in the same
// save (keep-last-1). [Review Focus]
func TestN0BufferedFallbackKeepsOne(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		settings := keepLast(0)

		// The paid, un-heard answer is stored, not turned into a marker.
		e.ask(t, chatID, "one", settings)
		first := e.commitAudioAnswer(t, chatID, false /*buffered*/, taggedWAV(1))
		if len(first) != 1 {
			t.Fatalf("the first buffered-fallback answer stored %d blobs, want 1 (it must be kept)", len(first))
		}
		e.requireStored(t, "first buffered-fallback answer", e.answers(t, chatID)[0], first)

		// A second buffered-fallback answer is stored too, and the first becomes a
		// marker: keep-last-1, so an N=0 thread never accumulates.
		e.ask(t, chatID, "two", settings)
		second := e.commitAudioAnswer(t, chatID, false, taggedWAV(2))
		if len(second) != 1 {
			t.Fatalf("the second buffered-fallback answer stored %d blobs, want 1", len(second))
		}
		answers := e.answers(t, chatID)
		if len(answers) != 2 {
			t.Fatalf("stored answers = %d, want 2 (a prune rewrites an answer, never drops one)", len(answers))
		}
		e.requirePruned(t, "first buffered-fallback answer after the second", answers[0], first)
		e.requireStored(t, "second buffered-fallback answer", answers[1], second)

		// The first answer's blob left in the save that stored the second.
		var deleted [][]string
		for _, d := range e.rec.deletes {
			if len(d) > 0 {
				deleted = append(deleted, d)
			}
		}
		if len(deleted) != 1 || !slices.Equal(deleted[0], first) {
			t.Fatalf("blob deletes per save = %v, want just %v in the second answer's save", deleted, first)
		}
	})
}

// A canceled/aborted speech run commits the not-stored marker turn instead of an
// empty canceled bubble: the run billed ADR-068's loud-zero incomplete and stored no
// audio bytes, and the marker is the only addition, so the bubble reads "not stored"
// and the turn is regenerate-able (it keeps an id). Nothing is stored or deleted.
func TestAbortedSpeechRunCommitsMarker(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		chatID := e.newChat(t, `{"settings":{"kind":"speech"},"messages":[]}`)
		e.ask(t, chatID, "speak", nil)

		// An aborted run produced no stored audio: no content parts, status canceled.
		if err := e.svc.CommitAssistant(context.Background(), e.owner, chatID, AssistantTurn{AudioAnswer: true}, "canceled"); err != nil {
			t.Fatalf("CommitAssistant(canceled): %v", err)
		}

		answers := e.answers(t, chatID)
		if len(answers) != 1 {
			t.Fatalf("stored answers = %d, want 1", len(answers))
		}
		a := answers[0]
		if a.Content == nil || string(a.Content) != `[{"type":"audio_removed"}]` {
			t.Fatalf("aborted turn content = %s, want exactly the audio_removed marker (not an empty bubble)", a.Content)
		}
		if a.ID == "" {
			t.Fatalf("aborted turn has no id; the marker turn must be regenerate-able")
		}
		if a.Role != "assistant" || a.Status != "canceled" {
			t.Fatalf("aborted turn = %+v, want a canceled assistant turn", a)
		}
		if e.rec.storedBlobs() != 0 {
			t.Fatalf("the aborted run stored %d blobs, want 0", e.rec.storedBlobs())
		}
		for i, d := range e.rec.deletes {
			if len(d) != 0 {
				t.Fatalf("save %d deleted %v; an aborted run deletes nothing", i, d)
			}
		}
	})
}

// With keep_last_audio = 1, each new answer's commit takes the audio of the one
// before it: after the third commit the first two answers are markers whose blob
// rows are gone, and the third is whole. The prune rides the commit of the answer
// it makes room for, so no commit leaves more than one answer stored.
func TestCommitPrunesBeyondKeepLast(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		settings := keepLast(1)

		first := e.turn(t, chatID, settings, 1)
		e.requireStored(t, "answer 1 after its own commit", e.answers(t, chatID)[0], first)

		second := e.turn(t, chatID, settings, 2)
		answers := e.answers(t, chatID)
		e.requirePruned(t, "answer 1 after the 2nd commit", answers[0], first)
		e.requireStored(t, "answer 2 after its own commit", answers[1], second)

		third := e.turn(t, chatID, settings, 3)
		answers = e.answers(t, chatID)
		if len(answers) != 3 {
			t.Fatalf("stored answers = %d, want all three messages kept (a prune rewrites an answer, it never drops one)", len(answers))
		}
		e.requirePruned(t, "answer 1", answers[0], first)
		e.requirePruned(t, "answer 2", answers[1], second)
		e.requireStored(t, "answer 3 (the newest)", answers[2], third)

		// The deletes went with the saves that made them necessary: none at the first
		// commit, the previous answer's blob at the second and the third.
		var deleted [][]string
		for _, d := range e.rec.deletes {
			if len(d) > 0 {
				deleted = append(deleted, d)
			}
		}
		if len(deleted) != 2 || !slices.Equal(deleted[0], first) || !slices.Equal(deleted[1], second) {
			t.Fatalf("blob deletes per save = %v, want %v then %v, each in the save of the answer that displaced it", deleted, first, second)
		}

		// The user turns around the answers are untouched.
		msgs := e.messages(t, chatID)
		if len(msgs) != 6 || msgs[0].Role != "user" || msgs[2].Role != "user" || msgs[4].Role != "user" {
			t.Fatalf("stored roles = %v, want user/assistant three times", roles(msgs))
		}
	})
}

func roles(msgs []storedMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// The default, and every thread stored before the setting existed, is "keep all":
// KeepLastAudio unset leaves every answer as it commits -- no marker, no blob row
// removed, no delete handed to the store. Three spellings of unset: no audio
// settings at all, audio settings without the field, and a negative count that
// reached the document by a path PrepareChatRun's clamp never saw (the document is
// schema-free), which reads as unset too.
func TestCommitKeepAllRetainsEverything(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		for _, tc := range []struct {
			name  string
			audio *ChatAudioSettings
		}{
			{"no audio settings (legacy chat)", nil},
			{"audio settings without keep_last_audio", &ChatAudioSettings{Voice: "alloy"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
				var ids [][]string
				for tag := byte(1); tag <= 3; tag++ {
					ids = append(ids, e.turn(t, chatID, tc.audio, tag))
				}
				answers := e.answers(t, chatID)
				if len(answers) != 3 {
					t.Fatalf("stored answers = %d, want 3", len(answers))
				}
				for i, a := range answers {
					e.requireStored(t, fmt.Sprintf("answer %d", i+1), a, ids[i])
				}
			})
		}
		t.Run("negative count written past PrepareChatRun", func(t *testing.T) {
			// Two earlier answers, then a third committed over a negative retention: the
			// document is built by hand, as a raw client could write it.
			const old1, old2 = gatewayBlobPrefix + "blob_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", gatewayBlobPrefix + "blob_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			chatID := e.newChat(t, fmt.Sprintf(`{"settings":{"kind":"speech","audio":{"keep_last_audio":-2}},"messages":[`+
				`{"id":"u1","role":"user","content":"a"},{"id":"a1","role":"assistant","status":"complete","content":[{"type":"audio_url","audio_url":{"url":%q}}]},`+
				`{"id":"u2","role":"user","content":"b"},{"id":"a2","role":"assistant","status":"complete","content":[{"type":"audio_url","audio_url":{"url":%q}}]},`+
				`{"id":"u3","role":"user","content":"c"}]}`, old1, old2))
			e.answerWith(t, chatID, taggedWAV(9))

			doc := string(e.messagesRaw(t, chatID))
			if strings.Contains(doc, removedAudioPartType) || !strings.Contains(doc, old1) || !strings.Contains(doc, old2) {
				t.Fatalf("a negative keep_last_audio pruned: %.600s", doc)
			}
		})
		for i, d := range e.rec.deletes {
			if len(d) != 0 {
				t.Fatalf("save %d asked the store to delete %v; keep-all deletes nothing", i, d)
			}
		}
	})
}

func (e *retentionEnv) messagesRaw(t *testing.T, chatID string) json.RawMessage {
	t.Helper()
	got, err := e.svc.GetChat(context.Background(), e.owner, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	return got.Content
}

// N counts answers, not blobs (ADR-070 stores one long answer as several): with
// keep_last_audio = 2 a three-blob answer, a one-blob answer and a two-blob answer
// leave the last two ANSWERS stored (three blobs between them) and prune the
// oldest answer whole -- all three of its blobs, not just the first. Counting
// blobs would have kept only the newest answer's two.
func TestCommitKeepLastCountsAnswersNotBlobs(t *testing.T) {
	forEachRetentionLeg(t, func(t *testing.T, e *retentionEnv) {
		chatID := e.newChat(t, `{"settings":{},"messages":[]}`)
		settings := keepLast(2)

		e.ask(t, chatID, "chapter one", settings)
		long := e.answerWith(t, chatID, taggedWAV(1), taggedWAV(2), taggedWAV(3))
		if len(long) != 3 {
			t.Fatalf("the long answer stored %d blobs, want 3", len(long))
		}
		e.ask(t, chatID, "short", settings)
		short := e.answerWith(t, chatID, taggedWAV(4))

		// Two answers, four blobs: within N = 2, so nothing is pruned yet.
		answers := e.answers(t, chatID)
		e.requireStored(t, "long answer, two answers in", answers[0], long)
		e.requireStored(t, "short answer, two answers in", answers[1], short)

		e.ask(t, chatID, "chapter two", settings)
		pair := e.answerWith(t, chatID, taggedWAV(5), taggedWAV(6))

		answers = e.answers(t, chatID)
		e.requirePruned(t, "long answer (the oldest of three)", answers[0], long)
		e.requireStored(t, "short answer", answers[1], short)
		e.requireStored(t, "two-blob answer (the newest)", answers[2], pair)
	})
}
