// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { prepareImageDataUrl, prepareMaskDataUrl, MAX_IMAGE_BYTES } from './imageAttach';
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
// A src a test has registered here decodes to that size (or, as null, not at all)
// whatever imageMode says: the mask tests need a source and a mask of different sizes.
let sizesBySrc = new Map<string, [number, number] | null>();
let drawImageSpy: ReturnType<typeof vi.fn>;
let toDataUrlSpy: ReturnType<typeof vi.fn>;

class MockImage {
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  naturalWidth = 0;
  naturalHeight = 0;
  set src(value: string) {
    queueMicrotask(() => {
      const registered = sizesBySrc.get(value);
      if (registered === null) {
        this.onerror?.();
        return;
      }
      if (registered) {
        [this.naturalWidth, this.naturalHeight] = registered;
        this.onload?.();
        return;
      }
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
  sizesBySrc = new Map();
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

// An edit's mask must be exactly as large as the edit's source -- the gateway refuses
// it otherwise (images.mask_invalid) -- so the mask is sized to the source's decoded
// pixels, whatever the source is: an uploaded one, already downscaled to the 1568 px
// cap, or a refined one, a stored image at its natural size.
describe('prepareMaskDataUrl', () => {
  const SOURCE = '/api/portal/chat-blobs/blob_source';
  // What reading sizedFile's one-byte content as a PNG data URL gives.
  const MASK_URL = 'data:image/png;base64,eA==';
  const maskFile = () => sizedFile('mask.png', 'image/png', 1024);
  function sizes(mask: [number, number], source: [number, number] | null) {
    sizesBySrc.set(MASK_URL, mask);
    sizesBySrc.set(SOURCE, source);
  }

  it('returns a mask that already is the source size as it is: no re-encode', async () => {
    sizes([1792, 1024], [1792, 1024]);
    expect(await prepareMaskDataUrl(maskFile(), SOURCE)).toBe(MASK_URL);
    expect(drawImageSpy).not.toHaveBeenCalled();
  });

  it('draws a larger mask of the same shape at the source size, as a PNG', async () => {
    sizes([3584, 2048], [1792, 1024]);
    const url = await prepareMaskDataUrl(maskFile(), SOURCE);
    expect(url).toBe('data:image/jpeg;base64,SCALED'); // the canvas's output, whatever it is
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1792, 1024);
    expect(toDataUrlSpy).toHaveBeenCalledWith('image/png');
  });

  it('draws a smaller mask of the same shape up to the source size', async () => {
    sizes([896, 512], [1792, 1024]);
    await prepareMaskDataUrl(maskFile(), SOURCE);
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1792, 1024);
  });

  it('tolerates the rounding of a downscaled source: its shape is the mask shape to the pixel', async () => {
    // 3001x2000 downscaled to the cap is 1568x1045, not exactly 3:2 any more.
    sizes([3001, 2000], [1568, 1045]);
    await prepareMaskDataUrl(maskFile(), SOURCE);
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1568, 1045);
  });

  it('does not stretch a mask of another shape onto the source: the gateway says it does not fit', async () => {
    sizes([1000, 1000], [1792, 1024]);
    // Within the cap it is sent as it is, at its own size, and refused by the gateway
    // with both sizes named -- not bent into the source's shape and edited wrongly.
    expect(await prepareMaskDataUrl(maskFile(), SOURCE)).toBe(MASK_URL);
    expect(drawImageSpy).not.toHaveBeenCalled();
  });

  it('caps a mask of another shape the way every attached image is capped', async () => {
    sizes([3000, 3000], [1792, 1024]);
    await prepareMaskDataUrl(maskFile(), SOURCE);
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1568, 1568);
    expect(toDataUrlSpy).toHaveBeenCalledWith('image/png', 0.82);
  });

  it('falls back to the cap when the source cannot be read', async () => {
    sizes([4000, 3000], null);
    await prepareMaskDataUrl(maskFile(), SOURCE);
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1568, 1176);
  });

  it('falls back to the cap when the source reports no size', async () => {
    sizes([4000, 3000], [0, 0]);
    await prepareMaskDataUrl(maskFile(), SOURCE);
    expect(drawImageSpy).toHaveBeenCalledWith(expect.anything(), 0, 0, 1568, 1176);
  });

  it('sends the mask as it is when there is no canvas to draw it on', async () => {
    sizes([3584, 2048], [1792, 1024]);
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(null);
    expect(await prepareMaskDataUrl(maskFile(), SOURCE)).toBe(MASK_URL);
  });

  it("rejects a mask that is not a PNG with reason 'type' before decoding", async () => {
    await expect(
      prepareMaskDataUrl(sizedFile('mask.jpg', 'image/jpeg', 1024), SOURCE),
    ).rejects.toMatchObject({ reason: 'type' });
    expect(drawImageSpy).not.toHaveBeenCalled();
  });

  it("rejects an oversized mask with reason 'size' before decoding", async () => {
    await expect(
      prepareMaskDataUrl(sizedFile('mask.png', 'image/png', MAX_IMAGE_BYTES + 1), SOURCE),
    ).rejects.toMatchObject({ reason: 'size' });
  });

  it("rejects a mask that fails to decode with reason 'decode'", async () => {
    sizesBySrc.set(MASK_URL, null);
    await expect(prepareMaskDataUrl(maskFile(), SOURCE)).rejects.toMatchObject({
      reason: 'decode',
    });
  });
});
