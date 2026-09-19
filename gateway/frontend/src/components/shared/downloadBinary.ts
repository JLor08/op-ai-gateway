// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Saving a data: URL as a real file. The sibling downloadText wraps a STRING in
// a Blob -- it was written for PEM/text blobs and its own doc says so -- so
// handing it a data URL saves a text file whose contents are the data URL
// itself. That is not a smaller problem than a missing button: the user thinks
// they saved their image.
//
// The media type is taken from the data URL's own prefix, which Task 7 fills
// from the upstream response's output_format. Nothing here assumes PNG.
export function downloadBinary(filename: string, dataUrl: string) {
  const match = /^data:([^;,]+);base64,(.*)$/s.exec(dataUrl);
  if (!match) return;
  const [, mime, b64] = match;
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  const url = URL.createObjectURL(new Blob([bytes], { type: mime }));
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
  } finally {
    URL.revokeObjectURL(url);
  }
}

// extensionFor maps a media type to a file extension for the download name. A
// bare fallback rather than a lookup table: the only producer today is
// sd-server via output_format, and inventing entries for types nothing emits
// would be dead code.
export function extensionFor(dataUrl: string): string {
  const match = /^data:image\/([a-z0-9+.-]+);base64,/i.exec(dataUrl);
  return match ? match[1].replace('jpeg', 'jpg') : 'bin';
}
