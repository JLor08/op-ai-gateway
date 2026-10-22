// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"regexp"
	"strings"
	"testing"
)

// The wire shape of a stored chat image reference. Spelled out here rather than
// read from the production constant so a drift in either one fails a test.
const gatewayBlobPrefix = "/api/portal/chat-blobs/"

var blobIDPattern = regexp.MustCompile(`^blob_[0-9a-f]{32}$`)

// recordingChatStore wraps a real chat store and records what the service
// layer asks of it: every SaveChatWithBlobs call with the blobs it carried and
// the blob ids it asked to delete (deletes, one entry per save, nil when the
// save deleted nothing), and every ChatBlobOwner lookup. hooks let a test change
// the world at the moment of the save (a chat deleted mid-run) or make the owner
// lookup fail.
type recordingChatStore struct {
	ChatStore
	saves        [][]store.ChatBlob
	deletes      [][]string
	ownerLookups []string
	ownerErr     error
	beforeSave   func()
}

func (r *recordingChatStore) SaveChatWithBlobs(ctx context.Context, chat store.Chat, blobs []store.ChatBlob, deleteBlobIDs []string) error {
	if r.beforeSave != nil {
		r.beforeSave()
	}
	r.saves = append(r.saves, blobs)
	r.deletes = append(r.deletes, deleteBlobIDs)
	return r.ChatStore.SaveChatWithBlobs(ctx, chat, blobs, deleteBlobIDs)
}

func (r *recordingChatStore) ChatBlobOwner(ctx context.Context, id string) (string, error) {
	r.ownerLookups = append(r.ownerLookups, id)
	if r.ownerErr != nil {
		return "", r.ownerErr
	}
	return r.ChatStore.ChatBlobOwner(ctx, id)
}

// storedBlobs is how many blobs were handed to the store across every save.
func (r *recordingChatStore) storedBlobs() int {
	n := 0
	for _, blobs := range r.saves {
		n += len(blobs)
	}
	return n
}

type blobTestEnv struct {
	svc    *Service
	rec    *recordingChatStore
	mem    *store.MemoryChatStore
	cipher *capture.Cipher
	owner  auth.Token
}

// newBlobTestEnv builds a Service over a recording memory chat store. cipher
// selects the sealed (KeyVersion 1) or the plain RAM-fallback (KeyVersion 0)
// persistence; maxBlobBytes 0 leaves the service default in place.
func newBlobTestEnv(t *testing.T, withCipher bool, maxBlobBytes int) *blobTestEnv {
	t.Helper()
	mem := store.NewMemoryChatStore(0)
	rec := &recordingChatStore{ChatStore: mem}
	deps := ServiceDeps{Chats: rec, MaxChatBlobBytes: maxBlobBytes}
	var cipher *capture.Cipher
	if withCipher {
		var err error
		cipher, err = capture.New(captureTestKey)
		if err != nil {
			t.Fatalf("capture.New: %v", err)
		}
		deps.Cipher = cipher
	}
	return &blobTestEnv{svc: NewService(deps), rec: rec, mem: mem, cipher: cipher, owner: chatToken("usr_a")}
}

func (e *blobTestEnv) newChat(t *testing.T, content string) string {
	t.Helper()
	created, err := e.svc.CreateChat(context.Background(), e.owner, CreateChatRequest{Content: json.RawMessage(content)})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return created.ID
}

func (e *blobTestEnv) emptyChat(t *testing.T) string {
	t.Helper()
	return e.newChat(t, `{"settings":{},"messages":[]}`)
}

func (e *blobTestEnv) stored(t *testing.T, chatID string) json.RawMessage {
	t.Helper()
	got, err := e.svc.GetChat(context.Background(), e.owner, chatID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	return got.Content
}

func (e *blobTestEnv) send(chatID string, content json.RawMessage) ([]ChatAPIMessage, error) {
	history, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		UserMessage: content,
		Settings:    ChatRunSettings{Model: "m"},
	})
	return history, err
}

// sendImage runs one image turn through the service and returns the id of the
// blob the image was stored under (read back from the stored document).
func (e *blobTestEnv) sendImage(t *testing.T, chatID string, raw []byte) string {
	t.Helper()
	if _, err := e.send(chatID, imageContent("look", dataURL("image/png", raw))); err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	urls := storedImageURLs(t, e.stored(t, chatID))
	if len(urls) == 0 {
		t.Fatal("the stored document carries no image url")
	}
	return strings.TrimPrefix(urls[len(urls)-1], gatewayBlobPrefix)
}

// blobBytes opens a stored blob row the way the serving path will: through
// the cipher when the row is sealed.
func (e *blobTestEnv) blobBytes(t *testing.T, row store.ChatBlob) []byte {
	t.Helper()
	if row.KeyVersion == 0 {
		return row.Blob
	}
	if e.cipher == nil {
		t.Fatal("a sealed blob in a service with no cipher")
	}
	plain, err := e.cipher.Open(row.Blob)
	if err != nil {
		t.Fatalf("Open blob: %v", err)
	}
	return plain
}

func blobTestBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed
	}
	return b
}

func dataURL(mediaType string, raw []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// imageContent is a user message's content array: one text part and one image
// part carrying url.
func imageContent(text, url string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`[{"type":"text","text":%q},{"type":"image_url","image_url":{"url":%q}}]`, text, url))
}

// storedImageURLs returns every image_url.url in a stored document, in order.
func storedImageURLs(t *testing.T, content json.RawMessage) []string {
	t.Helper()
	var doc struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("stored document is not JSON: %v", err)
	}
	var urls []string
	for _, m := range doc.Messages {
		var parts []struct {
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.ImageURL != nil {
				urls = append(urls, p.ImageURL.URL)
			}
		}
	}
	return urls
}

// The core behaviour: an inline data: image in the user's turn is stored in
// chat_blobs (sealed when a cipher is configured), the part's url is rewritten
// to the gateway path before the document is sealed, and the history the run
// is handed still carries the bytes inline.
func TestPrepareChatRunExtractsInlineImageIntoChatBlobs(t *testing.T) {
	for _, withCipher := range []bool{true, false} {
		t.Run(fmt.Sprintf("cipher=%v", withCipher), func(t *testing.T) {
			e := newBlobTestEnv(t, withCipher, 0)
			chatID := e.emptyChat(t)
			raw := blobTestBytes(2048, 7)
			url := dataURL("image/png", raw)

			history, err := e.send(chatID, json.RawMessage(fmt.Sprintf(
				`[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"detail":"low","url":%q}}]`, url)))
			if err != nil {
				t.Fatalf("PrepareChatRun: %v", err)
			}

			// The immediate run keeps the bytes it was just given: no round trip.
			if len(history) != 1 || !strings.Contains(string(history[0].Content), url) {
				t.Fatalf("the returned run history must keep the inline data url, got %s", history[0].Content)
			}

			// The sealed document holds a gateway reference instead of the bytes.
			doc := string(e.stored(t, chatID))
			if strings.Contains(doc, "data:") {
				t.Fatalf("the stored document still carries inline bytes: %.200s", doc)
			}
			urls := storedImageURLs(t, json.RawMessage(doc))
			if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) {
				t.Fatalf("stored image urls = %v, want one %s url", urls, gatewayBlobPrefix)
			}
			id := strings.TrimPrefix(urls[0], gatewayBlobPrefix)
			if !blobIDPattern.MatchString(id) {
				t.Fatalf("blob id %q, want blob_<32 hex>", id)
			}
			// Everything else in the part survives the rewrite, in its original order.
			for _, want := range []string{`"text":"what is this"`, `"image_url":{"detail":"low","url":"` + gatewayBlobPrefix + id + `"}`} {
				if !strings.Contains(doc, want) {
					t.Fatalf("stored document lost %s: %.300s", want, doc)
				}
			}

			// One blob, owned by this chat, holding the decoded bytes.
			if len(e.rec.saves) != 1 || len(e.rec.saves[0]) != 1 {
				t.Fatalf("saves = %d (blobs %d), want one save carrying one blob", len(e.rec.saves), e.rec.storedBlobs())
			}
			row, err := e.mem.ChatBlobByID(context.Background(), id)
			if err != nil {
				t.Fatalf("ChatBlobByID: %v", err)
			}
			if row.ChatID != chatID || row.ContentType != "image/png" || row.ByteLen != int64(len(raw)) || row.CreatedAt.IsZero() {
				t.Fatalf("blob row = %+v, want chat %s, image/png, %d bytes, a created_at", row, chatID, len(raw))
			}
			if got := e.blobBytes(t, row); !bytes.Equal(got, raw) {
				t.Fatalf("stored bytes differ from the uploaded bytes (%d vs %d)", len(got), len(raw))
			}
			if withCipher {
				if row.KeyVersion != capture.KeyVersion || bytes.Equal(row.Blob, raw) {
					t.Fatalf("a configured cipher must seal the blob: KeyVersion %d, plaintext at rest %v", row.KeyVersion, bytes.Equal(row.Blob, raw))
				}
			} else if row.KeyVersion != 0 {
				t.Fatalf("no cipher: KeyVersion = %d, want 0 (plain RAM fallback)", row.KeyVersion)
			}
		})
	}
}

// An edit / regenerate hands the service the whole history. The document stored
// is rewritten, but the caller's slice is not: the run is still handed the
// inline bytes the history carried, and the request value is left as received.
func TestPrepareChatRunEditedHistoryKeepsInlineBytesForTheRun(t *testing.T) {
	e := newBlobTestEnv(t, false, 0)
	chatID := e.emptyChat(t)
	url := dataURL("image/gif", blobTestBytes(200, 6))
	msg := json.RawMessage(fmt.Sprintf(`{"id":"u1","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}`, url))
	edited := []json.RawMessage{msg}

	history, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		EditedHistory: edited, Settings: ChatRunSettings{Model: "m"},
	})
	if err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	if !strings.Contains(string(history[0].Content), url) {
		t.Fatalf("the run history lost the inline bytes: %.200s", history[0].Content)
	}
	if !strings.Contains(string(edited[0]), url) {
		t.Fatalf("the caller's edited_history was rewritten in place: %.200s", edited[0])
	}
	if urls := storedImageURLs(t, e.stored(t, chatID)); len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) {
		t.Fatalf("stored urls = %v, want one gateway url", urls)
	}
}

// The cap is measured on the document that is actually sealed, so it runs
// after the rewrite: a thread whose inline image alone is bigger than the 4 MiB
// text cap now takes its turn, because the image is no longer in the document.
func TestPrepareChatRunMeasuresTheSizeCapAfterTheRewrite(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	chatID := e.emptyChat(t)
	raw := blobTestBytes(3900<<10, 1) // 3.9 MiB of image, ~5.2 MiB as a data url
	content := imageContent("a big one", dataURL("image/png", raw))
	if len(content) <= MaxChatContentBytes {
		t.Fatalf("precondition: the inline turn is %d bytes, not over the %d cap", len(content), MaxChatContentBytes)
	}

	// Control: the same bytes inline in the document trip the cap. Autosave does
	// not extract, so this is what every inline image used to run into.
	inline := json.RawMessage(`{"settings":{},"messages":[{"id":"m1","role":"user","content":` + string(content) + `}]}`)
	if _, err := e.svc.SaveChat(context.Background(), e.owner, chatID, UpdateChatRequest{Content: &inline}); !errors.Is(err, ErrChatTooLarge) {
		t.Fatalf("control: bare SaveChat of the inline document = %v, want ErrChatTooLarge", err)
	}

	if _, err := e.send(chatID, content); err != nil {
		t.Fatalf("PrepareChatRun with a %d byte inline image: %v", len(raw), err)
	}
	if got := e.stored(t, chatID); len(got) > 4<<10 {
		t.Fatalf("the sealed document is %d bytes; the image should have left it", len(got))
	}
	id := strings.TrimPrefix(storedImageURLs(t, e.stored(t, chatID))[0], gatewayBlobPrefix)
	row, err := e.mem.ChatBlobByID(context.Background(), id)
	if err != nil || row.ByteLen != int64(len(raw)) {
		t.Fatalf("the image must be in chat_blobs in full: row %+v, err %v", row, err)
	}
}

// A document that already carries a gateway url stores nothing new, however
// many later turns, edits or autosaves go over it; and a later turn does not
// re-verify the urls the chat already stored.
func TestPrepareChatRunStoresAnImageExactlyOnce(t *testing.T) {
	e := newBlobTestEnv(t, true, 0)
	chatID := e.emptyChat(t)
	id := e.sendImage(t, chatID, blobTestBytes(512, 3))
	if e.rec.storedBlobs() != 1 {
		t.Fatalf("after the first turn %d blobs stored, want 1", e.rec.storedBlobs())
	}
	url := gatewayBlobPrefix + id

	// A later plain turn: nothing new, no ownership lookups for what the chat
	// stored itself.
	if _, err := e.send(chatID, json.RawMessage(`"and now?"`)); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if e.rec.storedBlobs() != 1 || len(e.rec.ownerLookups) != 0 {
		t.Fatalf("a later turn stored %d blobs and made %d owner lookups, want 1 and 0", e.rec.storedBlobs(), len(e.rec.ownerLookups))
	}

	// An autosave of the very same document: not an extraction point at all.
	same := e.stored(t, chatID)
	if _, err := e.svc.SaveChat(context.Background(), e.owner, chatID, UpdateChatRequest{Content: &same}); err != nil {
		t.Fatalf("autosave: %v", err)
	}

	// An edit / regenerate resubmits the whole history, gateway urls included:
	// they are verified (one lookup for the one id) and kept as they are.
	var doc chatDoc
	if err := json.Unmarshal(e.stored(t, chatID), &doc); err != nil {
		t.Fatal(err)
	}
	history, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		EditedHistory: doc.Messages, Settings: ChatRunSettings{Model: "m"},
	})
	if err != nil {
		t.Fatalf("edited_history run: %v", err)
	}
	if e.rec.storedBlobs() != 1 {
		t.Fatalf("an edit re-stored the image: %d blobs, want 1", e.rec.storedBlobs())
	}
	if len(e.rec.ownerLookups) != 1 {
		t.Fatalf("owner lookups = %v, want exactly one for the one resubmitted id", e.rec.ownerLookups)
	}
	// The reference is kept in the stored document (below); the run's history
	// carries the image itself (the resolution, service_chats_resolve.go).
	if got := historyImageURLs(history); len(got) != 1 || got[0] != dataURL("image/png", blobTestBytes(512, 3)) {
		t.Fatalf("the run history must carry the resolved image, got %.80v", got)
	}
	if got := storedImageURLs(t, e.stored(t, chatID)); len(got) != 1 || got[0] != url {
		t.Fatalf("stored urls after the edit = %v, want [%s]", got, url)
	}
}

// The allow-list, and the strict reading of "a base64 data url": anything that
// is not exactly <allow-listed type>;base64,<standard padded base64> is
// refused, and a refusal stores nothing -- not the turn, not an image that came
// with it.
func TestPrepareChatRunRefusesUnsupportedInlineData(t *testing.T) {
	urlSafe := base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe}) // "-__-": not the standard alphabet
	cases := []struct{ name, url string }{
		{"html", dataURL("text/html", []byte("<script>alert(1)</script>"))},
		{"svg", dataURL("image/svg+xml", []byte("<svg onload=alert(1)/>"))},
		{"octet-stream", dataURL("application/octet-stream", []byte{1, 2, 3})},
		{"image/jpg is not a registered type", dataURL("image/jpg", []byte{1, 2, 3})},
		{"image/bmp", dataURL("image/bmp", []byte{1, 2, 3})},
		{"no media type", "data:;base64,AAEC"},
		{"charset parameter", "data:image/png;charset=utf-8;base64,AAEC"},
		{"not base64 at all", "data:image/png,AAEC"},
		{"url-safe alphabet", "data:image/png;base64," + urlSafe},
		{"unpadded", "data:image/png;base64,AQ"},
		{"garbage payload", "data:image/png;base64,%%%%"},
		{"empty payload", "data:image/png;base64,"},
		{"no comma", "data:image/png;base64"},
		{"upper-case scheme still validated", "DATA:text/html;base64,PGI+"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newBlobTestEnv(t, true, 0)
			chatID := e.emptyChat(t)
			before := string(e.stored(t, chatID))

			// A valid image rides along in the same turn: refusing the bad one must
			// refuse the whole turn.
			content := json.RawMessage(fmt.Sprintf(
				`[{"type":"image_url","image_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q}}]`,
				dataURL("image/png", blobTestBytes(64, 1)), tc.url))
			_, err := e.send(chatID, content)
			if !errors.Is(err, ErrChatBlobTypeUnsupported) {
				t.Fatalf("PrepareChatRun = %v, want ErrChatBlobTypeUnsupported", err)
			}
			if e.rec.storedBlobs() != 0 || len(e.rec.saves) != 0 {
				t.Fatalf("a refused turn reached the store: %d saves, %d blobs", len(e.rec.saves), e.rec.storedBlobs())
			}
			if after := string(e.stored(t, chatID)); after != before {
				t.Fatalf("a refused turn changed the document:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// The four allow-listed types are accepted and stored under their canonical
// lower-case type; the data url's own spelling (case, line-wrapped base64, set
// bits in the unused tail of the last character) does not change what is
// stored: the decoded bytes, exactly.
func TestPrepareChatRunNormalisesAcceptedInlineData(t *testing.T) {
	raw := blobTestBytes(300, 9)
	wrapped := base64.StdEncoding.EncodeToString(raw)
	wrapped = wrapped[:76] + "\r\n" + wrapped[76:200] + "\n" + wrapped[200:]
	cases := []struct {
		name, url, wantType string
		want                []byte
	}{
		{"png", dataURL("image/png", raw), "image/png", raw},
		{"jpeg", dataURL("image/jpeg", raw), "image/jpeg", raw},
		{"webp", dataURL("image/webp", raw), "image/webp", raw},
		{"gif", dataURL("image/gif", raw), "image/gif", raw},
		{"upper-case type", dataURL("IMAGE/PNG", raw), "image/png", raw},
		{"upper-case scheme and flag", "DATA:image/webp;BASE64," + base64.StdEncoding.EncodeToString(raw), "image/webp", raw},
		{"line-wrapped base64", "data:image/jpeg;base64," + wrapped, "image/jpeg", raw},
		// "AB==" is one zero byte spelled with its spare bits set; the canonical
		// spelling is "AA==". The bytes are the same, so it is accepted.
		{"set bits in the spare tail", "data:image/png;base64,AB==", "image/png", []byte{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newBlobTestEnv(t, false, 0)
			chatID := e.emptyChat(t)
			id := e.sendImageURL(t, chatID, tc.url)
			row, err := e.mem.ChatBlobByID(context.Background(), id)
			if err != nil {
				t.Fatalf("ChatBlobByID: %v", err)
			}
			if row.ContentType != tc.wantType || !bytes.Equal(row.Blob, tc.want) || row.ByteLen != int64(len(tc.want)) {
				t.Fatalf("row = type %q, %d bytes (equal %v), want %q and the %d decoded bytes", row.ContentType, len(row.Blob), bytes.Equal(row.Blob, tc.want), tc.wantType, len(tc.want))
			}
		})
	}
}

func (e *blobTestEnv) sendImageURL(t *testing.T, chatID, url string) string {
	t.Helper()
	if _, err := e.send(chatID, imageContent("look", url)); err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	urls := storedImageURLs(t, e.stored(t, chatID))
	if len(urls) != 1 {
		t.Fatalf("stored image urls = %v, want one", urls)
	}
	return strings.TrimPrefix(urls[0], gatewayBlobPrefix)
}

// MaxChatBlobBytes is enforced here: the cap is on decoded bytes, a payload
// exactly at the cap is stored, and one byte over is refused with nothing
// stored.
func TestPrepareChatRunEnforcesThePerBlobCap(t *testing.T) {
	const limit = 100
	e := newBlobTestEnv(t, true, limit)
	chatID := e.emptyChat(t)

	if _, err := e.send(chatID, imageContent("fits", dataURL("image/png", blobTestBytes(limit, 1)))); err != nil {
		t.Fatalf("a payload exactly at the cap must be stored: %v", err)
	}
	if e.rec.storedBlobs() != 1 {
		t.Fatalf("blobs stored = %d, want 1", e.rec.storedBlobs())
	}

	before := string(e.stored(t, chatID))
	_, err := e.send(chatID, json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q}}]`,
		dataURL("image/png", blobTestBytes(10, 2)), dataURL("image/png", blobTestBytes(limit+1, 3)))))
	if !errors.Is(err, ErrChatBlobTooLarge) {
		t.Fatalf("PrepareChatRun over the cap = %v, want ErrChatBlobTooLarge", err)
	}
	if e.rec.storedBlobs() != 1 || string(e.stored(t, chatID)) != before {
		t.Fatalf("a refused turn changed state: %d blobs stored, document changed %v", e.rec.storedBlobs(), string(e.stored(t, chatID)) != before)
	}
}

func TestNewServiceChatBlobCapDefaultsTo20MiB(t *testing.T) {
	for _, configured := range []int{0, -1} {
		if got := NewService(ServiceDeps{MaxChatBlobBytes: configured}).maxChatBlobBytes; got != 20<<20 {
			t.Fatalf("MaxChatBlobBytes %d -> %d, want the 20 MiB default", configured, got)
		}
	}
	if got := NewService(ServiceDeps{MaxChatBlobBytes: 4096}).maxChatBlobBytes; got != 4096 {
		t.Fatalf("an explicit MaxChatBlobBytes must be honoured, got %d", got)
	}
}

// A /chat-blobs/ url a client submits is only kept if THIS chat holds that
// blob. Another user's blob, another chat's blob (even of the same user) and an
// id that does not exist all fail the same way, so the answer cannot be used to
// probe for ids.
func TestPrepareChatRunRequiresSubmittedBlobUrlsToBelongToTheChat(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)
	mine := e.emptyChat(t)
	myBlob := e.sendImage(t, mine, blobTestBytes(256, 5))

	otherUser := chatToken("usr_b")
	theirs, err := e.svc.CreateChat(ctx, otherUser, CreateChatRequest{Content: json.RawMessage(`{"settings":{},"messages":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.PrepareChatRun(ctx, otherUser, theirs.ID, PrepareRunRequest{
		UserMessage: imageContent("secret", dataURL("image/png", blobTestBytes(256, 6))),
		Settings:    ChatRunSettings{Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	theirBlob := strings.TrimPrefix(storedImageURLs(t, mustContent(t, e.svc, otherUser, theirs.ID))[0], gatewayBlobPrefix)

	sibling := e.emptyChat(t) // same owner, different chat

	history := func(url string) []json.RawMessage {
		return []json.RawMessage{json.RawMessage(fmt.Sprintf(
			`{"id":"u1","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}`, url))}
	}
	refused := []struct {
		name   string
		chat   string
		blobID string
	}{
		{"another user's blob", mine, theirBlob},
		{"another chat of the same user", sibling, myBlob},
		{"an id that does not exist", mine, "blob_" + strings.Repeat("0", 32)},
		{"not even an id", mine, "../../x"},
	}
	for _, tc := range refused {
		t.Run("edited_history/"+tc.name, func(t *testing.T) {
			before := string(e.stored(t, tc.chat))
			saves := len(e.rec.saves)
			_, _, err := e.svc.PrepareChatRun(ctx, e.owner, tc.chat, PrepareRunRequest{
				EditedHistory: history(gatewayBlobPrefix + tc.blobID), Settings: ChatRunSettings{Model: "m"},
			})
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("PrepareChatRun = %v, want ErrChatBlobNotOwned", err)
			}
			if string(e.stored(t, tc.chat)) != before || len(e.rec.saves) != saves {
				t.Fatalf("a refused edit changed state")
			}
		})
		t.Run("user_message/"+tc.name, func(t *testing.T) {
			_, err := e.send(tc.chat, imageContent("graft", gatewayBlobPrefix+tc.blobID))
			if !errors.Is(err, ErrChatBlobNotOwned) {
				t.Fatalf("PrepareChatRun = %v, want ErrChatBlobNotOwned", err)
			}
		})
	}

	// The blob this chat does hold is kept untouched.
	if _, _, err := e.svc.PrepareChatRun(ctx, e.owner, mine, PrepareRunRequest{
		EditedHistory: history(gatewayBlobPrefix + myBlob), Settings: ChatRunSettings{Model: "m"},
	}); err != nil {
		t.Fatalf("an owned blob url must be accepted: %v", err)
	}
	if got := storedImageURLs(t, e.stored(t, mine)); len(got) != 1 || got[0] != gatewayBlobPrefix+myBlob {
		t.Fatalf("stored urls = %v, want the owned url unchanged", got)
	}
}

func mustContent(t *testing.T, svc *Service, owner auth.Token, chatID string) json.RawMessage {
	t.Helper()
	got, err := svc.GetChat(context.Background(), owner, chatID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Content
}

// A store failure while checking ownership is a failure, not a verdict: only
// "no such blob" means "not yours".
func TestPrepareChatRunOwnerLookupFailureIsNotAnOwnershipVerdict(t *testing.T) {
	e := newBlobTestEnv(t, false, 0)
	chatID := e.emptyChat(t)
	boom := errors.New("db down")
	e.rec.ownerErr = boom
	_, err := e.send(chatID, imageContent("x", gatewayBlobPrefix+"blob_"+strings.Repeat("a", 32)))
	if !errors.Is(err, boom) || errors.Is(err, ErrChatBlobNotOwned) {
		t.Fatalf("PrepareChatRun = %v, want the store error, not ErrChatBlobNotOwned", err)
	}
}

// Creating a chat is not an extraction point: the row does not exist yet, so a
// blob row for it would break the chat_id foreign key. A chat created with an
// inline image succeeds, keeps the image inline (and renders as before), and is
// migrated by its first run -- including when the inline image already takes
// up nearly the whole document cap.
func TestCreateChatKeepsInlineImagesAndTheFirstRunMigratesThem(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)
	legacyRaw := blobTestBytes(2900<<10, 4) // ~3.87 MiB of base64: just under the document cap
	legacyURL := dataURL("image/png", legacyRaw)
	content := fmt.Sprintf(`{"settings":{},"messages":[{"id":"u1","role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]},{"id":"a1","role":"assistant","content":"nice"}]}`, legacyURL)
	if len(content) > MaxChatContentBytes {
		t.Fatalf("precondition: the legacy document is %d bytes, over the cap", len(content))
	}

	chatID := e.newChat(t, content)
	if got := string(e.stored(t, chatID)); got != content {
		t.Fatalf("CreateChat must store the document verbatim, inline image included")
	}
	if len(e.rec.saves) != 0 {
		t.Fatalf("CreateChat reached SaveChatWithBlobs (%d saves)", len(e.rec.saves))
	}

	// Bare autosave is not an extraction point either.
	same := json.RawMessage(content)
	if _, err := e.svc.SaveChat(ctx, e.owner, chatID, UpdateChatRequest{Content: &same}); err != nil {
		t.Fatalf("autosave of a legacy document: %v", err)
	}
	if len(e.rec.saves) != 0 || string(e.stored(t, chatID)) != content {
		t.Fatalf("autosave extracted or rewrote: %d saves", len(e.rec.saves))
	}

	// The first run migrates the old image AND takes a second, new one -- the
	// inline total is far over the cap, the rewritten document is tiny.
	newRaw := blobTestBytes(1500<<10, 8)
	if _, err := e.send(chatID, imageContent("another", dataURL("image/jpeg", newRaw))); err != nil {
		t.Fatalf("first run of a near-cap legacy thread: %v", err)
	}
	urls := storedImageURLs(t, e.stored(t, chatID))
	if len(urls) != 2 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) || !strings.HasPrefix(urls[1], gatewayBlobPrefix) {
		t.Fatalf("stored urls = %.120v, want both images migrated to gateway urls", urls)
	}
	if e.rec.storedBlobs() != 2 {
		t.Fatalf("blobs stored = %d, want 2 (the legacy image and the new one)", e.rec.storedBlobs())
	}
	first, err := e.mem.ChatBlobByID(ctx, strings.TrimPrefix(urls[0], gatewayBlobPrefix))
	if err != nil || !bytes.Equal(e.blobBytes(t, first), legacyRaw) {
		t.Fatalf("the legacy image must survive migration byte for byte (err %v)", err)
	}
}

// What an extraction does not recognise it leaves alone: other messages stay
// byte-identical, external urls are not ours to store, and shapes that are not
// an image part cannot fail a run.
func TestPrepareChatRunLeavesEverythingElseUntouched(t *testing.T) {
	e := newBlobTestEnv(t, false, 0)
	const m1 = `{"id":"m1","role":"user","content":"plain"}`
	const m2 = `{"id":"m2","role":"assistant","status":"complete","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}`
	const m3 = `{"id":"m3","role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,AAEC"},{"type":"image_url","image_url":{"url":42}},{"type":"image_url","image_url":{}},{"type":"image_url"},"just a string",null,7]}`
	chatID := e.newChat(t, `{"settings":{},"messages":[`+m1+`,`+m2+`,`+m3+`]}`)

	if _, err := e.send(chatID, json.RawMessage(`"next"`)); err != nil {
		t.Fatalf("PrepareChatRun: %v", err)
	}
	var doc chatDoc
	if err := json.Unmarshal(e.stored(t, chatID), &doc); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{m1, m2, m3} {
		if got := string(doc.Messages[i]); got != want {
			t.Fatalf("message %d changed:\n got  %s\n want %s", i, got, want)
		}
	}
	if e.rec.storedBlobs() != 0 || len(e.rec.ownerLookups) != 0 {
		t.Fatalf("stored %d blobs / %d lookups for a document with nothing to extract", e.rec.storedBlobs(), len(e.rec.ownerLookups))
	}
}

// buildAPIHistory reads message keys the way encoding/json does, ignoring case
// -- so the extraction has to, or a part spelled "Image_Url" would reach the
// upstream (and the document) unchecked.
func TestPrepareChatRunReadsMessageKeysCaseInsensitively(t *testing.T) {
	e := newBlobTestEnv(t, false, 0)
	chatID := e.emptyChat(t)
	_, _, err := e.svc.PrepareChatRun(context.Background(), e.owner, chatID, PrepareRunRequest{
		EditedHistory: []json.RawMessage{json.RawMessage(`{"id":"u","role":"user","CONTENT":[{"type":"image_url","Image_Url":{"URL":"data:text/html;base64,PGI+"}}]}`)},
		Settings:      ChatRunSettings{Model: "m"},
	})
	if !errors.Is(err, ErrChatBlobTypeUnsupported) {
		t.Fatalf("PrepareChatRun = %v, want ErrChatBlobTypeUnsupported", err)
	}
}

// A generated image takes the same road: the assistant turn's data: part is
// extracted at the commit, so a generated image can exceed the text cap too.
func TestCommitAssistantExtractsGeneratedImages(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)
	chatID := e.emptyChat(t)
	if _, err := e.send(chatID, json.RawMessage(`"a cat"`)); err != nil {
		t.Fatal(err)
	}
	raw := blobTestBytes(5<<20, 2) // ~6.7 MiB of base64: over the 4 MiB document cap
	parts := json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"a revised prompt"},{"type":"image_url","image_url":{"url":%q}}]`, dataURL("image/webp", raw)))

	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: parts}, "complete"); err != nil {
		t.Fatalf("CommitAssistant: %v", err)
	}
	doc := string(e.stored(t, chatID))
	if strings.Contains(doc, "data:") || !strings.Contains(doc, `"status":"complete"`) || !strings.Contains(doc, `"text":"a revised prompt"`) {
		t.Fatalf("assistant turn not committed as a reference: %.300s", doc)
	}
	urls := storedImageURLs(t, json.RawMessage(doc))
	if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) {
		t.Fatalf("stored urls = %v", urls)
	}
	row, err := e.mem.ChatBlobByID(ctx, strings.TrimPrefix(urls[0], gatewayBlobPrefix))
	if err != nil {
		t.Fatalf("ChatBlobByID: %v", err)
	}
	if row.ChatID != chatID || row.ContentType != "image/webp" || !bytes.Equal(e.blobBytes(t, row), raw) {
		t.Fatalf("blob row = chat %s type %s, %d bytes: not the generated image", row.ChatID, row.ContentType, row.ByteLen)
	}
}

// The image pipeline (gateway/chat_runs_images.go) names a generated image
// "image/" + the upstream's output_format, and the run commit refuses any type
// outside chatBlobMediaTypes -- so a format the allow-list lacks is a generated
// image lost at commit. The two sets live in different packages and nothing
// links them, so this pins the formats the pipeline emits today (sd-server's
// png, jpeg and webp): narrowing the allow-list below them, or this set
// drifting from the pipeline's, fails here rather than in a user's chat.
func TestChatBlobAllowListCoversTheGeneratedImageFormats(t *testing.T) {
	ctx := context.Background()
	for _, mediaType := range []string{"image/png", "image/jpeg", "image/webp"} {
		t.Run(mediaType, func(t *testing.T) {
			if _, listed := chatBlobMediaTypes[mediaType]; !listed {
				t.Fatalf("%s is not in chatBlobMediaTypes: a generated image of that format would be refused at commit", mediaType)
			}
			e := newBlobTestEnv(t, false, 0)
			chatID := e.emptyChat(t)
			if _, err := e.send(chatID, json.RawMessage(`"a cat"`)); err != nil {
				t.Fatal(err)
			}
			raw := blobTestBytes(64, 5)
			parts := json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, dataURL(mediaType, raw)))
			if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: parts}, "complete"); err != nil {
				t.Fatalf("CommitAssistant refused a generated %s image: %v", mediaType, err)
			}
			urls := storedImageURLs(t, e.stored(t, chatID))
			if len(urls) != 1 || !strings.HasPrefix(urls[0], gatewayBlobPrefix) {
				t.Fatalf("stored urls = %v", urls)
			}
			row, err := e.mem.ChatBlobByID(ctx, strings.TrimPrefix(urls[0], gatewayBlobPrefix))
			if err != nil {
				t.Fatalf("ChatBlobByID: %v", err)
			}
			if row.ContentType != mediaType || !bytes.Equal(row.Blob, raw) {
				t.Fatalf("blob row = type %s, %d bytes: not the generated image", row.ContentType, len(row.Blob))
			}
		})
	}
}

// The allow-list and the cap apply to a generated image exactly as to an
// uploaded one, and a refusal leaves the transcript as it was.
func TestCommitAssistantRefusesBadGeneratedImages(t *testing.T) {
	e := newBlobTestEnv(t, false, 1000)
	chatID := e.emptyChat(t)
	if _, err := e.send(chatID, json.RawMessage(`"a cat"`)); err != nil {
		t.Fatal(err)
	}
	before := string(e.stored(t, chatID))

	for _, tc := range []struct {
		name string
		url  string
		want error
	}{
		{"bmp", dataURL("image/bmp", []byte{1}), ErrChatBlobTypeUnsupported},
		{"too large", dataURL("image/png", blobTestBytes(1001, 1)), ErrChatBlobTooLarge},
	} {
		parts := json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, tc.url))
		if err := e.svc.CommitAssistant(context.Background(), e.owner, chatID, AssistantTurn{ContentParts: parts}, "complete"); !errors.Is(err, tc.want) {
			t.Fatalf("%s: CommitAssistant = %v, want %v", tc.name, err, tc.want)
		}
	}
	if string(e.stored(t, chatID)) != before || e.rec.storedBlobs() != 0 {
		t.Fatalf("a refused commit changed state")
	}
}

// Checkpoints run every few seconds for the whole of a text run. They carry no
// image and must cost no blob work: no blobs, and no ownership lookups for the
// references the chat already holds.
func TestCheckpointAssistantDoesNoBlobWork(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, false, 0)
	chatID := e.emptyChat(t)
	e.sendImage(t, chatID, blobTestBytes(128, 1))
	lookups, blobs := len(e.rec.ownerLookups), e.rec.storedBlobs()

	for i := range 3 {
		if err := e.svc.CheckpointAssistant(ctx, e.owner, chatID, AssistantTurn{Content: strings.Repeat("x", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{Content: "done"}, "complete"); err != nil {
		t.Fatal(err)
	}
	if len(e.rec.ownerLookups) != lookups || e.rec.storedBlobs() != blobs {
		t.Fatalf("checkpoints did blob work: lookups %d->%d, blobs %d->%d", lookups, len(e.rec.ownerLookups), blobs, e.rec.storedBlobs())
	}
}

// A chat deleted while a run is committing: the save finds no chat. That must
// come back as store.ErrNotFound, which is what the run executor's
// commitFailureIsChatGone recognises -- so the run ends cleanly instead of as a
// 500 -- and the blob that was about to be stored is not left behind.
func TestRunCommitOnADeletedChatReportsNotFoundAndStoresNoBlob(t *testing.T) {
	ctx := context.Background()
	e := newBlobTestEnv(t, true, 0)

	t.Run("PrepareChatRun", func(t *testing.T) {
		chatID := e.emptyChat(t)
		e.rec.beforeSave = func() { _ = e.mem.DeleteChat(ctx, chatID) }
		defer func() { e.rec.beforeSave = nil }()
		_, err := e.send(chatID, imageContent("look", dataURL("image/png", blobTestBytes(64, 1))))
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("PrepareChatRun = %v, want store.ErrNotFound", err)
		}
	})
	t.Run("CommitAssistant", func(t *testing.T) {
		chatID := e.emptyChat(t)
		if _, err := e.send(chatID, json.RawMessage(`"a cat"`)); err != nil {
			t.Fatal(err)
		}
		e.rec.beforeSave = func() { _ = e.mem.DeleteChat(ctx, chatID) }
		defer func() { e.rec.beforeSave = nil }()
		parts := json.RawMessage(fmt.Sprintf(`[{"type":"image_url","image_url":{"url":%q}}]`, dataURL("image/png", blobTestBytes(64, 2))))
		err := e.svc.CommitAssistant(ctx, e.owner, chatID, AssistantTurn{ContentParts: parts}, "complete")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("CommitAssistant = %v, want store.ErrNotFound", err)
		}
		last := e.rec.saves[len(e.rec.saves)-1]
		if len(last) != 1 {
			t.Fatalf("the failed save carried %d blobs, want the one", len(last))
		}
		if _, err := e.mem.ChatBlobByID(ctx, last[0].ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the blob of a refused save is stored anyway: %v", err)
		}
	})
}
