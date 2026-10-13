// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Saving an image URL as a real file. The sibling downloadText wraps a STRING
// in a Blob -- it was written for PEM/text blobs and its own doc says so -- so
// handing it a data URL saves a text file whose contents are the data URL
// itself. That is not a smaller problem than a missing button: the user thinks
// they saved their image.
//
// Two kinds of url reach this, and the caller does not have to tell them apart:
//
//  - A data: URL (data:<mime>;base64,<payload>): what chats persisted before
//    the binary payload store existed, and what an old transcript still holds.
//    Decoded in place; the media type is the URL's own prefix. Nothing here
//    assumes PNG.
//  - A gateway url (/api/portal/chat-blobs/{id}, or any other http(s) / rooted
//    path): what a stored image is now. The bytes live behind an owner-scoped
//    endpoint, so they are fetched (with the session cookie) and the media
//    type is whatever the response's Content-Type says.
//
// The file name is a string, or a function of the extension: a gateway url has
// no media type until its response arrives, so a caller cannot build the final
// name up front. The data: branch takes the same form (extension from its own
// prefix) so the two stay interchangeable.
//
// Resolves to false, without ever throwing or rejecting, when nothing could be
// saved: a data: URL whose shape or base64 payload doesn't decode (a
// truncated/corrupted persisted transcript reaches exactly this: atob() throws
// a DOMException on it), a url that is neither of the two kinds, or a gateway
// fetch that fails (network error, non-2xx, unreadable body). This runs inside
// an onClick with no error boundary above it, so an uncaught throw here would
// surface as nothing at all -- the caller uses the boolean to tell the user
// the download failed instead of it failing silently.
export type DownloadName = string | ((extension: string) => string);

export async function downloadBinary(name: DownloadName, url: string): Promise<boolean> {
  if (url.startsWith('data:')) return saveDataUrl(name, url);
  if (isFetchableUrl(url)) return saveFetchedUrl(name, url);
  return false;
}

// A url worth a network round trip: a gateway path ("/api/portal/...") or an
// absolute http(s) url. Deliberately not "anything that isn't a data: URL": a
// scripting scheme, a blob: url, a bare word, and a protocol-relative
// "//host/x" (which a "/"-prefix test alone would let through to another
// host) must come back as a plain false, not as a fetch.
function isFetchableUrl(url: string): boolean {
  return /^https?:\/\//i.test(url) || /^\/(?!\/)/.test(url);
}

function resolveName(name: DownloadName, mediaType: string): string {
  return typeof name === 'string' ? name : name(extensionFor(mediaType));
}

// The existing data: branch, unchanged in behavior: decode the base64 payload
// into a Blob carrying the URL's own media type and hand it to saveBlob.
function saveDataUrl(name: DownloadName, dataUrl: string): boolean {
  const match = /^data:([^;,]+);base64,(.*)$/s.exec(dataUrl);
  if (!match) return false;
  const [, mime, b64] = match;
  let binary: string;
  try {
    binary = atob(b64);
  } catch {
    return false;
  }
  const bytes = new Uint8Array(binary.length);
  // codePointAt, not charCodeAt: they differ only on a surrogate PAIR, and
  // atob's output is binary-string by definition (every code unit 0..255), so
  // no surrogate can occur here and the two are exactly equivalent. The `?? 0`
  // is for the type only -- `i` is always in range.
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.codePointAt(i) ?? 0;
  saveBlob(new Blob([bytes], { type: mime }), resolveName(name, mime));
  return true;
}

// The gateway branch: fetch the bytes, then save them like any other blob.
// `credentials: 'same-origin'` is the point of passing init at all -- the
// chat-blobs endpoint is owner-scoped, so without the session cookie it
// answers 404 (never 200) for the owner's own image. The extension comes from
// the BLOB's type, which fetch fills from the response Content-Type: what the
// server actually served, not a guess from the url.
async function saveFetchedUrl(name: DownloadName, url: string): Promise<boolean> {
  try {
    const response = await fetch(url, { credentials: 'same-origin' });
    if (!response.ok) return false;
    const blob = await response.blob();
    saveBlob(blob, resolveName(name, blob.type));
    return true;
  } catch {
    return false;
  }
}

// Triggers the browser download of a Blob via a detached anchor, then revokes
// the object URL so the Blob can be collected.
function saveBlob(blob: Blob, filename: string): void {
  const objectUrl = URL.createObjectURL(blob);
  try {
    const a = document.createElement('a');
    a.href = objectUrl;
    a.download = filename;
    a.click();
  } finally {
    URL.revokeObjectURL(objectUrl);
  }
}

// extensionFor maps a media type to a file extension for the download name. It
// accepts a bare media type (a response's Content-Type: parameters and letter
// case are ignored) or a data: URL, whose prefix is one. A bare fallback
// rather than a lookup table: the only producer today is sd-server via
// output_format, and inventing entries for types nothing emits would be dead
// code.
export function extensionFor(mediaTypeOrDataUrl: string): string {
  const match = /^(?:data:)?image\/([a-z0-9+.-]+)/i.exec(mediaTypeOrDataUrl);
  return match ? match[1].toLowerCase().replace('jpeg', 'jpg') : 'bin';
}
