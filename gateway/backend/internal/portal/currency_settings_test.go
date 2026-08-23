// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"testing"
)

// TestNormalizePriceUnit asserts every known unit round-trips unchanged and
// an unknown/empty unit falls back to eur_cent (the shared default).
func TestNormalizePriceUnit(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{UnitEUR, UnitEUR},
		{UnitEURCent, UnitEURCent},
		{UnitUSD, UnitUSD},
		{UnitUSDCent, UnitUSDCent},
		{"bogus", UnitEURCent},
		{"", UnitEURCent},
	}
	for _, c := range cases {
		if got := NormalizePriceUnit(c.in); got != c.want {
			t.Fatalf("NormalizePriceUnit(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUpdateSystemSettingsPersistsCurrencyFactorAndUnit round-trips the
// currency_usd_per_eur factor and the energy_default_price_unit through the
// settings PUT->GET, and asserts a nil update (no touched fields) keeps the
// previously-stored values.
func TestUpdateSystemSettingsPersistsCurrencyFactorAndUnit(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	dto, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CurrencyUsdPerEur:      floatPtr(1.1),
		EnergyDefaultPriceUnit: strPtr(UnitUSD),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if dto.CurrencyUsdPerEur != 1.1 {
		t.Fatalf("UpdateSystemSettings() CurrencyUsdPerEur = %v, want 1.1", dto.CurrencyUsdPerEur)
	}
	if dto.EnergyDefaultPriceUnit != UnitUSD {
		t.Fatalf("UpdateSystemSettings() EnergyDefaultPriceUnit = %q, want %q", dto.EnergyDefaultPriceUnit, UnitUSD)
	}

	got := svc.SystemSettingsView(ctx)
	if got.CurrencyUsdPerEur != 1.1 {
		t.Fatalf("SystemSettingsView() CurrencyUsdPerEur = %v, want 1.1", got.CurrencyUsdPerEur)
	}
	if got.EnergyDefaultPriceUnit != UnitUSD {
		t.Fatalf("SystemSettingsView() EnergyDefaultPriceUnit = %q, want %q", got.EnergyDefaultPriceUnit, UnitUSD)
	}

	// A nil update (no fields touched) must keep both values unchanged.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{}); err != nil {
		t.Fatalf("UpdateSystemSettings(nil update) returned error: %v", err)
	}
	kept := svc.SystemSettingsView(ctx)
	if kept.CurrencyUsdPerEur != 1.1 {
		t.Fatalf("after nil update CurrencyUsdPerEur = %v, want 1.1 (kept)", kept.CurrencyUsdPerEur)
	}
	if kept.EnergyDefaultPriceUnit != UnitUSD {
		t.Fatalf("after nil update EnergyDefaultPriceUnit = %q, want %q (kept)", kept.EnergyDefaultPriceUnit, UnitUSD)
	}
}

// TestUpdateSystemSettingsRejectsNegativeCurrencyFactor asserts a negative
// currency_usd_per_eur is rejected (ErrEnergyDefaultInvalid, the reused
// energy-default validation error) and never persisted.
func TestUpdateSystemSettingsRejectsNegativeCurrencyFactor(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	// Seed a known-good value first so we can prove the rejected write left
	// it untouched.
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CurrencyUsdPerEur: floatPtr(1.1)}); err != nil {
		t.Fatalf("seed UpdateSystemSettings returned error: %v", err)
	}

	_, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CurrencyUsdPerEur: floatPtr(-0.5)})
	if !errors.Is(err, ErrEnergyDefaultInvalid) {
		t.Fatalf("negative currency_usd_per_eur err = %v, want ErrEnergyDefaultInvalid", err)
	}

	// Not persisted: the prior value must still be there.
	got := svc.SystemSettingsView(ctx)
	if got.CurrencyUsdPerEur != 1.1 {
		t.Fatalf("after rejected write CurrencyUsdPerEur = %v, want 1.1 (unchanged)", got.CurrencyUsdPerEur)
	}
}

// TestCurrencyUsdPerEurReader exercises the ctx-carrying Service accessor
// directly: it returns the stored value, and 0 when unset or when the store
// somehow holds a negative value (defensive; negative is rejected at write
// time so this should not occur via the API).
func TestCurrencyUsdPerEurReader(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})
	ctx := context.Background()

	if got := svc.CurrencyUsdPerEur(ctx); got != 0 {
		t.Fatalf("CurrencyUsdPerEur() unset = %v, want 0", got)
	}

	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CurrencyUsdPerEur: floatPtr(1.1)}); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if got := svc.CurrencyUsdPerEur(ctx); got != 1.1 {
		t.Fatalf("CurrencyUsdPerEur() after update = %v, want 1.1", got)
	}

	// Defensive: a raw negative value written directly to the store (bypassing
	// UpdateSystemSettings' validation) must still read back as 0.
	if err := settings.SetSystemSetting(ctx, currencyUsdPerEurKey, "-2", fixedClock()()); err != nil {
		t.Fatalf("SetSystemSetting returned error: %v", err)
	}
	if got := svc.CurrencyUsdPerEur(ctx); got != 0 {
		t.Fatalf("CurrencyUsdPerEur() with stored negative = %v, want 0", got)
	}

	// Nil-safe: no settings store at all.
	nilSvc := NewService(ServiceDeps{Clock: fixedClock()})
	if got := nilSvc.CurrencyUsdPerEur(ctx); got != 0 {
		t.Fatalf("CurrencyUsdPerEur() with nil store = %v, want 0", got)
	}
}
