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
  });
}
