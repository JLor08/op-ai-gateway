// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { DEFAULTS, kindSetting, metricsOf, normalizeDoc } from './chatDoc';

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
// client side: normalizeDoc's output is PUT verbatim by renameChat for a
// non-active chat, so it is a writer of the stored document too.
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
