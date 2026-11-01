// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { speakerTranscriptOf, type ChatContent, type ChatContentPart } from './chatContent';

const SPEAKER_PART: ChatContentPart = {
  type: 'speaker_transcript',
  text: 'Hello there. Hi.',
  turns: [
    { speaker: '0', start: 0, end: 1.5, text: 'Hello there.' },
    { speaker: '1', start: 1.5, end: 2, text: 'Hi.' },
  ],
};

describe('speakerTranscriptOf', () => {
  it('pulls the speaker_transcript part out of a content array', () => {
    expect(speakerTranscriptOf([SPEAKER_PART])).toEqual(SPEAKER_PART);
  });

  it('finds it beside other parts, wherever it sits', () => {
    const content: ChatContent = [{ type: 'text', text: 'note' }, SPEAKER_PART];
    expect(speakerTranscriptOf(content)).toEqual(SPEAKER_PART);
  });

  it('is undefined when the message holds none', () => {
    expect(speakerTranscriptOf('plain text')).toBeUndefined();
    expect(speakerTranscriptOf([])).toBeUndefined();
    expect(speakerTranscriptOf([{ type: 'text', text: 'a transcript' }])).toBeUndefined();
    expect(
      speakerTranscriptOf([
        { type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,AAAA' } },
      ]),
    ).toBeUndefined();
    expect(speakerTranscriptOf([{ type: 'audio_removed' }])).toBeUndefined();
  });

  it('keeps a speaker id verbatim, the empty string included', () => {
    const part = speakerTranscriptOf([
      {
        type: 'speaker_transcript',
        text: 'a b c',
        turns: [
          { speaker: '', text: 'a' },
          { speaker: 'SPEAKER_01', text: 'b' },
          { speaker: 'Alice', text: 'c' },
        ],
      },
    ]);
    expect(part?.turns.map((turn) => turn.speaker)).toEqual(['', 'SPEAKER_01', 'Alice']);
  });

  it('leaves the times out of a turn that has none, and keeps a start of 0', () => {
    const part = speakerTranscriptOf([
      {
        type: 'speaker_transcript',
        text: 'a b',
        turns: [
          { speaker: '0', start: 0, end: 2, text: 'a' },
          { speaker: '1', text: 'b' },
        ],
      },
    ]);
    expect(part?.turns[0]).toEqual({ speaker: '0', start: 0, end: 2, text: 'a' });
    expect(part?.turns[1]).toEqual({ speaker: '1', text: 'b' });
    expect(part?.turns[1]).not.toHaveProperty('start');
    expect(part?.turns[1]).not.toHaveProperty('end');
  });

  // The part is persisted JSON the portal reads back as history, so a part of an unexpected
  // shape must read as a smaller transcript rather than crash the row.
  describe('a part of an unexpected shape (history)', () => {
    const malformed = (part: Record<string, unknown>) =>
      speakerTranscriptOf([part] as unknown as ChatContentPart[]);

    it('reads turns that are no list as no turns, keeping the transcript text', () => {
      expect(malformed({ type: 'speaker_transcript', text: 'whole', turns: 'nope' })).toEqual({
        type: 'speaker_transcript',
        text: 'whole',
        turns: [],
      });
      expect(malformed({ type: 'speaker_transcript', text: 'whole' })?.turns).toEqual([]);
    });

    it('reads a text that is no string as empty', () => {
      expect(malformed({ type: 'speaker_transcript', turns: [] })?.text).toBe('');
    });

    it('drops entries that are no turn objects and mends the fields of one that is', () => {
      const part = malformed({
        type: 'speaker_transcript',
        text: 'x',
        turns: [
          null,
          'oops',
          42,
          { speaker: 7, start: 'soon', end: Number.NaN, text: 9 },
          { speaker: '1', start: 1, end: Number.POSITIVE_INFINITY, text: 'ok' },
        ],
      });
      expect(part?.turns).toEqual([
        { speaker: '', text: '' },
        { speaker: '1', start: 1, text: 'ok' },
      ]);
    });
  });
});
