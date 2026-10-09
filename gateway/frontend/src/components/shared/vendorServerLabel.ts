// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './types';
import { vendorLabel } from './vendorLabel';

// The usage-row fields that decide the Server cell. UsageEvent satisfies this;
// naming just these keeps the helper (and its spec) off the ~40-field row type.
export type ServerLabelRow = {
  server_name: string;
  host: string;
  provider: string;
  // Present on a vendor-account row. account_name is resolved by the backend only
  // for the account's owner or an elevated system admin (and not for a deleted
  // account), so "account_id present, account_name empty" is a normal state.
  account_id?: string;
  account_name?: string;
};

export type ServerLabel = {
  text: string;
  // The full account id, set only when `text` shows the shortened id (no resolved
  // name), so the cell can expose the whole id as a tooltip.
  title?: string;
};

// The provider kind a vendor-account row carries (internal/routing: vendor_openai,
// vendor_openai_subscription, vendor_anthropic). The vendor is the segment after
// the prefix, so the subscription variant maps to the same vendor as the api-key
// one and a future vendor_<kind> needs no change here.
const VENDOR_PROVIDER_PREFIX = 'vendor_';

function vendorOf(provider: string): string | null {
  if (!provider.startsWith(VENDOR_PROVIDER_PREFIX)) return null;
  const vendor = provider.slice(VENDOR_PROVIDER_PREFIX.length).split('_')[0];
  return vendor === '' ? null : vendor;
}

// Vendor account ids are "va_" + 32 hex characters; the first 8 after the prefix
// identify one well enough to tell a few accounts apart. The cut keeps the type
// prefix (so it still reads as a vendor-account id) and is marked with an
// ellipsis; an id that already fits is shown whole.
const SHORT_ID_LENGTH = 11;

export function shortAccountId(id: string | undefined): string {
  if (!id) return '';
  return id.length <= SHORT_ID_LENGTH ? id : `${id.slice(0, SHORT_ID_LENGTH)}…`;
}

/**
 * The Server-cell text of an Activity row. A self-hosted row shows its
 * server_name (or host); a vendor-account row has neither, so it shows
 * "<Vendor> · <account name>", falling back to the short account id (full id as
 * `title`) when the name is not resolved for this viewer. Anything else is "".
 */
export function serverLabel(t: Translation, row: ServerLabelRow): ServerLabel {
  const own = row.server_name || row.host;
  if (own) return { text: own };

  const vendor = vendorOf(row.provider);
  if (vendor === null) return { text: '' };

  const vendorText = vendorLabel(t, vendor);
  const name = row.account_name?.trim();
  if (name) return { text: `${vendorText} · ${name}` };

  const shortId = shortAccountId(row.account_id);
  if (!shortId) return { text: vendorText };
  return { text: `${vendorText} · ${shortId}`, title: row.account_id };
}
