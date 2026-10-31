// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SpokenAnswer } from './SpokenAnswer';
import type { AnswerSegment } from './shared/spokenSections';
import { messages, type Locale } from '../i18n';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const AUDIO_A = 'data:audio/wav;base64,AAAA';
const AUDIO_B = 'data:audio/wav;base64,BBBB';

// A two-section answer as segmentAnswer lays it out: body around and between the sections.
const SEGMENTS: readonly AnswerSegment[] = [
  { kind: 'section', text: 'Sum one.', index: 0 },
  { kind: 'body', text: 'The middle detail.' },
  { kind: 'section', text: 'Sum two.', index: 1 },
  { kind: 'body', text: 'The closing detail.' },
];

const wav = () => Promise.resolve(new Blob([new Uint8Array([1, 2, 3, 4])], { type: 'audio/wav' }));

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  // The blob-gone branch of a voice answer's section blocks (#324): when the stored audio is gone
  // every block shows the not-stored placeholder, and -- where the caller can speak a section
  // again -- a play control that re-synthesizes THAT section's text.
  describe(`SpokenAnswer blob-gone branch [${locale}]`, () => {
    const renderRemoved = (
      props: Partial<Parameters<typeof SpokenAnswer>[0]> = {},
      segments: readonly AnswerSegment[] = SEGMENTS,
    ) =>
      render(
        <SpokenAnswer
          t={t}
          segments={segments}
          sectionCount={2}
          audioUrls={[]}
          audioRemoved
          turnId="a1"
          raw={false}
          canRun
          {...props}
        />,
      );
    const blocks = () => Array.from(document.querySelectorAll('details'));
    const resynthIn = (block: HTMLElement) =>
      within(block).queryByRole('button', { name: t.chatVoiceResynth });

    it('offers a re-synth control in EVERY section block', () => {
      renderRemoved({ onResynthSection: vi.fn(wav) });

      const [first, second] = blocks();
      expect(screen.getAllByRole('button', { name: t.chatVoiceResynth })).toHaveLength(2);
      expect(first.querySelector('summary')).toContainElement(resynthIn(first));
      expect(second.querySelector('summary')).toContainElement(resynthIn(second));
    });

    it("re-synthesizes the clicked block's own section text", () => {
      const onResynthSection = vi.fn(wav);
      renderRemoved({ onResynthSection });
      const [first, second] = blocks();

      fireEvent.click(resynthIn(second) as HTMLElement);
      expect(onResynthSection).toHaveBeenCalledTimes(1);
      expect(onResynthSection).toHaveBeenLastCalledWith('Sum two.');

      fireEvent.click(resynthIn(first) as HTMLElement);
      expect(onResynthSection).toHaveBeenCalledTimes(2);
      expect(onResynthSection).toHaveBeenLastCalledWith('Sum one.');
    });

    it("hands over the section's text, not a position, when a neighbour could not be placed", () => {
      const onResynthSection = vi.fn(wav);
      renderRemoved({ onResynthSection }, [
        { kind: 'section', text: 'Only the second.', index: 1 },
      ]);

      fireEvent.click(screen.getByRole('button', { name: t.chatVoiceResynth }));

      expect(onResynthSection).toHaveBeenCalledWith('Only the second.');
    });

    it('still offers the whole-answer regenerate on the FIRST block only', () => {
      const onRegenerate = vi.fn();
      renderRemoved({ onRegenerate, onResynthSection: vi.fn(wav) });

      const [first, second] = blocks();
      expect(screen.getAllByRole('button', { name: t.chatRegenerate })).toHaveLength(1);
      expect(within(first).getAllByRole('button', { name: t.chatRegenerate })).toHaveLength(1);
      expect(within(second).queryByRole('button', { name: t.chatRegenerate })).toBeNull();

      fireEvent.click(screen.getByRole('button', { name: t.chatRegenerate }));
      expect(onRegenerate).toHaveBeenCalledTimes(1);
    });

    it('keeps the not-stored placeholder in every block beside the control', () => {
      renderRemoved({ onRegenerate: () => {}, onResynthSection: vi.fn(wav) });

      const placeholders = screen.getAllByRole('group', { name: t.chatAudioNotStored });
      expect(placeholders).toHaveLength(2);
      for (const placeholder of placeholders) {
        expect(
          within(placeholder).getByRole('button', { name: t.chatVoiceResynth }),
        ).toBeInTheDocument();
      }
    });

    it('closes the control with the same gate as the regenerate (no run while unavailable)', () => {
      const onResynthSection = vi.fn(wav);
      renderRemoved({ onRegenerate: () => {}, onResynthSection, canRun: false });

      for (const button of screen.getAllByRole('button', { name: t.chatVoiceResynth })) {
        expect(button).toBeDisabled();
      }
      fireEvent.click(screen.getAllByRole('button', { name: t.chatVoiceResynth })[0]);
      expect(onResynthSection).not.toHaveBeenCalled();
    });

    it("renders today's placeholders unchanged without onResynthSection", () => {
      renderRemoved({ onRegenerate: () => {} });

      const [first, second] = blocks();
      expect(screen.getAllByRole('group', { name: t.chatAudioNotStored })).toHaveLength(2);
      expect(screen.queryByRole('button', { name: t.chatVoiceResynth })).toBeNull();
      expect(within(first).getAllByRole('button', { name: t.chatRegenerate })).toHaveLength(1);
      expect(within(second).queryByRole('button', { name: t.chatRegenerate })).toBeNull();
    });

    it('is the bare placeholder in a block with neither a regenerate nor a re-synth', () => {
      renderRemoved();

      expect(screen.getAllByRole('group', { name: t.chatAudioNotStored })).toHaveLength(2);
      expect(screen.queryByRole('button')).toBeNull();
    });

    it('offers no re-synth where the audio is not marked removed (text-only blocks)', () => {
      renderRemoved({ audioRemoved: false, onResynthSection: vi.fn(wav) });

      expect(blocks()).toHaveLength(2);
      expect(screen.queryByRole('group', { name: t.chatAudioNotStored })).toBeNull();
      expect(screen.queryByRole('button', { name: t.chatVoiceResynth })).toBeNull();
    });

    it('offers no re-synth beside audio that is still stored and paired', () => {
      renderRemoved({
        audioUrls: [AUDIO_A, AUDIO_B],
        audioRemoved: false,
        onResynthSection: vi.fn(wav),
      });

      expect(screen.getAllByLabelText(t.chatGeneratedAudio)).toHaveLength(2);
      expect(screen.queryByRole('button', { name: t.chatVoiceResynth })).toBeNull();
    });

    it('offers no re-synth for audio that cannot be paired with the sections', () => {
      renderRemoved({
        audioUrls: [AUDIO_A, AUDIO_B, AUDIO_A],
        audioRemoved: false,
        onResynthSection: vi.fn(wav),
      });

      expect(screen.queryByRole('button', { name: t.chatVoiceResynth })).toBeNull();
      expect(screen.queryByRole('group', { name: t.chatAudioNotStored })).toBeNull();
    });
  });
}
