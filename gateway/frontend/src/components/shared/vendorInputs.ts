// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Client-side mirrors of the two vendor-related inputs the backend validates, so
// a malformed value is flagged inline instead of costing a round trip that ends
// in a 400. The backend stays the authority: these mirror its rules and are not
// meant to be stricter. (The one known difference is exotic whitespace such as
// U+0085, which Go's TrimSpace drops and the browser's trim does not; there the
// portal refuses a value the backend would have accepted.)

/** The most bytes of a model prefix (portal.vendorAccountModelPrefixMaxLen). */
export const MODEL_PREFIX_MAX_LENGTH = 64;

// The characters a model prefix may hold (portal.isModelIDByte): ASCII letters
// and digits plus . _ ~ : / @ + -. Everything else is refused -- spaces, control
// and non-ASCII characters, and the ones that start a query, fragment or escape
// (? # %) -- because the prefix is glued into model ids that travel in URL
// paths, JSON bodies and logs.
const MODEL_PREFIX_CHARS = /^[A-Za-z0-9._~:/@+-]*$/;

/**
 * The prefix as the backend reads it: surrounding whitespace is dropped
 * (portal.normalizeVendorAccountModelPrefix). "" means "no prefix".
 */
export function normalizeModelPrefix(raw: string): string {
  return raw.trim();
}

/**
 * Whether `raw` is an acceptable model prefix: empty (none), or at most
 * MODEL_PREFIX_MAX_LENGTH characters of [A-Za-z0-9._~:/@+-] without "..".
 * Judged on the trimmed value, like the backend. The charset is ASCII-only, so a
 * length in characters equals the backend's length in bytes for every value that
 * passes the charset check.
 */
export function isValidModelPrefix(raw: string): boolean {
  const prefix = normalizeModelPrefix(raw);
  return (
    prefix.length <= MODEL_PREFIX_MAX_LENGTH &&
    !prefix.includes('..') &&
    MODEL_PREFIX_CHARS.test(prefix)
  );
}

/** The most characters of a Codex client_version (portal.maxVendorOpenAICodexClientVersionLen). */
export const CODEX_CLIENT_VERSION_MAX_LENGTH = 64;

// A version-shaped token: a digit first, then letters, digits and . _ + -
// ("26.930.61225", "0.46.0-alpha.1"). A leading "v", spaces and separators are
// refused (portal.isValidVendorOpenAICodexClientVersion).
const CODEX_CLIENT_VERSION = /^[0-9][A-Za-z0-9._+-]*$/;

/**
 * Whether `raw` is an acceptable Codex client_version: blank (reset to the
 * built-in default), or 1 to CODEX_CLIENT_VERSION_MAX_LENGTH characters of
 * [A-Za-z0-9._+-] starting with a digit. Judged on the trimmed value, like the
 * backend. It checks the SHAPE only, never that the version exists.
 */
export function isValidCodexClientVersion(raw: string): boolean {
  const version = raw.trim();
  if (version === '') return true;
  return version.length <= CODEX_CLIENT_VERSION_MAX_LENGTH && CODEX_CLIENT_VERSION.test(version);
}
