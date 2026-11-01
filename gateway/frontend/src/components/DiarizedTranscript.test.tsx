// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { DiarizedTranscript, diarizedTranscriptText } from './DiarizedTranscript';
import type { SpeakerTranscriptPart } from './shared/chatContent';
import { messages, type Locale } from '../i18n';

afterEach(cleanup);

const part = (turns: SpeakerTranscriptPart['turns'], text = 'the whole transcript') =>
  ({ type: 'speaker_transcript', text, turns }) satisfies SpeakerTranscriptPart;

const TWO_SPEAKERS = part([
  { speaker: '0', start: 0, end: 3.2, text: 'Good morning, everyone.' },
  { speaker: '1', start: 3.2, end: 5.9, text: 'Morning. Shall we begin?' },
  { speaker: '0', start: 5.9, end: 8, text: 'Yes, please.' },
]);

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`DiarizedTranscript [${locale}]`, () => {
    const turnBlocks = () => within(screen.getByRole('list')).getAllByRole('listitem');
    // The blocks, in order, each carrying the label of the given speaker number.
    const expectSpeakers = (numbers: readonly number[]) => {
      const blocks = turnBlocks();
      expect(blocks).toHaveLength(numbers.length);
      numbers.forEach((number, index) => {
        expect(
          within(blocks[index]).getByText(t.chatTranscriptionSpeaker(number)),
        ).toBeInTheDocument();
      });
    };

    it('renders one block per turn, each with its speaker label and its text', () => {
      render(<DiarizedTranscript t={t} part={TWO_SPEAKERS} />);

      const blocks = turnBlocks();
      expect(blocks).toHaveLength(3);
      expect(within(blocks[0]).getByText(t.chatTranscriptionSpeaker(1))).toBeInTheDocument();
      expect(within(blocks[0]).getByText('Good morning, everyone.')).toBeInTheDocument();
      expect(within(blocks[1]).getByText(t.chatTranscriptionSpeaker(2))).toBeInTheDocument();
      expect(within(blocks[1]).getByText('Morning. Shall we begin?')).toBeInTheDocument();
    });

    it('names the list, so the speaker view is announced as one', () => {
      render(<DiarizedTranscript t={t} part={TWO_SPEAKERS} />);

      expect(
        screen.getByRole('list', { name: t.chatTranscriptionSpeakerView }),
      ).toBeInTheDocument();
    });

    it('gives a speaker the same label on every turn, numbered by first appearance', () => {
      render(<DiarizedTranscript t={t} part={TWO_SPEAKERS} />);

      expectSpeakers([1, 2, 1]);
    });

    it('handles three or more speakers, each with a label of its own', () => {
      render(
        <DiarizedTranscript
          t={t}
          part={part([
            { speaker: '0', text: 'one' },
            { speaker: '1', text: 'two' },
            { speaker: '2', text: 'three' },
            { speaker: '3', text: 'four' },
            { speaker: '1', text: 'five' },
          ])}
        />,
      );

      expectSpeakers([1, 2, 3, 4, 2]);
    });

    it('does not assume the speaker ids are integers, in order, or few', () => {
      const ids = ['SPEAKER_07', 'alice', '12', 'Dr. Müller', 'SPEAKER_07'];
      render(
        <DiarizedTranscript
          t={t}
          part={part(ids.map((speaker) => ({ speaker, text: speaker })))}
        />,
      );

      expectSpeakers([1, 2, 3, 4, 1]);
    });

    it('labels a turn whose speaker id is the empty string, with no blank gap', () => {
      render(
        <DiarizedTranscript
          t={t}
          part={part([
            { speaker: '', text: 'who said this' },
            { speaker: '1', text: 'and this' },
            { speaker: '   ', text: 'and this one' },
          ])}
        />,
      );

      // A blank id is a speaker like any other: its own label, its own number.
      expectSpeakers([1, 2, 3]);
    });

    it('shows the time span of a turn that has one', () => {
      render(<DiarizedTranscript t={t} part={TWO_SPEAKERS} />);

      const blocks = turnBlocks();
      expect(within(blocks[0]).getByText('0:00–0:03')).toBeInTheDocument();
      expect(within(blocks[1]).getByText('0:03–0:06')).toBeInTheDocument();
      expect(within(blocks[2]).getByText('0:06–0:08')).toBeInTheDocument();
    });

    it('shows a start of 0 (the first turn of a clip), not only a truthy one', () => {
      render(
        <DiarizedTranscript t={t} part={part([{ speaker: '0', start: 0, end: 1, text: 'a' }])} />,
      );

      expect(screen.getByText('0:00–0:01')).toBeInTheDocument();
    });

    it('omits the time of a turn that has none', () => {
      render(
        <DiarizedTranscript
          t={t}
          part={part([
            { speaker: '0', start: 1, end: 2, text: 'timed' },
            { speaker: '1', text: 'untimed' },
          ])}
        />,
      );

      const [timed, untimed] = turnBlocks();
      expect(within(timed).getByText('0:01–0:02')).toBeInTheDocument();
      expect(within(untimed).queryByText(/\d:\d\d/)).toBeNull();
      expect(within(untimed).getByText('untimed')).toBeInTheDocument();
    });

    it('shows no time at all when no turn has one', () => {
      render(
        <DiarizedTranscript
          t={t}
          part={part([
            { speaker: '0', text: 'a' },
            { speaker: '1', text: 'b' },
          ])}
        />,
      );

      expect(screen.queryByText(/\d:\d\d/)).toBeNull();
    });

    it('writes a time past a minute and past an hour as m:ss and h:mm:ss', () => {
      render(
        <DiarizedTranscript
          t={t}
          part={part([
            { speaker: '0', start: 61, end: 125.4, text: 'a' },
            { speaker: '1', start: 3723, end: 3725, text: 'b' },
          ])}
        />,
      );

      expect(screen.getByText('1:01–2:05')).toBeInTheDocument();
      expect(screen.getByText('1:02:03–1:02:05')).toBeInTheDocument();
    });

    it('keeps the line breaks of a turn text', () => {
      render(
        <DiarizedTranscript t={t} part={part([{ speaker: '0', text: 'line one\nline two' }])} />,
      );

      const text = screen.getByText(/line one/);
      expect(text).toHaveTextContent('line one line two');
      expect(getComputedStyle(text).whiteSpace).toBe('pre-wrap');
    });

    it('shows the whole transcript as one plain text in the raw view, with no labels', () => {
      render(<DiarizedTranscript t={t} part={TWO_SPEAKERS} raw />);

      expect(screen.getByText('the whole transcript')).toBeInTheDocument();
      expect(screen.queryByRole('list')).toBeNull();
      expect(screen.queryByText(t.chatTranscriptionSpeaker(1))).toBeNull();
    });

    it('falls back to the plain transcript when there are no turns to show', () => {
      render(<DiarizedTranscript t={t} part={part([], 'only the text survived')} />);

      expect(screen.getByText('only the text survived')).toBeInTheDocument();
      expect(screen.queryByRole('list')).toBeNull();
    });
  });

  describe(`diarizedTranscriptText [${locale}]`, () => {
    it('is the transcript line by line, each turn behind its speaker label', () => {
      expect(diarizedTranscriptText(t, TWO_SPEAKERS)).toBe(
        [
          `${t.chatTranscriptionSpeaker(1)}: Good morning, everyone.`,
          `${t.chatTranscriptionSpeaker(2)}: Morning. Shall we begin?`,
          `${t.chatTranscriptionSpeaker(1)}: Yes, please.`,
        ].join('\n'),
      );
    });

    it('is the plain transcript when there are no turns', () => {
      expect(diarizedTranscriptText(t, part([], 'just text'))).toBe('just text');
    });
  });
}
