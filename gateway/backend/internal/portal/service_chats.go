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
)

const (
	// maxChatTitleLen caps the plaintext chat title (in runes).
	maxChatTitleLen = 200
	// MaxChatContentBytes caps the pre-seal (raw JSON) content blob at 4 MiB.
	// Exported because the portal composer has to state the remaining capacity
	// of an image thread BEFORE the user commits to a multi-minute generation,
	// and it is served to the client on ChatListResponse rather than
	// duplicated there -- a second copy in the frontend would drift from this
	// one with nothing to catch it.
	MaxChatContentBytes = 4 << 20
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
// portal list endpoints) plus the limits a client needs to stay inside it.
//
// MaxContentBytes is MaxChatContentBytes: the client cannot otherwise know it
// (no other DTO carries it), and a client that hardcoded its own copy would
// drift from this one silently -- the failure mode being a composer that
// confidently states the wrong remaining capacity. It is NOT omitempty: a
// missing field and a zero are the same thing on the wire, and the portal
// reads zero as "capacity unknown" (no capacity line, no refusal), so an
// accidental zero would disable the feature rather than break loudly.
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
	keyVersion, blob, err := s.sealChat(content)
	if err != nil {
		return ChatDTO{}, err
	}
	now := s.clock().UTC()
	updated := store.Chat{
		ID:         row.ID,
		UserID:     row.UserID,
		Title:      title,
		KeyVersion: keyVersion,
		Blob:       blob,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  now,
	}
	if err := s.chats.UpdateChat(ctx, updated); err != nil {
		return ChatDTO{}, err
	}
	return ChatDTO{ID: row.ID, Title: title, CreatedAt: row.CreatedAt, UpdatedAt: now, Content: content}, nil
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
	// pre-existing chat) or "image". It is established by the FIRST send and
	// then forced by PrepareChatRun on every later send, because the composer's
	// affordances follow the thread rather than the currently-picked model --
	// see the spec's "the kind is pinned to the thread at first send".
	//
	// It is a UI constraint, NOT an authorization: this struct is the POST
	// body verbatim (chat_run_endpoints.go), so a client can submit any value
	// on the first send. The routing capability gate remains the only
	// authority on what the gateway will actually serve.
	//
	// Declared LAST and omitempty on purpose: that is what keeps an existing
	// text chat's persisted settings byte-identical.
	Kind string `json:"kind,omitempty"`
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

// PrepareChatRun commits the user turn (or replaces history for edit/regenerate)
// and the settings into the chat, self-heals the settings' server_override
// (see below), then returns the OpenAI message history the run executor will
// send (system prompt prepended when set) TOGETHER WITH the settings actually
// persisted — which the caller must feed into the run executor (via
// gateway.PrepareRunResult.Settings) instead of the raw request settings, so a
// self-healed clear is honored rather than silently overridden by the stale
// value the client submitted. Owner-scoped.
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

	if req.EditedHistory != nil {
		doc.Messages = req.EditedHistory
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
	// this function already uses (doc.Settings feeds newContent feeds
	// SaveChat), so a stale override is healed exactly once rather than
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
	var stored struct {
		Kind string `json:"kind"`
	}
	if len(doc.Settings) > 0 {
		_ = json.Unmarshal(doc.Settings, &stored) // best-effort: a malformed blob leaves the kind unpinned, which is the pre-feature behaviour
	}
	if hadPriorMessages {
		req.Settings.Kind = stored.Kind
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
	newContent, err := doc.marshal()
	if err != nil {
		return nil, ChatRunSettings{}, err
	}
	if _, err := s.SaveChat(ctx, owner, chatID, UpdateChatRequest{Title: &title, Content: &newContent}); err != nil {
		return nil, ChatRunSettings{}, err
	}

	return buildAPIHistory(req.Settings.SystemPrompt, doc.Messages), req.Settings, nil
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
	// generated image_url part. When it is non-EMPTY it is written as the
	// message's `content` and Content is ignored; otherwise Content is written
	// as before, byte for byte.
	//
	// The guard is on LENGTH, not on nil: json.Marshal turns a nil RawMessage
	// into `null` but FAILS outright on a non-nil zero-length one
	// ("unexpected end of JSON input"), which would break the terminal commit.
	ContentParts json.RawMessage
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
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	doc.Messages = append(doc.Messages, raw)

	newContent, err := doc.marshal()
	if err != nil {
		return err
	}
	_, err = s.SaveChat(ctx, owner, chatID, UpdateChatRequest{Content: &newContent})
	return err
}
