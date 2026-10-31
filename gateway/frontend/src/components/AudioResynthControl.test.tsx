// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AudioResynthControl } from './AudioResynthControl';
import { messages, type Locale } from '../i18n';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// Counts the object URLs it hands out (blob:resynth-1, blob:resynth-2, ...) so a test can tell
// which one the player holds, and records which were revoked: the whole point of the control's
// lifecycle is that none of them is leaked.
function stubObjectUrls() {
  let n = 0;
  const createObjectURL = vi.fn((_blob: Blob) => {
    n += 1;
    return `blob:resynth-${n}`;
  });
  const revokeObjectURL = vi.fn();
  vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL });
  return { createObjectURL, revokeObjectURL };
}

// A synthesis the test settles by hand, so it can land before, after or never relative to the
// control's unmount.
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function wav(bytes = [1, 2, 3, 4]): Blob {
  return new Blob([new Uint8Array(bytes)], { type: 'audio/wav' });
}

// Lets a settled promise's continuation run (and anything it set in state render).
async function flush() {
  await act(async () => {
    await Promise.resolve();
  });
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  // The play control of a spoken section whose stored audio was pruned: a click asks the gateway
  // for the audio again (the parent's onResynth, a Promise<Blob>) and the result plays from an
  // object URL the control owns. The section's text, which the parent renders, is the fallback
  // when that fails.
  describe(`AudioResynthControl [${locale}]`, () => {
    const playButton = () => screen.getByRole('button', { name: t.chatVoiceResynth });
    const player = () => screen.queryByLabelText(t.chatGeneratedAudio) as HTMLAudioElement | null;

    it('offers a play button and no player until it is clicked', () => {
      render(<AudioResynthControl t={t} onResynth={async () => wav()} />);
      expect(playButton()).toBeEnabled();
      expect(player()).toBeNull();
      expect(screen.queryByRole('alert')).toBeNull();
    });

    it('re-synthesizes once on click and plays an object URL made from the result', async () => {
      const { createObjectURL } = stubObjectUrls();
      const synth = deferred<Blob>();
      const onResynth = vi.fn(() => synth.promise);
      render(<AudioResynthControl t={t} onResynth={onResynth} />);

      fireEvent.click(playButton());

      // While the audio is being made the button is busy: it cannot be clicked again, and
      // there is no player yet.
      expect(onResynth).toHaveBeenCalledTimes(1);
      expect(playButton()).toBeDisabled();
      expect(player()).toBeNull();

      const blob = wav();
      synth.resolve(blob);

      await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:resynth-1'));
      expect(player()).toHaveAttribute('controls');
      expect(player()).toHaveProperty('autoplay', true);
      expect(createObjectURL).toHaveBeenCalledTimes(1);
      expect(createObjectURL).toHaveBeenCalledWith(blob);
      expect(onResynth).toHaveBeenCalledTimes(1);
      expect(playButton()).toBeEnabled();
      expect(screen.queryByRole('alert')).toBeNull();
    });

    it('shows an inline error and no player when the synthesis fails', async () => {
      const { createObjectURL } = stubObjectUrls();
      const onResynth = vi.fn(async () => {
        throw new Error('synth failed');
      });
      render(<AudioResynthControl t={t} onResynth={onResynth} />);

      fireEvent.click(playButton());

      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatVoiceResynthError);
      expect(player()).toBeNull();
      expect(document.querySelector('audio')).toBeNull();
      expect(createObjectURL).not.toHaveBeenCalled();
      // The failure is not a dead end: the button is free to try again.
      expect(playButton()).toBeEnabled();
    });

    it('treats an empty (zero-byte) result as a failure, not as silent audio', async () => {
      const { createObjectURL } = stubObjectUrls();
      render(<AudioResynthControl t={t} onResynth={async () => new Blob([])} />);

      fireEvent.click(playButton());

      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatVoiceResynthError);
      expect(document.querySelector('audio')).toBeNull();
      expect(createObjectURL).not.toHaveBeenCalled();
    });

    it('shows the error when the object URL cannot be made', async () => {
      const { createObjectURL } = stubObjectUrls();
      createObjectURL.mockImplementation(() => {
        throw new Error('no object url');
      });
      render(<AudioResynthControl t={t} onResynth={async () => wav()} />);

      fireEvent.click(playButton());

      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatVoiceResynthError);
      expect(document.querySelector('audio')).toBeNull();
    });

    it('clears the error line when the next attempt succeeds', async () => {
      stubObjectUrls();
      const onResynth = vi
        .fn<() => Promise<Blob>>()
        .mockRejectedValueOnce(new Error('synth failed'))
        .mockResolvedValueOnce(wav());
      render(<AudioResynthControl t={t} onResynth={onResynth} />);

      fireEvent.click(playButton());
      await screen.findByRole('alert');

      fireEvent.click(playButton());
      await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:resynth-1'));
      expect(screen.queryByRole('alert')).toBeNull();
    });

    describe('the object URL it owns', () => {
      it('is revoked when the control unmounts', async () => {
        const { revokeObjectURL } = stubObjectUrls();
        const { unmount } = render(<AudioResynthControl t={t} onResynth={async () => wav()} />);
        fireEvent.click(playButton());
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:resynth-1'));
        expect(revokeObjectURL).not.toHaveBeenCalled();

        unmount();

        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:resynth-1');
      });

      it('is revoked before a second click re-synthesizes, and the new one is played', async () => {
        const { createObjectURL, revokeObjectURL } = stubObjectUrls();
        const onResynth = vi.fn(async () => wav());
        render(<AudioResynthControl t={t} onResynth={onResynth} />);
        fireEvent.click(playButton());
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:resynth-1'));

        fireEvent.click(playButton());

        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:resynth-2'));
        expect(onResynth).toHaveBeenCalledTimes(2);
        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:resynth-1');
        // Revoked BEFORE the next one was made, not after: the old clip is not held while the
        // new one is synthesized.
        expect(revokeObjectURL.mock.invocationCallOrder[0]).toBeLessThan(
          createObjectURL.mock.invocationCallOrder[1],
        );
        // And only the one still playing is left to revoke on unmount.
        cleanup();
        expect(revokeObjectURL).toHaveBeenCalledTimes(2);
        expect(revokeObjectURL).toHaveBeenLastCalledWith('blob:resynth-2');
      });

      it('is revoked, and the player gone, when a later attempt fails', async () => {
        const { revokeObjectURL } = stubObjectUrls();
        const onResynth = vi
          .fn<() => Promise<Blob>>()
          .mockResolvedValueOnce(wav())
          .mockRejectedValueOnce(new Error('synth failed'));
        render(<AudioResynthControl t={t} onResynth={onResynth} />);
        fireEvent.click(playButton());
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:resynth-1'));

        fireEvent.click(playButton());

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatVoiceResynthError);
        expect(document.querySelector('audio')).toBeNull();
        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:resynth-1');
        // Nothing is left that the unmount would have to revoke again.
        cleanup();
        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
      });

      it('is never created for a result that lands after the control unmounted', async () => {
        const { createObjectURL, revokeObjectURL } = stubObjectUrls();
        const synth = deferred<Blob>();
        const { unmount } = render(<AudioResynthControl t={t} onResynth={() => synth.promise} />);
        fireEvent.click(playButton());

        unmount();
        synth.resolve(wav());
        await flush();

        // Nothing would ever revoke an object URL made now, so none is made.
        expect(createObjectURL).not.toHaveBeenCalled();
        expect(revokeObjectURL).not.toHaveBeenCalled();
      });

      it('drops a rejection that lands after the control unmounted', async () => {
        const { createObjectURL } = stubObjectUrls();
        const synth = deferred<Blob>();
        const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
        const { unmount } = render(<AudioResynthControl t={t} onResynth={() => synth.promise} />);
        fireEvent.click(playButton());

        unmount();
        synth.reject(new Error('synth failed'));
        await flush();

        expect(createObjectURL).not.toHaveBeenCalled();
        expect(errors).not.toHaveBeenCalled();
      });
    });

    it('disables the play button when disabled, and a click does nothing', () => {
      const onResynth = vi.fn(async () => wav());
      render(<AudioResynthControl t={t} onResynth={onResynth} disabled />);

      expect(playButton()).toBeDisabled();
      fireEvent.click(playButton());
      expect(onResynth).not.toHaveBeenCalled();
    });
  });
}
