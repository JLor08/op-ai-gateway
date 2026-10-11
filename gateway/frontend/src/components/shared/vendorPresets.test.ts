// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  defaultBaseUrl,
  isOpenAICompatibleVendor,
  OPENAI_COMPATIBLE_VENDORS,
} from './vendorPresets';
import { isValidBaseUrl } from './vendorInputs';

describe('OPENAI_COMPATIBLE_VENDORS', () => {
  it('lists the five OpenAI-compatible vendors, in dropdown order', () => {
    expect([...OPENAI_COMPATIBLE_VENDORS]).toEqual([
      'xai',
      'openrouter',
      'kilo',
      'google',
      'openai_compatible',
    ]);
  });
});

describe('isOpenAICompatibleVendor', () => {
  it.each(OPENAI_COMPATIBLE_VENDORS)('is true for %s', (vendor) => {
    expect(isOpenAICompatibleVendor(vendor)).toBe(true);
  });

  it.each(['openai', 'anthropic', '', 'mystery'])('is false for %j', (vendor) => {
    expect(isOpenAICompatibleVendor(vendor)).toBe(false);
  });
});

describe('defaultBaseUrl', () => {
  it.each([
    ['xai', 'https://api.x.ai'],
    ['openrouter', 'https://openrouter.ai/api'],
    ['kilo', 'https://api.kilo.ai/api'],
    ['google', 'https://generativelanguage.googleapis.com'],
    ['openai_compatible', 'https://llm.example.com'],
  ])('shows %s as %s', (vendor, root) => {
    expect(defaultBaseUrl(vendor)).toBe(root);
    // Every placeholder is itself a root the validator accepts.
    expect(isValidBaseUrl(root)).toBe(true);
  });

  it('is empty for a vendor with a fixed host', () => {
    expect(defaultBaseUrl('openai')).toBe('');
    expect(defaultBaseUrl('anthropic')).toBe('');
    expect(defaultBaseUrl('mystery')).toBe('');
  });
});
