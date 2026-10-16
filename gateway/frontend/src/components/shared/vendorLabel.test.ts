// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { messages, type Locale } from '../../i18n';
import { authTypeLabel, vendorLabel } from './vendorLabel';

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`vendorLabel [${locale}]`, () => {
    it.each([
      ['openai', t.vendorOpenAI],
      ['anthropic', t.vendorAnthropic],
      ['xai', t.vendorXAI],
      ['openrouter', t.vendorOpenRouter],
      ['kilo', t.vendorKilo],
      ['google', t.vendorGoogle],
      ['google_native', t.vendorGoogleNative],
      // The provider kind vendor_gemini, derived to its segment by vendorServerLabel.
      ['gemini', t.vendorGoogleNative],
      ['openai_compatible', t.vendorOpenAICompatible],
    ])('labels %s with its localized name', (vendor, label) => {
      expect(vendorLabel(t, vendor)).toBe(label);
      expect(label).not.toBe(vendor);
      expect(label.length).toBeGreaterThan(0);
    });

    it('shows an unknown vendor as sent', () => {
      expect(vendorLabel(t, 'mystery')).toBe('mystery');
      expect(vendorLabel(t, '')).toBe('');
    });
  });

  describe(`authTypeLabel [${locale}]`, () => {
    it.each([
      ['api_key', t.vendorAuthApiKey],
      ['subscription', t.vendorAuthSubscription],
    ])('labels %s with its localized name', (authType, label) => {
      expect(authTypeLabel(t, authType)).toBe(label);
      expect(label).not.toBe(authType);
    });

    it('shows an unknown authentication type as sent', () => {
      expect(authTypeLabel(t, 'oauth2')).toBe('oauth2');
    });
  });
}
