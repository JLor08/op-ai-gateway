// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
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
			APIToken:      "enc:sealed-upstream-credential",
			APIFlavors:    []string{"native"},
			ExtraHeaders:  map[string]string{"x-test": "1"},
		},
		SampleRate: 16000,
		Channels:   1,
		Format:     "s16le",
		Language:   "de",
		UserID:     "user-1",
		TokenID:    "token-1",
		// The billing attribution the live usage row carries; it must survive the
		// round trip like every other claim.
		ServiceID:   "svc-1",
		ServiceName: "Nightly Batch",
		ProjectID:   "proj-1",
		ProjectName: "Call Notes",
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
	// RFC 6455 wants a subprotocol to be an HTTP token: no '+', '/', '=', space
	// or comma. The URL-safe unpadded alphabet is a strict subset of that.
	shape := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for range 50 {
		token := mustMintLiveTicket(t, store, sampleLiveTicketClaims())
		if !shape.MatchString(token) {
			t.Fatalf("token %q is not in the URL-safe unpadded base64 alphabet", token)
		}
	}
}

// TestLiveTicketClaimsAreNotReadableFromTheToken pins the confidentiality half
// of the AEAD: the browser holds the token, so nothing in the claims (the
// upstream endpoint, the sealed upstream credential, the model, the user) may
// be recoverable by base64-decoding it.
func TestLiveTicketClaimsAreNotReadableFromTheToken(t *testing.T) {
	store, _ := newTicketTestStore()
	claims := sampleLiveTicketClaims()
	claims.Target.Endpoint = "http://secret-upstream:9000"
	claims.Target.Model = "secret-model-name"
	claims.Target.APIToken = "enc:secret-sealed-credential"
	claims.Target.APITokenHeader = "X-Secret-Header"
	claims.Target.ExtraHeaders = map[string]string{"x-secret-extra": "secret-extra-value"}
	claims.UserID = "secret-user-id"
	claims.TokenID = "secret-token-id"
	claims.ServiceID = "secret-service-id"
	claims.ServiceName = "secret-service-name"
	claims.ProjectID = "secret-project-id"
	claims.ProjectName = "secret-project-name"
	secrets := []string{
		"secret-upstream", "9000", "secret-model-name", "secret-sealed-credential",
		"X-Secret-Header", "secret-extra", "secret-user-id", "secret-token-id",
		"secret-service-id", "secret-service-name", "secret-project-id", "secret-project-name",
		"Endpoint", "APIToken", "Nonce", "audiocpp",
	}

	for range 20 {
		token := mustMintLiveTicket(t, store, claims)
		// The token itself, as the browser sees it...
		for _, secret := range secrets {
			if strings.Contains(token, secret) {
				t.Fatalf("token contains readable %q", secret)
			}
		}
		// ...and after the base64 decoding anyone can apply.
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("token is not base64url: %v", err)
		}
		for _, secret := range secrets {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("base64-decoded token contains readable %q", secret)
			}
		}
		if json.Valid(raw) {
			t.Fatalf("base64-decoded token is plain JSON; the claims are not encrypted")
		}

		// Confidentiality must not cost the round trip.
		got, ok := store.Redeem(token)
		if !ok {
			t.Fatalf("Redeem = false, want true")
		}
		if got.Target.Endpoint != claims.Target.Endpoint || got.Target.APIToken != claims.Target.APIToken {
			t.Fatalf("claims did not round-trip: %+v", got.Target)
		}
		if got.ServiceID != claims.ServiceID || got.ServiceName != claims.ServiceName || got.ProjectID != claims.ProjectID || got.ProjectName != claims.ProjectName {
			t.Fatalf("attribution did not round-trip: %+v", got)
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

	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}
	other := mustMintLiveTicket(t, store, sampleLiveTicketClaims())

	// Valid, readable claims that simply were never encrypted by the store.
	forgedClaims := sampleLiveTicketClaims()
	forgedClaims.Target.Endpoint = "http://evil.example:1"
	forgedClaims.Nonce = "forged"
	forgedClaims.Exp = time.Unix(1_800_000_000, 0).Add(time.Hour).Unix()
	forgedJSON, err := json.Marshal(forgedClaims)
	if err != nil {
		t.Fatalf("marshal forged claims: %v", err)
	}

	mid := len(token) / 2
	cases := map[string]string{
		// 0 is inside the GCM nonce, mid is ciphertext, the last characters
		// are the auth tag.
		"flipped nonce byte":          flip(token, 0),
		"flipped ciphertext byte":     flip(token, mid),
		"flipped tag byte":            flip(token, len(token)-5),
		"flipped last byte":           flip(token, len(token)-1),
		"truncated":                   token[:len(token)-4],
		"extended":                    token + "AAAA",
		"truncated to the GCM nonce":  token[:16],
		"shorter than the GCM nonce":  token[:8],
		"empty":                       "",
		"first half of two tickets":   token[:mid] + other[mid:],
		"unencrypted forged claims":   base64.RawURLEncoding.EncodeToString(forgedJSON),
		"legacy payload.mac shape":    base64.RawURLEncoding.EncodeToString(forgedJSON) + "." + token[:43],
		"trailing separator":          token + ".",
		"standard-alphabet re-encode": strings.NewReplacer("-", "+", "_", "/").Replace(token) + "=",
	}
	for name, tampered := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := store.Redeem(tampered); ok {
				t.Fatalf("Redeem(tampered) = true (%+v), want false", got)
			}
		})
	}

	// None of the rejected attempts may have consumed the genuine tickets.
	if _, ok := store.Redeem(token); !ok {
		t.Fatalf("genuine ticket no longer redeemable after tamper attempts")
	}
	if _, ok := store.Redeem(other); !ok {
		t.Fatalf("second genuine ticket no longer redeemable after tamper attempts")
	}
}

func TestLiveTicketRejectsNonCanonicalEncoding(t *testing.T) {
	store, _ := newTicketTestStore()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	// When the token's byte length is not a multiple of 3 its last base64
	// character carries 2 or 4 padding bits. Swapping in a sibling that shares
	// the data bits but not the padding bits decodes to the SAME bytes under a
	// lenient decoder, so it would give one ticket several spellings. The
	// Language length steers the byte length through all three residues.
	checked := 0
	for pad := range 3 {
		claims := sampleLiveTicketClaims()
		claims.Language = strings.Repeat("x", pad)
		token := mustMintLiveTicket(t, store, claims)
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("token is not base64url: %v", err)
		}
		var padBits uint
		switch len(raw) % 3 {
		case 1:
			padBits = 4
		case 2:
			padBits = 2
		default:
			continue // no padding bits, so no alternative spelling exists
		}
		mask := 1<<padBits - 1
		last := strings.IndexByte(alphabet, token[len(token)-1])
		if last < 0 {
			t.Fatalf("token %q does not end in a URL-safe base64 character", token)
		}
		for sibling := last &^ mask; sibling < last&^mask+mask+1; sibling++ {
			if sibling == last {
				continue
			}
			variant := token[:len(token)-1] + string(alphabet[sibling])
			lenient, err := base64.RawURLEncoding.DecodeString(variant)
			if err != nil || !bytes.Equal(lenient, raw) {
				t.Fatalf("test premise broken: a lenient decoder should read %q as the same bytes (err %v)", variant, err)
			}
			if got, ok := store.Redeem(variant); ok {
				t.Fatalf("non-canonical spelling accepted: %+v", got)
			}
			checked++
		}
		if _, ok := store.Redeem(token); !ok {
			t.Fatalf("canonical token rejected")
		}
	}
	if checked == 0 {
		t.Fatalf("no non-canonical spelling was exercised")
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
		strings.Repeat("A", 28), // exactly nonce + tag, an empty ciphertext of zeros
		strings.Repeat("A", 128),
		strings.Repeat("_", 64),
	} {
		if got, ok := store.Redeem(token); ok {
			t.Fatalf("Redeem(%.40q) = true (%+v), want false", token, got)
		}
	}
}

func TestLiveTicketRejectsAuthenticTicketWithMalformedClaims(t *testing.T) {
	store, clock := newTicketTestStore()
	// seal is the store's own authenticated encryption, so each token below is
	// genuinely the store's output; only what it carries is wrong.
	seal := func(plaintext []byte) string {
		token, err := store.seal(plaintext)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		return token
	}
	sealClaims := func(c liveTicketClaims) string {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return seal(raw)
	}

	good := sampleLiveTicketClaims()
	good.Nonce = "n-1"
	good.Exp = clock.cur.Add(liveTicketTTL).Unix()

	noNonce := good
	noNonce.Nonce = ""

	expired := good
	expired.Exp = clock.cur.Unix() - 1

	// Sanity: the helper really does produce a redeemable token, so the
	// rejections below are about the claims, not the sealing.
	if _, ok := store.Redeem(sealClaims(good)); !ok {
		t.Fatalf("hand-sealed well-formed ticket was rejected")
	}

	cases := map[string]string{
		"not json":        seal([]byte("this is not json")),
		"json array":      seal([]byte("[1,2,3]")),
		"empty plaintext": seal(nil),
		"no nonce":        sealClaims(noNonce),
		"already past":    sealClaims(expired),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := store.Redeem(token); ok {
				t.Fatalf("Redeem = true (%+v), want false", got)
			}
		})
	}
}

func TestLiveTicketSealedByDifferentStoreKeyIsRejected(t *testing.T) {
	issuer, _ := newTicketTestStore()
	verifier, _ := newTicketTestStore()
	token := mustMintLiveTicket(t, issuer, sampleLiveTicketClaims())

	if got, ok := verifier.Redeem(token); ok {
		t.Fatalf("a store redeemed a ticket sealed with another store's key: %+v", got)
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
