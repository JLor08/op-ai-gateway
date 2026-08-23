// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

export type CurrencyUnit = 'eur' | 'eur_cent' | 'usd' | 'usd_cent';
export const CURRENCY_UNITS: CurrencyUnit[] = ['eur', 'eur_cent', 'usd', 'usd_cent'];

// factor F = USD per 1 EUR. F <= 0 ⇒ USD unavailable.
export function availableUnits(factor: number): CurrencyUnit[] {
  return factor > 0 ? CURRENCY_UNITS : ['eur', 'eur_cent'];
}

const SUFFIX: Record<CurrencyUnit, string> = {
  eur: '€',
  eur_cent: 'ct',
  usd: '$',
  usd_cent: 'US-ct',
};

export function fromEur(eur: number, unit: CurrencyUnit, factor: number): number {
  switch (unit) {
    case 'eur':
      return eur;
    case 'eur_cent':
      return eur * 100;
    case 'usd':
      return eur * factor;
    case 'usd_cent':
      return eur * factor * 100;
  }
}

// Inverse: an entered value in `unit` → canonical EUR. F<=0 for a USD unit ⇒ 0 (never NaN/Inf).
export function toEur(value: number, unit: CurrencyUnit, factor: number): number {
  switch (unit) {
    case 'eur':
      return value;
    case 'eur_cent':
      return value / 100;
    case 'usd':
      return factor > 0 ? value / factor : 0;
    case 'usd_cent':
      return factor > 0 ? value / 100 / factor : 0;
  }
}

// 4 decimals for every unit; symbol prefix for currency, suffix for cent. 0/undefined/non-finite → em dash.
export function formatCost(eur: number | undefined, unit: CurrencyUnit, factor: number): string {
  if (!eur || !Number.isFinite(eur)) return '—';
  const v = fromEur(eur, unit, factor).toFixed(4);
  if (unit === 'eur') return `€ ${v}`;
  if (unit === 'usd') return `$ ${v}`;
  return `${v} ${SUFFIX[unit]}`;
}
