// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// ErrChatNotFound is returned when a chat does not exist or is not owned by
	// the requesting principal (owner-scoped, no existence leak). Mapped to 404.
	ErrChatNotFound = errors.New("portal.chat_not_found")
	// ErrChatTitleInvalid is returned when a chat title exceeds maxChatTitleLen.
	// Mapped to 400.
	ErrChatTitleInvalid = errors.New("portal.chat_title_invalid")
	// ErrChatTooLarge is returned when the pre-seal content blob exceeds
	// MaxChatContentBytes. Mapped to 400.
	ErrChatTooLarge = errors.New("portal.chat_too_large")
	// ErrChatCipherMissing is returned when a stored chat was sealed
	// (KeyVersion > 0) but no cipher is configured to open it — a
	// misconfiguration that cannot normally occur (a chat is only written with
	// KeyVersion > 0 when a live cipher existed at write time). Mapped to 500,
	// never to ErrChatNotFound (it must not look like "chat missing").
	ErrChatCipherMissing = errors.New("portal.chat_cipher_missing")
	// ErrChatBlobTypeUnsupported is returned when an inline image in a run's
	// content is not a base64 data url of an allow-listed image type (png, jpeg,
	// webp, gif). Mapped to 400.
	ErrChatBlobTypeUnsupported = errors.New("images.blob_type_unsupported")
	// ErrChatBlobTooLarge is returned when one inline image decodes to more than
	// the per-blob ceiling (config MaxChatBlobBytes). Mapped to 400.
	ErrChatBlobTooLarge = errors.New("images.blob_too_large")
	// ErrChatBlobNotOwned is returned when a content part already references a
	// /api/portal/chat-blobs/{id} url whose blob this chat does not hold --
	// another user's, another chat's, or none at all, indistinguishably. Mapped
	// to 400.
	ErrChatBlobNotOwned = errors.New("images.blob_not_owned")
	// ErrChatBlobNotFound is returned when a stored chat image cannot be served
	// to the caller: there is no such blob, it was evicted or its chat deleted,
	// it belongs to another user's chat, or its stored type is not a servable
	// image type -- all indistinguishably (owner-scoped, no existence leak).
	// Deliberately its own sentinel rather than ErrChatNotFound or
	// store.ErrNotFound: the serving endpoint answers with a blob-specific code.
	// Mapped to 404.
	ErrChatBlobNotFound = errors.New("portal.chat_blob_not_found")
	// ErrChatRunHistoryTooLarge is returned when resolving a run's stored images
	// would make its upstream history carry more than MaxRunHistoryBytes. The run
	// is refused before the turn is stored. Mapped to 400.
	ErrChatRunHistoryTooLarge = errors.New("portal.chat_run_history_too_large")
)

const (
	// chatKindImage is the ChatRunSettings.Kind of an image-generation thread.
	chatKindImage = "image"
	// chatKindSpeech is the ChatRunSettings.Kind of a text-to-speech thread: each
	// send turns the user's text into an audio answer via /v1/audio/speech.
	chatKindSpeech = "speech"
	// chatKindTranscription is the ChatRunSettings.Kind of a speech-to-text
	// thread: each send turns the audio the user attached into a text transcript
	// via /v1/audio/transcriptions.
	chatKindTranscription = "transcription"
	// maxChatTitleLen caps the plaintext chat title (in runes).
	maxChatTitleLen = 200
	// MaxChatContentBytes caps the pre-seal (raw JSON) content blob at 4 MiB. It
	// bounds the chat's TEXT document: images no longer count, they live in
	// chat_blobs (extracted before this check). Exported so it can be served on
	// ChatListResponse, informationally, rather than duplicated by a client.
	MaxChatContentBytes = 4 << 20
	// MaxChatRequestBytes caps a chat request BODY -- create, save and run
	// start -- at the content cap plus room for the JSON envelope around it
	// (title, settings, the other keys). The gateway reads those bodies at this
	// limit, the bundled nginx configs allow exactly this much on
	// /api/portal/chats, and a test pins that they never fall below it.
	MaxChatRequestBytes = MaxChatContentBytes + 1<<20
	// DefaultMaxChatBlobBytes is the per-blob ceiling (decoded bytes) for one
	// stored chat image when the service is built without one. It mirrors
	// config.Config.MaxChatBlobBytes's default and the portal composer's own 20
	// MiB upload limit.
	DefaultMaxChatBlobBytes = 20 << 20
	// DefaultMaxRunHistoryBytes bounds the decoded image bytes one run's upstream
	// history may carry when the service is built without a bound. It mirrors
	// config.Config.MaxRunHistoryBytes's default. Base64 adds a third on the wire,
	// so the default keeps the images of one request to about 43 MiB.
	DefaultMaxRunHistoryBytes = 32 << 20
)

// ChatSummaryDTO is the list DTO: plaintext metadata only, never the content.
type ChatSummaryDTO struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChatDTO is the full DTO returned by create/get/save: metadata plus the opaque
// content blob, echoed verbatim as json.RawMessage. The backend never
// interprets content — it only seals/opens it.
type ChatDTO struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Content   json.RawMessage `json:"content"`
}

// ChatListResponse wraps the summary list under a data key (mirrors the other
// portal list endpoints) plus the text-document limit.
//
// MaxContentBytes is MaxChatContentBytes, the cap on the chat's TEXT document.
// It is informational: it used to feed the composer's remaining-image-capacity
// line, which is gone now that images live in chat_blobs rather than inside the
// document, and the portal frontend no longer consumes it. It is still served
// so a client that wants to show the text cap need not hardcode a copy that
// would drift from this one silently. It is NOT omitempty: a missing field and a
// zero are the same thing on the wire, so an accidental zero would otherwise be
// indistinguishable from "not sent".
type ChatListResponse struct {
	Data            []ChatSummaryDTO `json:"data"`
	MaxContentBytes int              `json:"max_content_bytes"`
}

// CreateChatRequest carries the initial title + opaque content on create.
type CreateChatRequest struct {
	Title   string          `json:"title"`
	Content json.RawMessage `json:"content"`
}

// UpdateChatRequest is PATCH-style: only the provided fields are applied. A PUT
// that sends both title and content (the frontend's save) updates both.
type UpdateChatRequest struct {
	Title   *string          `json:"title,omitempty"`
	Content *json.RawMessage `json:"content,omitempty"`
}

// ListChats returns the principal's chat summaries, most-recently updated first
// (ordering is the store's responsibility).
func (s *Service) ListChats(ctx context.Context, owner auth.Token) ([]ChatSummaryDTO, error) {
	summaries, err := s.chats.ChatsByUser(ctx, owner.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]ChatSummaryDTO, 0, len(summaries))
	for _, c := range summaries {
		out = append(out, ChatSummaryDTO{ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out, nil
}

// CreateChat mints a new chat owned by the principal: it validates the title,
// seals the opaque content, stores the row, and echoes the DTO (content
// included).
func (s *Service) CreateChat(ctx context.Context, owner auth.Token, req CreateChatRequest) (ChatDTO, error) {
	title, err := normalizeChatTitle(req.Title)
	if err != nil {
		return ChatDTO{}, err
	}
	content := normalizeChatContent(req.Content)
	keyVersion, blob, err := s.sealChat(content)
	if err != nil {
		return ChatDTO{}, err
	}
	now := s.clock().UTC()
	chat := store.Chat{
		ID:         "chat_" + compactRandomHex(16),
		UserID:     owner.UserID,
		Title:      title,
		KeyVersion: keyVersion,
		Blob:       blob,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.chats.CreateChat(ctx, chat); err != nil {
		return ChatDTO{}, err
	}
	return ChatDTO{ID: chat.ID, Title: title, CreatedAt: now, UpdatedAt: now, Content: content}, nil
}

// GetChat loads, ownership-checks, and opens (decrypts + gunzips) a chat. A
// missing chat surfaces store.ErrNotFound; a foreign chat surfaces
// ErrChatNotFound — both map to 404 (no existence leak).
func (s *Service) GetChat(ctx context.Context, owner auth.Token, id string) (ChatDTO, error) {
	row, err := s.chats.ChatByID(ctx, id)
	if err != nil {
		return ChatDTO{}, err
	}
	if row.UserID != owner.UserID {
		return ChatDTO{}, ErrChatNotFound
	}
	content, err := s.openChat(row)
	if err != nil {
		return ChatDTO{}, err
	}
	return ChatDTO{ID: row.ID, Title: row.Title, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Content: content}, nil
}

// SaveChat applies the PATCH-style update to an owned chat: an unset field keeps
// its stored value (an unset content is re-opened so the re-sealed blob is
// unchanged). It re-seals and persists, echoing the updated DTO.
func (s *Service) SaveChat(ctx context.Context, owner auth.Token, id string, req UpdateChatRequest) (ChatDTO, error) {
	row, err := s.chats.ChatByID(ctx, id)
	if err != nil {
		return ChatDTO{}, err
	}
	if row.UserID != owner.UserID {
		return ChatDTO{}, ErrChatNotFound
	}
	title := row.Title
	if req.Title != nil {
		title, err = normalizeChatTitle(*req.Title)
		if err != nil {
			return ChatDTO{}, err
		}
	}
	var content json.RawMessage
	if req.Content != nil {
		content = normalizeChatContent(*req.Content)
	} else {
		content, err = s.openChat(row)
		if err != nil {
			return ChatDTO{}, err
		}
	}
	updated, err := s.sealedChatUpdate(row, title, content)
	if err != nil {
		return ChatDTO{}, err
	}
	if err := s.chats.UpdateChat(ctx, updated); err != nil {
		return ChatDTO{}, err
	}
	return ChatDTO{ID: row.ID, Title: title, CreatedAt: row.CreatedAt, UpdatedAt: updated.UpdatedAt, Content: content}, nil
}

// sealedChatUpdate is row's chat in its next state: title and content replaced
// (the content sealed, and capped at MaxChatContentBytes by sealChat), a fresh
// updated_at, owner and created_at as they were. SaveChat writes it with
// UpdateChat; the two run commits write it together with the blobs they
// extracted (commitChatRun).
func (s *Service) sealedChatUpdate(row store.ChatRow, title string, content json.RawMessage) (store.Chat, error) {
	keyVersion, blob, err := s.sealChat(content)
	if err != nil {
		return store.Chat{}, err
	}
	return store.Chat{
		ID:         row.ID,
		UserID:     row.UserID,
		Title:      title,
		KeyVersion: keyVersion,
		Blob:       blob,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  s.clock().UTC(),
	}, nil
}

// commitChatRun persists doc as row's chat together with the blobs
// extractChatBlobs took out of it, in ONE atomic store step: either the
// document and every blob it now references persist, or none does. It is the
// write both run commits end in (PrepareChatRun, writeAssistant).
//
// deleteBlobIDs are the chat's blobs the rewritten doc no longer references --
// the audio of answers the retention prune turned into the not-stored marker
// (pruneForRetention). They are deleted in the same step, so the saved document
// never points at a blob this save removed; nil, what PrepareChatRun passes,
// deletes nothing.
//
// The document is sealed here, after the extraction, so the MaxChatContentBytes
// cap measures the rewritten document -- a few hundred bytes per image -- not
// the bytes it used to carry.
//
// A chat deleted while the run was committing surfaces as store.ErrNotFound
// (the missing-row update, or the foreign key of a blob insert), which is the
// error the run executor's commitFailureIsChatGone recognises, so such a run
// ends cleanly rather than as a 500. A volatile memory store can accept the
// save and evict the chat straight away to stay within its budget; nothing here
// assumes the chat is still held afterwards.
func (s *Service) commitChatRun(ctx context.Context, row store.ChatRow, title string, doc chatDoc, blobs []store.ChatBlob, deleteBlobIDs []string) error {
	content, err := doc.marshal()
	if err != nil {
		return err
	}
	updated, err := s.sealedChatUpdate(row, title, content)
	if err != nil {
		return err
	}
	return s.chats.SaveChatWithBlobs(ctx, updated, blobs, deleteBlobIDs)
}

// DeleteChat removes an owned chat. A foreign or missing chat is 404 (no leak).
func (s *Service) DeleteChat(ctx context.Context, owner auth.Token, id string) error {
	row, err := s.chats.ChatByID(ctx, id)
	if err != nil {
		return err
	}
	if row.UserID != owner.UserID {
		return ErrChatNotFound
	}
	return s.chats.DeleteChat(ctx, id)
}

// sealChat gzips the opaque content, then seals it when a cipher is configured
// (KeyVersion capture.KeyVersion) or stores plain gzip in RAM-fallback mode
// (nil cipher, KeyVersion 0). It caps the pre-seal content at MaxChatContentBytes.
func (s *Service) sealChat(content json.RawMessage) (int, []byte, error) {
	if len(content) > MaxChatContentBytes {
		return 0, nil, ErrChatTooLarge
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(content); err != nil {
		return 0, nil, err
	}
	if err := zw.Close(); err != nil {
		return 0, nil, err
	}
	if s.cipher != nil {
		return capture.KeyVersion, s.cipher.Seal(gz.Bytes()), nil
	}
	return 0, gz.Bytes(), nil
}

// openChat reverses sealChat: KeyVersion > 0 was sealed (needs the cipher);
// KeyVersion 0 is plain gzip (RAM fallback). It returns the opaque content as
// json.RawMessage.
func (s *Service) openChat(row store.ChatRow) (json.RawMessage, error) {
	compressed := row.Blob
	if row.KeyVersion > 0 {
		if s.cipher == nil {
			return nil, ErrChatCipherMissing
		}
		var err error
		compressed, err = s.cipher.Open(row.Blob)
		if err != nil {
			return nil, err
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	plain, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(plain), nil
}

// normalizeChatTitle trims the title and rejects one longer than maxChatTitleLen
// runes. An empty title is allowed (the default).
func normalizeChatTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if utf8.RuneCountInString(title) > maxChatTitleLen {
		return "", ErrChatTitleInvalid
	}
	return title, nil
}

// normalizeChatContent replaces an absent/nil content with the JSON literal
// null so the stored blob is always valid JSON and round-trips verbatim.
func normalizeChatContent(content json.RawMessage) json.RawMessage {
	if content == nil {
		return json.RawMessage("null")
	}
	return content
}

// ChatAPIMessage is one OpenAI-shaped message (role + opaque content) the run
// executor sends to /v1/chat/completions. content is passed through verbatim so
// image parts survive.
type ChatAPIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ChatRunSettings are the per-chat generation settings a run uses.
type ChatRunSettings struct {
	Model        string  `json:"model"`
	SystemPrompt string  `json:"system_prompt"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"max_tokens"`
	RunAsTokenID string  `json:"run_as_token_id"`
	// ServerOverride optionally forces every request the background run for
	// this chat generates onto one specific AI-server — the per-CHAT analog of
	// the per-API-token server_override (see Service.CreateToken/UpdateToken +
	// validateServerOverride). It is self-healed against the chat OWNER's
	// CURRENT server-management rights every time PrepareChatRun runs (a value
	// the owner can no longer manage is silently cleared to "" and the cleared
	// settings are persisted, never rejecting/failing the run — see
	// PrepareChatRun below) and RE-AUTHORIZED again at request time by the
	// gateway's applyServerOverride, which is the actual runtime security
	// boundary; this self-heal only keeps a persisted, at-rest setting from
	// going silently stale between runs.
	ServerOverride string `json:"server_override,omitempty"`
	// ServerOverrideForceUnreachable, when true, routes to ServerOverride even
	// if the resolver deems it unreachable/under maintenance. Always false
	// whenever ServerOverride is "" — see the self-heal in PrepareChatRun.
	ServerOverrideForceUnreachable bool `json:"server_override_force_unreachable,omitempty"`
	// Kind pins what this thread's runs are: "" (text, the default and every
	// pre-existing chat), "image", "speech" or "transcription". It is established
	// by the FIRST send and then forced by PrepareChatRun on every later send,
	// because the composer's affordances follow the thread rather than the
	// currently-picked model -- see ADR-043 (b) in
	// docs/architecture/09-architecture-decisions.md.
	//
	// It is a UI constraint, NOT an authorization: this struct is the POST
	// body verbatim (chat_run_endpoints.go), so a client can submit any value
	// on the first send. The routing capability gate remains the only
	// authority on what the gateway will actually serve.
	//
	// Declared LAST and omitempty on purpose: that is what keeps an existing
	// text chat's persisted settings byte-identical.
	Kind string `json:"kind,omitempty"`
	// Image carries the image-generation knobs of an "image" thread. It is nil
	// (and omitted) for a text thread, so a text chat's persisted settings stay
	// byte-identical -- the same guarantee Kind's omitempty gives, and
	// PrepareChatRun clears it for any non-image kind to hold that even against a
	// raw API client. Unlike Kind it is NOT force-pinned: like the text knobs
	// (temperature/max_tokens) a client sends the thread's CURRENT image settings
	// on every send and they are persisted, so the composer can change size/n
	// between sends. Only the fields the upstream honors are carried;
	// buildImagesBody (chat_runs_images.go) forwards the set ones and the gateway
	// passes them through unexamined.
	Image *ChatImageSettings `json:"image,omitempty"`
	// Audio carries the text-to-speech knobs of a "speech" thread. It follows
	// Image's contract exactly: nil (and omitted) for a text or image thread, so
	// their persisted settings stay byte-identical; PrepareChatRun clears it for
	// any non-speech kind to hold that even against a raw API client; and it is
	// NOT force-pinned -- a client sends the thread's CURRENT audio settings on
	// every send and they are persisted, so the composer can change voice/speed
	// between sends. Declared last, after Image, on purpose: a field added later
	// must not move an existing chat's serialized bytes.
	Audio *ChatAudioSettings `json:"audio,omitempty"`
	// Transcription carries the speech-to-text knobs of a "transcription" thread.
	// It follows Audio's contract exactly: nil (and omitted) for a text, image or
	// speech thread, so their persisted settings stay byte-identical;
	// PrepareChatRun clears it for any non-transcription kind to hold that even
	// against a raw API client; and it is NOT force-pinned -- a client sends the
	// thread's CURRENT transcription settings on every send and they are
	// persisted, so the composer can change language/prompt between sends.
	// Declared last, after Audio, on purpose: a field added later must not move an
	// existing chat's serialized bytes.
	Transcription *ChatTranscriptionSettings `json:"transcription,omitempty"`
	// VoiceChat is the portal's Voice Chat model picks of a text thread (issue #317):
	// settings.voice_chat of the schema-free chat document, {asr_model, tts_model},
	// written by the client's own save and handed to the voice-chat ticket by the
	// client. The backend neither reads nor validates it (the ticket's co-load gate
	// resolves the models a conversation is started with); this field exists only so
	// that PrepareChatRun, which REPLACES the stored settings wholesale with this
	// struct on every user-turn commit, carries the key across instead of wiping it
	// -- the way it lifts the pinned Kind, and for the same reason: the picks would
	// otherwise be lost whenever a thread is sent to, with no client able to write
	// them back unless that thread is still the one it has open when the run ends.
	// It is the STORED value that is carried, whatever the POST body (this struct)
	// submitted, and it is held as the raw JSON so that a key this build does not
	// know survives untouched. nil (and omitted) for a thread that has no picks, so
	// its persisted settings stay byte-identical. Declared last, after
	// Transcription, on purpose: a field added later must not move an existing
	// chat's serialized bytes.
	VoiceChat json.RawMessage `json:"voice_chat,omitempty"`
}

// ChatImageSettings are the image-generation knobs a portal "image" thread
// forwards to /v1/images/generations. Every field is optional and omitted when
// unset, so an unset field leaves the upstream (sd-server) on its own default
// rather than being handed an inert zero. Only the fields sd-server's OpenAI
// generations endpoint actually reads are here -- size, n, output_format and
// output_compression. quality/style/response_format/user are ignored upstream,
// and the native sampler/steps/cfg knobs are delivered separately (a later
// phase), so neither belongs on this struct. See
// docs/architecture/cross-cutting/compatibility-and-inference.md section 3.4.
type ChatImageSettings struct {
	// Size is the "WxH" image size (e.g. "1024x1024"); "" leaves sd-server's own
	// default.
	Size string `json:"size,omitempty"`
	// N is how many images to generate (sd-server's batch_count, upstream-capped);
	// 0 leaves the default of one.
	N int `json:"n,omitempty"`
	// OutputFormat is "png", "jpeg" or "webp"; "" leaves the default ("png").
	OutputFormat string `json:"output_format,omitempty"`
	// OutputCompression is sd-server's 0..100 output_compression for a lossy
	// output_format (jpeg/webp); it is ignored for png. A POINTER so an explicit
	// 0 stays distinguishable from "unset" -- nil is omitted, so only a value the
	// composer moved off the upstream default is forwarded.
	OutputCompression *int `json:"output_compression,omitempty"`

	// The advanced sd-server knobs (Phase 1b). Unlike the OpenAI-shaped fields
	// above, these are NOT top-level on /v1/images/generations: sd-server reads
	// them only from a <sd_cpp_extra_args>{…}</sd_cpp_extra_args> block embedded in
	// the prompt (its own sanctioned extension point). buildImagesBody
	// (chat_runs_images.go) maps the set ones into that block -- NegativePrompt,
	// Seed and ClipSkip are top-level in the block; Steps, Sampler, Scheduler and
	// CFG live under its nested sample_params. The numerics are POINTERS so an
	// explicit 0 (a real seed/clip_skip) is distinguishable from "unset", like
	// OutputCompression; a nil/empty knob is omitted so sd-server keeps its own
	// (per-model) default. Passed opaquely -- the gateway does not own sd-server's
	// sampler/scheduler enum set or numeric caps.
	NegativePrompt string   `json:"negative_prompt,omitempty"`
	Sampler        string   `json:"sampler,omitempty"`
	Scheduler      string   `json:"scheduler,omitempty"`
	Steps          *int     `json:"steps,omitempty"`
	CFG            *float64 `json:"cfg,omitempty"`
	Seed           *int     `json:"seed,omitempty"`
	ClipSkip       *int     `json:"clip_skip,omitempty"`
	// Strength is the EDIT knob: how far an image edit departs from its source
	// (sd-server's own default is 0.75, which makes a weak edit, and a
	// reference-edit model may want it higher). Like the knobs above it rides the
	// <sd_cpp_extra_args> block, top-level, and is nil/omitted when unset so
	// sd-server keeps its default. sd-server ignores it for a generation (no source
	// image), so a thread that carries it can still generate.
	Strength *float64 `json:"strength,omitempty"`
}

// ChatAudioSettings are the text-to-speech knobs a portal "speech" thread
// forwards to /v1/audio/speech. Every field is optional and omitted when unset,
// so an unset field leaves the model on its own default rather than being handed
// an inert zero. Only the first-class knobs (voice, speed and language) live here;
// everything else -- the OpenAI "instructions" field and any other native audio.cpp
// knob -- rides Passthrough. buildSpeechBody (chat_runs_audio.go) forwards the set fields and
// merges Passthrough into the request JSON (the gateway relays the body to
// audio.cpp unchanged), refusing the keys that would override the body the run
// itself owns (model/input/stream/stream_format/response_format). The turn the run
// commits is always a WAV (a streamed synthesis is assembled into one), so there is
// no format knob. KeepLastAudio is the one field that is not a synthesis knob: it is
// the thread's own retention of the answers it has stored, and is never sent upstream.
type ChatAudioSettings struct {
	// Voice is the voice name; "" leaves the model's default voice. The composer
	// offers the voices the gateway has cached for the model in a combobox that
	// still takes free text, so it is whatever the user picked or typed.
	Voice string `json:"voice,omitempty"`
	// Speed is the speaking-rate multiplier, passed through as given (the
	// model's own range applies). A POINTER so an explicit value stays
	// distinguishable from "unset" -- nil is omitted, so only a speed the
	// composer set is forwarded.
	Speed *float64 `json:"speed,omitempty"`
	// Passthrough is the advanced extra-JSON surface: top-level request fields
	// merged into the /v1/audio/speech body verbatim, keyed by field name. The
	// values are raw JSON so a string, number, bool or object each reach the
	// upstream with the exact type the user wrote, and they round-trip through
	// the stored document untouched. A nil or empty map is omitted.
	Passthrough map[string]json.RawMessage `json:"passthrough,omitempty"`
	// Stream is the thread's choice to hear a streaming-capable model's audio while it
	// is generated (ADR-068, the portal speech-streaming run). It is a CHOICE, not a
	// request: the run streams only when the gateway's own gate also holds (the mapped
	// model is speech_streaming yes and its output byte rate is cached,
	// Server.speechRunStreamRate), and otherwise stays the buffered WAV run of ADR-063
	// whatever this says. nil (omitted) is the default, which streams where it can; only
	// an explicit false opts the thread out -- hence a pointer, so an unset field and a
	// chosen false stay distinguishable and an existing thread's persisted settings stay
	// byte-identical.
	Stream *bool `json:"stream,omitempty"`
	// KeepLastAudio is the thread's retention of its generated answers (#281): how many
	// of the newest speech answers' audio blobs the chat keeps. nil (omitted) keeps every
	// answer, the default and what a thread stored before this field exists reads as; 0
	// stores no answer audio at all; N >= 1 keeps the last N. A negative value is not a
	// count, so PrepareChatRun reads it as unset (nil). A POINTER, like Speed and Stream,
	// so the meaningful 0 stays distinguishable from "unset" (a plain int with omitempty
	// would drop it) and an existing thread's persisted settings stay byte-identical.
	// Declared after Stream on purpose: a field added later must not move an
	// existing chat's serialized bytes (Language was later appended after it, for the
	// same byte-stability reason).
	KeepLastAudio *int `json:"keep_last_audio,omitempty"`
	// Language is the ISO-639-1-ish code of the language to speak (e.g. "de"), the twin
	// of ChatTranscriptionSettings.Language; "" leaves the model on its default. It rides
	// the body as a top-level "language" field, which audio.cpp applies per model and
	// silently drops where the model has no such parameter. Declared after KeepLastAudio,
	// last, for the same reason: a stored chat's serialized bytes must not move.
	Language string `json:"language,omitempty"`
}

// ChatTranscriptionSettings are the speech-to-text knobs a portal
// "transcription" thread forwards to /v1/audio/transcriptions. Every field is
// optional and omitted when unset, so an unset field leaves the model on its own
// default rather than being handed an inert zero. Only the OpenAI-shaped
// first-class knobs live here; everything else -- the OpenAI "timestamp
// granularities" and any native audio.cpp knob -- rides Passthrough. The
// transcription request is multipart, so buildTranscriptionBody
// (chat_runs_transcription.go) forwards the set fields as form fields next to
// the audio file and merges Passthrough into the same form (the gateway relays
// the body to audio.cpp unchanged), refusing the keys that would override the
// form the run itself owns (model/file/response_format/stream). A raw-JSON
// passthrough value is written as its natural form text: a string as itself, any
// other value as its compact JSON. The gateway always asks for JSON and the
// answer is rendered as text, so there is no response_format knob.
type ChatTranscriptionSettings struct {
	// Language is the ISO-639-1 code of the spoken language (e.g. "de"); "" leaves
	// the model to detect it.
	Language string `json:"language,omitempty"`
	// Prompt is the optional text that steers the transcription's style or primes
	// it with names and jargon; "" sends none.
	Prompt string `json:"prompt,omitempty"`
	// Temperature is the sampling temperature, passed through as given (the
	// model's own range applies). A POINTER so an explicit 0 -- a real,
	// deterministic setting -- stays distinguishable from "unset", exactly as
	// ChatImageSettings.CFG does: nil is omitted, so only a temperature the
	// composer set is forwarded.
	Temperature *float64 `json:"temperature,omitempty"`
	// Passthrough is the advanced extra-JSON surface: extra request fields merged
	// into the /v1/audio/transcriptions form verbatim, keyed by field name. The
	// values are raw JSON so a string, number, bool or object each reach the
	// upstream with the exact type the user wrote, and they round-trip through
	// the stored document untouched. A nil or empty map is omitted.
	Passthrough map[string]json.RawMessage `json:"passthrough,omitempty"`
}

// PrepareRunRequest either appends a new user message or replaces the whole
// message history (edit/regenerate). Exactly one of UserMessage / EditedHistory
// is set.
type PrepareRunRequest struct {
	UserMessage   json.RawMessage
	EditedHistory []json.RawMessage
	Settings      ChatRunSettings
}

// chatDoc is the minimally-parsed content document: settings + messages are kept
// as raw JSON so unrelated fields round-trip untouched; only the trailing
// assistant message is ever constructed by the backend.
type chatDoc struct {
	Settings json.RawMessage   `json:"settings,omitempty"`
	Messages []json.RawMessage `json:"messages"`
}

func parseChatDoc(content json.RawMessage) chatDoc {
	var d chatDoc
	_ = json.Unmarshal(content, &d) // tolerant: a null/empty doc yields zero value
	if d.Messages == nil {
		d.Messages = []json.RawMessage{}
	}
	return d
}

func (d chatDoc) marshal() (json.RawMessage, error) {
	return json.Marshal(d)
}

// liftedVoiceChat is the stored settings.voice_chat to carry across PrepareChatRun's rewrite of the
// settings: the raw value itself, or nil when the thread has none -- the key absent, or a JSON null,
// which would persist a `null` that nobody wrote.
func liftedVoiceChat(stored json.RawMessage) json.RawMessage {
	if len(stored) == 0 || string(stored) == "null" {
		return nil
	}
	return stored
}

// PrepareChatRun commits the user turn (or replaces history for edit/regenerate)
// and the settings into the chat, self-heals the settings' server_override
// (see below), then returns the OpenAI message history the run executor will
// send (system prompt prepended when set) TOGETHER WITH the settings actually
// persisted — which the caller must feed into the run executor (via
// gateway.PrepareRunResult.Settings) instead of the raw request settings, so a
// self-healed clear is honored rather than silently overridden by the stale
// value the client submitted. For a text run the history carries its images as
// data urls, the stored references of the older turns resolved to their bytes
// (resolveChatBlobRefs); an image run's history is returned as stored, except
// that the source image of its turn -- the image its last user message carries,
// which makes the turn an edit -- is resolved to a data url too, or the run is
// refused (resolveImageRunSource); a transcription run's likewise, except that
// the source is the audio clip its last user message carries
// (resolveTranscriptionRunSource). Owner-scoped.
func (s *Service) PrepareChatRun(ctx context.Context, owner auth.Token, chatID string, req PrepareRunRequest) ([]ChatAPIMessage, ChatRunSettings, error) {
	row, err := s.chats.ChatByID(ctx, chatID)
	if err != nil {
		return nil, ChatRunSettings{}, err
	}
	if row.UserID != owner.UserID {
		return nil, ChatRunSettings{}, ErrChatNotFound
	}
	content, err := s.openChat(row)
	if err != nil {
		return nil, ChatRunSettings{}, err
	}
	doc := parseChatDoc(content)

	// hadPriorMessages is true once this chat has been through an earlier
	// send -- which is exactly "has a kind already been pinned," including
	// the pin of empty-string text. A chat's absent "kind" key in
	// doc.Settings cannot by itself distinguish "never sent" from "pinned to
	// text," because Kind is omitempty and both cases serialize the same way;
	// message count breaks that tie. Captured BEFORE the append/replace
	// immediately below so it reflects the chat as it was before THIS send,
	// not after.
	//
	// Assumption: CreateChatRequest.Content is client-supplied opaque JSON,
	// so a chat could in principle be created with messages already
	// populated before any PrepareChatRun call ever runs -- in which case
	// this flag would read true on what is logically the thread's first
	// send, and the stored (absent) kind would be forced rather than the
	// client's submitted one accepted. No current call site seeds messages
	// on create (chat creation always starts from an empty messages array);
	// if one ever does, this heuristic stops being safe and the pin would
	// need an explicit "has run" marker instead of inferring one from
	// message count.
	hadPriorMessages := len(doc.Messages) > 0

	// firstUnvetted is the index of the first message this request supplied: the
	// whole history for an edit/regenerate, only the new turn for an append. The
	// messages before it are what the chat itself stored, and extractChatBlobs
	// does not re-verify their blob references (see its doc).
	firstUnvetted := len(doc.Messages)
	if req.EditedHistory != nil {
		doc.Messages = req.EditedHistory
		firstUnvetted = 0
	} else if req.UserMessage != nil {
		userMsg, err := json.Marshal(map[string]any{
			"id":      "msg_" + compactRandomHex(8),
			"role":    "user",
			"content": req.UserMessage,
		})
		if err != nil {
			return nil, ChatRunSettings{}, err
		}
		doc.Messages = append(doc.Messages, userMsg)
	}

	// Move the inline images of the assembled messages -- the new turn's upload,
	// an edited history's, and a legacy chat's not yet migrated ones -- into
	// chat_blobs, and rewrite their urls in the document that is sealed below.
	// This is BEFORE the seal so the 4 MiB cap measures the rewritten document.
	//
	// runMessages is what the run itself is handed. It stays the PRE-rewrite
	// messages: the bytes the user just sent are in hand and go to the model as
	// they are, with no round trip through the store. extractChatBlobs rewrites a
	// copy of the slice, so runMessages is not touched by it.
	runMessages := doc.Messages
	blobs, err := s.extractChatBlobs(ctx, chatID, &doc, firstUnvetted)
	if err != nil {
		return nil, ChatRunSettings{}, err
	}

	// Self-heal the chat's per-run server override against the OWNER's
	// CURRENT server-management rights — the exact pattern of the
	// token create/update self-heal (validateServerOverride), and for the
	// same reason: the setting is persisted and can outlive the manage-grant
	// that was valid when it was set (the owner may lose can_manage_servers on
	// the server via a co-manager revoke, or the server may be deleted, at any
	// later point). The RUN's effective principal is always the chat's OWNER
	// (never a run-as token, which only borrows the owner's model/billing
	// attribution — see ChatRunSettings.RunAsTokenID — and neither grants nor
	// restricts server management), so there is no divergent principal to
	// re-check here. A value the owner can no longer manage is silently
	// cleared (never rejects/fails the run); blank stays blank, with no
	// AuthorizeServerManage call (validateServerOverride's own early return).
	// The cleared settings are PERSISTED below via the SAME content-save path
	// this function already uses (doc.Settings feeds the document commitChatRun
	// seals and saves), so a stale override is healed exactly once rather than
	// re-discovered — and re-ignored — on every future run. This is a
	// courtesy write, NOT the security boundary: every routed request is
	// re-authorized again at request time by the gateway's
	// applyServerOverride, which trusts nothing it reads from storage.
	req.Settings.ServerOverride = s.validateServerOverride(ctx, owner, req.Settings.ServerOverride)
	if req.Settings.ServerOverride == "" {
		req.Settings.ServerOverrideForceUnreachable = false
	}

	// The stored kind wins over whatever the client submitted. doc.Settings is
	// about to be REPLACED wholesale by the submitted settings (below), so the
	// pin has to be lifted out first or it is silently unpinned on every send.
	//
	// The force applies whenever hadPriorMessages -- UNCONDITIONALLY, even
	// when stored.Kind is "" (text). A guard of `stored.Kind != ""` would only
	// protect an established image pin: a text-pinned thread's second send
	// could then submit "image" and flip the thread, silently discarding
	// history for a run against the images endpoint. On a genuine first send
	// (hadPriorMessages false) nothing is forced, so the client's submitted
	// kind -- including "image" -- establishes the pin.
	//
	// The Voice Chat picks are lifted out of the stored settings the same way, for the
	// same reason, and unconditionally: they are the stored document's (see
	// ChatRunSettings.VoiceChat), never the run body's.
	var stored struct {
		Kind      string          `json:"kind"`
		VoiceChat json.RawMessage `json:"voice_chat"`
	}
	if len(doc.Settings) > 0 {
		_ = json.Unmarshal(doc.Settings, &stored) // best-effort: a malformed blob leaves the kind unpinned, which is the pre-feature behaviour
	}
	if hadPriorMessages {
		req.Settings.Kind = stored.Kind
	}
	req.Settings.VoiceChat = liftedVoiceChat(stored.VoiceChat)
	// Image settings belong to an image thread only. Clearing them for any other
	// kind keeps a text thread's persisted settings byte-identical even against a
	// raw API client that submits them (ChatRunSettings is the POST body
	// verbatim), mirroring the ServerOverrideForceUnreachable self-heal above.
	if req.Settings.Kind != chatKindImage {
		req.Settings.Image = nil
	}
	// The same holds for the audio settings and a speech thread: they are cleared
	// for every other kind, and a speech thread in turn carries no image settings
	// (the line above already clears them, as speech is not image).
	if req.Settings.Kind != chatKindSpeech {
		req.Settings.Audio = nil
	}
	// A negative retention is no count: it reads as unset, which keeps every answer.
	// 0 (store none) and a positive N (keep the last N) stay as submitted. The settings
	// are copied before the clamp so the caller's own struct is left as it was sent.
	if a := req.Settings.Audio; a != nil && a.KeepLastAudio != nil && *a.KeepLastAudio < 0 {
		clamped := *a
		clamped.KeepLastAudio = nil
		req.Settings.Audio = &clamped
	}
	// And for the transcription settings and a transcription thread: they are
	// cleared for every other kind (text, image and speech alike), while the two
	// lines above already clear the image and audio settings of a transcription
	// thread, as transcription is neither image nor speech.
	if req.Settings.Kind != chatKindTranscription {
		req.Settings.Transcription = nil
	}

	settingsRaw, err := json.Marshal(req.Settings)
	if err != nil {
		return nil, ChatRunSettings{}, err
	}
	doc.Settings = settingsRaw

	title := row.Title
	if title == "" {
		title = deriveChatTitle(doc.Messages)
	}
	title, err = normalizeChatTitle(title)
	if err != nil {
		return nil, ChatRunSettings{}, err
	}

	history := buildAPIHistory(req.Settings.SystemPrompt, runMessages)
	// The stored messages reference their images by url; the run's upstream gets
	// the images themselves (see resolveChatBlobRefs). The turn being sent is in
	// runMessages with its bytes inline already, so only the older references are
	// fetched. An image run reads nothing of the history but the last prompt's text
	// and -- when the last user message carries an image, which makes the turn an
	// edit -- that image: fetching, decrypting and encoding every image of the
	// thread would be wasted, and the run-size bound would refuse an image thread
	// for bytes that are never sent. So it resolves that one message and no other
	// (resolveImageRunSource), and refuses a source the chat does not hold rather
	// than going on without it.
	//
	// This is before the commit on purpose: a run refused for its history's size,
	// or whose images cannot be read, stores nothing, so retrying it is not a
	// duplicate turn.
	//
	// A transcription run is the audio twin of that: its input is the clip its
	// last user message carries, so it resolves that one message's audio and no
	// other (resolveTranscriptionRunSource), and refuses a clip the chat does not
	// hold. It reads audio parts and nothing else; every other run reads image
	// parts and nothing else, so a stored clip in a text, image or speech thread's
	// history is left as the reference it is.
	//
	// A speech run takes the text branch on purpose: its input is the last user
	// message's text and its history carries no image to resolve beyond what a
	// text thread's would, so it needs no special source resolution of its own.
	switch req.Settings.Kind {
	case chatKindImage:
		history, err = s.resolveImageRunSource(ctx, chatID, history)
	case chatKindTranscription:
		history, err = s.resolveTranscriptionRunSource(ctx, chatID, history)
	default:
		history, err = s.resolveChatBlobRefs(ctx, chatID, history)
	}
	if err != nil {
		return nil, ChatRunSettings{}, err
	}
	if err := s.commitChatRun(ctx, row, title, doc, blobs, nil); err != nil {
		return nil, ChatRunSettings{}, err
	}

	return history, req.Settings, nil
}

// buildAPIHistory maps stored messages to OpenAI role+content, prepending a
// system message when a prompt is set.
func buildAPIHistory(systemPrompt string, messages []json.RawMessage) []ChatAPIMessage {
	out := make([]ChatAPIMessage, 0, len(messages)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, ChatAPIMessage{Role: "system", Content: json.RawMessage(mustJSONString(systemPrompt))})
	}
	for _, m := range messages {
		var pm struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &pm); err != nil || pm.Role == "" {
			continue
		}
		out = append(out, ChatAPIMessage{Role: pm.Role, Content: pm.Content})
	}
	return out
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// deriveChatTitle produces a short title from the first user message text.
func deriveChatTitle(messages []json.RawMessage) string {
	for _, m := range messages {
		var pm struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &pm) != nil || pm.Role != "user" {
			continue
		}
		text := MessageText(pm.Content)
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			return ""
		}
		if utf8.RuneCountInString(text) > 40 {
			r := []rune(text)
			return strings.TrimRight(string(r[:40]), " ")
		}
		return text
	}
	return ""
}

// MessageText pulls the text out of a message content that is either a JSON
// string or an array of parts ([{type:"text",text:...}, ...]).
//
// Exported because the gateway's image-run executor needs the SAME extraction
// to build the upstream prompt from the last user message. A third parser for
// these two shapes (deriveChatTitle below is the first caller) would be a
// second definition of what a message's text is, free to drift from this one.
func MessageText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) == nil {
		for _, p := range parts {
			if p.Type == "text" {
				return p.Text
			}
		}
	}
	return ""
}

// ImagePurposeMask is the value of an image part's "purpose" member that marks it
// as the MASK of an image edit (inpainting) rather than a source image. The part
// is an ordinary image_url part otherwise -- {"type":"image_url","image_url":
// {"url":...},"purpose":"mask"} -- so it is stored as a chat blob, resolved back to
// bytes and owner-checked by the code that handles every other image of a message;
// only the image-run executor reads the tag, to send the part as the edit's `mask`
// form field instead of as a source. The web portal writes the tag (maskPart in
// frontend components/shared/chatContent.ts), the executor reads it
// (MessageImages); a part without it is a source.
const ImagePurposeMask = "mask"

// imagePurposeKey is the member of an image part that carries its purpose.
const imagePurposeKey = "purpose"

// MessageImage is one image part of a message content: its url, and whether it is
// tagged as the mask of an edit (ImagePurposeMask).
type MessageImage struct {
	URL  string
	Mask bool
}

// MessageImages is every image part of a message content, in order: what an
// image-run executor sends as an edit's source images and mask (an array content;
// a plain string has none). Exported for the same reason as MessageText, and it
// reads through rewriteImagePart -- the one definition of an image part the
// extraction at a run commit and the resolution of a run's history already share
// -- so the executor and the resolution cannot disagree about what counts as an
// image (an object whose image_url is an object with a string url, members
// matched without regard to case). The tag is read the way the portal's JavaScript
// reads it instead (isMaskPart): its member name exactly, the last of a repeated one,
// its value exactly, and a part that is not an image has no purpose to speak of.
func MessageImages(content json.RawMessage) []MessageImage {
	parts, ok := parseJSONArray(content)
	if !ok {
		return nil
	}
	var images []MessageImage
	for _, raw := range parts {
		var urls []string
		_, _, _ = rewriteImagePart(raw, func(url string) (string, json.RawMessage, error) {
			urls = append(urls, url)
			return url, nil, nil
		})
		if len(urls) == 0 {
			continue
		}
		mask := isMaskPart(raw)
		for _, url := range urls {
			images = append(images, MessageImage{URL: url, Mask: mask})
		}
	}
	return images
}

// isMaskPart reports whether an image part carries the mask tag.
//
// It reads the tag the way the portal's own JavaScript does (part.purpose, after
// JSON.parse): the key exactly, and the LAST of a repeated key -- not the first, and
// not "any of them". The tag is read by nothing but the portal and this function, so
// there is no other reader whose looser (case-insensitive) matching it has to share,
// and a part crafted with a repeated or differently-cased key must never be a mask to
// the executor and a source to the bubble that shows it.
func isMaskPart(raw json.RawMessage) bool {
	part, ok := parseJSONObject(raw)
	if !ok {
		return false
	}
	mask := false
	for _, member := range part {
		if member.key != imagePurposeKey {
			continue
		}
		var purpose string
		mask = json.Unmarshal(member.val, &purpose) == nil && purpose == ImagePurposeMask
	}
	return mask
}

// MessageAudio is the url of every audio part of a message content, in order: what
// a transcription-run executor sends as the clip to transcribe (an array content;
// a plain string has none). It is MessageImages' audio twin and exported for the
// same reason as MessageText, and it reads through rewriteBlobPart with the audio
// kind -- the one definition of an audio part the extraction at a run commit and
// the resolution of a transcription run's source already share -- so the executor
// and the resolution cannot disagree about what counts as a clip (an object whose
// audio_url is an object with a string url, members matched without regard to
// case). An audio part has no mask, nor any other tag, so only the urls are
// returned.
func MessageAudio(content json.RawMessage) []string {
	parts, ok := parseJSONArray(content)
	if !ok {
		return nil
	}
	var urls []string
	for _, raw := range parts {
		_, _, _ = rewriteBlobPart(raw, audioPartKind, func(_ blobPartKind, url string) (string, json.RawMessage, error) {
			urls = append(urls, url)
			return url, nil, nil
		})
	}
	return urls
}

// AssistantTurn is the generated assistant message a run persists.
type AssistantTurn struct {
	Reasoning   string
	Content     string
	TTFTMs      int64
	ReasoningMs int64
	// CharsPerSecond is the output CHARACTER rate (portal label "chars/s"),
	// serialized under the legacy `tps` key. TokensPerSecond is the real output
	// tokens/sec, present only once the turn completed. See issue #56.
	CharsPerSecond  float64
	TokensPerSecond float64
	// ContentParts is a structured message content (an array of OpenAI-style
	// content parts) for a turn whose output is not text -- an image run's
	// generated image_url parts or a speech run's audio_url part. When it is
	// non-EMPTY it is written as the message's `content` and Content is ignored;
	// otherwise Content is written as before, byte for byte.
	//
	// The guard is on LENGTH, not on nil: json.Marshal turns a nil RawMessage
	// into `null` but FAILS outright on a non-nil zero-length one
	// ("unexpected end of JSON input"), which would break the terminal commit.
	ContentParts json.RawMessage
	// AudioAnswer marks this turn as a SPEECH run's answer -- the only turns the
	// audio answer lifecycle (#281) acts on at commit time. It is set by the run
	// executor (finishRunWithParts, from the run's pinned kind) and false for
	// every text, image and transcription turn and every checkpoint, so the N=0
	// "do not store" rules and the abort marker below never touch them. An
	// image turn's generated image and a text turn's partial buffer are committed
	// exactly as before.
	AudioAnswer bool
	// AudioStreamed is whether a speech run ACTUALLY streamed its synthesis
	// through to the client (it took the consumeSpeechStream branch) rather than
	// falling back to the buffered one-shot WAV. It only has meaning with
	// AudioAnswer, and it is what tells the commit apart the two N=0 cases: a
	// streamed answer under keep_last_audio=0 was produced only to bill ADR-068's
	// loud non-zero and is committed as the not-stored marker (its audio is never
	// stored), while a buffered-fallback answer is STORED so the un-heard paid
	// audio is not lost -- see writeAssistant.
	AudioStreamed bool
}

func (s *Service) CheckpointAssistant(ctx context.Context, owner auth.Token, chatID string, turn AssistantTurn) error {
	return s.writeAssistant(ctx, owner, chatID, turn, "pending")
}

func (s *Service) CommitAssistant(ctx context.Context, owner auth.Token, chatID string, turn AssistantTurn, status string) error {
	return s.writeAssistant(ctx, owner, chatID, turn, status)
}

// writeAssistant upserts the trailing assistant message: if the last message is
// already an assistant turn it is replaced; otherwise one is appended. Preserves
// the message id across checkpoints.
func (s *Service) writeAssistant(ctx context.Context, owner auth.Token, chatID string, turn AssistantTurn, status string) error {
	row, err := s.chats.ChatByID(ctx, chatID)
	if err != nil {
		return err
	}
	if row.UserID != owner.UserID {
		return ErrChatNotFound
	}
	content, err := s.openChat(row)
	if err != nil {
		return err
	}
	doc := parseChatDoc(content)

	id := "msg_" + compactRandomHex(8)
	if n := len(doc.Messages); n > 0 {
		var last struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		}
		if json.Unmarshal(doc.Messages[n-1], &last) == nil && last.Role == "assistant" {
			if last.ID != "" {
				id = last.ID
			}
			doc.Messages = doc.Messages[:n-1] // drop, we re-append below
		}
	}
	raw, err := buildAssistantMessage(id, status, turn)
	if err != nil {
		return err
	}
	doc.Messages = append(doc.Messages, raw)

	// Only the turn just built is unvetted; the messages before it were stored --
	// and their references verified -- by an earlier commit, so a checkpoint, which
	// runs every few seconds, does no ownership lookups for them.
	return s.commitTurnWithRetention(ctx, chatID, row, doc, turn, status, len(doc.Messages)-1)
}

// buildAssistantMessage renders the stored assistant message JSON for turn under
// the given id and status. A non-empty ContentParts is written as the message's
// `content` (the image/speech/voice shape) and Content is ignored; otherwise the
// streamed text buffer is written. Shared by writeAssistant and CommitVoiceTurn so
// the two produce byte-identical assistant turns.
func buildAssistantMessage(id, status string, turn AssistantTurn) (json.RawMessage, error) {
	msg := map[string]any{
		"id":     id,
		"role":   "assistant",
		"status": status,
	}
	if len(turn.ContentParts) > 0 {
		msg["content"] = turn.ContentParts
	} else {
		msg["content"] = turn.Content
	}
	if turn.Reasoning != "" {
		msg["reasoning"] = turn.Reasoning
	}
	if turn.TTFTMs > 0 {
		msg["ttftMs"] = turn.TTFTMs
	}
	if turn.ReasoningMs > 0 {
		msg["reasoningMs"] = turn.ReasoningMs
	}
	if turn.CharsPerSecond > 0 {
		msg["tps"] = turn.CharsPerSecond
	}
	if turn.TokensPerSecond > 0 {
		// camelCase to match the transcript's other metric keys (ttftMs,
		// reasoningMs) so the portal reads it by a direct cast; the SSE metrics
		// event carries the snake_case tokens_per_second separately (issue #56).
		msg["tokensPerSecond"] = turn.TokensPerSecond
	}
	return json.Marshal(msg)
}

// commitTurnWithRetention is the shared tail of writeAssistant and CommitVoiceTurn:
// it applies the audio-answer lifecycle (#281) to the newly-built turn, extracts the
// new turn's inline blobs into chat_blobs, prunes older audio for the thread's
// retention, and persists doc in ONE SaveChatWithBlobs.
//
// newTurnFrom is the index of the FIRST message THIS commit added (the assistant
// turn for writeAssistant, the user turn for a voice run's user+assistant pair), so
// only those messages' blob references are verified -- the earlier ones were
// verified when they were stored, and re-checking every one on every turn would cost
// a query per image.
//
// The audio answer lifecycle, decided HERE before the blob extraction so nothing is
// ever moved into chat_blobs for an answer that is not kept:
//
//   - a canceled/aborted run commits the not-stored marker instead of an empty turn,
//     so the bubble reads "not stored" and the turn is regenerate-able; and
//   - under keep_last_audio = 0, a run that STREAMED its synthesis (the user heard it
//     live) commits the marker too -- the audio was produced only to bill ADR-068's
//     loud non-zero and must not be stored.
//
// A generated image or a synthesized clip arrives as an inline data: part of the
// turn just built; it is stored in chat_blobs and its url rewritten before the
// document is sealed (and so before the 4 MiB cap). The lazy going-forward prune then
// makes room for the answer under the thread's retention: older answers beyond
// keep_last_audio are rewritten to the not-stored marker and their blobs deleted in
// the very save that writes the document. A thread with no retention set (every
// legacy one, and a voice thread) prunes nothing.
func (s *Service) commitTurnWithRetention(ctx context.Context, chatID string, row store.ChatRow, doc chatDoc, turn AssistantTurn, status string, newTurnFrom int) error {
	keep, keepSet := storedKeepLastAudio(doc.Settings)
	storeNone := keepSet && keep == 0
	aborted := turn.AudioAnswer && status == "canceled"
	dropStreamed := turn.AudioAnswer && status == "complete" && storeNone && turn.AudioStreamed
	if aborted || dropStreamed {
		if err := markNewestAnswerNotStored(&doc); err != nil {
			return err
		}
	}
	blobs, err := s.extractChatBlobs(ctx, chatID, &doc, newTurnFrom)
	if err != nil {
		return err
	}
	var deleteBlobIDs []string
	if turn.AudioAnswer && status == "complete" && storeNone && !turn.AudioStreamed {
		deleteBlobIDs, err = pruneAudioBeyond(&doc, 1)
	} else {
		deleteBlobIDs, err = pruneForRetention(&doc)
	}
	if err != nil {
		return err
	}
	return s.commitChatRun(ctx, row, row.Title, doc, blobs, deleteBlobIDs)
}

// defaultVoiceSystemPrompt is the built-in voice system prompt: the one a voice run carries as
// its sole system message when the thread stores none (or a blank one). It asks the model to
// open every answer with a short <speak lang="..."> summary -- the part that is spoken -- and to
// write the fuller answer after it as text that is shown but not spoken. It is language-agnostic
// on purpose: the model uses the language the user is speaking.
//
// The portal keeps a copy of this text (DEFAULT_VOICE_SYSTEM_PROMPT in the frontend) to pre-fill
// the editable field and to store nothing when the user leaves it unchanged; the two must stay
// identical.
const defaultVoiceSystemPrompt = "You are in a spoken conversation. Begin every answer with ONE short, natural " +
	"spoken summary of the essential point, wrapped in a single " +
	"`<speak lang=\"…\">…</speak>` tag, using the language the user is speaking (set " +
	"`lang` to its two-letter code). Keep what is inside `<speak>` brief enough to " +
	"listen to — a sentence or two, the gist only. After the closing `</speak>`, " +
	"write the fuller answer (detail, lists, code, caveats) as normal text; that " +
	"part is shown but not spoken. Do not put anything before the `<speak>` tag."

// voiceSystemPrompt is the system prompt of a voice run, read from the chat document's
// settings: settings.voice_chat.system_prompt trimmed, or defaultVoiceSystemPrompt when the
// thread stores none -- the key absent, blank, or not a string. The document is schema-free,
// so this decodes the one member it needs (the picks beside it, and any unrelated malformed
// knob, must not switch the prompt off). It is read from the STORED document, never from the
// run ticket or a request field, so a client cannot plant a prompt for a single run.
func voiceSystemPrompt(settings json.RawMessage) string {
	var s struct {
		VoiceChat *struct {
			SystemPrompt string `json:"system_prompt"`
		} `json:"voice_chat"`
	}
	if len(settings) == 0 || json.Unmarshal(settings, &s) != nil || s.VoiceChat == nil {
		return defaultVoiceSystemPrompt
	}
	if prompt := strings.TrimSpace(s.VoiceChat.SystemPrompt); prompt != "" {
		return prompt
	}
	return defaultVoiceSystemPrompt
}

// VoiceRunHistory returns the chat's stored history as OpenAI messages with
// pendingText appended as the latest user message, for a voice run's LLM leg. It
// commits NOTHING: a voice run is speculative and cancellable, so ALL of its
// chat-document persistence is deferred to its finalize (CommitVoiceTurn) and a
// cancelled run leaves the chat untouched (spec 6.1). The older turns' stored image
// references are resolved to data urls (resolveChatBlobRefs), exactly as a text run's
// history is. The history opens with ONE system message: the voice system prompt
// (voiceSystemPrompt -- the thread's stored override, else the built-in default). The
// thread's typed-chat system prompt never reaches a voice run. Owner-scoped.
func (s *Service) VoiceRunHistory(ctx context.Context, owner auth.Token, chatID, pendingText string) ([]ChatAPIMessage, error) {
	row, err := s.chats.ChatByID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	if row.UserID != owner.UserID {
		return nil, ErrChatNotFound
	}
	content, err := s.openChat(row)
	if err != nil {
		return nil, err
	}
	doc := parseChatDoc(content)
	history := buildAPIHistory(voiceSystemPrompt(doc.Settings), doc.Messages)
	history, err = s.resolveChatBlobRefs(ctx, chatID, history)
	if err != nil {
		return nil, err
	}
	// The pending turn is the ASR'd text, in hand already -- no blob to resolve -- so
	// it is appended AFTER the resolution rather than fetched back out of the store.
	history = append(history, ChatAPIMessage{Role: "user", Content: json.RawMessage(mustJSONString(pendingText))})
	// Strip a prior assistant VOICE turn's audio from the LLM history. A stored voice
	// answer is [{text},{audio_url:/api/portal/chat-blobs/...}] (or [{text},{audio_removed}]
	// once pruned); resolveChatBlobRefs resolves image parts only and leaves audio
	// untouched (service_chats_resolve.go), so without this the audio_url -- a
	// gateway-relative url no upstream can fetch -- would ride verbatim into the next
	// /v1/chat/completions body from turn 2 of every conversation. The LLM gets the
	// clean answer TEXT (and any image, kept for a vision model); this is the voice
	// path alone, so the text/image run history is unchanged. It runs over the history
	// INCLUDING the pending turn: an audio-only turn at the tail is dropped, and the
	// pending user text must then be merged into the user turn before it.
	return stripVoiceAssistantAudio(history), nil
}

// stripVoiceAssistantAudio returns a voice run's LLM history with the audio content
// parts -- a synthesized answer's audio_url, and the not-stored audio marker -- removed
// from the ASSISTANT messages. Text and every non-audio part (an image kept for a vision
// model) are left as they are; a plain-string content (a text-only turn) has no part to
// strip and is untouched.
//
// An assistant turn that held ONLY audio (a TTS-only speech-run answer in a mixed
// thread) has nothing left for the LLM, and is OMITTED rather than sent with an empty
// content: a strict upstream (native Anthropic) rejects an empty assistant content on a
// non-final message. Omitting it alone would leave two same-role turns adjacent (user,
// user), which a strict-alternation chat template (several local models') rejects just as
// firmly, so the two neighbours are MERGED into one turn -- valid on every upstream, and
// with no invented placeholder text for the model to imitate. Only turns that the
// omission made adjacent are merged; same-role neighbours that were already adjacent in
// the stored thread are left exactly as stored. The input is not modified.
func stripVoiceAssistantAudio(history []ChatAPIMessage) []ChatAPIMessage {
	out := make([]ChatAPIMessage, 0, len(history))
	omitted := false // a turn was omitted since the last kept one
	for _, msg := range history {
		if msg.Role == "assistant" {
			content, changed, empty := withoutAudioParts(msg.Content)
			if empty {
				omitted = true
				continue
			}
			if changed {
				msg.Content = content
			}
		}
		out = appendVoiceHistoryTurn(out, msg, omitted)
		omitted = false
	}
	return out
}

// appendVoiceHistoryTurn appends msg to turns, first merging it into the last kept turn
// when an omitted audio-only turn between them left the two with the same role (afterOmit).
// A pair whose content cannot be merged losslessly stays two turns: an adjacent same-role
// pair is a smaller fault than a lost message.
func appendVoiceHistoryTurn(turns []ChatAPIMessage, msg ChatAPIMessage, afterOmit bool) []ChatAPIMessage {
	if n := len(turns); afterOmit && n > 0 && turns[n-1].Role == msg.Role {
		if merged, ok := mergeMessageContent(turns[n-1].Content, msg.Content); ok {
			turns[n-1].Content = merged
			return turns
		}
	}
	return append(turns, msg)
}

// mergeMessageContent joins the content of two same-role messages into one: two plain
// strings become one string separated by a blank line; otherwise the content is joined as
// a parts array (a string becoming one text part), so images and other parts survive. ok
// is false when either content is neither a string nor an array of parts.
func mergeMessageContent(a, b json.RawMessage) (json.RawMessage, bool) {
	var sa, sb string
	if json.Unmarshal(a, &sa) == nil && json.Unmarshal(b, &sb) == nil {
		return json.RawMessage(mustJSONString(strings.TrimSpace(sa + "\n\n" + sb))), true
	}
	pa, okA := contentAsParts(a)
	pb, okB := contentAsParts(b)
	if !okA || !okB {
		return nil, false
	}
	raw, err := json.Marshal(append(pa, pb...))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// contentAsParts is a message content as a parts array: an array as stored, a string as
// one text part (none when empty). ok is false for anything else.
func contentAsParts(content json.RawMessage) ([]json.RawMessage, bool) {
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) == nil {
		return parts, true
	}
	var s string
	if json.Unmarshal(content, &s) != nil {
		return nil, false
	}
	if s == "" {
		return nil, true
	}
	part, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: s})
	if err != nil {
		return nil, false
	}
	return []json.RawMessage{part}, true
}

// withoutAudioParts is the content of one assistant message with its audio parts dropped,
// whether that differs from content, and whether NOTHING is left (every part was audio, so
// the turn carries no text or image for the LLM). A content that is not an array of parts
// (a plain-string answer) and one with no audio part are unchanged (changed false, empty
// false); on empty the returned content is nil and the caller omits the turn.
func withoutAudioParts(content json.RawMessage) (stripped json.RawMessage, changed, empty bool) {
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return nil, false, false // not an array of parts (a plain-string answer): nothing to strip
	}
	kept := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		if !isAudioContentPart(p) {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(parts) {
		return nil, false, false // no audio part was present
	}
	if len(kept) == 0 {
		return nil, true, true
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, false, false
	}
	return raw, true, false
}

// isAudioContentPart reports whether a content part is an audio part -- a stored
// answer's audio_url, or the audio_removed not-stored marker -- and so is dropped from a
// voice run's LLM history.
func isAudioContentPart(raw json.RawMessage) bool {
	var p struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case speechAudioURLPartType, removedAudioPartType:
		return true
	default:
		return false
	}
}

// speechAudioURLPartType is the `type` of a stored speech/voice answer's audio part
// ({"type":"audio_url",...}); the gateway's speechAudioPartType is its twin on the
// producing side.
const speechAudioURLPartType = "audio_url"

// CommitVoiceTurn persists a voice turn: the user message (userText) AND the
// assistant turn (turn) in ONE SaveChatWithBlobs. Unlike a normal chat run -- which
// commits the user turn up front via PrepareChatRun and the assistant turn later via
// CommitAssistant -- a voice run defers BOTH to this single commit, so a cancelled
// speculative run (which never reaches this call) leaves the chat untouched and
// cancel-and-extend needs no undo (spec 6.1). Both messages are freshly appended
// (never an upsert): a voice run persisted nothing earlier, so there is no trailing
// pending turn of its own to replace. Owner-scoped.
func (s *Service) CommitVoiceTurn(ctx context.Context, owner auth.Token, chatID, userText string, turn AssistantTurn, status string) error {
	row, err := s.chats.ChatByID(ctx, chatID)
	if err != nil {
		return err
	}
	if row.UserID != owner.UserID {
		return ErrChatNotFound
	}
	content, err := s.openChat(row)
	if err != nil {
		return err
	}
	doc := parseChatDoc(content)
	newTurnFrom := len(doc.Messages)
	userMsg, err := json.Marshal(map[string]any{
		"id":      "msg_" + compactRandomHex(8),
		"role":    "user",
		"content": userText,
	})
	if err != nil {
		return err
	}
	assistantMsg, err := buildAssistantMessage("msg_"+compactRandomHex(8), status, turn)
	if err != nil {
		return err
	}
	doc.Messages = append(doc.Messages, userMsg, assistantMsg)
	return s.commitTurnWithRetention(ctx, chatID, row, doc, turn, status, newTurnFrom)
}
