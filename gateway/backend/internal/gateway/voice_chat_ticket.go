// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"sync"
	"time"
)

// voiceTicketTTL is how long a minted voice-chat ticket stays redeemable. It only
// has to cover the gap between the browser's authorize POST and the WebSocket
// upgrade it opens right after, so it is deliberately short.
const voiceTicketTTL = 30 * time.Second

// voiceTicketNonceBytes is the size of the random nonce inside the claims that
// makes every ticket unique and keys the single-use set.
const voiceTicketNonceBytes = 16

// voiceTicketKeyBytes is the size of the per-process AES-256 key.
const voiceTicketKeyBytes = 32

// voiceTicketAAD is the additional authenticated data bound into every ticket, so
// a ciphertext sealed for this purpose cannot be opened as anything else (the
// live-transcription ticket included) even if a key were ever reused for another
// AEAD in this process.
var voiceTicketAAD = []byte("op-ai-gateway/voice-chat-ticket/v1")

// voiceTicketEncoding is the token alphabet: URL-safe, no padding, so the whole
// token is a valid Sec-WebSocket-Protocol value (no '+', '/', '=', space or
// comma). Strict makes the decoder reject non-canonical trailing bits, so a
// token has exactly one accepted spelling and a flipped final character cannot
// slip through as the same bytes.
var voiceTicketEncoding = base64.RawURLEncoding.Strict()

// The reasons Redeem refuses a token. A caller maps every one of them to the same
// refusal on the wire; they are distinct so a log line and a test can tell a
// forged token from a stale one from a replayed one.
var (
	// errVoiceTicketInvalid is a token that is not a canonical token this store's
	// key sealed: malformed, truncated, tampered, foreign, or with unreadable claims.
	errVoiceTicketInvalid = errors.New("voice ticket: invalid")
	// errVoiceTicketExpired is a genuine token redeemed after its Exp.
	errVoiceTicketExpired = errors.New("voice ticket: expired")
	// errVoiceTicketReplayed is a genuine, unexpired token that was already redeemed.
	errVoiceTicketReplayed = errors.New("voice ticket: already redeemed")
)

// voiceTicketClaims is what a voice-chat ticket carries: the three resolved
// targets of the hands-free ASR -> LLM -> TTS loop, the chat the turns belong to,
// who asked, the audio settings and the validated PCM shape the authorize
// endpoint settled, so the WebSocket handler never takes a model, mapping or audio
// rate from the client. Mint sets Nonce and Exp; the caller supplies the rest. The
// fields are exported so they survive the JSON round-trip through the token.
//
// The claims travel ENCRYPTED: the browser holds the token but cannot read it, so
// a Target may carry what must not reach the end user (the internal upstream
// Endpoint, the sealed APIToken, upstream headers).
type voiceTicketClaims struct {
	// ASR, LLM and TTS are the resolved target of each leg of the loop: the
	// transcription model, the chat model and the speech model.
	ASR routing.Target
	LLM routing.Target
	TTS routing.Target

	// ChatID is the portal chat the voice turns are persisted to.
	ChatID string
	// UserID, TokenID, ServiceID and ProjectID attribute the session: who asked and
	// which service and project the usage belongs to.
	UserID    string
	TokenID   string
	ServiceID string
	ProjectID string

	// Audio is the chat's speech settings (voice, speed, streaming, passthrough),
	// snapshotted when the ticket was minted.
	Audio portal.ChatAudioSettings

	// SampleRate, Channels and SampleFormat are the validated shape of the PCM the
	// browser streams up (e.g. 16000, 1, "s16le").
	SampleRate   int
	Channels     int
	SampleFormat string

	Exp   time.Time
	Nonce string
}

// voiceTicketStore mints and redeems voice-chat tickets.
//
// A browser cannot send custom headers on a WebSocket upgrade, so it cannot carry
// the portal's X-OP-CSRF header. The authenticated authorize POST mints a ticket
// instead and the browser presents it on the upgrade as a Sec-WebSocket-Protocol
// value; the WebSocket handler redeems it before accepting. The ticket is
// therefore both the CSRF defense (an attacker's page cannot obtain one) and the
// server-resolved source of truth for routing, attribution and the chat.
//
// It is deliberately a parallel, typed twin of liveTicketStore rather than a
// shared generic: the crypto is tiny and isolated, and sharing it would refactor
// the live-transcription path for no gain. The scheme is the same.
//
// A token is base64url(gcmNonce || AES-256-GCM(key, JSON(claims))) with a fresh
// random GCM nonce per ticket. GCM gives confidentiality and integrity in one
// step: a tampered, truncated or foreign token fails to open, and nothing in the
// claims can be read from the token. The key is random per process, so a restart
// invalidates every outstanding ticket, which is fine for a 30-second TTL.
//
// Single-use is enforced by an in-process set of redeemed claim nonces, so it
// holds per gateway instance only; the same per-process key means a ticket opens
// only on the instance (and process lifetime) that minted it. The ticket is
// additionally session- and origin-bound at the WebSocket layer.
type voiceTicketStore struct {
	aead cipher.AEAD
	now  func() time.Time // a seam for tests; time.Now in production

	mu sync.Mutex
	// redeemed maps each redeemed nonce to its ticket's Exp. A nonce only has to be
	// remembered until its ticket would be rejected as expired anyway.
	redeemed map[string]time.Time
	// nextPrune is the earliest instant at which Redeem sweeps expired nonces
	// again, so pruning is amortised rather than per-call.
	nextPrune time.Time
}

// newVoiceTicketStore returns a store with a fresh random AES-256 key. A key that
// cannot be generated or used would leave every ticket forgeable or readable, so
// it panics rather than continue.
func newVoiceTicketStore() *voiceTicketStore {
	key := make([]byte, voiceTicketKeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("voice ticket store: generate key: %v", err))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(fmt.Sprintf("voice ticket store: init AES: %v", err))
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("voice ticket store: init GCM: %v", err))
	}
	return &voiceTicketStore{
		aead:     aead,
		now:      time.Now,
		redeemed: make(map[string]time.Time),
	}
}

// seal encrypts and authenticates plaintext under a fresh random GCM nonce and
// returns the token base64url(gcmNonce || ciphertext || tag).
func (s *voiceTicketStore) seal(plaintext []byte) (string, error) {
	gcmNonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(gcmNonce); err != nil {
		return "", fmt.Errorf("voice ticket: generate GCM nonce: %w", err)
	}
	sealed := s.aead.Seal(nil, gcmNonce, plaintext, voiceTicketAAD)
	return voiceTicketEncoding.EncodeToString(slices.Concat(gcmNonce, sealed)), nil
}

// open reverses seal. It reports false for anything that is not a canonical
// base64url token this store's key sealed: malformed, truncated, tampered or
// sealed by another key.
func (s *voiceTicketStore) open(token string) ([]byte, bool) {
	raw, err := voiceTicketEncoding.DecodeString(token)
	if err != nil {
		return nil, false
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize+s.aead.Overhead() {
		return nil, false
	}
	plaintext, err := s.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], voiceTicketAAD)
	if err != nil {
		return nil, false
	}
	return plaintext, true
}

// Mint stamps c with a fresh random Nonce and Exp = now + voiceTicketTTL
// (overwriting any caller-set values) and returns the encrypted token. It fails
// only if the system random source or JSON encoding does.
func (s *voiceTicketStore) Mint(c voiceTicketClaims) (string, error) {
	nonce := make([]byte, voiceTicketNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("voice ticket: generate nonce: %w", err)
	}
	c.Nonce = voiceTicketEncoding.EncodeToString(nonce)
	c.Exp = s.now().Add(voiceTicketTTL)

	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("voice ticket: encode claims: %w", err)
	}
	return s.seal(raw)
}

// Redeem opens a token and consumes it. It returns the claims and a nil error only
// for a well-formed token this store's key sealed whose Exp has not passed and
// whose nonce has not been redeemed before; otherwise it returns the zero claims
// and errVoiceTicketInvalid, errVoiceTicketExpired or errVoiceTicketReplayed, and
// consumes nothing. Concurrent redeems of the same token admit exactly one.
func (s *voiceTicketStore) Redeem(token string) (voiceTicketClaims, error) {
	raw, ok := s.open(token)
	if !ok {
		return voiceTicketClaims{}, errVoiceTicketInvalid
	}
	var c voiceTicketClaims
	if err := json.Unmarshal(raw, &c); err != nil || c.Nonce == "" {
		return voiceTicketClaims{}, errVoiceTicketInvalid
	}

	// The clock is read under the lock, after the decryption work, so a nonce can
	// never be pruned (its Exp already behind a later reader's clock) while a
	// redeemer holding an earlier reading is still about to admit it.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// A ticket is redeemable up to and including the instant it expires at.
	if now.After(c.Exp) {
		return voiceTicketClaims{}, errVoiceTicketExpired
	}
	if !now.Before(s.nextPrune) {
		for nonce, exp := range s.redeemed {
			if exp.Before(now) {
				delete(s.redeemed, nonce)
			}
		}
		s.nextPrune = now.Add(voiceTicketTTL)
	}
	if _, replayed := s.redeemed[c.Nonce]; replayed {
		return voiceTicketClaims{}, errVoiceTicketReplayed
	}
	s.redeemed[c.Nonce] = c.Exp
	return c, nil
}
