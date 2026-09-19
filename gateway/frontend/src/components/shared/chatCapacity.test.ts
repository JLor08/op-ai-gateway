// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { ESTIMATED_IMAGE_BYTES, documentBytes, imageCostBytes, imagesLeft } from './chatCapacity';

// Only `content` is modelled: the helper reads nothing else off a message
// (see CapacityMessage in chatCapacity.ts), and an inline literal carrying a
// `role` would trip TypeScript's excess-property check for no benefit.
const dataUrl = (chars: number) => `data:image/png;base64,${'A'.repeat(chars)}`;
const imageTurn = (chars: number) => ({
  content: [{ type: 'image_url', image_url: { url: dataUrl(chars) } }],
});

describe('documentBytes', () => {
  it('grows with the transcript it measures', () => {
    const empty = documentBytes([]);
    const one = documentBytes([imageTurn(1000)]);
    expect(one).toBeGreaterThan(empty + 1000);
  });
});

describe('imageCostBytes', () => {
  it('falls back to the estimate while the thread has produced no image', () => {
    expect(imageCostBytes([{ content: 'a cat' }])).toBe(ESTIMATED_IMAGE_BYTES);
  });

  it('uses the LARGEST image the thread has actually produced once it has one', () => {
    const cost = imageCostBytes([imageTurn(500), imageTurn(9000), imageTurn(200)]);
    expect(cost).toBeGreaterThanOrEqual(9000);
    expect(cost).toBeLessThan(9100);
  });
});

describe('imagesLeft', () => {
  it('is null when the server served no cap (never guess a number)', () => {
    expect(imagesLeft([], 0)).toBeNull();
    expect(imagesLeft([], -1)).toBeNull();
  });

  it('counts how many more images of the observed size fit', () => {
    // One 1000-char image stored, a cap with room for ~4 more of them.
    expect(imagesLeft([imageTurn(1000)], 6000)).toBe(4);
  });

  it('is 0 (not negative) once the remaining room is under one image', () => {
    expect(imagesLeft([imageTurn(1000)], 1200)).toBe(0);
    expect(imagesLeft([imageTurn(1000)], 100)).toBe(0);
  });

  it('uses the fallback estimate for a thread with no image yet', () => {
    expect(imagesLeft([], 4 * ESTIMATED_IMAGE_BYTES)).toBe(3);
  });
});
