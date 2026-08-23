// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Well-known Let's Encrypt ACME directory URLs, offered as the "Production"/
// "Staging" dropdown choices; anything else is "Custom URL" (a free-text field).
// Shared by CertificateSettings' own (internal-certificate) ACME dropdown and
// AcmeConfigFields (the edge/public per-context ACME sub-component) so all
// three pickers agree on what counts as "Production"/"Staging" vs "Custom".
export const ACME_DIRECTORY_PRODUCTION = 'https://acme-v02.api.letsencrypt.org/directory';
export const ACME_DIRECTORY_STAGING = 'https://acme-staging-v02.api.letsencrypt.org/directory';

export type AcmeDirectoryChoice = 'production' | 'staging' | 'custom';

export function acmeDirectoryChoiceFor(url: string): AcmeDirectoryChoice {
  if (url === ACME_DIRECTORY_PRODUCTION) return 'production';
  if (url === ACME_DIRECTORY_STAGING) return 'staging';
  return 'custom';
}

// Let's Encrypt's real-world weekly issuance ceilings for the two well-known
// directories. Round-1 review fix: these must be the SOLE source of the
// weekly limit whenever the directory is predefined -- the backend's real
// "unset" default for cert_edge_acme_weekly_limit/cert_public_acme_weekly_limit
// is 0 (nonNegativeIntSetting), which a naive `?? 50` fallback does not catch
// (0 is not nullish), so a fresh/untouched Production selection used to
// DISPLAY "50" while actually PERSISTING 0. See fixedAcmeWeeklyLimitFor below.
export const ACME_WEEKLY_LIMIT_PRODUCTION = 50;
export const ACME_WEEKLY_LIMIT_STAGING = 0;

// The fixed weekly-limit ceiling for a PREDEFINED directory (production ⇒ 50,
// staging ⇒ 0/unlimited) -- null for "custom", where the operator's own
// stored/edited value applies instead. Every caller that derives the
// EFFECTIVE weekly limit (the one number that is both displayed AND sent on
// save) must go through this function for the predefined case, so a
// predefined directory selection can never persist a number that contradicts
// what AcmeConfigFields displays for it.
export function fixedAcmeWeeklyLimitFor(choice: AcmeDirectoryChoice): number | null {
  if (choice === 'production') return ACME_WEEKLY_LIMIT_PRODUCTION;
  if (choice === 'staging') return ACME_WEEKLY_LIMIT_STAGING;
  return null;
}
