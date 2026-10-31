// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AudioPendingTurn } from './AudioPendingTurn';
import { messages, type Locale } from '../i18n';

afterEach(() => {
  cleanup();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  // The live controls of a streaming speech run (#281): Mute is the reversible, local silence
  // (the run goes on and is saved per the retention), Abort cancels the run itself.
  describe(`AudioPendingTurn live controls [${locale}]`, () => {
    const renderLive = (props: Partial<Parameters<typeof AudioPendingTurn>[0]> = {}) =>
      render(
        <AudioPendingTurn
          t={t}
          elapsedMs={4_000}
          roleLabel={t.assistantRole}
          live
          muted={false}
          onMute={vi.fn()}
          onAbort={vi.fn()}
          {...props}
        />,
      );

    it('offers Mute and Abort while the run plays live', () => {
      renderLive();
      expect(screen.getByRole('button', { name: t.chatAudioMute })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatAudioAbort })).toBeInTheDocument();
    });

    it('offers neither control while the run is not playing live (the silent wait)', () => {
      renderLive({ live: false });
      expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
      expect(screen.queryByRole('button', { name: t.chatAudioAbort })).toBeNull();
      expect(screen.getByText(t.chatAudioRunPending)).toBeInTheDocument();
    });

    it('offers no control whose handler it was not given', () => {
      renderLive({ onMute: undefined, onAbort: undefined });
      expect(screen.queryByRole('button')).toBeNull();
    });

    it('Mute asks to mute, its state carried by the label alone', () => {
      const onMute = vi.fn();
      renderLive({ onMute });
      const mute = screen.getByRole('button', { name: t.chatAudioMute });
      // One signal of its state, the label: no aria-pressed beside it (matches the
      // Voice Chat mutes' convention, #328).
      expect(mute).not.toHaveAttribute('aria-pressed');
      fireEvent.click(mute);
      expect(onMute).toHaveBeenCalledTimes(1);
      expect(onMute).toHaveBeenCalledWith(true);
    });

    it('shows Unmute while muted, its state carried by the label, and the click asks to unmute', () => {
      const onMute = vi.fn();
      renderLive({ muted: true, onMute });
      expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
      const unmute = screen.getByRole('button', { name: t.chatAudioUnmute });
      // One signal of its state, the label: no aria-pressed beside it (#328).
      expect(unmute).not.toHaveAttribute('aria-pressed');
      fireEvent.click(unmute);
      expect(onMute).toHaveBeenCalledWith(false);
    });

    it('Abort calls onAbort, and does not touch the mute', () => {
      const onAbort = vi.fn();
      const onMute = vi.fn();
      renderLive({ onAbort, onMute });
      fireEvent.click(screen.getByRole('button', { name: t.chatAudioAbort }));
      expect(onAbort).toHaveBeenCalledTimes(1);
      expect(onMute).not.toHaveBeenCalled();
    });

    it('keeps the live wording and the clock next to the controls', () => {
      renderLive();
      expect(screen.getByText(t.chatAudioStreamLive)).toBeInTheDocument();
      expect(screen.getByText('4s')).toBeInTheDocument();
    });
  });
}
