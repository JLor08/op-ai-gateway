// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './types';

/** The display name of a vendor-account vendor; an unknown vendor is shown as sent. */
export function vendorLabel(t: Translation, vendor: string): string {
  switch (vendor) {
    case 'openai':
      return t.vendorOpenAI;
    case 'anthropic':
      return t.vendorAnthropic;
    default:
      return vendor;
  }
}
