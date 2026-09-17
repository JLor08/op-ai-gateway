// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"errors"
	"fmt"
)

// The billable-unit vocabulary. An Event's measure is an XOR: BillingUnitTokens
// ("") means the five token columns are the measure, and any other value means
// BillingQuantity is the measure while every token-denominated column is 0.
//
// Two values, not three. "character" is deliberately absent: it comes from
// OpenAI's price list, not from this system -- audio.cpp's metering signal is an
// audio DURATION, so speech and transcription share BillingUnitAudioSecond.
const (
	// BillingUnitTokens is the historical default and a POSITIVE assertion that
	// the row is token-metered. It is never inferred and never defaulted TO from
	// an unknown value.
	BillingUnitTokens      = ""
	BillingUnitImage       = "image"
	BillingUnitAudioSecond = "audio_second"
)

var (
	// ErrBillingUnitUnknown reports a unit outside the vocabulary. Note that this
	// is an ERROR and not a clamp: unlike NormalizeSort's harmless default,
	// clamping to "" would assert token-metering about a request that is not
	// token-metered, which is the exact lie the (unit, quantity) pair exists to
	// prevent.
	ErrBillingUnitUnknown = errors.New("usage: unknown billing unit")
	// ErrBillingXORViolated reports a row carrying both a measure and a
	// contradicting one.
	ErrBillingXORViolated = errors.New("usage: billing unit/quantity XOR violated")
)

// ValidBillingUnit reports whether unit is in the vocabulary (including the
// token-metered ""). Producers use the constants above; this is for validating
// data that crossed a boundary.
func ValidBillingUnit(unit string) bool {
	switch unit {
	case BillingUnitTokens, BillingUnitImage, BillingUnitAudioSecond:
		return true
	default:
		return false
	}
}

// ValidateBillingXOR reports whether ev's measure is self-consistent.
//
// A token-metered row (BillingUnit == "") must carry no BillingQuantity. A
// non-token row must carry 0 in all SEVEN token-denominated columns: the five
// token counts plus PromptPerSecond and TokensPerSecond. The two rates are
// included deliberately -- they are not "token columns" in the narrow sense, but
// ComputeHistogram drops zeros by design, so a stray non-zero rate on a
// non-token row would enter the speed histograms with nothing to fail.
//
// This is the enforcement half of the contract. Without it, the energy engine's
// unit guard only picks a different wrong answer for a violating row instead of
// catching the violation.
func ValidateBillingXOR(ev Event) error {
	if !ValidBillingUnit(ev.BillingUnit) {
		return fmt.Errorf("%w: %q", ErrBillingUnitUnknown, ev.BillingUnit)
	}
	if ev.BillingUnit == BillingUnitTokens {
		if ev.BillingQuantity != 0 {
			return fmt.Errorf("%w: token-metered row carries billing_quantity %v", ErrBillingXORViolated, ev.BillingQuantity)
		}
		return nil
	}
	tokenColumns := []struct {
		name string
		zero bool
	}{
		{"input_tokens", ev.InputTokens == 0},
		{"output_tokens", ev.OutputTokens == 0},
		{"total_tokens", ev.TotalTokens == 0},
		{"cached_tokens", ev.CachedTokens == 0},
		{"cache_write_tokens", ev.CacheWriteTokens == 0},
		{"prompt_per_second", ev.PromptPerSecond == 0},
		{"tokens_per_second", ev.TokensPerSecond == 0},
	}
	for _, col := range tokenColumns {
		if !col.zero {
			return fmt.Errorf("%w: unit %q row carries non-zero %s", ErrBillingXORViolated, ev.BillingUnit, col.name)
		}
	}
	return nil
}
