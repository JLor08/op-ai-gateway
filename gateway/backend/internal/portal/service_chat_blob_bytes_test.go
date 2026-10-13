// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// payloadCountingStore counts the lookups the service makes (payload loads via
// ChatBlobByID, owner reads via ChatOwner, and full-transcript reads via
// ChatByID) and lets a test make them misreport: it is how the tests below pin
// that a foreign caller's request never loads the bytes, that a disagreeing
// store is not trusted, and that the serving path never reads a transcript.
type payloadCountingStore struct {
	ChatStore
	payloadLoads  int
	chatOwnerHits int
	chatByIDHits  int
	// byIDRow, when set, replaces the row ChatBlobByID returns.
	byIDRow *store.ChatBlob
	// chatOwnerErr, when set, is what ChatOwner answers.
	chatOwnerErr error
}

func (p *payloadCountingStore) ChatBlobByID(ctx context.Context, id string) (store.ChatBlob, error) {
	p.payloadLoads++
	if p.byIDRow != nil {
		return *p.byIDRow, nil
	}
	return p.ChatStore.ChatBlobByID(ctx, id)
}

func (p *payloadCountingStore) ChatOwner(ctx context.Context, chatID string) (string, error) {
	p.chatOwnerHits++
	if p.chatOwnerErr != nil {
		return "", p.chatOwnerErr
	}
	return p.ChatStore.ChatOwner(ctx, chatID)
}

func (p *payloadCountingStore) ChatByID(ctx context.Context, id string) (store.ChatRow, error) {
	p.chatByIDHits++
	return p.ChatStore.ChatByID(ctx, id)
}

// The owner reads their own image back: the bytes are exactly what was uploaded
// (unsealed on the cipher path, as stored on the RAM path) and the content type
// is the stored one.
func TestChatBlobBytesServesTheOwnersImage(t *testing.T) {
	for _, withCipher := range []bool{true, false} {
		t.Run(fmt.Sprintf("cipher=%v", withCipher), func(t *testing.T) {
			e := newBlobTestEnv(t, withCipher, 0)
			chatID := e.emptyChat(t)
			raw := blobTestBytes(4096, 3)
			id := e.sendImage(t, chatID, raw)

			got, contentType, err := e.svc.ChatBlobBytes(context.Background(), e.owner, id)
			if err != nil {
				t.Fatalf("ChatBlobBytes: %v", err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("served %d bytes, want the %d uploaded bytes", len(got), len(raw))
			}
			if contentType != "image/png" {
				t.Fatalf("content type = %q, want image/png", contentType)
			}
			if withCipher {
				row, err := e.mem.ChatBlobByID(context.Background(), id)
				if err != nil || bytes.Equal(row.Blob, raw) {
					t.Fatalf("the stored row must be sealed (err %v), or the test proves nothing about unsealing", err)
				}
			}
		})
	}
}

// The serving path is one image per <img> and a chat can hold dozens, so its
// ownership read must be the owner-only lookup, never ChatByID (which reads the
// whole sealed transcript): for the owner, for a foreign caller and for a missing
// id alike, ChatByID is never called, and a served image costs exactly one owner
// lookup and one payload load.
func TestChatBlobBytesReadsTheOwnerOnlyNeverTheTranscript(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	id := e.sendImage(t, e.emptyChat(t), blobTestBytes(512, 6))
	ctx := context.Background()

	counting := &payloadCountingStore{ChatStore: e.mem}
	svc := NewService(ServiceDeps{Chats: counting, Cipher: e.cipher})

	if _, _, err := svc.ChatBlobBytes(ctx, e.owner, id); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if counting.chatOwnerHits != 1 || counting.payloadLoads != 1 {
		t.Fatalf("a served image cost %d owner lookups and %d payload loads, want 1 and 1", counting.chatOwnerHits, counting.payloadLoads)
	}
	if _, _, err := svc.ChatBlobBytes(ctx, chatToken("usr_b"), id); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("foreign caller: %v", err)
	}
	if _, _, err := svc.ChatBlobBytes(ctx, e.owner, "blob_00000000000000000000000000000000"); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("missing id: %v", err)
	}
	if counting.chatByIDHits != 0 {
		t.Fatalf("the serving path called ChatByID %d times, want 0 (it reads the whole sealed transcript)", counting.chatByIDHits)
	}
}

// Another user's blob is ErrChatBlobNotFound -- the blob's own sentinel, which
// is neither ErrChatNotFound nor store.ErrNotFound -- and the payload is never
// loaded for them: ownership is decided before the bytes are touched.
func TestChatBlobBytesForeignOwnerIsNotFoundAndLoadsNoPayload(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	id := e.sendImage(t, e.emptyChat(t), blobTestBytes(512, 1))
	counting := &payloadCountingStore{ChatStore: e.mem}
	svc := NewService(ServiceDeps{Chats: counting, Cipher: e.cipher})

	got, contentType, err := svc.ChatBlobBytes(context.Background(), chatToken("usr_b"), id)
	if !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("foreign caller: err = %v, want ErrChatBlobNotFound", err)
	}
	if errors.Is(err, ErrChatNotFound) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign caller: err = %v must be the blob's own sentinel only", err)
	}
	if got != nil || contentType != "" {
		t.Fatalf("foreign caller got %d bytes / type %q, want nothing", len(got), contentType)
	}
	if counting.payloadLoads != 0 {
		t.Fatalf("the payload was loaded %d times for a foreign caller, want 0 (ownership first)", counting.payloadLoads)
	}
}

// The grafted-url case. CreateChat and a bare autosave do not validate the
// urls a transcript carries, so user B can store A's /chat-blobs/{id} in B's own
// chat. That reference must not make A's bytes readable to B: ownership is the
// BLOB's chat's owner, never the chat that merely mentions it.
func TestChatBlobBytesGraftedForeignIdInOwnChatIsNotFound(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	aID := e.sendImage(t, e.emptyChat(t), blobTestBytes(512, 5))

	bToken := chatToken("usr_b")
	graft := fmt.Sprintf(`{"settings":{},"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}]}`, gatewayBlobPrefix+aID)
	bChat, err := e.svc.CreateChat(context.Background(), bToken, CreateChatRequest{Content: json.RawMessage(graft)})
	if err != nil {
		t.Fatalf("CreateChat with a grafted url: %v", err)
	}
	// The graft really is stored: B's own chat carries A's blob url.
	if urls := storedImageURLs(t, bChat.Content); len(urls) != 1 || urls[0] != gatewayBlobPrefix+aID {
		t.Fatalf("B's chat urls = %v, want the grafted %s%s", urls, gatewayBlobPrefix, aID)
	}

	if _, _, err := e.svc.ChatBlobBytes(context.Background(), bToken, aID); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("B reading A's blob through a grafted url: err = %v, want ErrChatBlobNotFound", err)
	}
	// And A still can.
	if _, _, err := e.svc.ChatBlobBytes(context.Background(), e.owner, aID); err != nil {
		t.Fatalf("the owner lost access: %v", err)
	}
}

// A never-stored id, a blob whose chat was deleted (the blobs go with it), and
// a chat row that is gone while the blob row lingers are all the same answer.
func TestChatBlobBytesMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)
	chatID := e.emptyChat(t)
	id := e.sendImage(t, chatID, blobTestBytes(256, 2))

	if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, "blob_"+"0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("never-stored id: err = %v, want ErrChatBlobNotFound", err)
	}
	if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, ""); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("empty id: err = %v, want ErrChatBlobNotFound", err)
	}

	// The chat row is gone but the blob row is still there (an orphan the
	// store's cascade should make impossible): fail closed, no bytes.
	orphan := &payloadCountingStore{ChatStore: e.mem, chatOwnerErr: store.ErrNotFound}
	orphanSvc := NewService(ServiceDeps{Chats: orphan, Cipher: e.cipher})
	if _, _, err := orphanSvc.ChatBlobBytes(ctx, e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("blob whose chat row is gone: err = %v, want ErrChatBlobNotFound", err)
	}
	if orphan.payloadLoads != 0 {
		t.Fatalf("the payload was loaded for an orphan blob")
	}

	// Deleting the chat takes the blob with it.
	if err := e.svc.DeleteChat(ctx, e.owner, chatID); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if _, _, err := e.svc.ChatBlobBytes(ctx, e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("blob of a deleted chat: err = %v, want ErrChatBlobNotFound", err)
	}
}

// The blob can be evicted between the owner lookup and the payload load (the
// volatile RAM store drops whole chats under its byte budget): that race is a
// plain not-found, not a 500. The owner lookup and the chat still answer; only
// the payload load finds the blob gone.
func TestChatBlobBytesEvictedBetweenLookupsIsNotFound(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	id := e.sendImage(t, e.emptyChat(t), blobTestBytes(256, 9))
	svc := NewService(ServiceDeps{Chats: blobGoneStore{ChatStore: e.mem}, Cipher: e.cipher})

	if _, _, err := svc.ChatBlobBytes(context.Background(), e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("blob evicted after the owner lookup: err = %v, want ErrChatBlobNotFound", err)
	}
}

// blobGoneStore answers ChatBlobByID as if the blob had just been evicted.
type blobGoneStore struct{ ChatStore }

func (blobGoneStore) ChatBlobByID(context.Context, string) (store.ChatBlob, error) {
	return store.ChatBlob{}, store.ErrNotFound
}

// A row whose chat differs from the one the owner lookup named is not trusted:
// two lookups that disagree fail closed rather than serve bytes on the strength
// of the first.
func TestChatBlobBytesRowOfAnotherChatIsNotFound(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	id := e.sendImage(t, e.emptyChat(t), blobTestBytes(256, 4))
	row, err := e.mem.ChatBlobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("ChatBlobByID: %v", err)
	}
	row.ChatID = "chat_someone_elses"
	svc := NewService(ServiceDeps{Chats: &payloadCountingStore{ChatStore: e.mem, byIDRow: &row}, Cipher: e.cipher})

	if _, _, err := svc.ChatBlobBytes(context.Background(), e.owner, id); !errors.Is(err, ErrChatBlobNotFound) {
		t.Fatalf("row of another chat: err = %v, want ErrChatBlobNotFound", err)
	}
}

// Defense in depth for the XSS hardening: the allow-list is enforced at store
// time, but the serving path does not rely on that alone. A row whose type is
// not an allow-listed image type (tampered, or written by a future/other
// writer) is never served and its type is never echoed.
func TestChatBlobBytesNeverServesAnUnlistedType(t *testing.T) {
	for _, contentType := range []string{"text/html", "image/svg+xml", "application/javascript", "IMAGE/PNG", "image/png; charset=utf-8", ""} {
		t.Run(contentType, func(t *testing.T) {
			e := newBlobTestEnv(t, true, 0)
			chatID := e.emptyChat(t)
			plain := []byte("<script>alert(1)</script>")
			if err := e.mem.PutChatBlob(context.Background(), store.ChatBlob{
				ID: "blob_tampered", ChatID: chatID, KeyVersion: capture.KeyVersion, Blob: e.cipher.Seal(plain),
				ContentType: contentType, ByteLen: int64(len(plain)), CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("PutChatBlob: %v", err)
			}

			got, served, err := e.svc.ChatBlobBytes(context.Background(), e.owner, "blob_tampered")
			if !errors.Is(err, ErrChatBlobNotFound) {
				t.Fatalf("type %q: err = %v, want ErrChatBlobNotFound", contentType, err)
			}
			if got != nil || served != "" {
				t.Fatalf("type %q: got %d bytes / type %q, want nothing", contentType, len(got), served)
			}
		})
	}
}

// Failures that are not "no such blob for you" stay failures: a store error on
// the owner lookup, a sealed blob with no cipher to open it, and bytes that do
// not authenticate must not be reported as not-found (that would hide a broken
// store or key as an empty image) -- and must not serve anything.
func TestChatBlobBytesOperationalFailuresAreNotNotFound(t *testing.T) {
	ctx := context.Background()

	t.Run("store error on the owner lookup", func(t *testing.T) {
		e := newBlobTestEnv(t, true, 0)
		id := e.sendImage(t, e.emptyChat(t), blobTestBytes(64, 1))
		boom := errors.New("database is down")
		e.rec.ownerErr = boom
		_, _, err := e.svc.ChatBlobBytes(ctx, e.owner, id)
		if !errors.Is(err, boom) || errors.Is(err, ErrChatBlobNotFound) {
			t.Fatalf("err = %v, want the store error, not not-found", err)
		}
	})

	t.Run("sealed blob and no cipher", func(t *testing.T) {
		e := newBlobTestEnv(t, true, 0)
		id := e.sendImage(t, e.emptyChat(t), blobTestBytes(64, 1))
		svc := NewService(ServiceDeps{Chats: e.mem}) // same rows, no cipher
		_, _, err := svc.ChatBlobBytes(ctx, e.owner, id)
		if !errors.Is(err, ErrChatCipherMissing) || errors.Is(err, ErrChatBlobNotFound) {
			t.Fatalf("err = %v, want ErrChatCipherMissing", err)
		}
	})

	t.Run("bytes that do not authenticate", func(t *testing.T) {
		e := newBlobTestEnv(t, true, 0)
		chatID := e.emptyChat(t)
		garbage := bytes.Repeat([]byte{0xAB}, 64)
		if err := e.mem.PutChatBlob(ctx, store.ChatBlob{
			ID: "blob_garbage", ChatID: chatID, KeyVersion: capture.KeyVersion, Blob: garbage,
			ContentType: "image/png", ByteLen: int64(len(garbage)), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("PutChatBlob: %v", err)
		}
		got, _, err := e.svc.ChatBlobBytes(ctx, e.owner, "blob_garbage")
		if err == nil || errors.Is(err, ErrChatBlobNotFound) || got != nil {
			t.Fatalf("got %d bytes, err = %v; want a non-not-found open error and no bytes", len(got), err)
		}
	})
}
