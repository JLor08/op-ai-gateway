// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AudioNotStoredTurn } from './AudioNotStoredTurn';
import { messages, type Locale } from '../i18n';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  // The placeholder of an answer whose audio is gone (pruned, deleted, or never stored): one
  // line and a way to get the audio back -- no player and no download, there is nothing
  // to play or save.
  describe(`AudioNotStoredTurn [${locale}]`, () => {
    it('says the audio is not stored', () => {
      render(<AudioNotStoredTurn t={t} onRegenerate={() => {}} />);
      expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
    });

    it('is one named group: the line and its regenerate belong together', () => {
      render(<AudioNotStoredTurn t={t} onRegenerate={() => {}} />);
      const group = screen.getByRole('group', { name: t.chatAudioNotStored });
      expect(group).toContainElement(screen.getByRole('button', { name: t.chatRegenerate }));
    });

    it('offers a regenerate button under the existing regenerate label', () => {
      render(<AudioNotStoredTurn t={t} onRegenerate={() => {}} />);
      expect(screen.getByRole('button', { name: t.chatRegenerate })).toBeEnabled();
    });

    it('calls onRegenerate when regenerate is clicked', () => {
      const onRegenerate = vi.fn();
      render(<AudioNotStoredTurn t={t} onRegenerate={onRegenerate} />);

      fireEvent.click(screen.getByRole('button', { name: t.chatRegenerate }));

      expect(onRegenerate).toHaveBeenCalledTimes(1);
    });

    it('has no player and no download', () => {
      const { container } = render(<AudioNotStoredTurn t={t} onRegenerate={() => {}} />);
      expect(container.querySelector('audio')).toBeNull();
      expect(screen.queryByLabelText(t.chatGeneratedAudio)).toBeNull();
      expect(screen.queryByRole('button', { name: t.chatDownloadAudio })).toBeNull();
    });

    it('disables regenerate while a run may not start, like the other run-starting controls', () => {
      const onRegenerate = vi.fn();
      render(<AudioNotStoredTurn t={t} onRegenerate={onRegenerate} disabled />);

      const button = screen.getByRole('button', { name: t.chatRegenerate });
      expect(button).toBeDisabled();
      fireEvent.click(button);
      expect(onRegenerate).not.toHaveBeenCalled();
    });

    it('is the placeholder line alone where there is nothing to regenerate with', () => {
      render(<AudioNotStoredTurn t={t} />);
      expect(screen.getByText(t.chatAudioNotStored)).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.chatRegenerate })).toBeNull();
    });

    // The per-section re-synthesis (#324): where the caller can make the section's audio again it
    // hands an `onResynth`, and the placeholder gains a play control beside the regenerate.
    describe('with a way to re-synthesize the section', () => {
      const resynth = () => Promise.resolve(new Blob([new Uint8Array([1, 2])]));

      it('offers the re-synth play control inside the placeholder group', () => {
        render(<AudioNotStoredTurn t={t} onResynth={resynth} />);

        const group = screen.getByRole('group', { name: t.chatAudioNotStored });
        expect(group).toContainElement(screen.getByRole('button', { name: t.chatVoiceResynth }));
      });

      it('asks for the audio when the control is clicked', () => {
        const onResynth = vi.fn(resynth);
        render(<AudioNotStoredTurn t={t} onResynth={onResynth} />);

        fireEvent.click(screen.getByRole('button', { name: t.chatVoiceResynth }));

        expect(onResynth).toHaveBeenCalledTimes(1);
      });

      it('keeps the regenerate beside it', () => {
        const onRegenerate = vi.fn();
        render(<AudioNotStoredTurn t={t} onRegenerate={onRegenerate} onResynth={resynth} />);

        fireEvent.click(screen.getByRole('button', { name: t.chatRegenerate }));

        expect(onRegenerate).toHaveBeenCalledTimes(1);
        expect(screen.getByRole('button', { name: t.chatVoiceResynth })).toBeEnabled();
      });

      it('disables the control with the same gate as the regenerate', () => {
        const onResynth = vi.fn(resynth);
        render(<AudioNotStoredTurn t={t} onRegenerate={() => {}} onResynth={onResynth} disabled />);

        const button = screen.getByRole('button', { name: t.chatVoiceResynth });
        expect(button).toBeDisabled();
        fireEvent.click(button);
        expect(onResynth).not.toHaveBeenCalled();
      });

      it('has no re-synth control without an onResynth', () => {
        render(<AudioNotStoredTurn t={t} onRegenerate={() => {}} />);
        expect(screen.queryByRole('button', { name: t.chatVoiceResynth })).toBeNull();
      });
    });
  });
}
