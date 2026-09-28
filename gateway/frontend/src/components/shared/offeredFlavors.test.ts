// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { offeredFlavors } from './offeredFlavors';

describe('offeredFlavors', () => {
  it('keeps every flavor of a row without openai_images', () => {
    expect(offeredFlavors({ flavors: ['openai', 'anthropic'] })).toEqual(['openai', 'anthropic']);
  });

  it('keeps openai_images when the row has an image verdict', () => {
    expect(offeredFlavors({ flavors: ['openai', 'openai_images'], image: true })).toEqual([
      'openai',
      'openai_images',
    ]);
    expect(offeredFlavors({ flavors: ['openai_images'], image: true })).toEqual(['openai_images']);
  });

  it('drops openai_images when the row has no image verdict', () => {
    expect(offeredFlavors({ flavors: ['openai', 'openai_images'], image: false })).toEqual([
      'openai',
    ]);
    expect(offeredFlavors({ flavors: ['anthropic', 'openai_images'] })).toEqual(['anthropic']);
    expect(offeredFlavors({ flavors: ['openai_images'], image: false })).toEqual([]);
  });

  it('keeps the listing order of the remaining flavors', () => {
    expect(offeredFlavors({ flavors: ['anthropic', 'openai_images', 'openai'] })).toEqual([
      'anthropic',
      'openai',
    ]);
  });

  it('offers nothing for a row with no flavors, whatever its image flag', () => {
    expect(offeredFlavors({ flavors: [] })).toEqual([]);
    expect(offeredFlavors({ flavors: [], image: true })).toEqual([]);
  });
});
