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
});

describe('downloadBinary', () => {
  beforeEach(() => {
    vi.stubGlobal('URL', {
      ...URL,
      createObjectURL: vi.fn(() => 'blob:stub'),
      revokeObjectURL: vi.fn(),
    });
  });

  it('decodes the base64 payload into a Blob carrying the reported media type', () => {
    const blobs: Blob[] = [];
    const createObjectURL = vi.fn((b: Blob) => {
      blobs.push(b);
      return 'blob:stub';
    });
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

    const ok = downloadBinary('cat.png', 'data:image/png;base64,AAAA');

    expect(ok).toBe(true);
    expect(blobs[0].type).toBe('image/png');
    expect(blobs[0].size).toBe(3); // "AAAA" base64-decodes to 3 bytes
  });

  it('creates a detached anchor with the given filename and the object URL, then revokes it', () => {
    let anchor: HTMLAnchorElement | undefined;
    vi.spyOn(document, 'createElement').mockImplementation((tag: string, options?: unknown) => {
      const el = nativeCreateElement(tag, options as ElementCreationOptions);
      if (tag === 'a') anchor = el as HTMLAnchorElement;
      return el;
    });
    const revokeObjectURL = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL });

    downloadBinary('cat.png', 'data:image/png;base64,AAAA');

    expect(anchor?.download).toBe('cat.png');
    expect(anchor?.getAttribute('href')).toBe('blob:stub');
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:stub');
  });

  it('returns false and creates nothing for a string that does not match the data: URL shape', () => {
    const createObjectURL = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

    const ok = downloadBinary('cat.png', 'not-a-data-url');

    expect(ok).toBe(false);
    expect(createObjectURL).not.toHaveBeenCalled();
  });

  // The shape matches (data:<mime>;base64,<payload>) but the payload itself
  // is not valid base64 -- exactly what a truncated or corrupted persisted
  // transcript would hand this function. atob() throws a DOMException for
  // this; downloadBinary must not let that escape uncaught (it runs inside
  // an onClick), and must not report success.
  it('returns false, without throwing, for a data: URL whose base64 payload is invalid', () => {
    const createObjectURL = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL: vi.fn() });

    let ok: boolean = true;
    expect(() => {
      ok = downloadBinary('cat.png', 'data:image/png;base64,not-valid-base64!!!');
    }).not.toThrow();

    expect(ok).toBe(false);
    expect(createObjectURL).not.toHaveBeenCalled();
  });
});
