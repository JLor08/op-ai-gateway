// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_IMAGE_SETTINGS,
  DEFAULTS,
  imageSetting,
  imageSettingsFrom,
  kindSetting,
  metricsOf,
  normalizeDoc,
} from './chatDoc';

describe('metricsOf', () => {
  it('maps the snake_case SSE metrics onto the camelCase message fields', () => {
    expect(
      metricsOf({ ttft_ms: 120, reasoning_ms: 300, tps: 42.5, tokens_per_second: 12.5 }),
    ).toEqual({
      ttftMs: 120,
      reasoningMs: 300,
      tps: 42.5,
      tokensPerSecond: 12.5,
    });
  });

  it('drops zero and absent values so the summary hides them until meaningful', () => {
    // tokens/sec is absent mid-turn (the exact count only arrives with the
    // upstream usage chunk) and must not surface as a spurious 0 (issue #56).
    expect(metricsOf({ ttft_ms: 120, tps: 42.5 })).toEqual({ ttftMs: 120, tps: 42.5 });
    expect(metricsOf({ tokens_per_second: 0 })).toEqual({});
    expect(metricsOf(undefined)).toEqual({});
  });
});

// The backend declares ChatRunSettings.Kind last with `json:"kind,omitempty"`
// so a text thread's persisted settings stay byte-identical to what they were
// before image threads existed, and it has a test asserting the key never
// appears for one (service_chats_kind_test.go). These mirror that from the
// client side: normalizeDoc seeds the state that buildDoc writes back, so a
// kind key it invents or drops is invented or dropped on the server too, by
// the next save.
describe('the persisted kind mirrors the backend omitempty', () => {
  it('omits the key entirely for an empty or absent kind', () => {
    expect(kindSetting(undefined)).toEqual({});
    expect(kindSetting('')).toEqual({});
    expect(Object.keys(kindSetting(''))).toHaveLength(0);
  });

  it('carries a non-empty kind verbatim, including one this build does not know', () => {
    expect(kindSetting('image')).toEqual({ kind: 'image' });
    expect(kindSetting('video')).toEqual({ kind: 'video' });
  });

  it('never puts a kind key on a text document normalizeDoc rebuilds', () => {
    const settings = normalizeDoc({ settings: { model: 'm' }, messages: [] }).settings;
    expect('kind' in settings).toBe(false);
    const cleared = normalizeDoc({ settings: { model: 'm', kind: '' }, messages: [] }).settings;
    expect('kind' in cleared).toBe(false);
  });

  it('round-trips a pinned kind, which is what stops the autosave erasing it', () => {
    expect(normalizeDoc({ settings: { kind: 'image' }, messages: [] }).settings.kind).toBe('image');
    // Not narrowed to the kinds this build knows: an unfamiliar one survives
    // instead of being silently reset to text by the next save.
    expect(normalizeDoc({ settings: { kind: 'video' }, messages: [] }).settings.kind).toBe('video');
    // A mistyped kind is not a kind.
    expect('kind' in normalizeDoc({ settings: { kind: 7 }, messages: [] }).settings).toBe(false);
  });

  it('carries no kind key on the defaults a fresh chat is created with', () => {
    expect('kind' in DEFAULTS).toBe(false);
  });
});

// Image settings follow the same omitempty discipline: present only on an image
// thread, and never on the wire/persisted document of a text thread. Only the
// set knobs are sent, and output_compression rides only a lossy format.
describe('the persisted image settings mirror the backend omitempty', () => {
  it('omits the image key for a non-image thread', () => {
    expect(imageSetting('', DEFAULT_IMAGE_SETTINGS)).toEqual({});
    expect(imageSetting(undefined, DEFAULT_IMAGE_SETTINGS)).toEqual({});
    expect(imageSetting('video', { ...DEFAULT_IMAGE_SETTINGS, size: '512x512' })).toEqual({});
  });

  it('sends only the set knobs for an image thread', () => {
    // All defaults: only n (1) is non-empty, so only n is sent.
    expect(imageSetting('image', DEFAULT_IMAGE_SETTINGS)).toEqual({ image: { n: 1 } });
    expect(
      imageSetting('image', {
        size: '1024x1024',
        n: 3,
        output_format: 'png',
        output_compression: 80,
      }),
    ).toEqual({ image: { size: '1024x1024', n: 3, output_format: 'png' } });
  });

  it('sends output_compression only for a lossy format moved off the default', () => {
    // Never for png (ignored upstream).
    const png = imageSetting('image', {
      ...DEFAULT_IMAGE_SETTINGS,
      output_format: 'png',
      output_compression: 55,
    });
    expect('output_compression' in (png.image ?? {})).toBe(false);
    for (const format of ['jpeg', 'webp']) {
      // A moved-off-default value is sent...
      const moved = imageSetting('image', {
        ...DEFAULT_IMAGE_SETTINGS,
        output_format: format,
        output_compression: 55,
      });
      expect(moved.image?.output_compression).toBe(55);
      // ...but an untouched slider (the default) is omitted, like size/format.
      const untouched = imageSetting('image', { ...DEFAULT_IMAGE_SETTINGS, output_format: format });
      expect('output_compression' in (untouched.image ?? {})).toBe(false);
    }
  });

  it('round-trips image settings through normalizeDoc for an image thread', () => {
    // png is non-lossy, so no output_compression rides along (that case has its
    // own test above); this pins the core round-trip that stops the autosave
    // erasing the knobs.
    const settings = normalizeDoc({
      settings: { kind: 'image', image: { size: '768x768', n: 4, output_format: 'png' } },
      messages: [],
    }).settings;
    expect(settings.image).toEqual({ size: '768x768', n: 4, output_format: 'png' });
  });

  it('never puts an image key on a text document normalizeDoc rebuilds', () => {
    // Even a (malformed) client that stored image settings on a text thread must
    // not have them re-emitted -- imageSetting keys off the kind.
    const settings = normalizeDoc({
      settings: { model: 'm', image: { size: '512x512' } },
      messages: [],
    }).settings;
    expect('image' in settings).toBe(false);
  });

  it('coerces a missing/mistyped persisted image back to defaults', () => {
    expect(imageSettingsFrom(undefined)).toEqual(DEFAULT_IMAGE_SETTINGS);
    expect(imageSettingsFrom({ n: 'x' as unknown as number })).toEqual(DEFAULT_IMAGE_SETTINGS);
    expect(imageSettingsFrom({ size: '512x512' })).toEqual({
      ...DEFAULT_IMAGE_SETTINGS,
      size: '512x512',
    });
  });

  it('carries no image key on the defaults a fresh chat is created with', () => {
    expect('image' in DEFAULTS).toBe(false);
  });
});
