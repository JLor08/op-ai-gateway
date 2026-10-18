// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TranscriptionPendingTurn } from './TranscriptionPendingTurn';
import { messages, type Locale } from '../i18n';

afterEach(() => {
  cleanup();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`TranscriptionPendingTurn [${locale}]`, () => {
    it('shows the role, the wait label, the server-anchored clock and the no-news sentence', () => {
      render(<TranscriptionPendingTurn t={t} elapsedMs={107_000} roleLabel={t.assistantRole} />);
      expect(screen.getByText(t.assistantRole)).toBeInTheDocument();
      expect(screen.getByText(t.chatTranscriptionRunPending)).toBeInTheDocument();
      expect(screen.getByText('1:47')).toBeInTheDocument();
      expect(screen.getByText(t.chatTranscriptionNoIntermediateNews)).toBeInTheDocument();
    });

    // Before the first text the run has emitted no incremental event (a streaming model's first
    // delta replaces this turn, a one-shot model's answer ends the run), so there is nothing to
    // put a number or a bar on, and no text yet for a character counter to count.
    it('shows no progress figure, no reasoning summary and no player', () => {
      const { container } = render(
        <TranscriptionPendingTurn t={t} elapsedMs={3_000} roleLabel={t.assistantRole} />,
      );
      expect(screen.queryByRole('progressbar')).toBeNull();
      expect(container.textContent).not.toMatch(/%/);
      expect(screen.queryByText(new RegExp(t.chatCharsUnit))).toBeNull();
      expect(screen.queryByText(new RegExp(t.chatReasoningActive))).toBeNull();
      expect(container.querySelector('audio')).toBeNull();
      expect(screen.queryByTestId('chat-stream-cursor')).toBeNull();
    });

    it('is an assistant article, so it sits where the transcript will land', () => {
      const { container } = render(
        <TranscriptionPendingTurn t={t} elapsedMs={0} roleLabel={t.assistantRole} />,
      );
      expect(container.querySelector('article[data-role="assistant"]')).not.toBeNull();
    });

    it('hides the ticking clock from assistive technology, but not the label or sentence', () => {
      render(<TranscriptionPendingTurn t={t} elapsedMs={5_000} roleLabel={t.assistantRole} />);
      expect(screen.getByText('5s').closest('[aria-hidden="true"]')).not.toBeNull();
      expect(
        screen.getByText(t.chatTranscriptionRunPending).closest('[aria-hidden="true"]'),
      ).toBeNull();
      expect(
        screen.getByText(t.chatTranscriptionNoIntermediateNews).closest('[aria-hidden="true"]'),
      ).toBeNull();
    });

    it('ticks the clock once a second from the server anchor', async () => {
      vi.useFakeTimers();
      try {
        render(<TranscriptionPendingTurn t={t} elapsedMs={3_000} roleLabel={t.assistantRole} />);
        expect(screen.getByText('3s')).toBeInTheDocument();
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2_000);
        });
        expect(screen.getByText('5s')).toBeInTheDocument();
      } finally {
        vi.useRealTimers();
      }
    });
  });
}
