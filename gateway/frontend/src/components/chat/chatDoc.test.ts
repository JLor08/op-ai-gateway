// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_AUDIO_SETTINGS,
  DEFAULT_IMAGE_SETTINGS,
  DEFAULT_TRANSCRIPTION_SETTINGS,
  DEFAULTS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  MASK_PURPOSE,
  TRANSCRIPTION_AUDIO_ACCEPT,
  TRANSCRIPTION_RESERVED_FIELDS,
  attachModeOf,
  audioDataUrl,
  audioPassthroughMessage,
  audioSetting,
  audioSettingsFrom,
  audioTurnContent,
  bareAudioMediaType,
  deriveTitleFromTranscript,
  historyHasImage,
  imageSetting,
  imageSettingsFrom,
  isChatBlobUrl,
  isMaskPart,
  kindSetting,
  lastUserHasAudio,
  lastUserHasImage,
  maskPart,
  metricsOf,
  normalizeDoc,
  optionsOr,
  parseAudioPassthrough,
  parseTranscriptionPassthrough,
  speechPassthroughInvalid,
  transcriptionPassthroughInvalid,
  transcriptionPassthroughMessage,
  transcriptionSetting,
  transcriptionSettingsFrom,
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
    expect(DEFAULT_AUDIO_SETTINGS).toEqual({
      voice: '',
      speed: '',
      passthrough: '',
      stream: true,
      keepAudio: true,
      keepLast: '',
    });
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
    for (const key of ['model', 'input', 'stream', 'stream_format', 'response_format']) {
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

  it('does not reserve its look-alikes: voice, speed, a prefix or a nested key', () => {
    for (const text of [
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

  // The speech-streaming choice (ADR-068): ON is the default and is never written, so a
  // thread that never touched the toggle persists nothing for it; only the opt-out is a
  // value, and it survives a save/load. A missing or mistyped value is the default, never an
  // accidental opt-out.
  it('persists only the opt-out of speech streaming, and reads anything else as on', () => {
    expect(audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, stream: true })).toEqual({
      audio: {},
    });
    expect(audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, stream: false })).toEqual({
      audio: { stream: false },
    });
    expect(audioSettingsFrom({ stream: false }).stream).toBe(false);
    expect(audioSettingsFrom({ stream: true }).stream).toBe(true);
    expect(audioSettingsFrom({}).stream).toBe(true);
    expect(audioSettingsFrom({ stream: 'no' as unknown as boolean }).stream).toBe(true);
    // The opt-out round-trips with the other knobs.
    const wire = { voice: 'af_heart', stream: false };
    expect(audioSetting('speech', audioSettingsFrom(wire))).toEqual({ audio: wire });
    // Another kind of thread never carries it.
    expect(audioSetting('image', { ...DEFAULT_AUDIO_SETTINGS, stream: false })).toEqual({});
  });

  // The answer-audio retention knob (#281): keeping every answer is the default and is never
  // written, so a thread that never touched it persists nothing and a legacy chat keeps all.
  // `0` (do not store) and a positive N (keep the last N answers) are the two values, and
  // both must survive the full-replace PUT: the wire type, audioSetting and audioSettingsFrom
  // all carry them.
  describe('the answer-audio retention (keep_last_audio)', () => {
    it('defaults to keeping all answers: keepAudio on, keepLast blank', () => {
      expect(DEFAULT_AUDIO_SETTINGS.keepAudio).toBe(true);
      expect(DEFAULT_AUDIO_SETTINGS.keepLast).toBe('');
      expect(audioSetting('speech', DEFAULT_AUDIO_SETTINGS)).toEqual({ audio: {} });
    });

    it('writes nothing for keep-all and reads an absent field back as keep-all', () => {
      const audio = audioSetting('speech', DEFAULT_AUDIO_SETTINGS).audio ?? {};
      expect('keep_last_audio' in audio).toBe(false);
      const state = audioSettingsFrom(audio);
      expect(state.keepAudio).toBe(true);
      expect(state.keepLast).toBe('');
      expect(audioSettingsFrom(undefined)).toMatchObject({ keepAudio: true, keepLast: '' });
    });

    it('writes keep_last_audio: N for keep-last-N and round-trips it', () => {
      const state = { ...DEFAULT_AUDIO_SETTINGS, keepAudio: true, keepLast: '3' };
      const wire = audioSetting('speech', state);
      expect(wire).toEqual({ audio: { keep_last_audio: 3 } });
      expect(audioSettingsFrom(wire.audio)).toMatchObject({ keepAudio: true, keepLast: '3' });
      expect(audioSetting('speech', audioSettingsFrom(wire.audio))).toEqual(wire);
    });

    it('writes keep_last_audio: 0 for do-not-store and round-trips it', () => {
      const state = { ...DEFAULT_AUDIO_SETTINGS, keepAudio: false };
      const wire = audioSetting('speech', state);
      expect(wire).toEqual({ audio: { keep_last_audio: 0 } });
      expect(audioSettingsFrom(wire.audio)).toMatchObject({ keepAudio: false, keepLast: '' });
      expect(audioSetting('speech', audioSettingsFrom(wire.audio))).toEqual(wire);
    });

    it('lets do-not-store win over a leftover keepLast, and drops that text on the way back', () => {
      const state = { ...DEFAULT_AUDIO_SETTINGS, keepAudio: false, keepLast: '5' };
      expect(audioSetting('speech', state)).toEqual({ audio: { keep_last_audio: 0 } });
      expect(audioSettingsFrom({ keep_last_audio: 0 })).toMatchObject({
        keepAudio: false,
        keepLast: '',
      });
    });

    it('omits a blank or invalid keepLast (keep-all), so nothing is sent for it', () => {
      for (const keepLast of ['', '   ', 'abc', '0', '-2', '1.5', '2.0x', 'NaN', 'Infinity']) {
        const audio =
          audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, keepAudio: true, keepLast }).audio ??
          {};
        expect('keep_last_audio' in audio, keepLast).toBe(false);
      }
    });

    it('trims a keepLast before parsing it', () => {
      expect(audioSetting('speech', { ...DEFAULT_AUDIO_SETTINGS, keepLast: ' 4 ' }).audio).toEqual({
        keep_last_audio: 4,
      });
    });

    it('reads a negative or non-integer persisted value defensively as keep-all', () => {
      for (const keep of [-1, 1.5, Number.NaN, 'x' as unknown as number]) {
        expect(audioSettingsFrom({ keep_last_audio: keep }), String(keep)).toMatchObject({
          keepAudio: true,
          keepLast: '',
        });
      }
    });

    it('round-trips with the other knobs, and never reaches a non-speech thread', () => {
      const wire = { voice: 'af_heart', speed: 0.9, stream: false, keep_last_audio: 2 };
      expect(audioSetting('speech', audioSettingsFrom(wire))).toEqual({ audio: wire });
      const moved = { ...DEFAULT_AUDIO_SETTINGS, keepAudio: false };
      expect(audioSetting('', moved)).toEqual({});
      expect(audioSetting(undefined, moved)).toEqual({});
      expect(audioSetting('image', moved)).toEqual({});
      expect(audioSetting('transcription', moved)).toEqual({});
    });
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

  // An audio part is the INPUT of a transcription turn as well as the reply of a speech turn:
  // the one reader serves both, so a user turn's clip is listed like an assistant's.
  it('lists the clip of a transcription user turn, inline and once stored', () => {
    const inline = 'data:audio/webm;base64,AAAA';
    expect(audioUrlsOf(audioTurnContent(inline))).toEqual([inline]);
    expect(audioUrlsOf([textPart(''), audioPart(ref)])).toEqual([ref]);
  });
});

// Transcription (speech-to-text) settings are the audio settings' twin and follow the same
// discipline: present only on a "transcription" thread, never on the wire/persisted document
// of a text, image or speech thread (byte-identity), and carrying only what the user set.
describe('the persisted transcription settings mirror the backend omitempty', () => {
  const moved = {
    ...DEFAULT_TRANSCRIPTION_SETTINGS,
    language: 'de',
    prompt: 'Namen: Zoe',
    temperature: '0.2',
    passthrough: '{"beam": 5}',
  };

  it('omits the transcription key for any non-transcription thread', () => {
    expect(transcriptionSetting('', moved)).toEqual({});
    expect(transcriptionSetting(undefined, moved)).toEqual({});
    expect(transcriptionSetting('image', moved)).toEqual({});
    expect(transcriptionSetting('speech', moved)).toEqual({});
    expect(transcriptionSetting('video', moved)).toEqual({});
  });

  it('has blank defaults: a fresh thread leaves the model on its own language and temperature', () => {
    expect(DEFAULT_TRANSCRIPTION_SETTINGS).toEqual({
      language: '',
      prompt: '',
      temperature: '',
      passthrough: '',
    });
    // The key is present for a transcription thread even when nothing is set, as
    // audio's is for a speech thread: `transcription: {}`.
    expect(transcriptionSetting('transcription', DEFAULT_TRANSCRIPTION_SETTINGS)).toEqual({
      transcription: {},
    });
  });

  it('carries only what the user set, trimmed', () => {
    expect(transcriptionSetting('transcription', moved)).toEqual({
      transcription: {
        language: 'de',
        prompt: 'Namen: Zoe',
        temperature: 0.2,
        passthrough: { beam: 5 },
      },
    });
    expect(
      transcriptionSetting('transcription', {
        ...DEFAULT_TRANSCRIPTION_SETTINGS,
        language: '  de ',
        prompt: '  hello  ',
      }),
    ).toEqual({ transcription: { language: 'de', prompt: 'hello' } });
    // A blank (whitespace-only) language or prompt is "unset", not an empty string on the wire.
    expect(
      transcriptionSetting('transcription', {
        ...DEFAULT_TRANSCRIPTION_SETTINGS,
        language: '   ',
        prompt: ' \n ',
      }),
    ).toEqual({ transcription: {} });
  });

  it('keeps an explicit temperature of 0 (a value, not "unset") and drops a blank or unparseable one', () => {
    const wire = (temperature: string) =>
      transcriptionSetting('transcription', { ...DEFAULT_TRANSCRIPTION_SETTINGS, temperature })
        .transcription ?? {};
    expect(wire('0')).toEqual({ temperature: 0 });
    expect(wire(' 0.0 ')).toEqual({ temperature: 0 });
    expect(wire('1')).toEqual({ temperature: 1 });
    for (const blank of ['', '   ', 'abc', 'NaN', 'Infinity']) {
      expect('temperature' in wire(blank), JSON.stringify(blank)).toBe(false);
    }
  });

  it('omits a passthrough that is blank, `{}`, not an object or names a reserved field', () => {
    for (const passthrough of ['', '  ', '{}', '{"beam":', '[1]', '"x"', '{"model": "x"}']) {
      const tr =
        transcriptionSetting('transcription', { ...DEFAULT_TRANSCRIPTION_SETTINGS, passthrough })
          .transcription ?? {};
      expect('passthrough' in tr, JSON.stringify(passthrough)).toBe(false);
    }
  });

  it('keeps each value of a passthrough in the JSON type the user wrote', () => {
    const tr = transcriptionSetting('transcription', {
      ...DEFAULT_TRANSCRIPTION_SETTINGS,
      passthrough: '{"timestamp_granularities": ["word"], "beam": 5, "verbose": true, "q": "x"}',
    }).transcription;
    expect(tr?.passthrough).toEqual({
      timestamp_granularities: ['word'],
      beam: 5,
      verbose: true,
      q: 'x',
    });
  });

  // The client mirror of the backend's transcriptionReservedFields: the run owns these
  // form fields, and the backend would end the run
  // gateway.chat_run_transcription_reserved_field only after the turn is committed.
  it('reserves exactly the form fields the run owns', () => {
    expect([...TRANSCRIPTION_RESERVED_FIELDS]).toEqual([
      'model',
      'file',
      'response_format',
      'stream',
    ]);
  });

  it('rejects a passthrough naming a reserved field, in any letter case, and names it', () => {
    for (const key of ['model', 'file', 'stream', 'response_format']) {
      expect(parseTranscriptionPassthrough(JSON.stringify({ [key]: 'x' }))).toEqual({
        status: 'invalid',
        reservedField: key,
      });
      const shouted = key.toUpperCase();
      expect(parseTranscriptionPassthrough(JSON.stringify({ [shouted]: 1 }))).toEqual({
        status: 'invalid',
        reservedField: shouted, // as the user wrote it
      });
    }
    expect(parseTranscriptionPassthrough('{"File": "x"}')).toEqual({
      status: 'invalid',
      reservedField: 'File',
    });
    // A reserved key among others poisons the whole object, and the first in sorted order
    // is the one named, like the backend.
    expect(parseTranscriptionPassthrough('{"beam": 1, "stream": true, "file": "x"}')).toEqual({
      status: 'invalid',
      reservedField: 'file',
    });
  });

  it('keeps the two reserved sets apart: input is the speech run’s, file the transcription run’s', () => {
    expect(parseTranscriptionPassthrough('{"input": "x"}').status).toBe('ok');
    expect(parseAudioPassthrough('{"file": "x"}').status).toBe('ok');
    // The shared ones are reserved on both.
    expect(parseAudioPassthrough('{"model": "x"}').status).toBe('invalid');
    expect(parseTranscriptionPassthrough('{"model": "x"}').status).toBe('invalid');
  });

  it('classifies a passthrough text like the speech one: empty, valid or why it is not', () => {
    expect(parseTranscriptionPassthrough('')).toEqual({ status: 'empty' });
    expect(parseTranscriptionPassthrough('{}')).toEqual({ status: 'empty' });
    expect(parseTranscriptionPassthrough('{"beam": 1}')).toEqual({
      status: 'ok',
      value: { beam: 1 },
    });
    expect(parseTranscriptionPassthrough('{"beam":')).toEqual({ status: 'invalid' });
    expect(parseTranscriptionPassthrough('[1, 2]')).toEqual({ status: 'invalid' });
    expect(parseTranscriptionPassthrough('null')).toEqual({ status: 'invalid' });
    // No look-alike is reserved: a prefix, a nested key.
    for (const text of [
      '{"file_name": 1}',
      '{"models": 1, "files": 2, "streaming": 3}',
      '{"nested": {"file": "x", "stream": true}}',
    ]) {
      expect(parseTranscriptionPassthrough(text).status, text).toBe('ok');
    }
  });

  it('picks the message for an invalid passthrough: the reserved one names the field', () => {
    const t = {
      chatTranscriptionPassthroughInvalid: 'not an object',
      chatTranscriptionPassthroughReserved: (field: string) => `reserved ${field}`,
      chatTranscriptionPassthroughBadKey: (field: string) => `bad key ${field}`,
    };
    expect(transcriptionPassthroughMessage(parseTranscriptionPassthrough('[1]'), t)).toBe(
      'not an object',
    );
    expect(transcriptionPassthroughMessage(parseTranscriptionPassthrough('{"beam":'), t)).toBe(
      'not an object',
    );
    expect(transcriptionPassthroughMessage(parseTranscriptionPassthrough('{"file": 1}'), t)).toBe(
      'reserved file',
    );
  });

  it('refuses a run only for a transcription thread whose passthrough is not sendable', () => {
    const bad = { ...DEFAULT_TRANSCRIPTION_SETTINGS, passthrough: '{"beam":' };
    expect(transcriptionPassthroughInvalid('transcription', bad)).toBe(true);
    expect(transcriptionPassthroughInvalid('transcription', { ...bad, passthrough: '[1]' })).toBe(
      true,
    );
    // Blank, `{}` and a real object all pass: there is nothing to drop or it is sent.
    expect(transcriptionPassthroughInvalid('transcription', DEFAULT_TRANSCRIPTION_SETTINGS)).toBe(
      false,
    );
    expect(transcriptionPassthroughInvalid('transcription', { ...bad, passthrough: '{}' })).toBe(
      false,
    );
    expect(
      transcriptionPassthroughInvalid('transcription', { ...bad, passthrough: '{"beam": 1}' }),
    ).toBe(false);
    // ...and a reserved field is refused, in any case (the backend would fail it post-commit).
    for (const key of ['model', 'File', 'RESPONSE_FORMAT', 'stream']) {
      expect(
        transcriptionPassthroughInvalid('transcription', {
          ...bad,
          passthrough: JSON.stringify({ [key]: 'x' }),
        }),
        key,
      ).toBe(true);
    }
    // Other kinds have no transcription passthrough, so stale text in the state never blocks them.
    for (const kind of ['', undefined, 'image', 'speech']) {
      expect(transcriptionPassthroughInvalid(kind, bad), String(kind)).toBe(false);
      expect(
        transcriptionPassthroughInvalid(kind, { ...bad, passthrough: '{"model": "x"}' }),
        String(kind),
      ).toBe(false);
    }
  });

  it('does not let the speech passthrough guard see a transcription thread, or the reverse', () => {
    const badAudio = { ...DEFAULT_AUDIO_SETTINGS, passthrough: '{"seed":' };
    expect(speechPassthroughInvalid('transcription', badAudio)).toBe(false);
    const badTranscription = { ...DEFAULT_TRANSCRIPTION_SETTINGS, passthrough: '{"beam":' };
    expect(transcriptionPassthroughInvalid('speech', badTranscription)).toBe(false);
  });

  it('round-trips language, prompt, temperature and passthrough through transcriptionSettingsFrom', () => {
    const wire = {
      language: 'de',
      prompt: 'Namen: Zoe',
      temperature: 0.4,
      passthrough: { beam: 5, verbose: false, q: 'x', timestamp_granularities: ['word'] },
    };
    const state = transcriptionSettingsFrom(wire);
    expect(state.language).toBe('de');
    expect(state.prompt).toBe('Namen: Zoe');
    expect(state.temperature).toBe('0.4');
    // Shown to the composer as editable JSON text, and read back to the same object.
    expect(JSON.parse(state.passthrough)).toEqual(wire.passthrough);
    expect(transcriptionSetting('transcription', state)).toEqual({ transcription: wire });
  });

  it('coerces a missing/mistyped persisted transcription back to defaults', () => {
    expect(transcriptionSettingsFrom(undefined)).toEqual(DEFAULT_TRANSCRIPTION_SETTINGS);
    expect(
      transcriptionSettingsFrom({
        language: 7 as unknown as string,
        prompt: {} as unknown as string,
        temperature: 'x' as unknown as number,
        passthrough: 'oops' as unknown as Record<string, unknown>,
      }),
    ).toEqual(DEFAULT_TRANSCRIPTION_SETTINGS);
    expect(
      transcriptionSettingsFrom({ passthrough: [1] as unknown as Record<string, unknown> }),
    ).toEqual(DEFAULT_TRANSCRIPTION_SETTINGS);
    expect(transcriptionSettingsFrom({ passthrough: {} })).toEqual(DEFAULT_TRANSCRIPTION_SETTINGS);
  });

  it('keeps a temperature of 0 across a save/load', () => {
    expect(transcriptionSettingsFrom({ temperature: 0 }).temperature).toBe('0');
    expect(
      transcriptionSetting('transcription', transcriptionSettingsFrom({ temperature: 0 }))
        .transcription?.temperature,
    ).toBe(0);
  });

  it('round-trips through the document for a transcription thread (normalizeDoc)', () => {
    const wire = {
      language: 'de',
      prompt: 'Namen: Zoe',
      temperature: 0,
      passthrough: { beam: 5 },
    };
    const settings = normalizeDoc({
      settings: { model: 'whisper', kind: 'transcription', transcription: wire },
      messages: [],
    }).settings;
    expect(settings.kind).toBe('transcription');
    expect(settings.transcription).toEqual(wire);
  });

  it('never puts a transcription key on a text, image or speech document normalizeDoc rebuilds', () => {
    // Even a (malformed) client that stored transcription settings on another kind of
    // thread must not have them re-emitted -- transcriptionSetting keys off the kind.
    for (const kind of [undefined, 'image', 'speech']) {
      const settings = normalizeDoc({
        settings: {
          model: 'm',
          ...(kind ? { kind } : {}),
          transcription: { language: 'de' },
        },
        messages: [],
      }).settings;
      expect('transcription' in settings, String(kind)).toBe(false);
    }
  });

  it('keeps a speech thread on its audio key and a transcription thread off the others', () => {
    const speech = normalizeDoc({
      settings: {
        model: 'm',
        kind: 'speech',
        audio: { voice: 'x' },
        transcription: { language: 'de' },
      },
      messages: [],
    }).settings;
    expect(speech.audio).toEqual({ voice: 'x' });
    expect('transcription' in speech).toBe(false);

    const transcription = normalizeDoc({
      settings: {
        model: 'm',
        kind: 'transcription',
        audio: { voice: 'x' },
        image: { size: '512x512' },
        transcription: { language: 'de' },
      },
      messages: [],
    }).settings;
    expect(transcription.transcription).toEqual({ language: 'de' });
    expect('audio' in transcription).toBe(false);
    expect('image' in transcription).toBe(false);
  });

  it('carries no transcription key on the defaults a fresh chat is created with', () => {
    expect('transcription' in DEFAULTS).toBe(false);
  });

  it('writes the transcription kind through kindSetting unchanged, like any other', () => {
    expect(kindSetting('transcription')).toEqual({ kind: 'transcription' });
    expect(kindSetting('')).toEqual({});
    expect(kindSetting(undefined)).toEqual({});
  });
});

describe('what an attachment means on a transcription thread', () => {
  const flags = { vision: false, image: false, imageEdit: false };

  it('is none: a transcription run reads only its audio, so an image would be dropped in silence', () => {
    expect(attachModeOf('transcription', flags)).toBe('none');
    expect(attachModeOf('transcription', { ...flags, vision: true })).toBe('none');
    expect(attachModeOf('transcription', { vision: false, image: true, imageEdit: true })).toBe(
      'none',
    );
  });

  it('leaves the text, image and speech meanings as they were', () => {
    expect(attachModeOf('', { ...flags, vision: true })).toBe('vision');
    expect(attachModeOf('image', { ...flags, image: true, imageEdit: true })).toBe('edit');
    expect(attachModeOf('speech', { ...flags, vision: true })).toBe('none');
  });
});

// The INPUT clip of a transcription turn is an audio_url part of the USER message: the same
// part type a speech turn's reply uses, told apart by the role it sits in. The backend's
// DecodeAudioDataURL refuses a data url whose media type carries a parameter, so the data url the
// composer builds (audioDataUrl) must carry the bare type. (The composer's clip is always the
// audio/wav it converted the upload to; see audioAttach.)
describe('the audio input part of a transcription turn', () => {
  // The conversion to WAV happens in the browser, so the picker offers any audio, not a list.
  it('offers any audio to the picker, and the extensions a dialog matches without a MIME type', () => {
    const accepted = TRANSCRIPTION_AUDIO_ACCEPT.split(',');
    expect(accepted).toContain('audio/*');
    for (const extension of ['.wav', '.webm', '.mp3', '.m4a', '.ogg', '.flac']) {
      expect(accepted).toContain(extension);
    }
  });

  it('builds the inline data url of a clip', () => {
    expect(audioDataUrl('audio/wav', 'UklGRg==')).toBe('data:audio/wav;base64,UklGRg==');
  });

  it('strips a media-type parameter: audio/webm;codecs=opus becomes audio/webm', () => {
    expect(audioDataUrl('audio/webm;codecs=opus', 'AAAA')).toBe('data:audio/webm;base64,AAAA');
    expect(audioDataUrl('audio/ogg; codecs=opus', 'AAAA')).toBe('data:audio/ogg;base64,AAAA');
    expect(audioDataUrl('audio/mp4;codecs="mp4a.40.2"', 'AAAA')).toBe('data:audio/mp4;base64,AAAA');
  });

  it('normalizes case and surrounding space of the type, like the backend reads it', () => {
    expect(bareAudioMediaType('audio/webm;codecs=opus')).toBe('audio/webm');
    expect(bareAudioMediaType('  Audio/WebM ; Codecs=Opus ')).toBe('audio/webm');
    expect(bareAudioMediaType('audio/mpeg')).toBe('audio/mpeg');
    expect(bareAudioMediaType('')).toBe('');
  });

  it('reads back through audioUrlsOf, and is never mistaken for an image', () => {
    const content = audioTurnContent(audioDataUrl('audio/webm;codecs=opus', 'AAAA'));
    expect(audioUrlsOf(content)).toEqual(['data:audio/webm;base64,AAAA']);
    expect(imageUrlsOf(content)).toEqual([]);
    const message: ChatUiMessage = { id: 'u', role: 'user', content };
    // An audio input is no image: the replay guards that look for one stay quiet.
    expect(historyHasImage([message])).toBe(false);
    expect(lastUserHasImage([message])).toBe(false);
  });
});

// A transcription thread's first user turn is audio only, so the usual title (the first user
// message's text) is empty; the transcript is what names the chat (Fork 4).
describe('titling a transcription chat from its transcript', () => {
  const user: ChatUiMessage = {
    id: 'u1',
    role: 'user',
    content: audioTurnContent(audioDataUrl('audio/webm', 'AAAA')),
  };
  const assistant = (content: ChatUiMessage['content'], id = 'a1'): ChatUiMessage => ({
    id,
    role: 'assistant',
    content,
    status: 'complete',
  });

  it('is the transcript, as one short line', () => {
    expect(deriveTitleFromTranscript([user, assistant('Hello world')])).toBe('Hello world');
    expect(deriveTitleFromTranscript([user, assistant('  Hello\n\nworld \t again ')])).toBe(
      'Hello world again',
    );
    expect(
      deriveTitleFromTranscript([
        user,
        assistant('This is a rather long transcript of a voice memo about the quarterly plan'),
      ]),
    ).toBe('This is a rather long transcript of a vo');
  });

  it('reads the transcript from a committed text part as it does from a string', () => {
    expect(
      deriveTitleFromTranscript([user, assistant([{ type: 'text', text: 'Guten Morgen' }])]),
    ).toBe('Guten Morgen');
  });

  it('is empty when there is no transcript to name the chat by', () => {
    expect(deriveTitleFromTranscript([])).toBe('');
    // The user turn is audio only: nothing there to read.
    expect(deriveTitleFromTranscript([user])).toBe('');
    expect(deriveTitleFromTranscript([user, assistant('')])).toBe('');
    expect(deriveTitleFromTranscript([user, assistant('  \n ')])).toBe('');
    expect(deriveTitleFromTranscript([user, assistant([])])).toBe('');
  });

  it('takes the latest assistant turn, and never the user turn’s own text', () => {
    const typed: ChatUiMessage = { id: 'u2', role: 'user', content: 'typed words' };
    expect(deriveTitleFromTranscript([typed])).toBe('');
    expect(
      deriveTitleFromTranscript([
        user,
        assistant('first take', 'a1'),
        user,
        assistant('second', 'a2'),
      ]),
    ).toBe('second');
  });
});

describe('the user turn of a transcription chat', () => {
  const url = 'data:audio/webm;base64,AAAA';
  const clipTurn = (id = 'u'): ChatUiMessage => ({
    id,
    role: 'user',
    content: [{ type: 'audio_url', audio_url: { url } }],
  });

  it('is the clip alone, as one audio_url part', () => {
    expect(audioTurnContent(url)).toEqual([{ type: 'audio_url', audio_url: { url } }]);
  });

  describe('lastUserHasAudio', () => {
    it('is true when the last user message carries a clip', () => {
      expect(lastUserHasAudio([clipTurn()])).toBe(true);
      expect(
        lastUserHasAudio([
          {
            id: 'u',
            role: 'user',
            content: [{ type: 'text', text: 'x' }, ...audioTurnContentOf(url)],
          },
        ]),
      ).toBe(true);
      // A stored reference counts: it is what a committed turn holds.
      expect(
        lastUserHasAudio([
          {
            id: 'u',
            role: 'user',
            content: [{ type: 'audio_url', audio_url: { url: '/api/portal/chat-blobs/blob_a' } }],
          },
        ]),
      ).toBe(true);
    });

    it('is false for a text turn, an image turn and an empty history', () => {
      expect(lastUserHasAudio([])).toBe(false);
      expect(lastUserHasAudio([{ id: 'u', role: 'user', content: 'hello' }])).toBe(false);
      expect(
        lastUserHasAudio([
          {
            id: 'u',
            role: 'user',
            content: [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }],
          },
        ]),
      ).toBe(false);
    });

    // Role-aware: a transcript is text, but an assistant turn that held audio (a speech
    // reply in a history that was edited into a transcription thread) is never the input.
    it('looks at the LAST user message only', () => {
      const text: ChatUiMessage = { id: 'u2', role: 'user', content: 'now just text' };
      expect(lastUserHasAudio([clipTurn('u1'), text])).toBe(false);
      expect(lastUserHasAudio([text, clipTurn('u1')])).toBe(true);
      const reply: ChatUiMessage = {
        id: 'a',
        role: 'assistant',
        content: [{ type: 'audio_url', audio_url: { url } }],
      };
      expect(lastUserHasAudio([text, reply])).toBe(false);
      // The assistant turn after a clip does not hide it.
      expect(lastUserHasAudio([clipTurn(), { id: 'a', role: 'assistant', content: 'hi' }])).toBe(
        true,
      );
    });
  });
});

// Local helper: the parts of a clip turn, for composing a mixed text + audio message.
function audioTurnContentOf(url: string) {
  return audioTurnContent(url) as Exclude<ChatUiMessage['content'], string>;
}

// The transcription run sends the passthrough as multipart form fields: a key becomes the NAME
// of a part, in a header line that a quote or a backslash cuts short and a line break splits in
// two. The backend refuses such a key (gateway.chat_run_transcription_dispatch_failed) only after
// the turn is committed, so the composer refuses it first.
describe('a transcription passthrough key that cannot be a form field name', () => {
  const badKeyOf = (raw: string) => {
    const parse = parseTranscriptionPassthrough(raw);
    return parse.status === 'invalid' ? parse.badKey : undefined;
  };

  it.each([
    ['an empty key', '{"": 1}', ''],
    ['a double quote', '{"a\\"b": 1}', 'a"b'],
    ['a backslash', '{"a\\\\b": 1}', 'a\\b'],
    ['a line feed', '{"a\\nb": 1}', 'a\nb'],
    ['a carriage return', '{"a\\rb": 1}', 'a\rb'],
    ['a NUL', '{"a\\u0000b": 1}', 'a\u0000b'],
    ['a tab', '{"a\\tb": 1}', 'a\tb'],
    ['DEL', '{"a\\u007fb": 1}', 'a\u007fb'],
  ])('is refused: %s', (_name, raw, key) => {
    expect(parseTranscriptionPassthrough(raw)).toEqual({ status: 'invalid', badKey: key });
  });

  it('finds the bad key among good ones, whichever is first', () => {
    expect(badKeyOf('{"beam": 5, "x\\"y": 1, "temperature_inc": 0.2}')).toBe('x"y');
    expect(badKeyOf('{"x\\"y": 1, "beam": 5}')).toBe('x"y');
  });

  it('accepts what a form field name may be', () => {
    for (const raw of [
      '{"beam": 5}',
      '{"timestamp_granularities[]": "word"}',
      '{"with space": 1, "dots.and-dashes": 2, "ünïcode": 3, "日本語": 4}',
      '{"single\'quote": 1}',
    ]) {
      expect(parseTranscriptionPassthrough(raw).status, raw).toBe('ok');
    }
  });

  it("is only for a key at the top: the VALUES are the user's own JSON", () => {
    expect(
      parseTranscriptionPassthrough('{"nested": {"a\\"b": 1}, "text": "q\\"uote\\n"}').status,
    ).toBe('ok');
  });

  // A reserved name is the more useful thing to say, and it is the older refusal.
  it('names a reserved field before a bad key', () => {
    expect(parseTranscriptionPassthrough('{"x\\"y": 1, "Model": 2}')).toEqual({
      status: 'invalid',
      reservedField: 'Model',
    });
  });

  it('is a transcription rule: the speech passthrough is a JSON body, where any key is fine', () => {
    expect(parseAudioPassthrough('{"a\\"b": 1, "": 2}').status).toBe('ok');
    expect(parseAudioPassthrough('{"a\\nb": 1}').status).toBe('ok');
  });

  it('refuses a transcription run for it, like any other unsendable passthrough', () => {
    const settings = { ...DEFAULT_TRANSCRIPTION_SETTINGS, passthrough: '{"a\\"b": 1}' };
    expect(transcriptionPassthroughInvalid('transcription', settings)).toBe(true);
    expect(transcriptionPassthroughInvalid('speech', settings)).toBe(false);
  });

  it('says so, naming the key as the user wrote it (escaped, so a line break is readable)', () => {
    const t = {
      chatTranscriptionPassthroughInvalid: 'not an object',
      chatTranscriptionPassthroughReserved: (field: string) => `reserved ${field}`,
      chatTranscriptionPassthroughBadKey: (field: string) => `bad key ${field}`,
    };
    expect(transcriptionPassthroughMessage(parseTranscriptionPassthrough('{"a\\"b": 1}'), t)).toBe(
      'bad key "a\\"b"',
    );
    expect(transcriptionPassthroughMessage(parseTranscriptionPassthrough('{"": 1}'), t)).toBe(
      'bad key ""',
    );
    expect(transcriptionPassthroughMessage(parseTranscriptionPassthrough('{"a\\nb": 1}'), t)).toBe(
      'bad key "a\\nb"',
    );
  });
});
