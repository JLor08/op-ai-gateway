// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"time"
)

// liveTicketTTL is how long a minted live-transcription ticket stays
// redeemable. It only has to cover the gap between the browser's authorize POST
// and the WebSocket upgrade it opens right after, so it is deliberately short.
const liveTicketTTL = 30 * time.Second

// liveTicketNonceBytes is the size of the random nonce that makes every ticket
// unique and keys the single-use set.
const liveTicketNonceBytes = 16

// liveTicketKeyBytes is the size of the per-process HMAC key.
const liveTicketKeyBytes = 32

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
// The claims are SIGNED, NOT ENCRYPTED: the browser holds the token and can
// base64-decode the claims, Target included. Whatever the minting caller puts
// here is readable by the end user, so it must not contain anything the end user
// may not see.
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
// A token is base64url(JSON(claims)) + "." + base64url(HMAC-SHA256(key,
// base64url(JSON(claims)))). The key is random per process, so a restart
// invalidates every outstanding ticket, which is fine for a 30-second TTL.
//
// Single-use is enforced by an in-process set of redeemed nonces, so it holds
// per gateway instance only. Cross-instance single-use (a shared
// redeemed-nonce store) is a documented hardening follow-up: the signed, Exp
// bounded token verifies on any instance that shares a key, and the ticket is
// additionally session- and origin-bound at the WebSocket layer.
type liveTicketStore struct {
	key []byte
	now func() time.Time // a seam for tests; time.Now in production

	mu sync.Mutex
	// redeemed maps each redeemed nonce to its ticket's Exp. A nonce only has to
	// be remembered until its ticket would be rejected as expired anyway.
	redeemed map[string]int64
	// nextPrune is the earliest instant (Unix seconds) at which Redeem sweeps
	// expired nonces again, so pruning is amortised rather than per-call.
	nextPrune int64
}

// newLiveTicketStore returns a store with a fresh random HMAC key. A key that
// cannot be generated would leave every ticket forgeable, so it panics rather
// than continue.
func newLiveTicketStore() *liveTicketStore {
	key := make([]byte, liveTicketKeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("live ticket store: generate HMAC key: %v", err))
	}
	return &liveTicketStore{
		key:      key,
		now:      time.Now,
		redeemed: make(map[string]int64),
	}
}

// mac returns the raw HMAC-SHA256 of a base64url payload under the store's key.
func (s *liveTicketStore) mac(payload string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// sign returns the base64url form of mac, the second half of a token.
func (s *liveTicketStore) sign(payload string) string {
	return liveTicketEncoding.EncodeToString(s.mac(payload))
}

// Mint stamps claims with a fresh random Nonce and Exp = now + liveTicketTTL
// (overwriting any caller-set values) and returns the signed token. It fails
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
	payload := liveTicketEncoding.EncodeToString(raw)
	return payload + "." + s.sign(payload), nil
}

// Redeem verifies a token and consumes it. It returns the claims and true only
// for a well-formed token signed with this store's key whose Exp has not passed
// and whose nonce has not been redeemed before; otherwise it returns false and
// consumes nothing. Concurrent redeems of the same token admit exactly one.
func (s *liveTicketStore) Redeem(token string) (liveTicketClaims, bool) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" || strings.Contains(sig, ".") {
		return liveTicketClaims{}, false
	}
	gotMAC, err := liveTicketEncoding.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(gotMAC, s.mac(payload)) != 1 {
		return liveTicketClaims{}, false
	}

	raw, err := liveTicketEncoding.DecodeString(payload)
	if err != nil {
		return liveTicketClaims{}, false
	}
	var claims liveTicketClaims
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Nonce == "" {
		return liveTicketClaims{}, false
	}

	// The clock is read under the lock, after the signature work, so a nonce
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
