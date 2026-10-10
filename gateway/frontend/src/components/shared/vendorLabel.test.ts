// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { messages, type Locale } from '../../i18n';
import { vendorLabel } from './vendorLabel';

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
}
