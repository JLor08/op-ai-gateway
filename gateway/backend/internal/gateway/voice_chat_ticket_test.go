// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"reflect"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newVoiceTicketTestStore returns a store whose clock is a hand-driven fake, pinned to a
// whole second so the Exp boundary math is exact. The returned pointer is the clock: a test
// advances it between store calls, never concurrently with them.
func newVoiceTicketTestStore() (*voiceTicketStore, *time.Time) {
	cur := time.Unix(1_800_000_000, 0)
	store := newVoiceTicketStore()
	store.now = func() time.Time { return cur }
	return store, &cur
}

// sampleVoiceTicketClaims carries a distinct, recognisable target per leg (so a swap or a
// dropped leg fails the round trip) plus a ChatAudioSettings with the pointer and
// raw-JSON fields, which are the ones a JSON round trip could plausibly mangle.
func sampleVoiceTicketClaims() voiceTicketClaims {
	speed := 1.25
	stream := true
	target := func(leg, model string) routing.Target {
		return routing.Target{
			RouteID:       "route-" + leg,
			ServerID:      "server-" + leg,
			Provider:      "audiocpp",
			Endpoint:      "http://" + leg + ".internal:9000",
			Model:         model,
			ProviderModel: model + "-upstream",
			Timeout:       90 * time.Second,
			APIFlavor:     "native",
			APIToken:      "enc:sealed-" + leg + "-credential",
			APIFlavors:    []string{"native"},
			ExtraHeaders:  map[string]string{"x-leg": leg},
		}
	}
	return voiceTicketClaims{
		ASR:       target("asr", "asr-model"),
		LLM:       target("llm", "llm-model"),
		TTS:       target("tts", "tts-model"),
		ChatID:    "chat-1",
		UserID:    "user-1",
		TokenID:   "token-1",
		ServiceID: "svc-1",
		ProjectID: "proj-1",
		Audio: portal.ChatAudioSettings{
			Voice:       "anna",
			Speed:       &speed,
			Stream:      &stream,
			Language:    "de",
			Passthrough: map[string]json.RawMessage{"top_p": json.RawMessage(`0.9`)},
		},
		SampleRate:   16000,
		Channels:     1,
		SampleFormat: "s16le",
	}
}

func mustMintVoiceTicket(t *testing.T, store *voiceTicketStore, claims voiceTicketClaims) string {
	t.Helper()
	token, err := store.Mint(claims)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return token
}

func TestVoiceTicketMintRedeemRoundTrips(t *testing.T) {
	store, now := newVoiceTicketTestStore()
	minted := sampleVoiceTicketClaims()

	token := mustMintVoiceTicket(t, store, minted)

	got, err := store.Redeem(token)
	if err != nil {
		t.Fatalf("Redeem of a fresh ticket: %v", err)
	}
	if got.Nonce == "" {
		t.Fatalf("redeemed Nonce is empty, want the random nonce Mint set")
	}
	if want := now.Add(voiceTicketTTL); !got.Exp.Equal(want) {
		t.Fatalf("redeemed Exp = %v, want now+voiceTicketTTL = %v", got.Exp, want)
	}
	// The three targets, the chat, the identity and the audio settings are what the voice
	// socket trusts instead of anything the client sends, so each must survive intact.
	for leg, pair := range map[string][2]routing.Target{
		"ASR": {got.ASR, minted.ASR},
		"LLM": {got.LLM, minted.LLM},
		"TTS": {got.TTS, minted.TTS},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("%s target differs after the round trip:\n got  %+v\n want %+v", leg, pair[0], pair[1])
		}
	}
	if got.ChatID != "chat-1" {
		t.Fatalf("ChatID = %q, want chat-1", got.ChatID)
	}
	if !reflect.DeepEqual(got.Audio, minted.Audio) {
		t.Fatalf("Audio differs after the round trip:\n got  %+v\n want %+v", got.Audio, minted.Audio)
	}
	// Nonce and Exp are Mint's to set (Exp is compared above, by instant, since a time.Time
	// does not DeepEqual across a JSON round trip); every other field must round-trip exactly.
	minted.Nonce, minted.Exp = got.Nonce, got.Exp
	if !reflect.DeepEqual(got, minted) {
		t.Fatalf("redeemed claims differ from minted:\n got  %+v\n want %+v", got, minted)
	}
}

func TestVoiceTicketRedeemRejectsExpired(t *testing.T) {
	store, now := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())

	*now = now.Add(voiceTicketTTL + time.Second)

	got, err := store.Redeem(token)
	if err == nil {
		t.Fatalf("Redeem after the TTL succeeded (%+v), want an error", got)
	}
	if !errors.Is(err, errVoiceTicketExpired) {
		t.Fatalf("Redeem after the TTL error = %v, want errVoiceTicketExpired", err)
	}
	if !reflect.DeepEqual(got, voiceTicketClaims{}) {
		t.Fatalf("a rejected Redeem returned claims: %+v", got)
	}
}

func TestVoiceTicketRedeemableUpToAndIncludingExp(t *testing.T) {
	store, now := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())

	// now > Exp rejects, so the Exp instant itself is still valid and the next
	// instant is not.
	*now = now.Add(voiceTicketTTL)
	if _, err := store.Redeem(token); err != nil {
		t.Fatalf("Redeem at exactly Exp: %v", err)
	}

	late := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())
	*now = now.Add(voiceTicketTTL + time.Nanosecond)
	if _, err := store.Redeem(late); !errors.Is(err, errVoiceTicketExpired) {
		t.Fatalf("Redeem one nanosecond after Exp error = %v, want errVoiceTicketExpired", err)
	}
}

func TestVoiceTicketRedeemIsSingleUse(t *testing.T) {
	store, _ := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())

	if _, err := store.Redeem(token); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	got, err := store.Redeem(token)
	if err == nil {
		t.Fatalf("second Redeem succeeded (%+v), want an error", got)
	}
	if !errors.Is(err, errVoiceTicketReplayed) {
		t.Fatalf("second Redeem error = %v, want errVoiceTicketReplayed", err)
	}
}

func TestVoiceTicketRedeemIsSingleUseUnderConcurrency(t *testing.T) {
	store, _ := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())

	const racers = 16
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Redeem(token); err == nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	if n := admitted.Load(); n != 1 {
		t.Fatalf("%d of %d concurrent redeems were admitted, want exactly 1", n, racers)
	}
}

func TestVoiceTicketRedeemRejectsTampered(t *testing.T) {
	store, _ := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())

	// Flip one byte of the DECODED token and re-encode it canonically, so the failure comes
	// from GCM authentication and not merely from the strict base64 decoder.
	raw, err := voiceTicketEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode freshly minted token: %v", err)
	}
	raw[len(raw)/2] ^= 0x01
	flipped := voiceTicketEncoding.EncodeToString(raw)
	if flipped == token {
		t.Fatalf("test bug: the flipped token equals the original")
	}

	if got, err := store.Redeem(flipped); err == nil {
		t.Fatalf("Redeem of a tampered token succeeded (%+v), want an error", got)
	} else if !errors.Is(err, errVoiceTicketInvalid) {
		t.Fatalf("tampered Redeem error = %v, want errVoiceTicketInvalid", err)
	}

	// A tampered attempt must consume nothing: the genuine token still redeems.
	if _, err := store.Redeem(token); err != nil {
		t.Fatalf("genuine token after a tampered attempt: %v", err)
	}
}

func TestVoiceTicketRedeemRejectsMalformedTokens(t *testing.T) {
	store, _ := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())

	for name, bad := range map[string]string{
		"empty":         "",
		"not base64":    "!!not base64!!",
		"too short":     "AAAA",
		"truncated":     token[:len(token)-4],
		"padded":        token + "=",
		"extra suffix":  token + "AAAA",
		"standard base": "a+b/c==",
	} {
		if got, err := store.Redeem(bad); err == nil {
			t.Fatalf("%s: Redeem succeeded (%+v), want an error", name, got)
		}
	}
}

func TestVoiceTicketRejectsATokenSealedByAnotherKey(t *testing.T) {
	minter, _ := newVoiceTicketTestStore()
	other, _ := newVoiceTicketTestStore()
	token := mustMintVoiceTicket(t, minter, sampleVoiceTicketClaims())

	if got, err := other.Redeem(token); err == nil {
		t.Fatalf("a store redeemed a ticket sealed with another store's key: %+v", got)
	}
}

func TestVoiceTicketMintIgnoresCallerNonceAndExp(t *testing.T) {
	store, now := newVoiceTicketTestStore()
	claims := sampleVoiceTicketClaims()
	claims.Nonce = "caller-chosen"
	claims.Exp = now.Add(24 * time.Hour)

	got, err := store.Redeem(mustMintVoiceTicket(t, store, claims))
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.Nonce == "caller-chosen" {
		t.Fatalf("Mint kept the caller-supplied Nonce")
	}
	if want := now.Add(voiceTicketTTL); !got.Exp.Equal(want) {
		t.Fatalf("Exp = %v, want the store's now+TTL = %v, not the caller's", got.Exp, want)
	}
}

func TestVoiceTicketTokensAreUniqueAndWebSocketProtocolSafe(t *testing.T) {
	store, _ := newVoiceTicketTestStore()
	claims := sampleVoiceTicketClaims()

	// The token rides the Sec-WebSocket-Protocol header, whose values are RFC 7230 tokens:
	// no padding '=', no '+' or '/', no space or comma.
	protocolSafe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	first := mustMintVoiceTicket(t, store, claims)
	second := mustMintVoiceTicket(t, store, claims)
	if first == second {
		t.Fatalf("two mints of identical claims produced the same token")
	}
	for _, token := range []string{first, second} {
		if !protocolSafe.MatchString(token) {
			t.Fatalf("token %q is not a valid Sec-WebSocket-Protocol value", token)
		}
	}
}

// redeemedLen is the size of the redeemed-nonce set, for the pruning test.
func (s *voiceTicketStore) redeemedLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.redeemed)
}

func TestVoiceTicketRedeemedSetIsPrunedOnceTicketsExpire(t *testing.T) {
	store, now := newVoiceTicketTestStore()

	for range 5 {
		if _, err := store.Redeem(mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())); err != nil {
			t.Fatalf("Redeem: %v", err)
		}
	}
	if n := store.redeemedLen(); n != 5 {
		t.Fatalf("redeemed set = %d, want 5 before any ticket expires", n)
	}

	// Once every earlier ticket is past its Exp, the next redeem sweeps their nonces: a
	// nonce only has to outlive the ticket it guards, so the set does not grow forever.
	*now = now.Add(2 * voiceTicketTTL)
	if _, err := store.Redeem(mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())); err != nil {
		t.Fatalf("Redeem after the earlier tickets expired: %v", err)
	}
	if n := store.redeemedLen(); n != 1 {
		t.Fatalf("redeemed set = %d, want 1 (only the live ticket's nonce) after the sweep", n)
	}
}
