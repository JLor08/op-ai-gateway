// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

export type ImageAttachReason = 'type' | 'size' | 'decode';

export class ImageAttachError extends Error {
  readonly reason: ImageAttachReason;
  // `Error(message, { cause })` and the global `ErrorOptions` type only exist under
  // lib ES2022+; this project targets ES2020, so forward the cause onto a declared
  // field manually to preserve it for debugging while staying type-clean.
  readonly cause?: unknown;
  constructor(reason: ImageAttachReason, message?: string, options?: { cause?: unknown }) {
    super(message ?? reason);
    this.name = 'ImageAttachError';
    this.reason = reason;
    if (options && 'cause' in options) this.cause = options.cause;
  }
}

export const MAX_IMAGE_BYTES = 20 * 1024 * 1024;
const ALLOWED_TYPES = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp']);
const HEIC_TYPES = new Set([
  'image/heic',
  'image/heif',
  'image/heic-sequence',
  'image/heif-sequence',
]);

// Browsers frequently report an empty MIME type for .heic/.heif files, so fall
// back to the filename extension when the type is missing/unknown.
function isHeicFile(file: File): boolean {
  if (HEIC_TYPES.has(file.type.toLowerCase())) return true;
  return /\.hei[cf]$/i.test(file.name);
}

// Cap the longest edge of a persisted image. A larger transcript blows the
// ~5 MB localStorage quota (base64 adds ~33 %), so the write silently fails and
// the images vanish on reload; downscaling keeps the transcript under quota.
const MAX_EDGE = 1568;

function readAsDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    // readAsDataURL always yields a string result; the typeof guard just
    // avoids ever falling back to Object's default `[object ...]`
    // stringification for the (never-hit-in-practice) ArrayBuffer/null cases.
    reader.onload = () => {
      const { result } = reader;
      resolve(typeof result === 'string' ? result : '');
    };
    reader.onerror = () => reject(new Error('read failed'));
    reader.readAsDataURL(blob);
  });
}

function loadImage(src: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new ImageAttachError('decode', 'image decode failed'));
    img.src = src;
  });
}

async function downscaleDataUrl(dataUrl: string, mime: string): Promise<string> {
  const img = await loadImage(dataUrl);
  const { naturalWidth: w, naturalHeight: h } = img;
  if (!w || !h) return dataUrl; // unknown dims: keep original
  const longest = Math.max(w, h);
  if (longest <= MAX_EDGE) return dataUrl; // already small enough
  const scale = MAX_EDGE / longest;
  // Math.max(1, …): a degenerate aspect ratio (> ~3136:1) would otherwise round
  // the short edge to 0, giving a 0-sized canvas whose toDataURL is "data:,"
  // (a broken <img>). Clamp to a 1px minimum so the result is always valid.
  const cw = Math.max(1, Math.round(w * scale));
  const ch = Math.max(1, Math.round(h * scale));
  const canvas = document.createElement('canvas');
  canvas.width = cw;
  canvas.height = ch;
  const ctx = canvas.getContext('2d');
  if (!ctx) return dataUrl; // no canvas 2d context: keep original
  ctx.drawImage(img, 0, 0, cw, ch);
  // Preserve alpha-capable formats so transparency is not flattened to black:
  //   JPEG -> JPEG   (no alpha anyway; smallest for photos)
  //   WEBP -> WEBP   (keeps alpha AND lossy compression; a browser without WEBP
  //                   encoding has canvas.toDataURL fall back to PNG per spec,
  //                   which is still alpha-safe)
  //   PNG / GIF -> PNG (lossless, alpha preserved; GIF also loses animation —
  //                   already the case with a single-frame canvas draw)
  // The quality arg is ignored for the lossless PNG path.
  const outMime = mime === 'image/jpeg' || mime === 'image/webp' ? mime : 'image/png';
  return canvas.toDataURL(outMime, 0.82);
}

export async function prepareImageDataUrl(file: File): Promise<string> {
  // HEIC/HEIF: convert to JPEG in the browser (heic-to wraps LGPL libheif) before
  // the normal validation, so Apple-camera uploads work. The 20 MB cap is checked
  // on the original file before the (potentially expensive) conversion.
  if (isHeicFile(file)) {
    if (file.size > MAX_IMAGE_BYTES) {
      throw new ImageAttachError('size', 'image too large');
    }
    let jpeg: Blob;
    try {
      // Dynamic import: heic-to bundles the libheif WASM decoder (multiple MB)
      // and is only needed for the rare HEIC upload, so it must never end up
      // in the eagerly loaded portal bundle.
      const { heicTo } = await import('heic-to');
      jpeg = await heicTo({ blob: file, type: 'image/jpeg', quality: 0.9 });
    } catch (err) {
      throw new ImageAttachError('decode', 'HEIC conversion failed', { cause: err });
    }
    const dataUrl = await readAsDataUrl(jpeg);
    return downscaleDataUrl(dataUrl, 'image/jpeg');
  }
  if (!ALLOWED_TYPES.has(file.type.toLowerCase())) {
    throw new ImageAttachError('type', 'unsupported image type');
  }
  if (file.size > MAX_IMAGE_BYTES) {
    throw new ImageAttachError('size', 'image too large');
  }
  const dataUrl = await readAsDataUrl(file);
  return downscaleDataUrl(dataUrl, file.type.toLowerCase());
}
