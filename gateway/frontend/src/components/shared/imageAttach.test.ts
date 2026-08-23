// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { prepareImageDataUrl, MAX_IMAGE_BYTES } from './imageAttach';
import { heicTo } from 'heic-to';

// heic-to loads a libheif WASM decoder at runtime; jsdom can't run it, so mock
// the conversion to a deterministic JPEG blob.
vi.mock('heic-to', () => ({
  heicTo: vi.fn(async () => new Blob(['jpeg-bytes'], { type: 'image/jpeg' })),
  isHeic: vi.fn(async () => true),
}));

function sizedFile(name: string, type: string, size: number): File {
  const f = new File(['x'], name, { type });
  Object.defineProperty(f, 'size', { value: size });
  return f;
}

// jsdom ships no real image decoder or canvas raster, so stub the browser
// image/canvas APIs the downscaler leans on. `imageMode` lets each test choose
// the decoded dimensions (or force a decode failure); the canvas always
// re-encodes to a sentinel data URL so a re-encode is observable.
let imageMode: 'large' | 'small' | 'error' | 'extreme' = 'small';
let drawImageSpy: ReturnType<typeof vi.fn>;
let toDataUrlSpy: ReturnType<typeof vi.fn>;

class MockImage {
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  naturalWidth = 0;
  naturalHeight = 0;
  set src(_value: string) {
    queueMicrotask(() => {
      if (imageMode === 'error') {
        this.onerror?.();
        return;
      }
      if (imageMode === 'large') {
        this.naturalWidth = 4000;
        this.naturalHeight = 3000;
      } else if (imageMode === 'extreme') {
        this.naturalWidth = 4000;
        this.naturalHeight = 1;
      } else {
        this.naturalWidth = 800;
        this.naturalHeight = 600;
      }
      this.onload?.();
    });
  }
}

beforeEach(() => {
  imageMode = 'small';
  drawImageSpy = vi.fn();
  toDataUrlSpy = vi.fn(() => 'data:image/jpeg;base64,SCALED');
  vi.stubGlobal('Image', MockImage);
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({
    drawImage: drawImageSpy,
  } as unknown as CanvasRenderingContext2D);
  vi.spyOn(HTMLCanvasElement.prototype, 'toDataURL').mockImplementation(
    toDataUrlSpy as unknown as HTMLCanvasElement['toDataURL'],
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('prepareImageDataUrl', () => {
  it('returns a small JPEG unchanged (no re-encode)', async () => {
    imageMode = 'small';
    const url = await prepareImageDataUrl(sizedFile('a.jpg', 'image/jpeg', 1024));
    expect(url.startsWith('data:')).toBe(true);
    expect(url).not.toBe('data:image/jpeg;base64,SCALED');
    expect(drawImageSpy).not.toHaveBeenCalled();
  });

  it('downscales a large image to <=1568px longest edge and re-encodes', async () => {
    imageMode = 'large';
    const url = await prepareImageDataUrl(sizedFile('big.jpg', 'image/jpeg', 1024 * 1024));
    expect(url).toBe('data:image/jpeg;base64,SCALED');
    // 4000x3000 -> scale 1568/4000 -> 1568x1176 (aspect preserved, longest edge == 1568).
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1568, 1176);
    expect(toDataUrlSpy).toHaveBeenCalledWith('image/jpeg', 0.82);
  });

  it('keeps a large PNG as PNG when downscaling (alpha preserved)', async () => {
    imageMode = 'large';
    await prepareImageDataUrl(sizedFile('big.png', 'image/png', 1024 * 1024));
    expect(toDataUrlSpy).toHaveBeenCalledWith('image/png', 0.82);
  });

  it('keeps a large WEBP as WEBP when downscaling (alpha preserved)', async () => {
    imageMode = 'large';
    await prepareImageDataUrl(sizedFile('big.webp', 'image/webp', 1024 * 1024));
    expect(toDataUrlSpy).toHaveBeenCalledWith('image/webp', 0.82);
  });

  it('re-encodes a large GIF as PNG (alpha preserved, animation dropped)', async () => {
    imageMode = 'large';
    await prepareImageDataUrl(sizedFile('big.gif', 'image/gif', 1024 * 1024));
    expect(toDataUrlSpy).toHaveBeenCalledWith('image/png', 0.82);
  });

  it('clamps a degenerate aspect ratio so the canvas is never 0-sized', async () => {
    imageMode = 'extreme'; // 4000x1 -> short edge would round to 0 without the guard
    await prepareImageDataUrl(sizedFile('strip.png', 'image/png', 1024 * 1024));
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1568, 1);
  });

  it("rejects an image that fails to decode with reason 'decode'", async () => {
    imageMode = 'error';
    await expect(
      prepareImageDataUrl(sizedFile('broken.jpg', 'image/jpeg', 1024)),
    ).rejects.toMatchObject({ reason: 'decode' });
  });

  it("rejects an unsupported type with reason 'type' before decoding", async () => {
    await expect(prepareImageDataUrl(sizedFile('a.txt', 'text/plain', 10))).rejects.toMatchObject({
      reason: 'type',
    });
    expect(drawImageSpy).not.toHaveBeenCalled();
  });

  it("rejects an oversized file with reason 'size' before decoding", async () => {
    await expect(
      prepareImageDataUrl(sizedFile('big.jpg', 'image/jpeg', MAX_IMAGE_BYTES + 1)),
    ).rejects.toMatchObject({ reason: 'size' });
    expect(drawImageSpy).not.toHaveBeenCalled();
  });

  it('converts a HEIC file (by MIME) to JPEG and accepts it', async () => {
    vi.mocked(heicTo).mockClear();
    const url = await prepareImageDataUrl(sizedFile('photo.heic', 'image/heic', 2048));
    expect(url.startsWith('data:image/')).toBe(true);
    expect(heicTo).toHaveBeenCalledWith(expect.objectContaining({ type: 'image/jpeg' }));
  });

  it('converts a HEIC by empty MIME + .heic extension', async () => {
    vi.mocked(heicTo).mockClear();
    const url = await prepareImageDataUrl(sizedFile('photo.heic', '', 2048));
    expect(url.startsWith('data:image/')).toBe(true);
    expect(heicTo).toHaveBeenCalledTimes(1);
  });

  it("rejects an oversized HEIC with reason 'size' before converting", async () => {
    vi.mocked(heicTo).mockClear();
    await expect(
      prepareImageDataUrl(sizedFile('big.heic', 'image/heic', MAX_IMAGE_BYTES + 1)),
    ).rejects.toMatchObject({ reason: 'size' });
    expect(heicTo).not.toHaveBeenCalled();
  });
});
