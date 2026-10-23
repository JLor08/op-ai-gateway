// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/base64"
	"encoding/json"
	"op-ai-gateway/internal/routing"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ticketTestClock is a hand-driven clock so expiry tests never sleep. The
// tests advance it between store calls, never concurrently with them.
type ticketTestClock struct{ cur time.Time }

func (c *ticketTestClock) now() time.Time { return c.cur }

// redeemedLen is the size of the redeemed-nonce set, for the pruning test.
func (s *liveTicketStore) redeemedLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.redeemed)
}

// newTicketTestStore returns a store whose clock is the returned fake, pinned
// to a whole second so the Exp boundary math below is exact.
func newTicketTestStore() (*liveTicketStore, *ticketTestClock) {
	clock := &ticketTestClock{cur: time.Unix(1_800_000_000, 0)}
	store := newLiveTicketStore()
	store.now = clock.now
	return store, clock
}

func sampleLiveTicketClaims() liveTicketClaims {
	return liveTicketClaims{
		Target: routing.Target{
			RouteID:       "route-1",
			ServerID:      "server-1",
			Provider:      "audiocpp",
			Endpoint:      "http://audio.internal:9000",
			Model:         "whisper-live",
			ProviderModel: "whisper-large-v3",
			Timeout:       90 * time.Second,
			APIFlavor:     "native",
			APIFlavors:    []string{"native"},
			ExtraHeaders:  map[string]string{"x-test": "1"},
		},
		SampleRate: 16000,
		Channels:   1,
		Format:     "s16le",
		Language:   "de",
		UserID:     "user-1",
		TokenID:    "token-1",
	}
}

func mustMintLiveTicket(t *testing.T, store *liveTicketStore, claims liveTicketClaims) string {
	t.Helper()
	token, err := store.Mint(claims)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return token
}

func TestLiveTicketMintRedeemRoundTrip(t *testing.T) {
	store, clock := newTicketTestStore()
	minted := sampleLiveTicketClaims()

	token := mustMintLiveTicket(t, store, minted)

	got, ok := store.Redeem(token)
	if !ok {
		t.Fatalf("Redeem of a fresh ticket = false, want true")
	}
	if got.Nonce == "" {
		t.Fatalf("redeemed Nonce is empty, want the random nonce Mint set")
	}
	if want := clock.cur.Add(liveTicketTTL).Unix(); got.Exp != want {
		t.Fatalf("redeemed Exp = %d, want now+liveTicketTTL = %d", got.Exp, want)
	}
	// Nonce and Exp are Mint's to set; every other field must round-trip
	// exactly, Target and the audio params included.
	minted.Nonce, minted.Exp = got.Nonce, got.Exp
	if !reflect.DeepEqual(got, minted) {
		t.Fatalf("redeemed claims differ from minted:\n got  %+v\n want %+v", got, minted)
	}
}

func TestLiveTicketMintIgnoresCallerNonceAndExp(t *testing.T) {
	store, clock := newTicketTestStore()
	claims := sampleLiveTicketClaims()
	claims.Nonce = "caller-chosen"
	claims.Exp = clock.cur.Add(24 * time.Hour).Unix()

	got, ok := store.Redeem(mustMintLiveTicket(t, store, claims))
	if !ok {
		t.Fatalf("Redeem = false, want true")
	}
	if got.Nonce == "caller-chosen" {
		t.Fatalf("Mint kept the caller-supplied Nonce")
	}
	if want := clock.cur.Add(liveTicketTTL).Unix(); got.Exp != want {
		t.Fatalf("Exp = %d, want the store's now+TTL = %d, not the caller's", got.Exp, want)
	}
}

func TestLiveTicketTokenIsAValidSubprotocolToken(t *testing.T) {
	store, _ := newTicketTestStore()
	shape := regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	for range 50 {
		token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
		if !shape.MatchString(token) {
			t.Fatalf("token %q is not payload.mac in the URL-safe unpadded alphabet", token)
		}
	}
}

func TestLiveTicketMintsDistinctTokensForEqualClaims(t *testing.T) {
	store, _ := newTicketTestStore()
	a := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
	b := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
	if a == b {
		t.Fatalf("two Mints of equal claims produced the same token; the nonce is not fresh")
	}
}

func TestLiveTicketSingleUse(t *testing.T) {
	store, _ := newTicketTestStore()
	token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())

	if _, ok := store.Redeem(token); !ok {
		t.Fatalf("first Redeem = false, want true")
	}
	if got, ok := store.Redeem(token); ok {
		t.Fatalf("second Redeem = true (%+v), want false: a ticket is single-use", got)
	}
	if _, ok := store.Redeem(token); ok {
		t.Fatalf("third Redeem = true, want false")
	}
}

func TestLiveTicketExpired(t *testing.T) {
	store, clock := newTicketTestStore()
	token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())

	clock.cur = clock.cur.Add(liveTicketTTL + time.Second)
	if got, ok := store.Redeem(token); ok {
		t.Fatalf("Redeem after the TTL = true (%+v), want false", got)
	}
}

func TestLiveTicketRedeemableUpToAndIncludingExp(t *testing.T) {
	store, clock := newTicketTestStore()
	token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())

	// now > Exp rejects, so the Exp second itself is still valid.
	clock.cur = clock.cur.Add(liveTicketTTL)
	if _, ok := store.Redeem(token); !ok {
		t.Fatalf("Redeem at exactly Exp = false, want true")
	}
}

func TestLiveTicketExpiredTicketIsNotRedeemableEvenWhenNeverUsed(t *testing.T) {
	store, clock := newTicketTestStore()
	stale := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
	clock.cur = clock.cur.Add(10 * liveTicketTTL)
	fresh := mustMintLiveTicket(t, store, sampleLiveTicketClaims())

	if _, ok := store.Redeem(stale); ok {
		t.Fatalf("stale ticket redeemed")
	}
	if _, ok := store.Redeem(fresh); !ok {
		t.Fatalf("fresh ticket not redeemed after a stale one was rejected")
	}
}

func TestLiveTicketTampered(t *testing.T) {
	store, _ := newTicketTestStore()
	token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
	payload, mac, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token %q has no '.' separator", token)
	}

	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}

	otherPayload, _, _ := strings.Cut(mustMintLiveTicket(t, store, sampleLiveTicketClaims()), ".")

	// A different, validly-encoded payload under the original MAC.
	forgedClaims := sampleLiveTicketClaims()
	forgedClaims.Target.Endpoint = "http://evil.example:1"
	forgedClaims.Nonce = "forged"
	forgedClaims.Exp = time.Unix(1_800_000_000, 0).Add(time.Hour).Unix()
	forgedJSON, err := json.Marshal(forgedClaims)
	if err != nil {
		t.Fatalf("marshal forged claims: %v", err)
	}
	forgedPayload := base64.RawURLEncoding.EncodeToString(forgedJSON)

	cases := map[string]string{
		"flipped payload byte":         flip(payload, len(payload)/2) + "." + mac,
		"flipped first payload byte":   flip(payload, 0) + "." + mac,
		"flipped last payload byte":    flip(payload, len(payload)-1) + "." + mac,
		"flipped mac byte":             payload + "." + flip(mac, len(mac)/2),
		"flipped first mac byte":       payload + "." + flip(mac, 0),
		"flipped last mac byte":        payload + "." + flip(mac, len(mac)-1),
		"other ticket's payload":       otherPayload + "." + mac,
		"forged payload, original mac": forgedPayload + "." + mac,
		"truncated mac":                payload + "." + mac[:len(mac)-2],
		"extended mac":                 payload + "." + mac + "AA",
		"empty mac":                    payload + ".",
		"empty payload":                "." + mac,
		"swapped halves":               mac + "." + payload,
		"third segment":                token + ".extra",
	}
	for name, tampered := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := store.Redeem(tampered); ok {
				t.Fatalf("Redeem(tampered) = true (%+v), want false", got)
			}
		})
	}

	// None of the rejected attempts may have consumed the genuine ticket.
	if _, ok := store.Redeem(token); !ok {
		t.Fatalf("genuine ticket no longer redeemable after tamper attempts")
	}
}

func TestLiveTicketRejectsNonCanonicalMACEncoding(t *testing.T) {
	store, _ := newTicketTestStore()
	token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
	payload, mac, _ := strings.Cut(token, ".")

	// A 32-byte MAC is 43 base64 characters whose last one carries 4 data bits
	// and 2 padding bits. Swapping in a sibling that shares the data bits but
	// not the padding bits decodes to the SAME bytes under a lenient decoder,
	// so it would give one ticket several spellings. It must be refused.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, mac[len(mac)-1])
	if last < 0 {
		t.Fatalf("MAC %q does not end in a URL-safe base64 character", mac)
	}
	for sibling := last &^ 3; sibling < last&^3+4; sibling++ {
		if sibling == last {
			continue
		}
		variant := payload + "." + mac[:len(mac)-1] + string(alphabet[sibling])
		if _, err := base64.RawURLEncoding.DecodeString(variant[len(payload)+1:]); err != nil {
			t.Fatalf("test premise broken: a lenient decoder should accept %q: %v", variant, err)
		}
		if got, ok := store.Redeem(variant); ok {
			t.Fatalf("non-canonical MAC spelling accepted: %+v", got)
		}
	}
	if _, ok := store.Redeem(token); !ok {
		t.Fatalf("canonical token rejected")
	}
}

func TestLiveTicketGarbageTokens(t *testing.T) {
	store, _ := newTicketTestStore()
	for _, token := range []string{
		"",
		".",
		"..",
		"garbage",
		"a.b",
		"a.b.c",
		"!!!.???",
		"+++.///",
		"aGVsbG8=.aGVsbG8=",
		strings.Repeat("A", 4096),
		strings.Repeat("A", 2048) + "." + strings.Repeat("A", 2048),
		"Bearer abc.def",
		"live-ticket.abc",
	} {
		if got, ok := store.Redeem(token); ok {
			t.Fatalf("Redeem(%.40q) = true (%+v), want false", token, got)
		}
	}
}

func TestLiveTicketRejectsValidMACOverMalformedClaims(t *testing.T) {
	store, clock := newTicketTestStore()
	sign := func(payload string) string { return payload + "." + store.sign(payload) }
	encode := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	claimsJSON := func(c liveTicketClaims) string {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return encode(string(raw))
	}

	good := sampleLiveTicketClaims()
	good.Nonce = "n-1"
	good.Exp = clock.cur.Add(liveTicketTTL).Unix()

	noNonce := good
	noNonce.Nonce = ""

	expired := good
	expired.Exp = clock.cur.Unix() - 1

	// Sanity: the helper really does produce a redeemable token, so the
	// rejections below are about the claims, not the signing.
	if _, ok := store.Redeem(sign(claimsJSON(good))); !ok {
		t.Fatalf("hand-signed well-formed ticket was rejected")
	}

	cases := map[string]string{
		"not json":      sign(encode("this is not json")),
		"json array":    sign(encode("[1,2,3]")),
		"empty payload": sign(""),
		"no nonce":      sign(claimsJSON(noNonce)),
		"already past":  sign(claimsJSON(expired)),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := store.Redeem(token); ok {
				t.Fatalf("Redeem = true (%+v), want false", got)
			}
		})
	}
}

func TestLiveTicketSignedByDifferentStoreKeyIsRejected(t *testing.T) {
	issuer, _ := newTicketTestStore()
	verifier, _ := newTicketTestStore()
	token := mustMintLiveTicket(t, issuer, sampleLiveTicketClaims())

	if got, ok := verifier.Redeem(token); ok {
		t.Fatalf("a store redeemed a ticket signed with another store's key: %+v", got)
	}
	// The rejection must not have burned the nonce on the issuing store.
	if _, ok := issuer.Redeem(token); !ok {
		t.Fatalf("issuing store rejected its own ticket after a foreign store refused it")
	}
}

func TestLiveTicketConcurrentRedeemAdmitsExactlyOne(t *testing.T) {
	const (
		tickets   = 40
		redeemers = 16
	)
	store := newLiveTicketStore()

	for i := range tickets {
		token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())

		var (
			admitted atomic.Int32
			ready    sync.WaitGroup
			done     sync.WaitGroup
		)
		start := make(chan struct{})
		ready.Add(redeemers)
		done.Add(redeemers)
		for range redeemers {
			go func() {
				defer done.Done()
				ready.Done()
				<-start
				if _, ok := store.Redeem(token); ok {
					admitted.Add(1)
				}
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()

		if got := admitted.Load(); got != 1 {
			t.Fatalf("ticket %d: %d concurrent redeemers admitted, want exactly 1", i, got)
		}
	}
}

func TestLiveTicketRedeemedSetIsPrunedOnceTicketsExpire(t *testing.T) {
	store, clock := newTicketTestStore()
	for range 20 {
		if _, ok := store.Redeem(mustMintLiveTicket(t, store, sampleLiveTicketClaims())); !ok {
			t.Fatalf("Redeem = false, want true")
		}
	}
	if got := store.redeemedLen(); got != 20 {
		t.Fatalf("redeemed set holds %d nonces before expiry, want 20 (a live nonce must be remembered)", got)
	}

	clock.cur = clock.cur.Add(liveTicketTTL + time.Second)
	if _, ok := store.Redeem(mustMintLiveTicket(t, store, sampleLiveTicketClaims())); !ok {
		t.Fatalf("Redeem of a fresh ticket = false, want true")
	}
	if got := store.redeemedLen(); got != 1 {
		t.Fatalf("redeemed set holds %d nonces after the old tickets expired, want 1 (only the fresh one)", got)
	}
}
