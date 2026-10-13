// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
	"time"
)

const (
	// chatBlobURLPrefix is where a stored chat image is served from. A content
	// part's image_url.url is rewritten to chatBlobURLPrefix + blob id once the
	// bytes live in chat_blobs.
	chatBlobURLPrefix = "/api/portal/chat-blobs/"
	// chatBlobIDPrefix starts every blob id; the rest is 32 random hex digits.
	chatBlobIDPrefix = "blob_"
)

// chatBlobMediaTypes is the allow-list of media types a stored chat image may
// carry. The type is attacker-influenced (it comes from a client's data url) and
// becomes the Content-Type of a same-origin, cookie-authenticated URL, so
// nothing outside this set is ever stored -- in particular never text/html or
// image/svg+xml. Extend it deliberately, per consumer.
//
// It must stay in sync with the image pipeline's output formats: a generated
// image is committed as "image/" + the upstream's output_format (see
// imagePartsFrom / imageSubtypePattern in gateway/chat_runs_images.go), and the
// commit refuses a type missing from this set, losing the image. Every format
// the pipeline can emit has to be listed here; TestChatBlobAllowListCoversTheGeneratedImageFormats
// pins the ones it emits today (png, jpeg, webp).
var chatBlobMediaTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
	"image/gif":  {},
}

// extractChatBlobs moves the inline data: images of a chat document into
// chat_blobs rows and rewrites their urls to the gateway path, so the document
// that is sealed next no longer carries the bytes. It is called at exactly the
// two run commits (PrepareChatRun, writeAssistant) -- never on create, where the
// chat row a blob would reference does not exist yet, and never on a bare
// autosave, which only carries what an earlier run already stored.
//
// It walks doc.Messages; a message's content that is an array of parts is
// searched for {image_url:{url}} parts, and for each one:
//
//   - a data: url must be exactly data:<allow-listed type>;base64,<standard
//     padded base64>. The decoded bytes are stored (sealed when a cipher is
//     configured), so what is stored is what was uploaded, byte for byte, and
//     anything that cannot be decoded losslessly -- another alphabet, missing
//     padding, a charset parameter -- is refused rather than guessed at. The
//     part's url becomes chatBlobURLPrefix + a new blob id.
//   - a chatBlobURLPrefix url is left alone, and verified first when it sits in
//     a message at index >= verifyFrom: the blob must belong to chatID, or a
//     client could graft another chat's or user's id into its own transcript.
//     verifyFrom exists because only what this request supplied needs
//     verifying; the messages the chat itself stored were verified when they
//     were stored, and re-checking every one of them on every turn (and on
//     every checkpoint) would cost a query per image.
//   - any other url (an https link, a url this code does not know) is not ours
//     to touch.
//
// doc.Messages is replaced by a rewritten COPY, never edited in place: the
// slice may be the caller's own (edited_history) and the caller still needs the
// pre-rewrite messages. On error nothing is returned and doc is unchanged, so a
// refused turn stores nothing. The caller seals the rewritten document and
// persists it with the returned blobs in one SaveChatWithBlobs.
func (s *Service) extractChatBlobs(ctx context.Context, chatID string, doc *chatDoc, verifyFrom int) ([]store.ChatBlob, error) {
	x := &blobExtraction{s: s, ctx: ctx, chatID: chatID, now: s.clock().UTC(), verified: map[string]struct{}{}}
	var rewritten []json.RawMessage
	for i, msg := range doc.Messages {
		x.verify = i >= verifyFrom
		out, changed, err := x.message(msg)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if rewritten == nil {
			rewritten = slices.Clone(doc.Messages)
		}
		rewritten[i] = out
	}
	if rewritten != nil {
		doc.Messages = rewritten
	}
	return x.blobs, nil
}

// blobExtraction is one extractChatBlobs call's state.
type blobExtraction struct {
	s      *Service
	ctx    context.Context
	chatID string
	now    time.Time
	// verify is whether gateway urls in the message being walked are checked
	// against the chat's ownership (see extractChatBlobs).
	verify bool
	// verified remembers the ids already confirmed to belong to chatID, so a
	// history that references one image several times asks the store once.
	verified map[string]struct{}
	blobs    []store.ChatBlob
}

// message rewrites the image parts of one message. The object is rebuilt only
// along the path that changed, keys in their original order, every other value
// byte for byte; a message with nothing to rewrite is returned as it came.
//
// Keys are matched without regard to case because the readers of this document
// are: buildAPIHistory decodes into tagged structs, and encoding/json matches
// those case-insensitively, so a "CONTENT" or "Image_Url" key reaches the
// upstream as the real one. Matching exactly would let exactly that spelling
// past the allow-list.
func (x *blobExtraction) message(raw json.RawMessage) (json.RawMessage, bool, error) {
	msg, ok := parseJSONObject(raw)
	if !ok {
		return raw, false, nil
	}
	changed := false
	for i := range msg {
		if !strings.EqualFold(msg[i].key, "content") {
			continue
		}
		val, c, err := rewriteImageParts(msg[i].val, x.visit)
		if err != nil {
			return nil, false, err
		}
		if c {
			msg[i].val, changed = val, true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := msg.marshal()
	return out, err == nil, err
}

// visit is url as an imageURLVisitor: an extraction only ever changes the url of
// a part, never replaces the part.
func (x *blobExtraction) visit(url string) (string, json.RawMessage, error) {
	out, err := x.url(url)
	return out, nil, err
}

// url decides what one image url becomes: the gateway url of a freshly stored
// blob (inline data), itself (a gateway url this chat holds, or any other url),
// or an error.
func (x *blobExtraction) url(url string) (string, error) {
	switch {
	case isDataURL(url):
		return x.store(url)
	case strings.HasPrefix(url, chatBlobURLPrefix):
		if x.verify {
			if err := x.checkOwned(strings.TrimPrefix(url, chatBlobURLPrefix)); err != nil {
				return "", err
			}
		}
	}
	return url, nil
}

// store decodes one data url into a new chat_blobs entry and returns the
// gateway url that replaces it.
func (x *blobExtraction) store(url string) (string, error) {
	mediaType, raw, err := x.s.decodeInlineImage(url)
	if err != nil {
		return "", err
	}
	id, err := compactRandomHexWithError(16)
	if err != nil {
		return "", err
	}
	id = chatBlobIDPrefix + id
	keyVersion, sealed := x.s.sealChatBlob(raw)
	x.blobs = append(x.blobs, store.ChatBlob{
		ID:          id,
		ChatID:      x.chatID, // SaveChatWithBlobs refuses a blob for any other chat
		KeyVersion:  keyVersion,
		Blob:        sealed,
		ContentType: mediaType,
		ByteLen:     int64(len(raw)),
		CreatedAt:   x.now,
	})
	return chatBlobURLPrefix + id, nil
}

// checkOwned requires the blob id to belong to this chat. A blob that does not
// exist and one that belongs to someone else are the same answer, so the check
// cannot be used to probe for ids; any other store error is not a verdict and
// is returned as the failure it is.
func (x *blobExtraction) checkOwned(id string) error {
	if _, ok := x.verified[id]; ok {
		return nil
	}
	owner, err := x.s.chats.ChatBlobOwner(x.ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrChatBlobNotOwned
	}
	if err != nil {
		return err
	}
	if owner != x.chatID {
		return ErrChatBlobNotOwned
	}
	x.verified[id] = struct{}{}
	return nil
}

// isDataURL reports whether url is a data: url. The scheme is case-insensitive,
// so a url spelled DATA: is validated like any other rather than slipping past.
func isDataURL(url string) bool {
	return len(url) >= len("data:") && strings.EqualFold(url[:len("data:")], "data:")
}

// decodeInlineImage parses data:<type>;base64,<payload> and returns the media
// type (canonical lower case) and the decoded bytes. Only that exact form is
// accepted: a type outside the allow-list, a parameter (charset=...), a payload
// that is not base64, an empty payload, or base64 that is not the standard
// padded alphabet (the url-safe alphabet, missing padding) is
// ErrChatBlobTypeUnsupported -- all of them are "not an image this store can
// hold and give back unchanged". What the decoder is lenient about -- line
// breaks, and set bits in the unused tail of the last character -- cannot change
// the decoded bytes, which are what is stored, so those are simply normalised
// away: what is served back is re-encoded as standard padded base64.
func (s *Service) decodeInlineImage(url string) (string, []byte, error) {
	header, payload, ok := strings.Cut(url[len("data:"):], ",")
	if !ok {
		return "", nil, ErrChatBlobTypeUnsupported
	}
	mediaType, flag, ok := strings.Cut(header, ";")
	if !ok || !strings.EqualFold(strings.TrimSpace(flag), "base64") {
		return "", nil, ErrChatBlobTypeUnsupported
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if _, allowed := chatBlobMediaTypes[mediaType]; !allowed {
		return "", nil, ErrChatBlobTypeUnsupported
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) == 0 {
		return "", nil, ErrChatBlobTypeUnsupported
	}
	if len(raw) > s.maxChatBlobBytes {
		return "", nil, ErrChatBlobTooLarge
	}
	return mediaType, raw, nil
}

// sealChatBlob returns the key version and the bytes to store for a blob: the
// bytes sealed with the capture cipher (nonce||AEAD, no gzip -- images are
// already compressed) when one is configured, else the plain bytes with key
// version 0, which only the volatile RAM store ever holds (the same
// arrangement as the sealed chat document, see sealChat).
func (s *Service) sealChatBlob(raw []byte) (int, []byte) {
	if s.cipher != nil {
		return capture.KeyVersion, s.cipher.Seal(raw)
	}
	return 0, raw
}

// ChatBlobBytes returns the bytes and media type of the stored chat image id for
// owner, or ErrChatBlobNotFound.
//
// This is the ONE place the serving endpoint's authorization is decided, and it
// is decided on the blob, not on any transcript that mentions it: the owner of
// a blob is the owner of the chat the blob BELONGS to (chat_blobs.chat_id ->
// chats.user_id). A transcript can carry a url to anyone's blob -- create and a
// bare autosave store the urls they are given and are not validated, by design;
// the check is here, at read -- so a chat that merely references an id grants
// nothing, and the caller's own chat is not consulted at all.
//
// The order is deliberate: the owner is resolved from the cheap, metadata-only
// lookups first (ChatBlobOwner, then ChatOwner -- never ChatByID, which reads
// the chat's whole sealed transcript, and a chat of many images asks this once
// per image), and the payload is loaded only for the verified owner, so another
// user's request neither reads nor decrypts their bytes. Every way of not being the owner -- no such blob, a foreign chat, a
// chat that is gone, a blob evicted between the lookups, a store that disagrees
// with itself about which chat a blob belongs to -- is the same
// ErrChatBlobNotFound; a store or cipher failure is returned as the failure it
// is (never as not-found, which would turn a broken key into an empty image).
//
// The media type is checked against the allow-list again here, exactly (the
// store keeps the canonical lower-case spelling, so a row that differs is not
// something this code wrote), rather than trusted because the store-time check
// should have kept anything else out: an unlisted type (text/html,
// image/svg+xml, a type with parameters, ...) is never served, whatever the row
// says, and the type returned is therefore always one of the allow-listed
// strings.
//
// The returned slice may alias the store's own memory on the plain (RAM) path;
// the caller must write it, not modify it.
func (s *Service) ChatBlobBytes(ctx context.Context, owner auth.Token, id string) ([]byte, string, error) {
	chatID, err := s.chats.ChatBlobOwner(ctx, id)
	if err != nil {
		return nil, "", notFoundAsBlob(err)
	}
	chatOwner, err := s.chats.ChatOwner(ctx, chatID)
	if err != nil {
		return nil, "", notFoundAsBlob(err)
	}
	if chatOwner != owner.UserID {
		return nil, "", ErrChatBlobNotFound
	}
	row, err := s.chats.ChatBlobByID(ctx, id)
	if err != nil {
		return nil, "", notFoundAsBlob(err)
	}
	if row.ChatID != chatID {
		return nil, "", ErrChatBlobNotFound
	}
	if _, listed := chatBlobMediaTypes[row.ContentType]; !listed {
		log.Printf("portal: chat blob %s has a content type that is not servable; refusing to serve it", id)
		return nil, "", ErrChatBlobNotFound
	}
	plain, err := s.openChatBlob(row)
	if err != nil {
		return nil, "", err
	}
	return plain, row.ContentType, nil
}

// notFoundAsBlob turns the store's not-found into the blob sentinel and leaves
// every other error alone.
func notFoundAsBlob(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrChatBlobNotFound
	}
	return err
}

// openChatBlob reverses sealChatBlob: KeyVersion > 0 was sealed with the
// capture cipher (nonce||AEAD) and needs it; KeyVersion 0 is the plain RAM
// fallback.
func (s *Service) openChatBlob(row store.ChatBlob) ([]byte, error) {
	if row.KeyVersion == 0 {
		return row.Blob, nil
	}
	if s.cipher == nil {
		return nil, ErrChatCipherMissing
	}
	return s.cipher.Open(row.Blob)
}

// imageURLVisitor decides what one image part becomes. out is the url the part
// carries afterwards (out == url leaves it as it is); a non-nil replacement
// replaces the whole part instead; an error aborts the walk.
type imageURLVisitor func(url string) (out string, replacement json.RawMessage, err error)

// rewriteImageParts walks one message's content: when it is an array, every
// part that is an image part -- an object whose image_url is an object with a
// string url -- goes through visit. Every other shape is not an image part and
// is left as it is, as is a content that is not an array (a plain string).
//
// It is the ONE definition of what an image url is, shared by the extraction at
// a run commit and the resolution of a run's history, so the two cannot disagree
// about a spelling: members are matched without regard to case (the readers of
// this document decode into tagged structs, which encoding/json matches that
// way), and a part that changes is rebuilt along the changed path only, members
// in their original order, every other value byte for byte. The returned bool is
// whether anything changed; when it is false the input comes back as it was.
func rewriteImageParts(content json.RawMessage, visit imageURLVisitor) (json.RawMessage, bool, error) {
	parts, ok := parseJSONArray(content)
	if !ok {
		return content, false, nil
	}
	changed := false
	for i := range parts {
		out, c, err := rewriteImagePart(parts[i], visit)
		if err != nil {
			return nil, false, err
		}
		if c {
			parts[i], changed = out, true
		}
	}
	if !changed {
		return content, false, nil
	}
	out, err := json.Marshal(parts)
	return out, err == nil, err
}

// rewriteImagePart is rewriteImageParts for one part.
func rewriteImagePart(raw json.RawMessage, visit imageURLVisitor) (json.RawMessage, bool, error) {
	part, ok := parseJSONObject(raw)
	if !ok {
		return raw, false, nil
	}
	changed := false
	for i := range part {
		if !strings.EqualFold(part[i].key, "image_url") {
			continue
		}
		res, err := rewriteImageTarget(part[i].val, visit)
		if err != nil {
			return nil, false, err
		}
		if res.replacement != nil {
			return res.replacement, true, nil
		}
		if res.changed {
			part[i].val, changed = res.val, true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := part.marshal()
	return out, err == nil, err
}

// memberRewrite is the outcome of visiting one member of an image part. When
// replacement is non-nil it replaces the whole part and nothing else matters;
// otherwise val is the member's new value when changed, and is unused when not.
type memberRewrite struct {
	val         json.RawMessage
	changed     bool
	replacement json.RawMessage
}

// rewriteImageTarget is rewriteImagePart for the value of one image_url member:
// anything that is not an object is left as it is, and so is an object none of
// whose url members changes.
func rewriteImageTarget(raw json.RawMessage, visit imageURLVisitor) (memberRewrite, error) {
	target, ok := parseJSONObject(raw)
	if !ok {
		return memberRewrite{}, nil
	}
	changed := false
	for k := range target {
		if !strings.EqualFold(target[k].key, "url") {
			continue
		}
		res, err := rewriteImageURL(target[k].val, visit)
		if err != nil {
			return memberRewrite{}, err
		}
		if res.replacement != nil {
			return res, nil
		}
		if res.changed {
			target[k].val, changed = res.val, true
		}
	}
	if !changed {
		return memberRewrite{}, nil
	}
	val, err := target.marshal()
	if err != nil {
		return memberRewrite{}, err
	}
	return memberRewrite{val: val, changed: true}, nil
}

// rewriteImageURL sends the value of one url member through visit. A value that
// is not a string is not an image url and is left as it is, as is a url the
// visitor returns unchanged.
func rewriteImageURL(raw json.RawMessage, visit imageURLVisitor) (memberRewrite, error) {
	var url string
	if json.Unmarshal(raw, &url) != nil {
		return memberRewrite{}, nil
	}
	rewritten, replacement, err := visit(url)
	if err != nil {
		return memberRewrite{}, err
	}
	if replacement != nil {
		return memberRewrite{replacement: replacement}, nil
	}
	if rewritten == url {
		return memberRewrite{}, nil
	}
	val, err := json.Marshal(rewritten)
	if err != nil {
		return memberRewrite{}, err
	}
	return memberRewrite{val: val, changed: true}, nil
}

// jsonField is one member of a JSON object, its value as raw JSON.
type jsonField struct {
	key string
	val json.RawMessage
}

// jsonObject is a JSON object that remembers its member order. A Go map would
// re-emit the keys sorted, reordering the stored document's parts (the part's
// "type" first is the documented shape) for no reason.
type jsonObject []jsonField

// parseJSONObject splits raw into its members. ok is false for anything that is
// not a JSON object -- including a malformed one, which is then left to
// whatever else reads it.
func parseJSONObject(raw json.RawMessage) (jsonObject, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return nil, false
	}
	var obj jsonObject
	for dec.More() {
		keyTok, err := dec.Token()
		key, isString := keyTok.(string)
		if err != nil || !isString {
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		obj = append(obj, jsonField{key: key, val: val})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, false
	}
	return obj, true
}

// parseJSONArray splits raw into its elements; ok is false unless raw is a JSON
// array.
func parseJSONArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil || elems == nil {
		return nil, false
	}
	return elems, true
}

// marshal writes the object back with its members in the order they were read
// and their values untouched.
func (o jsonObject) marshal() (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(f.key)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(f.val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
