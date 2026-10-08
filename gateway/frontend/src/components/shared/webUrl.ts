// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

/**
 * Whether `value` is a real web address (http or https). A URL the backend hands
 * the portal -- a vendor sign-in or verification page -- is still passed on to
 * window.open, so never open anything else (a `javascript:` URL, a `data:`
 * URL, a relative path).
 */
export function isWebUrl(value: string): boolean {
  try {
    const { protocol } = new URL(value);
    return protocol === 'https:' || protocol === 'http:';
  } catch {
    return false;
  }
}
