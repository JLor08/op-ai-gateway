// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { downloadBinary, extensionFor } from './downloadBinary';

// Captured once, before any test can have spied on it. afterEach below
// restores mocks, but capturing the native function at module scope (rather
// than re-reading document.createElement inside each test body) is the
// belt-and-suspenders version: it can't ever bind to a still-installed spy
// left over from a retried attempt (vite.config.ts sets retry: 2).
const nativeCreateElement = document.createElement.bind(document);

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('extensionFor', () => {
  it('maps image/png to png', () => {
    expect(extensionFor('data:image/png;base64,AAAA')).toBe('png');
  });

  it('maps image/webp to webp', () => {
    expect(extensionFor('data:image/webp;base64,AAAA')).toBe('webp');
  });

  // The one branch a reader would not predict from the name alone: jpeg is
  // renamed to the conventional jpg extension rather than kept verbatim.
  it('renames image/jpeg to the jpg extension, not jpeg', () => {
    expect(extensionFor('data:image/jpeg;base64,AAAA')).toBe('jpg');
  });

  it('falls back to bin for a media type nothing here emits', () => {
    expect(extensionFor('data:application/octet-stream;base64,AAAA')).toBe('bin');
  });

  it('falls back to bin for a string that is not a data URL at all', () => {
    expect(extensionFor('not-a-data-url')).toBe('bin');
  });

  // A gateway url carries no media type of its own: the extension comes from
  // the served response's Content-Type, which is a bare media type (possibly
  // with parameters and in any case), not a data: URL prefix.
  it('maps a bare Content-Type media type, with the same jpeg -> jpg rename', () => {
    expect(extensionFor('image/png')).toBe('png');
    expect(extensionFor('image/jpeg')).toBe('jpg');
    expect(extensionFor('image/webp')).toBe('webp');
    expect(extensionFor('image/gif')).toBe('gif');
  });

  it('ignores Content-Type parameters and letter case', () => {
    expect(extensionFor('image/png; charset=binary')).toBe('png');
    expect(extensionFor('IMAGE/WEBP')).toBe('webp');
  });

  it('falls back to bin for an empty or non-image Content-Type', () => {
    expect(extensionFor('')).toBe('bin');
    expect(extensionFor('application/octet-stream')).toBe('bin');
    expect(extensionFor('text/html; charset=utf-8')).toBe('bin');
  });

  // The subtype of a served Content-Type is server input and ends up in a file
  // name, so only the formats the gateway stores map to an extension. Echoing
  // the subtype back would let "image/bat" save as "x.bat".
  it.each(['image/bat', 'image/svg+xml', 'image/x-icon', 'image/constructor', 'image/__proto__'])(
    'falls back to bin for the image subtype outside the allow-list: %s',
    (type) => {
      expect(extensionFor(type)).toBe('bin');
      expect(extensionFor(`data:${type};base64,AAAA`)).toBe('bin');
    },
  );

  // A speech turn's answer is a WAV (the only audio the chat stores, ADR-056).
  it('maps audio/wav to wav, from a data: URL and from a bare Content-Type', () => {
    expect(extensionFor('data:audio/wav;base64,AAAA')).toBe('wav');
    expect(extensionFor('audio/wav')).toBe('wav');
    expect(extensionFor('audio/wav; charset=binary')).toBe('wav');
    expect(extensionFor('AUDIO/WAV')).toBe('wav');
  });

  // A transcription turn's INPUT clip is stored as the type it was attached or recorded in (the
  // chat-blob allow-list: wav, webm, mpeg, mp4, ogg), so each saves under its own extension --
  // mp4 audio is the conventional .m4a, mpeg is .mp3.
  it.each([
    ['audio/webm', 'webm'],
    ['audio/mpeg', 'mp3'],
    ['audio/mp4', 'm4a'],
    ['audio/ogg', 'ogg'],
    ['audio/wav', 'wav'],
  ])(
    'maps the stored input type %s to .%s, from a data: URL and from a Content-Type',
    (type, ext) => {
      expect(extensionFor(`data:${type};base64,AAAA`)).toBe(ext);
      expect(extensionFor(type)).toBe(ext);
      expect(extensionFor(`${type}; codecs=opus`)).toBe(ext);
      expect(extensionFor(type.toUpperCase())).toBe(ext);
    },
  );

  // Same rule as for images: the served subtype is server input and lands in a
  // file name, so an audio subtype nothing stores is `bin`, never echoed.
  it.each(['audio/bat', 'audio/flac', 'audio/constructor', 'audio/__proto__'])(
    'falls back to bin for the audio subtype outside the allow-list: %s',
    (type) => {
      expect(extensionFor(type)).toBe('bin');
      expect(extensionFor(`data:${type};base64,AAAA`)).toBe('bin');
    },
  );

  // The allow-list is keyed on the WHOLE media type, not the subtype alone: a
  // known subtype under the wrong top-level type must not borrow its extension.
  it.each(['audio/png', 'image/wav', 'video/wav', 'text/wav'])(
    'does not mix a subtype with the wrong top-level type: %s',
    (type) => {
      expect(extensionFor(type)).toBe('bin');
    },
  );
});

describe('downloadBinary', () => {
  beforeEach(() => {
    vi.stubGlobal('URL', {
      ...URL,
      createObjectURL: vi.fn(() => 'blob:stub'),
      revokeObjectURL: vi.fn(),
    });
  });

  it('decodes the base64 payload into a Blob carrying the reported media type', async () => {
    const blobs: Blob[] = [];
    const createObjectURL = vi.fn((b: Blob) => {
      blobs.push(b);
      return 'blob:stub';
    });
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

    const ok = await downloadBinary('cat.png', 'data:image/png;base64,AAAA');

    expect(ok).toBe(true);
    expect(blobs[0].type).toBe('image/png');
    expect(blobs[0].size).toBe(3); // "AAAA" base64-decodes to 3 bytes
  });

  it('creates a detached anchor with the given filename and the object URL, then revokes it', async () => {
    let anchor: HTMLAnchorElement | undefined;
    vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
      const el = nativeCreateElement(tag, options as ElementCreationOptions);
      if (tag === 'a') anchor = el as HTMLAnchorElement;
      return el;
    });
    const revokeObjectURL = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL });

    await downloadBinary('cat.png', 'data:image/png;base64,AAAA');

    expect(anchor?.download).toBe('cat.png');
    expect(anchor?.getAttribute('href')).toBe('blob:stub');
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:stub');
  });

  it('returns false and creates nothing for a string that is neither a data: URL nor a fetchable url', async () => {
    const createObjectURL = vi.fn();
    const fetchMock = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });
    vi.stubGlobal('fetch', fetchMock);

    const ok = await downloadBinary('cat.png', 'not-a-data-url');

    expect(ok).toBe(false);
    expect(createObjectURL).not.toHaveBeenCalled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  // The shape matches (data:<mime>;base64,<payload>) but the payload itself
  // is not valid base64 -- exactly what a truncated or corrupted persisted
  // transcript would hand this function. atob() throws a DOMException for
  // this; downloadBinary must not let that escape (it runs inside an
  // onClick), and must not report success.
  it('returns false, without throwing, for a data: URL whose base64 payload is invalid', async () => {
    const createObjectURL = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

    await expect(
      downloadBinary('cat.png', 'data:image/png;base64,not-valid-base64!!!'),
    ).resolves.toBe(false);

    expect(createObjectURL).not.toHaveBeenCalled();
  });

  // The filename may be a function of the extension: a gateway url has no media
  // type until the response arrives, so the caller cannot build the final name
  // up front. The data: branch takes the same form, with the extension read
  // from its own prefix, so a caller does not care which kind of url it holds.
  it('hands a data: URL its own extension when the filename is a function of it', async () => {
    let anchor: HTMLAnchorElement | undefined;
    vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
      const el = nativeCreateElement(tag, options as ElementCreationOptions);
      if (tag === 'a') {
        anchor = el as HTMLAnchorElement;
        // jsdom cannot navigate to the blob: URL the download builds.
        anchor.click = () => {};
      }
      return el;
    });
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const ok = await downloadBinary((ext) => `cat.${ext}`, 'data:image/jpeg;base64,AAAA');

    expect(ok).toBe(true);
    expect(anchor?.download).toBe('cat.jpg');
    // Back-compat: a persisted data: URL is decoded in place, never fetched.
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('saves an inline data:audio/wav URL as a .wav file, decoded in place', async () => {
    let anchor: HTMLAnchorElement | undefined;
    vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
      const el = nativeCreateElement(tag, options as ElementCreationOptions);
      if (tag === 'a') {
        anchor = el as HTMLAnchorElement;
        anchor.click = () => {};
      }
      return el;
    });
    const blobs: Blob[] = [];
    vi.stubGlobal('URL', {
      ...URL,
      createObjectURL: vi.fn((b: Blob) => {
        blobs.push(b);
        return 'blob:stub';
      }),
      revokeObjectURL: vi.fn(),
    });
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const ok = await downloadBinary((ext) => `reply.${ext}`, 'data:audio/wav;base64,AAAA');

    expect(ok).toBe(true);
    expect(anchor?.download).toBe('reply.wav');
    expect(blobs[0].type).toBe('audio/wav');
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

// A stored image is a short /api/portal/chat-blobs/{id} url, not a data: URL.
// The bytes are fetched from the owner-scoped endpoint, which answers with the
// raw image and its Content-Type.
describe('downloadBinary from a gateway url', () => {
  const GATEWAY_URL = '/api/portal/chat-blobs/6f1c2a52-0f43-4d0b-bb0d-8c7a3a1d9e10';

  function okResponse(bytes: number[], type: string) {
    return {
      ok: true,
      status: 200,
      blob: async () => new Blob([new Uint8Array(bytes)], { type }),
    };
  }

  let anchor: HTMLAnchorElement | undefined;
  let clicks: number;
  let createObjectURL: ReturnType<typeof vi.fn>;
  let revokeObjectURL: ReturnType<typeof vi.fn>;
  const blobs: Blob[] = [];

  beforeEach(() => {
    anchor = undefined;
    clicks = 0;
    blobs.length = 0;
    vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
      const el = nativeCreateElement(tag, options as ElementCreationOptions);
      if (tag === 'a') {
        anchor = el as HTMLAnchorElement;
        anchor.click = () => {
          clicks += 1;
        };
      }
      return el;
    });
    createObjectURL = vi.fn((b: Blob) => {
      blobs.push(b);
      return 'blob:stub';
    });
    revokeObjectURL = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL });
  });

  it('fetches the url with credentials and saves the response bytes as a download', async () => {
    const fetchMock = vi.fn(async () => okResponse([1, 2, 3, 4, 5], 'image/png'));
    vi.stubGlobal('fetch', fetchMock);

    const ok = await downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL);

    expect(ok).toBe(true);
    // Same-origin with credentials: the endpoint is owner-scoped, so the
    // session cookie has to reach it or the save is a 404.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith(GATEWAY_URL, { credentials: 'same-origin' });
    expect(blobs).toHaveLength(1);
    expect(blobs[0].type).toBe('image/png');
    expect(blobs[0].size).toBe(5);
    expect(anchor?.getAttribute('href')).toBe('blob:stub');
    expect(clicks).toBe(1);
  });

  it('revokes the object URL once the download has been triggered', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => okResponse([1], 'image/png')),
    );

    await downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL);

    expect(revokeObjectURL).toHaveBeenCalledTimes(1);
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:stub');
    expect(clicks).toBe(1);
  });

  // The extension comes from what the server actually served, so a webp or
  // jpeg image is not saved as `.png`.
  it.each([
    ['image/png', 'png'],
    ['image/jpeg', 'jpg'],
    ['image/webp', 'webp'],
    ['image/gif', 'gif'],
    ['image/png; charset=binary', 'png'],
  ])('derives the extension from the response Content-Type %s -> .%s', async (type, ext) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => okResponse([1, 2], type)),
    );

    await downloadBinary((extension) => `cat.${extension}`, GATEWAY_URL);

    expect(anchor?.download).toBe(`cat.${ext}`);
  });

  // A speech turn's answer lives behind the same endpoint: the chat-blobs path
  // stays fetchable and the served audio/wav is saved as .wav.
  it('fetches a stored audio reply and saves it under the .wav extension', async () => {
    const fetchMock = vi.fn(async () => okResponse([1, 2, 3, 4], 'audio/wav'));
    vi.stubGlobal('fetch', fetchMock);

    const ok = await downloadBinary((ext) => `reply.${ext}`, GATEWAY_URL);

    expect(ok).toBe(true);
    expect(fetchMock).toHaveBeenCalledWith(GATEWAY_URL, { credentials: 'same-origin' });
    expect(blobs[0].type).toBe('audio/wav');
    expect(anchor?.download).toBe('reply.wav');
    expect(clicks).toBe(1);
  });

  // A transcription turn's input clip lives behind the same endpoint, in the type it was
  // attached or recorded in: each saves under its own extension, not as `bin`.
  it.each([
    ['audio/webm', 'webm'],
    ['audio/mpeg', 'mp3'],
    ['audio/mp4', 'm4a'],
    ['audio/ogg', 'ogg'],
  ])('fetches a stored %s input clip and saves it under .%s', async (type, ext) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => okResponse([1, 2, 3], type)),
    );

    const ok = await downloadBinary((e) => `input.${e}`, GATEWAY_URL);

    expect(ok).toBe(true);
    expect(blobs[0].type).toBe(type);
    expect(anchor?.download).toBe(`input.${ext}`);
  });

  it('falls back to the bin extension when the response carries no image Content-Type', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => okResponse([1], '')),
    );

    await downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL);

    expect(anchor?.download).toBe('cat.bin');
  });

  it('uses a plain string filename verbatim', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => okResponse([1], 'image/webp')),
    );

    await downloadBinary('already-named.webp', GATEWAY_URL);

    expect(anchor?.download).toBe('already-named.webp');
  });

  // The extension is allow-listed, not echoed from the served subtype: a
  // hostile Content-Type must not choose the saved file's extension.
  it('saves a response whose image subtype is not allow-listed as .bin, not .bat', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => okResponse([1], 'image/bat')),
    );

    const ok = await downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL);

    expect(ok).toBe(true);
    expect(anchor?.download).toBe('cat.bin');
  });

  // The failure modes are all reported as `false`, never thrown: this runs
  // inside an onClick with no error boundary, and the caller turns the boolean
  // into the visible "download failed" message.
  it('returns false and downloads nothing when the endpoint answers with an error status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({ ok: false, status: 404, blob: async () => new Blob(['nope']) })),
    );

    await expect(downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL)).resolves.toBe(false);

    expect(createObjectURL).not.toHaveBeenCalled();
    expect(clicks).toBe(0);
  });

  it('returns false, without throwing, when the fetch itself fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('Failed to fetch');
      }),
    );

    await expect(downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL)).resolves.toBe(false);

    expect(createObjectURL).not.toHaveBeenCalled();
    expect(clicks).toBe(0);
  });

  it('returns false, without throwing, when reading the response body fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: true,
        status: 200,
        blob: async () => {
          throw new TypeError('body stream interrupted');
        },
      })),
    );

    await expect(downloadBinary((ext) => `cat.${ext}`, GATEWAY_URL)).resolves.toBe(false);

    expect(createObjectURL).not.toHaveBeenCalled();
    expect(clicks).toBe(0);
  });

  // Only a rooted path under the chat-blobs endpoint is fetched, so the
  // session cookie can only ever reach this origin's own endpoint. Everything
  // else never reaches the network: a scripting scheme, a blob: url, a bare
  // word, an absolute url (cross-origin, and same-host too -- the only
  // producer emits a path), the protocol-relative "//host" and its backslash /
  // tab spellings ("/\host" and "/<tab>/host" resolve cross-origin once the
  // URL parser has normalised them), and a rooted path to some other endpoint.
  it.each([
    'javascript:alert(1)',
    '//evil.example/x.png',
    '/\\evil.example/x.png',
    '/\t/evil.example/x.png',
    '/\n/evil.example/x.png',
    'https://evil.example/x.png',
    'https://evil.example/api/portal/chat-blobs/6f1c2a52',
    'http://gw.example/api/portal/chat-blobs/6f1c2a52',
    '/api/portal/other/6f1c2a52',
    '/api/portal/chat-blobs',
    ' /api/portal/chat-blobs/6f1c2a52',
    'blob:foo',
    'chat-blobs/x',
    '',
  ])('does not fetch %j', async (url) => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    await expect(downloadBinary((ext) => `cat.${ext}`, url)).resolves.toBe(false);

    expect(fetchMock).not.toHaveBeenCalled();
    expect(createObjectURL).not.toHaveBeenCalled();
  });
});
