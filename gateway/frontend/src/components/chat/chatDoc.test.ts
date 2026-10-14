// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_IMAGE_SETTINGS,
  DEFAULTS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  MASK_PURPOSE,
  imageSetting,
  imageSettingsFrom,
  isMaskPart,
  kindSetting,
  lastUserHasImage,
  maskPart,
  metricsOf,
  normalizeDoc,
  optionsOr,
  userTurnContent,
  type ChatUiMessage,
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
        ...DEFAULT_IMAGE_SETTINGS,
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

  it('sends the advanced knobs, parsing the blank-able numerics to wire numbers', () => {
    const image = imageSetting('image', {
      ...DEFAULT_IMAGE_SETTINGS,
      negative_prompt: 'blurry',
      sampler: 'euler_a',
      scheduler: 'karras',
      steps: '28',
      cfg: '7.5',
      seed: '0', // 0 is a real seed, not "unset"
      clip_skip: '2',
    }).image;
    expect(image).toMatchObject({
      negative_prompt: 'blurry',
      sampler: 'euler_a',
      scheduler: 'karras',
      steps: 28,
      cfg: 7.5,
      seed: 0,
      clip_skip: 2,
    });
  });

  it('omits a blank or unparseable advanced numeric', () => {
    const image =
      imageSetting('image', {
        ...DEFAULT_IMAGE_SETTINGS,
        steps: '',
        cfg: 'abc',
        seed: '   ',
        strength: '   ',
      }).image ?? {};
    expect('steps' in image).toBe(false);
    expect('cfg' in image).toBe(false);
    expect('seed' in image).toBe(false);
    expect('strength' in image).toBe(false);
  });

  // strength is the edit knob: blank leaves sd-server's own default (0.75), a
  // value is sent as a wire number (the gateway puts it in the <sd_cpp_extra_args>
  // block), and 0 is a real value, not "unset".
  describe('strength', () => {
    it('is blank by default, so a fresh image thread sends none', () => {
      expect(DEFAULT_IMAGE_SETTINGS.strength).toBe('');
      expect('strength' in (imageSetting('image', DEFAULT_IMAGE_SETTINGS).image ?? {})).toBe(false);
    });

    it('is sent as a number when set, 0 included', () => {
      expect(
        imageSetting('image', { ...DEFAULT_IMAGE_SETTINGS, strength: '0.9' }).image?.strength,
      ).toBe(0.9);
      expect(
        imageSetting('image', { ...DEFAULT_IMAGE_SETTINGS, strength: '0' }).image?.strength,
      ).toBe(0);
    });

    it('is never put on a text thread', () => {
      expect(imageSetting(undefined, { ...DEFAULT_IMAGE_SETTINGS, strength: '0.9' })).toEqual({});
    });

    it('round-trips through imageSettingsFrom (number -> blank-able string)', () => {
      expect(imageSettingsFrom({ strength: 0.9 }).strength).toBe('0.9');
      expect(imageSettingsFrom({ strength: 0 }).strength).toBe('0');
      expect(imageSettingsFrom(undefined).strength).toBe('');
      expect(imageSettingsFrom({ strength: 'x' as unknown as number }).strength).toBe('');
    });

    it('survives a save/load of the document (normalizeDoc)', () => {
      const saved = {
        settings: { model: 'm', kind: 'image', image: { strength: 0.6 } },
        messages: [],
      };
      expect(imageSettingsFrom(normalizeDoc(saved).settings.image).strength).toBe('0.6');
    });
  });

  it('round-trips advanced knobs through imageSettingsFrom (numbers -> blank-able strings)', () => {
    expect(imageSettingsFrom({ sampler: 'euler_a', steps: 28, cfg: 7.5, seed: 0 })).toMatchObject({
      sampler: 'euler_a',
      steps: '28',
      cfg: '7.5',
      seed: '0',
    });
    // An absent numeric comes back as a blank string, never "undefined"/"NaN".
    expect(imageSettingsFrom(undefined).steps).toBe('');
    expect(imageSettingsFrom(undefined).negative_prompt).toBe('');
  });
});

// The per-model sd-server options endpoint omits every field it does not know
// (omitempty on the wire), so "absent" and "empty" both mean "unknown" and the
// dropdowns fall back to the Phase 1b-i static superset.
describe('optionsOr', () => {
  it('uses the fetched list when it is non-empty', () => {
    expect(optionsOr(['euler_a'], IMAGE_SAMPLERS)).toEqual(['euler_a']);
    expect(optionsOr(['karras', 'simple'], IMAGE_SCHEDULERS)).toEqual(['karras', 'simple']);
  });

  it('falls back to the static list when the fetched list is empty', () => {
    expect(optionsOr([], IMAGE_SAMPLERS)).toBe(IMAGE_SAMPLERS);
  });

  it('falls back to the static list when the fetched list is absent', () => {
    expect(optionsOr(undefined, IMAGE_SAMPLERS)).toBe(IMAGE_SAMPLERS);
    expect(optionsOr(undefined, IMAGE_SCHEDULERS)).toBe(IMAGE_SCHEDULERS);
  });
});

// Masked inpainting (ADR-043 R1). The mask of an edit is stored as an ordinary
// image part of the user message that carries a TAG -- {"purpose":"mask"} beside
// image_url -- so it is stored, resolved and owner-checked as an image like the
// source is, and the gateway's executor (portal.MessageImages / ImagePurposeMask)
// can send it as the `mask` form field instead of as a second source. The key and
// the value are the wire contract with that reader.
describe('the mask part', () => {
  const src = 'data:image/png;base64,SRC';
  const mask = 'data:image/png;base64,MASK';
  const textPart = (text: string) => ({ type: 'text' as const, text });
  const imagePart = (url: string) => ({ type: 'image_url' as const, image_url: { url } });
  const user = (content: ChatUiMessage['content'], id = 'u'): ChatUiMessage => ({
    id,
    role: 'user',
    content,
  });

  it('is an image_url part tagged with purpose "mask", the way the executor reads it', () => {
    expect(maskPart(mask)).toEqual({
      type: 'image_url',
      image_url: { url: mask },
      purpose: 'mask',
    });
    expect(MASK_PURPOSE).toBe('mask');
  });

  it('is told from a source by the tag alone', () => {
    expect(isMaskPart(maskPart(mask))).toBe(true);
    expect(isMaskPart(imagePart(src))).toBe(false);
    expect(isMaskPart(textPart('hi'))).toBe(false);
  });

  describe('lastUserHasImage', () => {
    it('counts a source image of the last user message', () => {
      expect(lastUserHasImage([user([textPart('x'), imagePart(src)])])).toBe(true);
    });

    it('counts a source that comes with a mask', () => {
      expect(lastUserHasImage([user([textPart('x'), imagePart(src), maskPart(mask)])])).toBe(true);
    });

    it('does not take a mask for a source: a message with a mask alone is no edit', () => {
      expect(lastUserHasImage([user([textPart('x'), maskPart(mask)])])).toBe(false);
    });

    it("reads only the last user message: an earlier turn's source and mask make no edit of this one", () => {
      const earlier = user([textPart('old'), imagePart(src), maskPart(mask)], 'u1');
      const later = user('plain', 'u2');
      expect(lastUserHasImage([earlier, later])).toBe(false);
    });
  });

  describe('userTurnContent', () => {
    it('is the plain text of a turn with no image', () => {
      expect(userTurnContent('a cat', [], null, 'edit')).toBe('a cat');
    });

    it('is the text then the source for an edit with no mask, with nothing else on the part', () => {
      const content = userTurnContent('make it blue', [src], null, 'edit');
      expect(content).toEqual([textPart('make it blue'), imagePart(src)]);
      // The no-mask edit is what it was before masks: the source part has no tag key.
      expect(Object.keys((content as { type: string }[])[1]).sort()).toEqual(['image_url', 'type']);
    });

    it('is the text, the source and then the tagged mask for a masked edit', () => {
      expect(userTurnContent('fill the sky', [src], mask, 'edit')).toEqual([
        textPart('fill the sky'),
        imagePart(src),
        maskPart(mask),
      ]);
    });

    it('never carries a mask without a source image', () => {
      expect(userTurnContent('x', [], mask, 'edit')).toBe('x');
    });

    it('never carries a mask outside an edit: a text model reading an image has no use for it', () => {
      expect(userTurnContent('x', [src], mask, 'vision')).toEqual([textPart('x'), imagePart(src)]);
    });

    it('keeps several images of a vision turn as sources', () => {
      expect(userTurnContent('x', [src, src], null, 'vision')).toEqual([
        textPart('x'),
        imagePart(src),
        imagePart(src),
      ]);
    });
  });
});
