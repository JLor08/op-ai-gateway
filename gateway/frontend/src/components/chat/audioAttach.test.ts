// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { beforeEach, describe, expect, it, vi } from 'vitest';

import { AudioAttachError, MAX_AUDIO_BYTES, prepareAudioClip } from './audioAttach';
import { AudioDecodeError, audioBlobToWav } from '../shared/audioToWav';

// The conversion itself is audioToWav.test.ts's; jsdom has no WebAudio to run it. Here it is a
// stand-in that "converts" by prefixing the bytes, so what a test reads back out of the clip's data
// url says whether the CONVERTED blob (and not the upload, merely relabelled) is what was encoded.
vi.mock('../shared/audioToWav', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../shared/audioToWav')>()),
  audioBlobToWav: vi.fn(async (blob: Blob) => new Blob(['WAV:', blob], { type: 'audio/wav' })),
}));

const toWav = vi.mocked(audioBlobToWav);

// The data url of a converted clip whose upload held `text`.
const wavUrl = (text: string) => `data:audio/wav;base64,${btoa(`WAV:${text}`)}`;

// The reason a clip was refused for, or undefined when it was not.
async function refusal(file: Blob, name = ''): Promise<string | undefined> {
  try {
    await prepareAudioClip(file, name);
  } catch (err) {
    if (err instanceof AudioAttachError) return err.reason;
    throw err;
  }
  return undefined;
}

beforeEach(() => {
  toWav.mockClear();
});

describe('prepareAudioClip', () => {
  describe('converting to WAV', () => {
    it.each([
      ['audio/webm;codecs=opus', ''],
      ['audio/mp4', 'memo.m4a'],
      ['audio/mpeg', 'memo.mp3'],
      ['audio/ogg', 'memo.ogg'],
      ['audio/wav', 'memo.wav'],
      ['audio/x-wav', 'memo.wav'],
    ])('sends a %s clip as an audio/wav data url of the converted bytes', async (type, name) => {
      const upload = new Blob(['bytes'], { type });

      const clip = await prepareAudioClip(upload, name);

      expect(toWav).toHaveBeenCalledTimes(1);
      expect(toWav).toHaveBeenCalledWith(upload);
      expect(clip).toEqual({ url: wavUrl('bytes'), name });
    });

    // The converter is the browser's decoder, which sniffs the bytes: any audio it can read is
    // welcome, whatever the platform called it.
    it.each([
      ['a format the picker did not list', new File(['x'], 'song.flac', { type: 'audio/flac' })],
      [
        'a video container (Chrome types an audio .webm so)',
        new Blob(['x'], { type: 'video/webm' }),
      ],
      ['a Firefox .ogg', new File(['x'], 'memo.ogg', { type: 'application/ogg' })],
      ['a file with no type at all', new File(['x'], 'memo', { type: '' })],
      ['a generic type', new File(['x'], 'memo.bin', { type: 'application/octet-stream' })],
    ])('hands %s to the converter instead of refusing it by its type', async (_name, file) => {
      const clip = await prepareAudioClip(file, file instanceof File ? file.name : '');

      expect(toWav).toHaveBeenCalledWith(file);
      expect(clip.url).toBe(wavUrl('x'));
    });

    it('stores the clip as audio/wav whatever type the converter reports', async () => {
      toWav.mockResolvedValueOnce(new Blob(['pcm'], { type: 'audio/webm' }));

      const clip = await prepareAudioClip(new Blob(['x'], { type: 'audio/webm' }));

      expect(clip.url).toBe(`data:audio/wav;base64,${btoa('pcm')}`);
    });

    it('keeps the name of a picked file, and none for a recording', async () => {
      expect((await prepareAudioClip(new Blob(['x'], { type: 'audio/webm' }), 'a.webm')).name).toBe(
        'a.webm',
      );
      expect((await prepareAudioClip(new Blob(['x'], { type: 'audio/webm' }))).name).toBe('');
    });
  });

  describe('refusing a clip', () => {
    it.each([
      ['an image', new File(['x'], 'cat.png', { type: 'image/png' })],
      ['a text file', new File(['x'], 'notes.txt', { type: 'text/plain' })],
      ['a pdf', new File(['x'], 'paper.pdf', { type: 'application/pdf' })],
    ])('refuses %s as the wrong type, without converting it', async (_name, file) => {
      expect(await refusal(file, file.name)).toBe('type');
      expect(toWav).not.toHaveBeenCalled();
    });

    it('refuses an empty clip, without converting it', async () => {
      expect(await refusal(new File([], 'silence.wav', { type: 'audio/wav' }), 'silence.wav')).toBe(
        'empty',
      );
      expect(toWav).not.toHaveBeenCalled();
    });

    it('refuses an upload over the size the backend stores, without decoding it', async () => {
      const big = new Blob(['x'], { type: 'audio/wav' });
      Object.defineProperty(big, 'size', { value: MAX_AUDIO_BYTES + 1 });

      expect(await refusal(big)).toBe('size');
      expect(toWav).not.toHaveBeenCalled();
    });

    // A small compressed upload can decode to a long WAV (16 kHz mono 16 bit is 32 KB a second),
    // and the backend would refuse that blob when the turn is committed, after the transcript
    // was extended: the size that counts is the converted one.
    it('refuses a clip whose WAV is over the size the backend stores', async () => {
      const long = new Blob(['x'], { type: 'audio/wav' });
      Object.defineProperty(long, 'size', { value: MAX_AUDIO_BYTES + 1 });
      toWav.mockResolvedValueOnce(long);

      expect(await refusal(new Blob(['small'], { type: 'audio/webm' }))).toBe('size');
    });

    it('accepts a WAV of exactly the ceiling', async () => {
      const exact = new Blob(['x'], { type: 'audio/wav' });
      Object.defineProperty(exact, 'size', { value: MAX_AUDIO_BYTES });
      toWav.mockResolvedValueOnce(exact);

      expect(await refusal(new Blob(['small'], { type: 'audio/webm' }))).toBeUndefined();
    });

    it('refuses audio the browser cannot decode, as a decode failure', async () => {
      toWav.mockRejectedValueOnce(new AudioDecodeError('corrupt'));

      expect(await refusal(new File(['x'], 'broken.mp3', { type: 'audio/mpeg' }))).toBe('decode');
    });

    it('refuses any other conversion failure as an unreadable clip', async () => {
      toWav.mockRejectedValueOnce(new Error('out of memory'));

      expect(await refusal(new Blob(['x'], { type: 'audio/webm' }))).toBe('read');
    });
  });
});
