// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
		parts, ok := parseJSONArray(msg[i].val)
		if !ok {
			continue
		}
		partsChanged := false
		for j := range parts {
			out, c, err := x.part(parts[j])
			if err != nil {
				return nil, false, err
			}
			if c {
				parts[j], partsChanged = out, true
			}
		}
		if !partsChanged {
			continue
		}
		val, err := json.Marshal(parts)
		if err != nil {
			return nil, false, err
		}
		msg[i].val, changed = val, true
	}
	if !changed {
		return raw, false, nil
	}
	out, err := msg.marshal()
	return out, err == nil, err
}

// part rewrites one content part: an object whose image_url is an object with a
// string url. Every other shape is not an image part and is left as it is.
func (x *blobExtraction) part(raw json.RawMessage) (json.RawMessage, bool, error) {
	part, ok := parseJSONObject(raw)
	if !ok {
		return raw, false, nil
	}
	changed := false
	for i := range part {
		if !strings.EqualFold(part[i].key, "image_url") {
			continue
		}
		target, ok := parseJSONObject(part[i].val)
		if !ok {
			continue
		}
		targetChanged := false
		for k := range target {
			if !strings.EqualFold(target[k].key, "url") {
				continue
			}
			var url string
			if json.Unmarshal(target[k].val, &url) != nil {
				continue
			}
			rewritten, err := x.url(url)
			if err != nil {
				return nil, false, err
			}
			if rewritten == url {
				continue
			}
			val, err := json.Marshal(rewritten)
			if err != nil {
				return nil, false, err
			}
			target[k].val, targetChanged = val, true
		}
		if !targetChanged {
			continue
		}
		val, err := target.marshal()
		if err != nil {
			return nil, false, err
		}
		part[i].val, changed = val, true
	}
	if !changed {
		return raw, false, nil
	}
	out, err := part.marshal()
	return out, err == nil, err
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
