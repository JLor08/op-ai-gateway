// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Shared client-side "save this text as a file" helper. Used by every panel that
// hands the operator a PEM/text blob to save locally (the certificate views: the
// internal CA's root/bundle, and the edge certificate's bundle/key). Extracted
// out of CertificateSettings.tsx so a second consumer (EdgeCertificatePanel) does
// not have to duplicate it.
export function downloadText(filename: string, content: string, mime: string) {
  const blob = new Blob([content], { type: mime });
  const url = URL.createObjectURL(blob);
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.click();
  } finally {
    URL.revokeObjectURL(url);
  }
}
