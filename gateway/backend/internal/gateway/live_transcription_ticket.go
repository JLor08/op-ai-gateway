// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
	"slices"
	"sync"
	"time"
)

// liveTicketTTL is how long a minted live-transcription ticket stays
// redeemable. It only has to cover the gap between the browser's authorize POST
// and the WebSocket upgrade it opens right after, so it is deliberately short.
const liveTicketTTL = 30 * time.Second

// liveTicketNonceBytes is the size of the random nonce inside the claims that
// makes every ticket unique and keys the single-use set.
const liveTicketNonceBytes = 16

// liveTicketKeyBytes is the size of the per-process AES-256 key.
const liveTicketKeyBytes = 32

// liveTicketAAD is the additional authenticated data bound into every ticket, so
// a ciphertext sealed for this purpose cannot be opened as anything else even if
// the key were ever reused for another AEAD in this process.
var liveTicketAAD = []byte("op-ai-gateway/live-transcription-ticket/v1")

// liveTicketEncoding is the token alphabet: URL-safe, no padding, so the whole
// token is a valid Sec-WebSocket-Protocol value (no '+', '/', '=', space or
// comma). Strict makes the decoder reject non-canonical trailing bits, so a
// token has exactly one accepted spelling and a flipped final character cannot
// slip through as the same bytes.
var liveTicketEncoding = base64.RawURLEncoding.Strict()

// liveTicketClaims is what a live-transcription ticket carries: the routing and
// billing decision the authorize endpoint made, so the WebSocket handler never
// takes a model, mapping or audio rate from the client. Mint sets Nonce and Exp;
// the caller supplies the rest. The fields are exported so they survive the JSON
// round-trip through the token.
//
// The claims travel ENCRYPTED: the browser holds the token but cannot read it,
// so Target may carry what must not reach the end user (the internal upstream
// Endpoint, the sealed APIToken, upstream headers).
type liveTicketClaims struct {
	Nonce      string
	Exp        int64 // Unix seconds; a ticket is redeemable while now <= Exp
	Target     routing.Target
	SampleRate int
	Channels   int
	Format     string
	Language   string
	UserID     string
	TokenID    string

	// ServiceID/ServiceName/ProjectID/ProjectName are the billing attribution the
	// authenticated token carried at mint time. The WebSocket has no principal of its
	// own, so the usage row it records takes its service and project from here, as the
	// buffered transcription row takes them off the request's token.
	ServiceID   string
	ServiceName string
	ProjectID   string
	ProjectName string
}

// params is the validated PCM shape the WebSocket relay forwards upstream and bills at,
// lifted out of the claims so the live relay/billing core takes the SAME liveParams the
// bearer HTTP relay builds from its query string.
func (c liveTicketClaims) params() liveParams {
	return liveParams{SampleRate: c.SampleRate, Channels: c.Channels, Format: c.Format, Language: c.Language}
}

// attribution is who the WebSocket session's usage row is billed to: the service and
// project the ticket endpoint sealed off the authenticated token, so a WebSocket row --
// whose upgrade carries no principal -- is attributed like a buffered transcription's.
func (c liveTicketClaims) attribution() liveAttribution {
	return liveAttribution{
		UserID:      c.UserID,
		TokenID:     c.TokenID,
		ServiceID:   c.ServiceID,
		ServiceName: c.ServiceName,
		ProjectID:   c.ProjectID,
		ProjectName: c.ProjectName,
	}
}

// liveTicketStore mints and redeems live-transcription tickets.
//
// A browser cannot send custom headers on a WebSocket upgrade, so it cannot
// carry the portal's X-OP-CSRF header. The authenticated authorize POST mints a
// ticket instead and the browser presents it on the upgrade as a
// Sec-WebSocket-Protocol value; the WebSocket handler redeems it before
// accepting. The ticket is therefore both the CSRF defense (an attacker's page
// cannot obtain one) and the server-resolved source of truth for routing and
// billing.
//
// A token is base64url(gcmNonce || AES-256-GCM(key, JSON(claims))) with a fresh
// random GCM nonce per ticket. GCM gives confidentiality and integrity in one
// step: a tampered, truncated or foreign token fails to open, and nothing in the
// claims can be read from the token. The key is random per process, so a restart
// invalidates every outstanding ticket, which is fine for a 30-second TTL.
//
// Single-use is enforced by an in-process set of redeemed claim nonces, so it
// holds per gateway instance only. Cross-instance single-use (a shared
// redeemed-nonce store) is a documented hardening follow-up. The same
// per-process key means a ticket opens only on the instance (and process
// lifetime) that minted it: with several instances, or behind a load balancer
// that is not sticky, the key would have to be shared as well. The ticket is
// additionally session- and origin-bound at the WebSocket layer.
type liveTicketStore struct {
	aead cipher.AEAD
	now  func() time.Time // a seam for tests; time.Now in production

	mu sync.Mutex
	// redeemed maps each redeemed nonce to its ticket's Exp. A nonce only has to
	// be remembered until its ticket would be rejected as expired anyway.
	redeemed map[string]int64
	// nextPrune is the earliest instant (Unix seconds) at which Redeem sweeps
	// expired nonces again, so pruning is amortised rather than per-call.
	nextPrune int64
}

// newLiveTicketStore returns a store with a fresh random AES-256 key. A key
// that cannot be generated or used would leave every ticket forgeable or
// readable, so it panics rather than continue.
func newLiveTicketStore() *liveTicketStore {
	key := make([]byte, liveTicketKeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("live ticket store: generate key: %v", err))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(fmt.Sprintf("live ticket store: init AES: %v", err))
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("live ticket store: init GCM: %v", err))
	}
	return &liveTicketStore{
		aead:     aead,
		now:      time.Now,
		redeemed: make(map[string]int64),
	}
}

// seal encrypts and authenticates plaintext under a fresh random GCM nonce and
// returns the token base64url(gcmNonce || ciphertext || tag).
func (s *liveTicketStore) seal(plaintext []byte) (string, error) {
	gcmNonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(gcmNonce); err != nil {
		return "", fmt.Errorf("live ticket: generate GCM nonce: %w", err)
	}
	sealed := s.aead.Seal(nil, gcmNonce, plaintext, liveTicketAAD)
	return liveTicketEncoding.EncodeToString(slices.Concat(gcmNonce, sealed)), nil
}

// open reverses seal. It reports false for anything that is not a canonical
// base64url token this store's key sealed: malformed, truncated, tampered or
// sealed by another key.
func (s *liveTicketStore) open(token string) ([]byte, bool) {
	raw, err := liveTicketEncoding.DecodeString(token)
	if err != nil {
		return nil, false
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize+s.aead.Overhead() {
		return nil, false
	}
	plaintext, err := s.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], liveTicketAAD)
	if err != nil {
		return nil, false
	}
	return plaintext, true
}

// Mint stamps claims with a fresh random Nonce and Exp = now + liveTicketTTL
// (overwriting any caller-set values) and returns the encrypted token. It fails
// only if the system random source or JSON encoding does.
func (s *liveTicketStore) Mint(claims liveTicketClaims) (string, error) {
	nonce := make([]byte, liveTicketNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("live ticket: generate nonce: %w", err)
	}
	claims.Nonce = liveTicketEncoding.EncodeToString(nonce)
	claims.Exp = s.now().Add(liveTicketTTL).Unix()

	raw, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("live ticket: encode claims: %w", err)
	}
	return s.seal(raw)
}

// Redeem opens a token and consumes it. It returns the claims and true only for
// a well-formed token this store's key sealed whose Exp has not passed and whose
// nonce has not been redeemed before; otherwise it returns false and consumes
// nothing. Concurrent redeems of the same token admit exactly one.
func (s *liveTicketStore) Redeem(token string) (liveTicketClaims, bool) {
	raw, ok := s.open(token)
	if !ok {
		return liveTicketClaims{}, false
	}
	var claims liveTicketClaims
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Nonce == "" {
		return liveTicketClaims{}, false
	}

	// The clock is read under the lock, after the decryption work, so a nonce
	// can never be pruned (its Exp already behind a later reader's clock) while
	// a redeemer holding an earlier reading is still about to admit it.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Unix()
	if now > claims.Exp {
		return liveTicketClaims{}, false
	}
	if now >= s.nextPrune {
		for nonce, exp := range s.redeemed {
			if exp < now {
				delete(s.redeemed, nonce)
			}
		}
		s.nextPrune = now + int64(liveTicketTTL/time.Second)
	}
	if _, replayed := s.redeemed[claims.Nonce]; replayed {
		return liveTicketClaims{}, false
	}
	s.redeemed[claims.Nonce] = claims.Exp
	return claims, true
}
