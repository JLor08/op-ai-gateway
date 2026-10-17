// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AudioTurn } from './AudioTurn';
import { messages, type Locale } from '../i18n';

// Captured once, before any test can have spied on it (vitest's retry: 2 would
// otherwise let a retried attempt capture the previous attempt's still-installed
// spy -- see ChatMessage.test.tsx for the full story).
const nativeCreateElement = document.createElement.bind(document);

const BLOB_URL = '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10';
const OTHER_BLOB_URL = '/api/portal/chat-blobs/0a9d3f3e-2b0c-4f55-8f55-6d6f1c5a7a11';
const DATA_URL = 'data:audio/wav;base64,AAAA';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function wavResponse(bytes = [1, 2, 3, 4]) {
  return {
    ok: true,
    status: 200,
    blob: async () => new Blob([new Uint8Array(bytes)], { type: 'audio/wav' }),
  };
}

// Counts the object URLs it hands out (blob:audio-1, blob:audio-2, ...) so a test can
// tell the PLAYBACK object URL from the one a download builds, and records which were
// revoked: the whole point of the playback path is that none of them is leaked.
function stubObjectUrls() {
  let n = 0;
  const created: Blob[] = [];
  const createObjectURL = vi.fn((blob: Blob) => {
    created.push(blob);
    n += 1;
    return `blob:audio-${n}`;
  });
  const revokeObjectURL = vi.fn();
  vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL });
  return { createObjectURL, revokeObjectURL, created };
}

// Records the file name of every anchor the download clicks (jsdom cannot navigate to
// the blob: URL it builds).
function recordDownloads() {
  const names: string[] = [];
  vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
    const el = nativeCreateElement(tag, options as ElementCreationOptions);
    if (tag === 'a') {
      const anchor = el as HTMLAnchorElement;
      anchor.click = () => names.push(anchor.download);
    }
    return el;
  });
  return names;
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`AudioTurn [${locale}]`, () => {
    const player = () => screen.getByLabelText(t.chatGeneratedAudio) as HTMLAudioElement;

    describe('a stored reply served by the gateway', () => {
      // The blob endpoint answers no Range requests (ADR-056), which Safari/iOS needs
      // to play a media url directly -- so the bytes are fetched once, same-origin,
      // and the <audio> element plays an object URL made from them.
      it('plays an object URL made from one same-origin fetch, not the endpoint url', async () => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn(async () => wavResponse());
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);

        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        expect(player()).toHaveAttribute('controls');
        expect(fetchMock).toHaveBeenCalledTimes(1);
        expect(fetchMock).toHaveBeenCalledWith(
          BLOB_URL,
          expect.objectContaining({ credentials: 'same-origin' }),
        );
        expect(createObjectURL).toHaveBeenCalledTimes(1);
        const blob = createObjectURL.mock.calls[0][0] as Blob;
        expect(blob.type).toBe('audio/wav');
        expect(blob.size).toBe(4);
      });

      it('has no source while the bytes are on their way, and no error', () => {
        stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(() => new Promise(() => {})),
        );

        render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);

        expect(player()).not.toHaveAttribute('src');
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      });

      // Review Focus 3: the object URL pins the whole WAV in memory until revoked.
      it('revokes the object URL when the turn unmounts', async () => {
        const { revokeObjectURL } = stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => wavResponse()),
        );

        const { unmount } = render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        expect(revokeObjectURL).not.toHaveBeenCalled();

        unmount();

        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:audio-1');
      });

      it('revokes the old object URL and plays the new one when the url changes', async () => {
        const { revokeObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn(async () => wavResponse());
        vi.stubGlobal('fetch', fetchMock);

        const { rerender } = render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));

        rerender(<AudioTurn t={t} url={OTHER_BLOB_URL} turnId="msg_a1b2" />);

        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-2'));
        expect(fetchMock).toHaveBeenCalledTimes(2);
        expect(fetchMock).toHaveBeenLastCalledWith(OTHER_BLOB_URL, expect.anything());
        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:audio-1');
      });

      // Unmounting while the request is in flight must not leave an object URL behind
      // that nothing will ever revoke: the late response is dropped (and the request
      // aborted so the bytes stop coming).
      it('creates no object URL when it unmounts before the fetch settles', async () => {
        const { createObjectURL, revokeObjectURL } = stubObjectUrls();
        let resolveFetch: (response: ReturnType<typeof wavResponse>) => void = () => {};
        const fetchMock = vi.fn(
          () =>
            new Promise<ReturnType<typeof wavResponse>>((resolve) => {
              resolveFetch = resolve;
            }),
        );
        vi.stubGlobal('fetch', fetchMock);

        const { unmount } = render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);
        expect(fetchMock).toHaveBeenCalledTimes(1);
        unmount();
        const init = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
        expect(init[1].signal?.aborted).toBe(true);

        resolveFetch(wavResponse());
        // Let every continuation of the (now abandoned) request run.
        await new Promise((resolve) => setTimeout(resolve, 0));

        expect(createObjectURL).not.toHaveBeenCalled();
        expect(revokeObjectURL).not.toHaveBeenCalled();
      });

      it('tells the user when the stored reply cannot be fetched', async () => {
        const { createObjectURL } = stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => ({ ok: false, status: 404, blob: async () => new Blob([]) })),
        );

        render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);

        expect(await screen.findByText(t.chatAudioLoadError)).toBeInTheDocument();
        expect(screen.getByRole('alert')).toHaveTextContent(t.chatAudioLoadError);
        expect(player()).not.toHaveAttribute('src');
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      it('tells the user, without throwing, when the request itself fails', async () => {
        stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => {
            throw new TypeError('Failed to fetch');
          }),
        );

        render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);

        expect(await screen.findByText(t.chatAudioLoadError)).toBeInTheDocument();
      });
    });

    describe('an inline data: reply', () => {
      it('plays the data: URL directly, with no fetch and no object URL', () => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} url={DATA_URL} turnId="msg_a1b2" />);

        expect(player()).toHaveAttribute('src', DATA_URL);
        expect(player()).toHaveAttribute('controls');
        expect(fetchMock).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      it('revokes nothing on unmount, since it created nothing', () => {
        const { revokeObjectURL } = stubObjectUrls();
        vi.stubGlobal('fetch', vi.fn());

        const { unmount } = render(<AudioTurn t={t} url={DATA_URL} turnId="msg_a1b2" />);
        unmount();

        expect(revokeObjectURL).not.toHaveBeenCalled();
      });
    });

    // The url comes out of a persisted transcript. Only the two shapes the gateway
    // produces are ever played or fetched (with the session cookie): anything else is
    // an error, not a cross-origin media load.
    describe('a url that is neither a chat blob nor inline audio', () => {
      it.each([
        'https://evil.example/reply.wav',
        '//evil.example/reply.wav',
        '/\\evil.example/reply.wav',
        '/some/other/path.wav',
        'blob:https://evil.example/1',
        'javascript:alert(1)',
        'data:text/html;base64,PHNjcmlwdD4=',
      ])('refuses %s without fetching it', (url) => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} url={url} turnId="msg_a1b2" />);

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatAudioLoadError);
        expect(player()).not.toHaveAttribute('src');
        expect(fetchMock).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
      });
    });

    describe('the download button', () => {
      it('saves a stored reply as a .wav named after the turn', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => wavResponse()),
        );

        render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['generated-audio-msg_a1b2.wav']));
        expect(screen.queryByText(t.chatAudioDownloadError)).not.toBeInTheDocument();
      });

      it('saves an inline reply as a .wav, decoded in place', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} url={DATA_URL} turnId="msg_a1b2" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['generated-audio-msg_a1b2.wav']));
        expect(fetchMock).not.toHaveBeenCalled();
      });

      // The turn id is in the name so two speech turns of one conversation do not both
      // offer a `generated-audio.wav` (the second save would become "(1)").
      it('names two turns differently, and falls back to a bare name without an id', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        vi.stubGlobal('fetch', vi.fn());

        const first = render(<AudioTurn t={t} url={DATA_URL} turnId="msg_first" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));
        first.unmount();
        const second = render(<AudioTurn t={t} url={DATA_URL} turnId="msg_second" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));
        second.unmount();
        render(<AudioTurn t={t} url={DATA_URL} turnId="" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toHaveLength(3));
        expect(names).toEqual([
          'generated-audio-msg_first.wav',
          'generated-audio-msg_second.wav',
          'generated-audio.wav',
        ]);
      });

      // downloadBinary reports a failure as `false`; it must not be silent.
      it('tells the user when the reply cannot be saved', async () => {
        stubObjectUrls();
        recordDownloads();
        // Plays fine (the data: URL is handed to <audio> as is) but does not decode.
        render(<AudioTurn t={t} url="data:audio/wav;base64,not-valid-base64!!" turnId="x" />);

        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        expect(await screen.findByText(t.chatAudioDownloadError)).toBeInTheDocument();
        expect(screen.getByRole('alert')).toHaveTextContent(t.chatAudioDownloadError);
      });

      it('clears an earlier failure once a later save succeeds', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        // Call 1 is the playback fetch, call 2 the first (failing) download, call 3 the
        // retry.
        const fetchMock = vi
          .fn()
          .mockResolvedValueOnce(wavResponse())
          .mockResolvedValueOnce({ ok: false, status: 500, blob: async () => new Blob([]) })
          .mockResolvedValueOnce(wavResponse());
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} url={BLOB_URL} turnId="msg_a1b2" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));
        expect(await screen.findByText(t.chatAudioDownloadError)).toBeInTheDocument();

        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toHaveLength(1));
        await waitFor(() =>
          expect(screen.queryByText(t.chatAudioDownloadError)).not.toBeInTheDocument(),
        );
      });
    });
  });
}
