// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package certissue

import (
	"net/http"
	"testing"
	"time"
)

// jitterLower and jitterUpper mirror retryBackoffExceptRateLimit's own jitter
// band (time.Duration(mathrand.IntN(1000)+1) * time.Millisecond, i.e. [1ms,
// 1000ms] inclusive) -- and, not by coincidence, x/crypto/acme's own
// defaultBackoff jitter band, which this function is a deliberate
// reimplementation of everywhere except the 429 case (see acme.go's doc
// comment on retryBackoffExceptRateLimit).
const (
	jitterLower = time.Millisecond
	jitterUpper = 1000 * time.Millisecond
)

// nonRateLimitedResponse builds a *http.Response that is retriable (per
// x/crypto/acme's isRetriable: code<=399 || code>=500 || code==429) but is
// NOT a 429, with an optional Retry-After header. A nil Header map is a
// deliberate stand-in for "the CA answered without a Retry-After header at
// all" -- resp.Header.Get on a nil map returns "" just like an empty one, so
// http.Header{} vs nil are behaviorally identical for this function.
func nonRateLimitedResponse(retryAfter string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: http.StatusInternalServerError, Header: h}
}

// TestRetryBackoffExceptRateLimitRefusesOn429 pins the ENTIRE reason this
// override exists: a 429 rate-limit response must yield NO delay (0),
// regardless of the attempt number or of any Retry-After header the CA
// attaches (a 429 always carries one in practice -- see the doc comment) --
// x/crypto/acme's retryTimer.backoff treats a non-positive duration as "stop
// retrying, surface the last response's error" (http.go), which is exactly
// what lets Obtain fail fast on a rate limit instead of x/crypto/acme's own
// defaultBackoff, which would sleep for the CA's requested cool-down (often
// minutes) before giving up.
func TestRetryBackoffExceptRateLimitRefusesOn429(t *testing.T) {
	for _, n := range []int{0, -3, 1, 2, 5, 30, 1000} {
		for _, retryAfter := range []string{"", "120", "3600"} {
			h := http.Header{}
			if retryAfter != "" {
				h.Set("Retry-After", retryAfter)
			}
			resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: h}
			if d := retryBackoffExceptRateLimit(n, nil, resp); d != 0 {
				t.Fatalf("retryBackoffExceptRateLimit(%d, nil, 429 Retry-After=%q) = %s, want 0", n, retryAfter, d)
			}
		}
	}
}

// TestRetryBackoffExceptRateLimitExponentialGrowsAndCaps pins the truncated
// exponential shape (2^(n-1) seconds, doubling each attempt) for every
// NON-429 retriable response that carries no usable Retry-After header, and
// proves the attempt count is clamped so neither a zero/negative n nor an
// arbitrarily large one produces something absurd -- n<1 is treated as 1,
// n>30 is treated as 30 (mirroring x/crypto/acme's own defaultBackoff
// clamp), and the total (base+jitter) never exceeds the 10s cap.
func TestRetryBackoffExceptRateLimitExponentialGrowsAndCaps(t *testing.T) {
	// base(n) for n already clamped to [1,4]: doubles every attempt and stays
	// under the 10s cap even with the maximum possible jitter added, so these
	// four attempts are the only ones NOT forced to the exact cap.
	base := func(n int) time.Duration { return time.Duration(1<<uint(n-1)) * time.Second }

	t.Run("grows exponentially below the cap", func(t *testing.T) {
		const samplesPerAttempt = 40
		for n := 1; n <= 4; n++ {
			lo, hi := base(n)+jitterLower, base(n)+jitterUpper
			seen := map[time.Duration]bool{}
			for i := 0; i < samplesPerAttempt; i++ {
				d := retryBackoffExceptRateLimit(n, nil, nonRateLimitedResponse(""))
				if d < lo || d > hi {
					t.Fatalf("n=%d: retryBackoffExceptRateLimit = %s, want in [%s,%s]", n, d, lo, hi)
				}
				seen[d] = true
			}
			// The jitter must actually vary across calls -- an implementation
			// that dropped the random component (returning a fixed value inside
			// the band by coincidence) would still pass the bounds check above
			// but fail here. 40 samples across a >=1000-value band makes "all
			// identical by chance" astronomically unlikely.
			if len(seen) < 2 {
				t.Fatalf("n=%d: got the same delay across %d samples (%v); jitter looks deterministic", n, samplesPerAttempt, seen)
			}
		}
	})

	t.Run("attempt count is clamped so huge or non-positive n cannot overflow", func(t *testing.T) {
		// n<1 is clamped to 1: same band as an explicit n=1.
		for _, n := range []int{0, -1, -1000} {
			d := retryBackoffExceptRateLimit(n, nil, nonRateLimitedResponse(""))
			lo, hi := base(1)+jitterLower, base(1)+jitterUpper
			if d < lo || d > hi {
				t.Fatalf("n=%d (clamped to 1): retryBackoffExceptRateLimit = %s, want in [%s,%s]", n, d, lo, hi)
			}
		}
		// n>30 is clamped to 30 -- base(30) alone (2^29 seconds) already vastly
		// exceeds the 10s cap, so both n=30 and a huge n must land on EXACTLY
		// the cap, deterministically (no jitter can push it higher, and the cap
		// is a hard ceiling so jitter cannot be observed here either).
		for _, n := range []int{5, 6, 10, 30, 1000, 1 << 20} {
			for i := 0; i < 5; i++ {
				d := retryBackoffExceptRateLimit(n, nil, nonRateLimitedResponse(""))
				if d != 10*time.Second {
					t.Fatalf("n=%d: retryBackoffExceptRateLimit = %s, want exactly the 10s cap", n, d)
				}
			}
		}
	})

	t.Run("every non-429 delay is positive and never exceeds the cap", func(t *testing.T) {
		for n := -5; n <= 40; n++ {
			d := retryBackoffExceptRateLimit(n, nil, nonRateLimitedResponse(""))
			if d <= 0 {
				t.Fatalf("n=%d: retryBackoffExceptRateLimit = %s, want > 0", n, d)
			}
			if d > 10*time.Second {
				t.Fatalf("n=%d: retryBackoffExceptRateLimit = %s, want <= 10s", n, d)
			}
		}
	})
}

// TestRetryBackoffExceptRateLimitHonorsRetryAfterOnNonRateLimitedResponse
// pins the OTHER branch a 429 never reaches (retryBackoffExceptRateLimit
// checks the 429 short-circuit before it can even see a header): on any
// OTHER retriable status, a parseable Retry-After header wins over whatever
// the exponential curve for that attempt number would have produced -- in
// particular, a long CA-requested wait at a SMALL attempt number (where the
// exponential term alone would be short) still comes through as the longer,
// server-requested duration, never silently shortened to the exponential's
// value.
func TestRetryBackoffExceptRateLimitHonorsRetryAfterOnNonRateLimitedResponse(t *testing.T) {
	const n = 1 // exponential alone would give ~1s -- far shorter than 45s below.
	want := 45 * time.Second
	lo, hi := want+jitterLower, want+jitterUpper
	for i := 0; i < 10; i++ {
		d := retryBackoffExceptRateLimit(n, nil, nonRateLimitedResponse("45"))
		if d < lo || d > hi {
			t.Fatalf("retryBackoffExceptRateLimit(%d, nil, Retry-After=45s) = %s, want in [%s,%s] (Retry-After should win over the ~1s exponential value)", n, d, lo, hi)
		}
	}

	// An UNPARSEABLE Retry-After value must not silently produce a zero/garbage
	// delay -- it falls back to the exponential branch for that attempt number,
	// exactly as if no header had been sent at all.
	t.Run("unparseable header falls back to the exponential value", func(t *testing.T) {
		base := time.Duration(1<<uint(n-1)) * time.Second
		lo, hi := base+jitterLower, base+jitterUpper
		d := retryBackoffExceptRateLimit(n, nil, nonRateLimitedResponse("not-a-duration"))
		if d < lo || d > hi {
			t.Fatalf("retryBackoffExceptRateLimit with an unparseable Retry-After = %s, want in [%s,%s] (fall back to exponential)", d, lo, hi)
		}
	})
}
