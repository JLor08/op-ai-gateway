// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { segmentAnswer, type AnswerSegment, type SpokenSection } from './spokenSections';

// The span of `needle` in `text` as the gateway persists it: a RUNE (code point) offset and
// length, so a multi-byte character before it moves neither.
function spanOf(text: string, needle: string, from = 0): SpokenSection {
  const at = text.indexOf(needle, from);
  return {
    text: needle,
    start: Array.from(text.slice(0, at)).length,
    len: Array.from(needle).length,
  };
}

const body = (text: string): AnswerSegment => ({ kind: 'body', text });
const section = (text: string, index: number): AnswerSegment => ({ kind: 'section', text, index });

describe('segmentAnswer', () => {
  it('splits the answer into [body][section][body][section]... at the spans', () => {
    const text = 'Sum one.\n\nThe middle detail.\n\nSum two.\n\nThe closing detail.';

    expect(segmentAnswer(text, [spanOf(text, 'Sum one.'), spanOf(text, 'Sum two.')])).toEqual([
      section('Sum one.', 0),
      body('The middle detail.'),
      section('Sum two.', 1),
      body('The closing detail.'),
    ]);
  });

  it('keeps the words before the first section and after the last as body', () => {
    const text = 'Before.\n\nSum.\n\nAfter.';

    expect(segmentAnswer(text, [spanOf(text, 'Sum.')])).toEqual([
      body('Before.'),
      section('Sum.', 0),
      body('After.'),
    ]);
  });

  it('leaves no body segment between sections that touch, nor around a bare answer', () => {
    const text = 'One.\n\nTwo.';

    expect(segmentAnswer(text, [spanOf(text, 'One.'), spanOf(text, 'Two.')])).toEqual([
      section('One.', 0),
      section('Two.', 1),
    ]);
  });

  it('trims the seams: no trailing space on the lead-in, no leading blank lines on the tail', () => {
    const text = 'Lead-in   Sum.\n\n\n  Indented tail.';

    expect(segmentAnswer(text, [spanOf(text, 'Sum.')])).toEqual([
      body('Lead-in'),
      section('Sum.', 0),
      body('  Indented tail.'),
    ]);
  });

  it('slices by RUNE offset: a multi-byte character before a section does not shift the cut', () => {
    // 'Käse 🧀 Sum. Sum. Detail.': the second "Sum." starts at code point 12 (an umlaut and an
    // emoji sit before it, so a UTF-16 unit or byte offset would land elsewhere).
    const text = 'Käse 🧀 Sum. Sum. Detail.';

    expect(segmentAnswer(text, [{ text: 'Sum.', start: 12, len: 4 }])).toEqual([
      body('Käse 🧀 Sum.'),
      section('Sum.', 0),
      body('Detail.'),
    ]);
  });

  it('treats the section text as optional: the span alone locates it', () => {
    expect(segmentAnswer('Sum.\n\nDetail.', [{ start: 0, len: 4 }])).toEqual([
      section('Sum.', 0),
      body('Detail.'),
    ]);
  });

  it('finds a section by value when its span does not line up, after the previous section', () => {
    const text = 'Hi. mid Hi. end';

    // The second span points nowhere; its text is found AFTER the first section (not at it).
    expect(
      segmentAnswer(text, [
        { text: 'Hi.', start: 0, len: 3 },
        { text: 'Hi.', start: 99, len: 3 },
      ]),
    ).toEqual([section('Hi.', 0), body('mid'), section('Hi.', 1), body('end')]);
  });

  it('shows a section it cannot place with its text and leaves the answer whole', () => {
    // Showing a spoken text twice beats dropping part of the answer.
    expect(segmentAnswer('Something else.', [{ text: 'Sum.', start: 0, len: 4 }])).toEqual([
      section('Sum.', 0),
      body('Something else.'),
    ]);
  });

  it('drops a section that has neither a usable span nor a text, keeping the others indexed', () => {
    const text = 'One.\n\nDetail.\n\nThree.';

    expect(
      segmentAnswer(text, [spanOf(text, 'One.'), { start: 500, len: 4 }, spanOf(text, 'Three.')]),
    ).toEqual([section('One.', 0), body('Detail.'), section('Three.', 2)]);
  });

  it('does not let a span reach back over an earlier section', () => {
    const text = 'One. Two. Rest.';

    // The second section claims to start inside the first: it is placed by value after it.
    expect(
      segmentAnswer(text, [
        { text: 'One.', start: 0, len: 4 },
        { text: 'Two.', start: 2, len: 4 },
      ]),
    ).toEqual([section('One.', 0), section('Two.', 1), body('Rest.')]);
  });

  it('returns the whole text as one body when there are no sections', () => {
    expect(segmentAnswer('Just text.', [])).toEqual([body('Just text.')]);
  });
});
