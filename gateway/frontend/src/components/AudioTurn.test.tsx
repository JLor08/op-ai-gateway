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
const THIRD_BLOB_URL = '/api/portal/chat-blobs/3c7e9d10-5a41-4b8e-9f27-1d2c3b4a5e6f';
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

// A canonical mono 16-bit WAV of the given sample rate holding `pcm` (what a speech part is).
function wavBytes(pcm: number[], rate = 16000): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(44 + pcm.length);
  const view = new DataView(out.buffer);
  const ascii = (offset: number, text: string) =>
    text.split('').forEach((c, i) => view.setUint8(offset + i, c.charCodeAt(0)));
  ascii(0, 'RIFF');
  view.setUint32(4, 36 + pcm.length, true);
  ascii(8, 'WAVE');
  ascii(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, rate, true);
  view.setUint32(28, rate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, 'data');
  view.setUint32(40, pcm.length, true);
  out.set(pcm, 44);
  return out;
}

function wavResponseOf(bytes: Uint8Array<ArrayBuffer>) {
  return {
    ok: true,
    status: 200,
    blob: async () => new Blob([bytes], { type: 'audio/wav' }),
  };
}

function dataUrlOf(bytes: Uint8Array<ArrayBuffer>): string {
  return `data:audio/wav;base64,${btoa(String.fromCharCode(...bytes))}`;
}

// The PCM of a stitched clip (everything after its 44-byte header) and its header's data size.
async function stitched(blob: Blob) {
  const bytes = new Uint8Array(await blob.arrayBuffer());
  const view = new DataView(bytes.buffer);
  return {
    type: blob.type,
    dataSize: view.getUint32(40, true),
    rate: view.getUint32(24, true),
    pcm: Array.from(bytes.slice(44)),
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

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);

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

      // One part is today's path, untouched: the blob the server served is what plays, as it
      // is -- not parsed, not rewrapped (these bytes are not even a WAV).
      it('plays the very blob it was served when there is only one part', async () => {
        const { createObjectURL } = stubObjectUrls();
        const served = new Blob([new Uint8Array([1, 2, 3, 4])], { type: 'audio/wav' });
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => ({ ok: true, status: 200, blob: async () => served })),
        );

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);

        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        expect(createObjectURL).toHaveBeenCalledTimes(1);
        expect(createObjectURL.mock.calls[0][0]).toBe(served);
        expect(screen.getAllByLabelText(t.chatGeneratedAudio)).toHaveLength(1);
      });

      it('has no source while the bytes are on their way, and no error', () => {
        stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(() => new Promise(() => {})),
        );

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);

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

        const { unmount } = render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);
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

        const { rerender } = render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));

        rerender(<AudioTurn t={t} urls={[OTHER_BLOB_URL]} turnId="msg_a1b2" />);

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

        const { unmount } = render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);
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

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);

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

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);

        expect(await screen.findByText(t.chatAudioLoadError)).toBeInTheDocument();
      });
    });

    // A long answer is saved as several ordered parts. The assistant player stitches them
    // into ONE clip: one <audio>, one object URL, and one download of the whole answer.
    describe('a reply saved as several parts', () => {
      const PARTS = [BLOB_URL, OTHER_BLOB_URL, THIRD_BLOB_URL];
      const PCM_OF: Record<string, number[]> = {
        [BLOB_URL]: [1, 0, 2, 0],
        [OTHER_BLOB_URL]: [3, 0, 4, 0, 5, 0],
        [THIRD_BLOB_URL]: [6, 0],
      };
      const ALL_PCM = [1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 6, 0];
      const servedParts = () => vi.fn(async (url: string) => wavResponseOf(wavBytes(PCM_OF[url])));

      it('renders ONE player that plays one object URL made from the stitched parts', async () => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = servedParts();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);

        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        expect(screen.getAllByLabelText(t.chatGeneratedAudio)).toHaveLength(1);
        expect(screen.getAllByRole('button', { name: t.chatDownloadAudio })).toHaveLength(1);
        // Every part, same-origin with the session cookie, once.
        expect(fetchMock).toHaveBeenCalledTimes(3);
        for (const url of PARTS) {
          expect(fetchMock).toHaveBeenCalledWith(
            url,
            expect.objectContaining({ credentials: 'same-origin' }),
          );
        }
        expect(createObjectURL).toHaveBeenCalledTimes(1);
        expect(await stitched(createObjectURL.mock.calls[0][0] as Blob)).toEqual({
          type: 'audio/wav',
          dataSize: ALL_PCM.length,
          rate: 16000,
          pcm: ALL_PCM,
        });
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      });

      it('has no source while the parts are on their way, and no error', () => {
        stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(() => new Promise(() => {})),
        );

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);

        expect(player()).not.toHaveAttribute('src');
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      });

      it('downloads the SAME stitched blob as one file, without fetching again', async () => {
        const { createObjectURL } = stubObjectUrls();
        const names = recordDownloads();
        const fetchMock = servedParts();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['generated-audio-msg_a1b2.wav']));
        expect(fetchMock).toHaveBeenCalledTimes(3);
        // Playback's object URL, then the download's: both made from the one stitched blob.
        expect(createObjectURL).toHaveBeenCalledTimes(2);
        expect(createObjectURL.mock.calls[1][0]).toBe(createObjectURL.mock.calls[0][0]);
        expect(screen.queryByText(t.chatAudioDownloadError)).not.toBeInTheDocument();
      });

      // Playback failing (one part missing) must not make the answer unsaveable once the part
      // is back: the download builds the clip itself, like a single part's download fetches.
      it('downloads by stitching the parts itself when playback never got them', async () => {
        const { createObjectURL } = stubObjectUrls();
        const names = recordDownloads();
        let missing = true;
        const fetchMock = vi.fn(async (url: string) =>
          url === OTHER_BLOB_URL && missing
            ? { ok: false, status: 404, blob: async () => new Blob([]) }
            : wavResponseOf(wavBytes(PCM_OF[url])),
        );
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);
        expect(await screen.findByText(t.chatAudioLoadError)).toBeInTheDocument();
        expect(player()).not.toHaveAttribute('src');
        expect(createObjectURL).not.toHaveBeenCalled();

        missing = false;
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['generated-audio-msg_a1b2.wav']));
        expect(await stitched(createObjectURL.mock.calls[0][0] as Blob)).toMatchObject({
          pcm: ALL_PCM,
        });
        expect(screen.queryByText(t.chatAudioDownloadError)).not.toBeInTheDocument();
      });

      it('tells the user when the clip cannot be saved', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => ({ ok: false, status: 500, blob: async () => new Blob([]) })),
        );

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);
        await screen.findByText(t.chatAudioLoadError);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        expect(await screen.findByText(t.chatAudioDownloadError)).toBeInTheDocument();
        expect(names).toEqual([]);
      });

      it('tells the user when one part cannot be fetched, and plays nothing', async () => {
        const { createObjectURL } = stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(async (url: string) =>
            url === THIRD_BLOB_URL
              ? { ok: false, status: 404, blob: async () => new Blob([]) }
              : wavResponseOf(wavBytes(PCM_OF[url])),
          ),
        );

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);

        expect(await screen.findByText(t.chatAudioLoadError)).toBeInTheDocument();
        expect(player()).not.toHaveAttribute('src');
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      // Parts of different formats would play at the wrong speed: an error, never garbage.
      it('tells the user when the parts are not all of one format', async () => {
        const { createObjectURL } = stubObjectUrls();
        vi.stubGlobal(
          'fetch',
          vi.fn(async (url: string) =>
            wavResponseOf(wavBytes(PCM_OF[url], url === OTHER_BLOB_URL ? 24000 : 16000)),
          ),
        );

        render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);

        expect(await screen.findByText(t.chatAudioLoadError)).toBeInTheDocument();
        expect(player()).not.toHaveAttribute('src');
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      it('revokes the stitched object URL when the turn unmounts', async () => {
        const { revokeObjectURL } = stubObjectUrls();
        vi.stubGlobal('fetch', servedParts());

        const { unmount } = render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        expect(revokeObjectURL).not.toHaveBeenCalled();

        unmount();

        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:audio-1');
      });

      it('creates no object URL when it unmounts before the parts arrive', async () => {
        const { createObjectURL, revokeObjectURL } = stubObjectUrls();
        const resolvers: Array<(r: ReturnType<typeof wavResponseOf>) => void> = [];
        const fetchMock = vi.fn(
          () =>
            new Promise<ReturnType<typeof wavResponseOf>>((resolve) => {
              resolvers.push(resolve);
            }),
        );
        vi.stubGlobal('fetch', fetchMock);

        const { unmount } = render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);
        expect(fetchMock).toHaveBeenCalledTimes(3);
        unmount();
        for (const call of fetchMock.mock.calls as unknown as Array<[string, RequestInit]>) {
          expect(call[1].signal?.aborted).toBe(true);
        }
        resolvers.forEach((resolve, i) => resolve(wavResponseOf(wavBytes([i, 0]))));
        await new Promise((resolve) => setTimeout(resolve, 0));

        expect(createObjectURL).not.toHaveBeenCalled();
        expect(revokeObjectURL).not.toHaveBeenCalled();
      });

      // ChatMessage builds a fresh urls array on every render: the SAME parts must not be
      // fetched again for it, and different parts must replace the clip.
      it('does not refetch for a new array of the same parts, but does for different ones', async () => {
        const { revokeObjectURL } = stubObjectUrls();
        const fetchMock = servedParts();
        vi.stubGlobal('fetch', fetchMock);

        const { rerender } = render(<AudioTurn t={t} urls={PARTS} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));

        rerender(<AudioTurn t={t} urls={[...PARTS]} turnId="msg_a1b2" />);
        expect(fetchMock).toHaveBeenCalledTimes(3);
        expect(revokeObjectURL).not.toHaveBeenCalled();
        expect(player()).toHaveAttribute('src', 'blob:audio-1');

        rerender(<AudioTurn t={t} urls={[BLOB_URL, THIRD_BLOB_URL]} turnId="msg_a1b2" />);
        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-2'));
        expect(fetchMock).toHaveBeenCalledTimes(5);
        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:audio-1');
      });

      // The run result carries inline parts until the chat is stored: those are decoded in
      // place and stitched like stored ones, with no request.
      it('stitches inline data: parts without a request', async () => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(
          <AudioTurn
            t={t}
            urls={[dataUrlOf(wavBytes([1, 0, 2, 0])), dataUrlOf(wavBytes([3, 0]))]}
            turnId="msg_a1b2"
          />,
        );

        await waitFor(() => expect(player()).toHaveAttribute('src', 'blob:audio-1'));
        expect(fetchMock).not.toHaveBeenCalled();
        expect(await stitched(createObjectURL.mock.calls[0][0] as Blob)).toMatchObject({
          pcm: [1, 0, 2, 0, 3, 0],
        });
      });

      it('refuses a part that is neither a chat blob nor inline audio, fetching nothing', () => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(
          <AudioTurn t={t} urls={[BLOB_URL, 'https://evil.example/reply.wav']} turnId="msg_a1b2" />,
        );

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatAudioLoadError);
        expect(player()).not.toHaveAttribute('src');
        expect(fetchMock).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
      });
    });

    describe('an inline data: reply', () => {
      it('plays the data: URL directly, with no fetch and no object URL', () => {
        const { createObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} urls={[DATA_URL]} turnId="msg_a1b2" />);

        expect(player()).toHaveAttribute('src', DATA_URL);
        expect(player()).toHaveAttribute('controls');
        expect(fetchMock).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
      });

      it('revokes nothing on unmount, since it created nothing', () => {
        const { revokeObjectURL } = stubObjectUrls();
        vi.stubGlobal('fetch', vi.fn());

        const { unmount } = render(<AudioTurn t={t} urls={[DATA_URL]} turnId="msg_a1b2" />);
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

        render(<AudioTurn t={t} urls={[url]} turnId="msg_a1b2" />);

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatAudioLoadError);
        expect(player()).not.toHaveAttribute('src');
        expect(fetchMock).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
      });
    });

    // The same player also shows a transcription turn's INPUT clip in the user's bubble
    // (variant "input"): it is the user's own audio, so it is announced and saved as such, not
    // as generated audio. Playback and the object-URL discipline are the one code path.
    describe('as the input clip of a user turn', () => {
      const clip = () => screen.getByLabelText(t.chatAttachedAudio) as HTMLAudioElement;

      it('is announced as the attached audio, not as generated audio', () => {
        stubObjectUrls();
        vi.stubGlobal('fetch', vi.fn());

        render(<AudioTurn t={t} urls={[DATA_URL]} turnId="msg_u1" variant="input" />);

        expect(clip()).toHaveAttribute('controls');
        expect(clip()).toHaveAttribute('src', DATA_URL);
        expect(screen.queryByLabelText(t.chatGeneratedAudio)).not.toBeInTheDocument();
      });

      it('plays a stored clip from one object URL and revokes it on unmount', async () => {
        const { createObjectURL, revokeObjectURL } = stubObjectUrls();
        const fetchMock = vi.fn(async () => wavResponse());
        vi.stubGlobal('fetch', fetchMock);

        const { unmount } = render(
          <AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_u1" variant="input" />,
        );
        await waitFor(() => expect(clip()).toHaveAttribute('src', 'blob:audio-1'));
        expect(fetchMock).toHaveBeenCalledTimes(1);
        expect(createObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).not.toHaveBeenCalled();

        unmount();

        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:audio-1');
      });

      it('saves under an input name, with the extension of the clip own type', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        vi.stubGlobal(
          'fetch',
          vi.fn(async () => ({
            ok: true,
            status: 200,
            blob: async () => new Blob([new Uint8Array([1])], { type: 'audio/mp4' }),
          })),
        );

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_u1" variant="input" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['input-audio-msg_u1.m4a']));
      });

      it('falls back to a bare input name without a turn id', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        vi.stubGlobal('fetch', vi.fn());

        render(
          <AudioTurn t={t} urls={['data:audio/webm;base64,AAAA']} turnId="" variant="input" />,
        );
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['input-audio.webm']));
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

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));

        await waitFor(() => expect(names).toEqual(['generated-audio-msg_a1b2.wav']));
        expect(screen.queryByText(t.chatAudioDownloadError)).not.toBeInTheDocument();
      });

      it('saves an inline reply as a .wav, decoded in place', async () => {
        stubObjectUrls();
        const names = recordDownloads();
        const fetchMock = vi.fn();
        vi.stubGlobal('fetch', fetchMock);

        render(<AudioTurn t={t} urls={[DATA_URL]} turnId="msg_a1b2" />);
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

        const first = render(<AudioTurn t={t} urls={[DATA_URL]} turnId="msg_first" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));
        first.unmount();
        const second = render(<AudioTurn t={t} urls={[DATA_URL]} turnId="msg_second" />);
        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadAudio }));
        second.unmount();
        render(<AudioTurn t={t} urls={[DATA_URL]} turnId="" />);
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
        render(<AudioTurn t={t} urls={['data:audio/wav;base64,not-valid-base64!!']} turnId="x" />);

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

        render(<AudioTurn t={t} urls={[BLOB_URL]} turnId="msg_a1b2" />);
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
