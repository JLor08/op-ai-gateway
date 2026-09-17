// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The portal side of the usage_events (billing_unit, billing_quantity) pair.
//
// The doctrine this implements is the repo's own, inverted. telemetry-usage-
// observability.md states that a measured value must never be indistinguishable
// from a measured zero (implemented in formatLiveTps). Mixed billable units add
// the converse: a NOT-APPLICABLE must never be indistinguishable from a zero.
// A non-token row's five token columns are 0 because tokens do not apply to it,
// not because nothing was measured -- so they render as an em dash.

import type { MessageKey } from './shared/types';

/** A row is token-metered iff its billing_unit is empty or absent. */
export function isTokenMetered(row: { billing_unit?: string }): boolean {
  return !row.billing_unit;
}

// A CLASSIFICATION of the population, not rendered text. It carries no cell
// string on purpose: the not-applicable state's glyph and the two applicable
// states' number belong to the renderer, which is the only place that knows the
// surface's number format (TokenAggregateValue takes a `format`, and ProjectsView
// passes toLocaleString). A formatter-free `String(value)` in here would be a
// ready-looking field that silently drops a surface's thousands separators.
export type TokenAggregate = {
  /** True when the population mixes token-metered and non-token rows: the number is correct for the token-metered SUBSET only. */
  mixed: boolean;
  /** False when tokens do not apply to any row in the population. */
  applicable: boolean;
};

/**
 * The three-state rule for a token-denominated aggregate over a population that
 * may mix units. Every surface that shows one goes through it -- the grouped
 * table, the four Activity stat tiles, the Dashboard 24h-token tile and the
 * project-token rollups (rows and total) -- so all four agree. TokenAggregateValue
 * renders the result, including the tooltip each exceptional state needs.
 *
 * An empty population (totalRequests === 0) is APPLICABLE -- the number, not a
 * dash: "no rows at all" is not "tokens do not apply here".
 *
 * It takes only the two counts, because the classification does not depend on
 * the aggregate's own value; the caller renders that value itself.
 */
export function tokenAggregate(nonTokenRequests: number, totalRequests: number): TokenAggregate {
  if (totalRequests > 0 && nonTokenRequests >= totalRequests) {
    return { mixed: false, applicable: false };
  }
  return { mixed: nonTokenRequests > 0, applicable: true };
}

/**
 * The i18n keys the help tables below are allowed to name.
 *
 * Derived from MessageKey (the string-valued keys of Translation) by prefix, so
 * a missing or misspelled key is a BUILD error rather than a silently absent
 * tooltip: the literal drops out of the union and the table stops type-checking.
 * That keeps the `t[helpKey]` lookup at the call site cast-free -- an `as any`
 * or a non-null assertion there is exactly the naming trap theming-and-i18n.md
 * warns about, because it would let a typo ship.
 */
export type HelpMessageKey = Extract<
  MessageKey,
  `energySourceHelp${string}` | `billingUnitHelp${string}`
>;

// Tooltip copy for the two opaque wire enums this view renders as chips.
//
// The chip keeps showing the RAW wire value -- that is the portal's
// forward-compatibility convention for every opaque wire enum, and an
// unrecognised value from a newer backend must never be replaced by a
// misleading label. The tooltip is layered on top, and a value with no entry
// here gets NO tooltip: silence is honest, a wrong explanation is not.
//
// Both tables key on the FULL enum value. theming-and-i18n.md records the naming
// trap this invites: a table keyed on a truncated prefix compiles, type-checks,
// and shows a raw string to operators.
export const ENERGY_SOURCE_HELP_KEYS: Record<string, HelpMessageKey> = {
  measured: 'energySourceHelpMeasured',
  estimated: 'energySourceHelpEstimated',
  modeled: 'energySourceHelpModeled',
  unpriceable: 'energySourceHelpUnpriceable',
  '': 'energySourceHelpPending',
};

export const BILLING_UNIT_HELP_KEYS: Record<string, HelpMessageKey> = {
  image: 'billingUnitHelpImage',
  audio_second: 'billingUnitHelpAudioSecond',
};
