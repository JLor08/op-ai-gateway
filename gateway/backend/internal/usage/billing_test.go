// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"errors"
	"testing"
)

func TestValidBillingUnit(t *testing.T) {
	for _, tc := range []struct {
		unit string
		want bool
	}{
		{BillingUnitTokens, true},
		{BillingUnitImage, true},
		{BillingUnitAudioSecond, true},
		{"images", false},
		{"IMAGE", false},
		{"character", false}, // no producer in this system: audio.cpp meters duration, not characters
		{"audio_minute", false},
	} {
		if got := ValidBillingUnit(tc.unit); got != tc.want {
			t.Errorf("ValidBillingUnit(%q) = %v, want %v", tc.unit, got, tc.want)
		}
	}
}

func TestValidateBillingXORAcceptsTokenMeteredAndNonToken(t *testing.T) {
	tokenMetered := Event{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
		CachedTokens: 1, CacheWriteTokens: 2,
		PromptPerSecond: 5, TokensPerSecond: 6,
	}
	if err := ValidateBillingXOR(tokenMetered); err != nil {
		t.Errorf("token-metered event rejected: %v", err)
	}

	nonToken := Event{BillingUnit: BillingUnitImage, BillingQuantity: 3}
	if err := ValidateBillingXOR(nonToken); err != nil {
		t.Errorf("non-token event rejected: %v", err)
	}
}

func TestValidateBillingXORRejectsAllSevenTokenColumns(t *testing.T) {
	// The five token counts PLUS the two per-second rates. The rates are NOT
	// among "the five token columns", and ComputeHistogram drops zeros by
	// design, so a stray non-zero rate on a non-token row would silently enter
	// the speed histograms with nothing to fail.
	for name, mutate := range map[string]func(*Event){
		"input_tokens":       func(e *Event) { e.InputTokens = 1 },
		"output_tokens":      func(e *Event) { e.OutputTokens = 1 },
		"total_tokens":       func(e *Event) { e.TotalTokens = 1 },
		"cached_tokens":      func(e *Event) { e.CachedTokens = 1 },
		"cache_write_tokens": func(e *Event) { e.CacheWriteTokens = 1 },
		"prompt_per_second":  func(e *Event) { e.PromptPerSecond = 0.1 },
		"tokens_per_second":  func(e *Event) { e.TokensPerSecond = 0.1 },
	} {
		ev := Event{BillingUnit: BillingUnitImage, BillingQuantity: 3}
		mutate(&ev)
		if err := ValidateBillingXOR(ev); !errors.Is(err, ErrBillingXORViolated) {
			t.Errorf("%s: err = %v, want ErrBillingXORViolated", name, err)
		}
	}
}

func TestValidateBillingXORRejectsUnknownUnitAndTokenMeteredQuantity(t *testing.T) {
	if err := ValidateBillingXOR(Event{BillingUnit: "images", BillingQuantity: 1}); !errors.Is(err, ErrBillingUnitUnknown) {
		t.Error("an unknown unit must be rejected, never clamped to \"\" -- \"\" is a positive assertion of token-metering")
	}
	if err := ValidateBillingXOR(Event{BillingQuantity: 5}); !errors.Is(err, ErrBillingXORViolated) {
		t.Error("a token-metered row with a quantity must be rejected")
	}
}
