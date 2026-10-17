// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_AUDIO_SETTINGS,
  DEFAULT_IMAGE_SETTINGS,
  DEFAULTS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  MASK_PURPOSE,
  attachModeOf,
  audioPassthroughMessage,
  audioSetting,
  audioSettingsFrom,
  imageSetting,
  imageSettingsFrom,
  isChatBlobUrl,
  isMaskPart,
  kindSetting,
  lastUserHasImage,
  maskPart,
  metricsOf,
  normalizeDoc,
  optionsOr,
  parseAudioPassthrough,
  speechPassthroughInvalid,
  userTurnContent,
  type ChatUiMessage,
} from './chatDoc';
import { audioUrlsOf, imageUrlsOf } from '../shared/chatContent';

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

  // The backend reads the tag the way this side does (the key exactly, the last of a
  // repeated one wins -- portal.MessageImages / isMaskPart, pinned there by
  // TestMessageImages), so a crafted part is a mask to the executor exactly when it is
  // one to the bubble that shows it.
  it('reads a repeated or re-cased tag the way the backend does: exact key, last one wins', () => {
    const part = (members: string) =>
      JSON.parse(`{"type":"image_url","image_url":{"url":"u"},${members}}`);
    expect(isMaskPart(part('"purpose":"mask","purpose":"reference"'))).toBe(false);
    expect(isMaskPart(part('"purpose":"reference","purpose":"mask"'))).toBe(true);
    expect(isMaskPart(part('"Purpose":"mask"'))).toBe(false);
    expect(isMaskPart(part('"purpose":"mask","purpose":3'))).toBe(false);
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

// What a refine hands the composer is one of a generated turn's image urls, kept as
// the reference it is: the list is every image part but the edit's mask, and a stored
// image's url is told by its serving path.
describe('the images of a message', () => {
  const ref = '/api/portal/chat-blobs/blob_abc';
  const textPart = (text: string) => ({ type: 'text' as const, text });
  const imagePart = (url: string) => ({ type: 'image_url' as const, image_url: { url } });

  it('lists the urls of the image parts in order, text and the mask left out', () => {
    expect(
      imageUrlsOf([
        textPart('x'),
        imagePart(ref),
        imagePart('data:image/png;base64,SRC'),
        maskPart('data:image/png;base64,MASK'),
      ]),
    ).toEqual([ref, 'data:image/png;base64,SRC']);
  });

  it('is empty for a text-only message, whatever its shape', () => {
    expect(imageUrlsOf('plain')).toEqual([]);
    expect(imageUrlsOf([textPart('x')])).toEqual([]);
    expect(imageUrlsOf([])).toEqual([]);
  });

  it('keeps a stored reference exactly as it is: no copy, no rewrite', () => {
    expect(imageUrlsOf([imagePart(ref)])[0]).toBe(ref);
  });

  it('tells a chat-bound stored image from bytes that go wherever the composer goes', () => {
    expect(isChatBlobUrl(ref)).toBe(true);
    expect(isChatBlobUrl('data:image/png;base64,AAAA')).toBe(false);
    expect(isChatBlobUrl('https://example.test/api/portal/chat-blobs/blob_abc')).toBe(false);
    expect(isChatBlobUrl('')).toBe(false);
  });
});

// Audio (text-to-speech) settings follow the image settings' discipline exactly:
// present only on a "speech" thread, never on the wire/persisted document of a
// text or image thread, and carrying only what the user moved off the default.
describe('the persisted audio settings mirror the backend omitempty', () => {
  it('omits the audio key for any non-speech thread', () => {
    const moved = { ...DEFAULT_AUDIO_SETTINGS, voice: 'af_heart', speed: '1.2' };
    expect(audioSetting('', moved)).toEqual({});
    expect(audioSetting(undefined, moved)).toEqual({});
    expect(audioSetting('image', moved)).toEqual({});
    expect(audioSetting('video', moved)).toEqual({});
  });

  it('has blank defaults: a fresh speech thread leaves the model on its own voice and speed', () => {
    expect(DEFAULT_AUDIO_SETTINGS).toEqual({ voice: '', speed: '', passthrough: '' });
    // The key is present for a speech thread (as image's is for an image one),
    // but carries nothing: every knob stays the model's default.
    expect(audioSetting('speech', DEFAULT_AUDIO_SETTINGS)).toEqual({ audio: {} });
  });

  it('sends voice and speed when set, speed parsed to a wire number', () => {
    expect(
      audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, voice: 'af_heart', speed: '1.25' }),
    ).toEqual({ audio: { voice: 'af_heart', speed: 1.25 } });
  });

  it('trims the voice and sends nothing for a whitespace-only one', () => {
    expect(audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, voice: '  af_heart ' })).toEqual({
      audio: { voice: 'af_heart' },
    });
    expect(audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, voice: '   ' })).toEqual({
      audio: {},
    });
  });

  it('omits a blank or unparseable speed, so the model keeps its own', () => {
    for (const speed of ['', '   ', 'fast', 'NaN', 'Infinity']) {
      const audio = audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, speed }).audio ?? {};
      expect('speed' in audio).toBe(false);
    }
  });

  it('sends the passthrough as the JSON object the user wrote, value types intact', () => {
    const passthrough = JSON.stringify({
      instructions: 'speak softly',
      seed: 7,
      phonemes: true,
      nested: { a: [1, 2] },
    });
    expect(audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, passthrough }).audio).toEqual({
      passthrough: { instructions: 'speak softly', seed: 7, phonemes: true, nested: { a: [1, 2] } },
    });
  });

  it('omits a passthrough that is blank, empty, malformed or not an object', () => {
    for (const passthrough of ['', '   ', '{}', '{"a":', 'null', '[1]', '"text"', '7', 'a=b']) {
      const audio = audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, passthrough }).audio ?? {};
      expect('passthrough' in audio).toBe(false);
    }
  });

  it('classifies a passthrough text for the composer: empty, valid or why it is not', () => {
    expect(parseAudioPassthrough('')).toEqual({ status: 'empty' });
    expect(parseAudioPassthrough('  \n ')).toEqual({ status: 'empty' });
    expect(parseAudioPassthrough('{}')).toEqual({ status: 'empty' });
    expect(parseAudioPassthrough('{"seed": 1}')).toEqual({ status: 'ok', value: { seed: 1 } });
    expect(parseAudioPassthrough('{"seed":')).toEqual({ status: 'invalid' });
    expect(parseAudioPassthrough('seed=1')).toEqual({ status: 'invalid' });
    expect(parseAudioPassthrough('[1, 2]')).toEqual({ status: 'invalid' });
    expect(parseAudioPassthrough('null')).toEqual({ status: 'invalid' });
    expect(parseAudioPassthrough('"text"')).toEqual({ status: 'invalid' });
  });

  // The client mirror of the backend's speechReservedFields: the run owns these fields,
  // and the backend would end the run gateway.chat_run_speech_reserved_field only after
  // the turn is committed. Matched ignoring case, like the backend.
  it('rejects a passthrough naming a reserved field, in any letter case, and names it', () => {
    for (const key of ['model', 'input', 'stream', 'response_format']) {
      expect(parseAudioPassthrough(JSON.stringify({ [key]: 'x' }))).toEqual({
        status: 'invalid',
        reservedField: key,
      });
      const shouted = key.toUpperCase();
      expect(parseAudioPassthrough(JSON.stringify({ [shouted]: 1 }))).toEqual({
        status: 'invalid',
        reservedField: shouted, // as the user wrote it
      });
    }
    expect(parseAudioPassthrough('{"Model": "x"}')).toEqual({
      status: 'invalid',
      reservedField: 'Model',
    });
    // A reserved key among others still poisons the whole object (nothing is dropped
    // in silence), and the first in sorted order is the one named, like the backend.
    expect(parseAudioPassthrough('{"seed": 1, "stream": true, "input": "x"}')).toEqual({
      status: 'invalid',
      reservedField: 'input',
    });
  });

  it('does not reserve its look-alikes: stream_format, voice, speed, a prefix or a nested key', () => {
    for (const text of [
      '{"stream_format": "sse"}',
      '{"voice": "af"}',
      '{"speed": 1.2}',
      '{"models": 1, "inputs": 2, "response": 3, "format": 4}',
      '{"nested": {"model": "x", "stream": true}}',
    ]) {
      expect(parseAudioPassthrough(text).status, text).toBe('ok');
    }
  });

  it('picks the message for an invalid passthrough: the reserved one names the field', () => {
    const t = {
      chatAudioPassthroughInvalid: 'not an object',
      chatSpeechPassthroughReserved: (field: string) => `reserved ${field}`,
    };
    expect(audioPassthroughMessage(parseAudioPassthrough('[1]'), t)).toBe('not an object');
    expect(audioPassthroughMessage(parseAudioPassthrough('{"seed":'), t)).toBe('not an object');
    expect(audioPassthroughMessage(parseAudioPassthrough('{"model": 1}'), t)).toBe(
      'reserved model',
    );
  });

  it('refuses a run only for a speech thread whose passthrough is not a JSON object', () => {
    const bad = { ...DEFAULT_AUDIO_SETTINGS, passthrough: '{"seed":' };
    expect(speechPassthroughInvalid('speech', bad)).toBe(true);
    expect(speechPassthroughInvalid('speech', { ...bad, passthrough: '[1]' })).toBe(true);
    // Blank, `{}` and a real object all pass: there is nothing to drop or it is sent.
    expect(speechPassthroughInvalid('speech', DEFAULT_AUDIO_SETTINGS)).toBe(false);
    expect(speechPassthroughInvalid('speech', { ...bad, passthrough: '{}' })).toBe(false);
    expect(speechPassthroughInvalid('speech', { ...bad, passthrough: '{"seed": 1}' })).toBe(false);
    // ...and so is one naming a field the run owns (the backend would fail it post-commit).
    expect(speechPassthroughInvalid('speech', { ...bad, passthrough: '{"model": "x"}' })).toBe(
      true,
    );
    expect(speechPassthroughInvalid('image', { ...bad, passthrough: '{"model": "x"}' })).toBe(
      false,
    );
    // Other kinds have no passthrough, so stale text in the state never blocks them.
    expect(speechPassthroughInvalid('', bad)).toBe(false);
    expect(speechPassthroughInvalid(undefined, bad)).toBe(false);
    expect(speechPassthroughInvalid('image', bad)).toBe(false);
  });

  it('round-trips voice, speed and passthrough through audioSettingsFrom', () => {
    const wire = {
      voice: 'af_heart',
      speed: 0.9,
      passthrough: { instructions: 'calm', seed: 0, flag: false },
    };
    const state = audioSettingsFrom(wire);
    expect(state.voice).toBe('af_heart');
    expect(state.speed).toBe('0.9');
    // Shown to the composer as editable JSON text, and read back to the same object.
    expect(JSON.parse(state.passthrough)).toEqual(wire.passthrough);
    expect(audioSetting('speech', state)).toEqual({ audio: wire });
  });

  it('coerces a missing/mistyped persisted audio back to defaults', () => {
    expect(audioSettingsFrom(undefined)).toEqual(DEFAULT_AUDIO_SETTINGS);
    expect(
      audioSettingsFrom({
        voice: 7 as unknown as string,
        speed: 'x' as unknown as number,
        passthrough: 'oops' as unknown as Record<string, unknown>,
      }),
    ).toEqual(DEFAULT_AUDIO_SETTINGS);
    expect(audioSettingsFrom({ passthrough: [1] as unknown as Record<string, unknown> })).toEqual(
      DEFAULT_AUDIO_SETTINGS,
    );
    expect(audioSettingsFrom({ passthrough: {} })).toEqual(DEFAULT_AUDIO_SETTINGS);
  });

  it('keeps a speed of 0 (a value, not "unset") across a save/load', () => {
    expect(audioSettingsFrom({ speed: 0 }).speed).toBe('0');
    expect(audioSetting('speech', audioSettingsFrom({ speed: 0 })).audio?.speed).toBe(0);
  });

  it('round-trips through the document for a speech thread (normalizeDoc)', () => {
    const settings = normalizeDoc({
      settings: {
        model: 'tts',
        kind: 'speech',
        audio: { voice: 'bf_emma', speed: 1.1, passthrough: { seed: 3 } },
      },
      messages: [],
    }).settings;
    expect(settings.kind).toBe('speech');
    expect(settings.audio).toEqual({ voice: 'bf_emma', speed: 1.1, passthrough: { seed: 3 } });
  });

  it('never puts an audio key on a text or image document normalizeDoc rebuilds', () => {
    // Even a (malformed) client that stored audio settings on another kind of thread
    // must not have them re-emitted -- audioSetting keys off the kind.
    const text = normalizeDoc({
      settings: { model: 'm', audio: { voice: 'x' } },
      messages: [],
    }).settings;
    expect('audio' in text).toBe(false);
    const image = normalizeDoc({
      settings: { model: 'm', kind: 'image', audio: { voice: 'x' } },
      messages: [],
    }).settings;
    expect('audio' in image).toBe(false);
  });

  it('keeps an image thread free of the audio key, and a speech thread free of the image key', () => {
    const speech = normalizeDoc({
      settings: { model: 'm', kind: 'speech', image: { size: '512x512' } },
      messages: [],
    }).settings;
    expect('image' in speech).toBe(false);
  });

  it('carries no audio key on the defaults a fresh chat is created with', () => {
    expect('audio' in DEFAULTS).toBe(false);
  });

  it('writes the speech kind through kindSetting unchanged, like any other', () => {
    expect(kindSetting('speech')).toEqual({ kind: 'speech' });
    expect(kindSetting('')).toEqual({});
    expect(kindSetting('image')).toEqual({ kind: 'image' });
  });
});

describe('what an attachment means on a speech thread', () => {
  const flags = { vision: false, image: false, imageEdit: false };

  it('is none: text-to-speech takes no image input, even from a vision-capable model', () => {
    expect(attachModeOf('speech', flags)).toBe('none');
    expect(attachModeOf('speech', { ...flags, vision: true })).toBe('none');
    // A (hypothetical) edit-capable image model on a speech thread still takes none.
    expect(attachModeOf('speech', { vision: false, image: true, imageEdit: true })).toBe('none');
  });

  it('leaves the text and image meanings as they were', () => {
    expect(attachModeOf('', { ...flags, vision: true })).toBe('vision');
    expect(attachModeOf('', flags)).toBe('none');
    expect(attachModeOf('image', { ...flags, image: true, imageEdit: true })).toBe('edit');
    expect(attachModeOf('image', { ...flags, image: true })).toBe('none');
  });
});

// The audio ANSWER part mirrors the image one: `audio_url` carries the WAV (inline
// while the run streams, a /api/portal/chat-blobs/{id} reference once committed) and
// is read by its own helper, so the image list never picks it up.
describe('the audio of a message', () => {
  const ref = '/api/portal/chat-blobs/blob_wav';
  const textPart = (text: string) => ({ type: 'text' as const, text });
  const imagePart = (url: string) => ({ type: 'image_url' as const, image_url: { url } });
  const audioPart = (url: string) => ({ type: 'audio_url' as const, audio_url: { url } });

  it('lists the urls of the audio parts in order, text and images left out', () => {
    expect(
      audioUrlsOf([
        textPart('x'),
        audioPart(ref),
        imagePart('data:image/png;base64,AAAA'),
        audioPart('data:audio/wav;base64,UklGRg=='),
      ]),
    ).toEqual([ref, 'data:audio/wav;base64,UklGRg==']);
  });

  it('is empty for a message with no audio, whatever its shape', () => {
    expect(audioUrlsOf('plain')).toEqual([]);
    expect(audioUrlsOf([textPart('x'), imagePart(ref)])).toEqual([]);
    expect(audioUrlsOf([])).toEqual([]);
  });

  it('is never mistaken for an image, so an image list stays images only', () => {
    expect(imageUrlsOf([audioPart(ref)])).toEqual([]);
  });

  it('keeps a stored reference exactly as it is: no copy, no rewrite', () => {
    expect(audioUrlsOf([audioPart(ref)])[0]).toBe(ref);
  });
});
