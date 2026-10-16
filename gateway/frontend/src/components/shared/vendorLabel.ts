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
    case 'xai':
      return t.vendorXAI;
    case 'openrouter':
      return t.vendorOpenRouter;
    case 'kilo':
      return t.vendorKilo;
    case 'google':
      return t.vendorGoogle;
    // The usage row's provider kind vendor_gemini is derived to the gemini segment.
    case 'google_native':
    case 'gemini':
      return t.vendorGoogleNative;
    case 'openai_compatible':
      return t.vendorOpenAICompatible;
    default:
      return vendor;
  }
}

/** The display name of a vendor-account authentication type; an unknown one is shown as sent. */
export function authTypeLabel(t: Translation, authType: string): string {
  switch (authType) {
    case 'api_key':
      return t.vendorAuthApiKey;
    case 'subscription':
      return t.vendorAuthSubscription;
    default:
      return authType;
  }
}
