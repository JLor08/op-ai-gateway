// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Client-side reader for the two vendor credential files a user can import a
// subscription from: Codex's `auth.json` and Claude Code's `.credentials.json`.
// It runs in the browser, so the file never leaves the page until the user
// submits the extracted fields; it keeps ONLY the fields the token import needs.
//
// The result is UI-facing. A failure carries a stable `code` the renderer maps to
// localised text, plus an English `error` for logs and tests. Both are built from
// fixed strings only: never from the file's content and never from a JSON.parse
// failure (V8's SyntaxError message quotes a fragment of the input, which for a
// credential file is a token).

export type CredentialVendor = 'openai' | 'anthropic';

/** The fields a vendor-account token import takes, extracted from a credential file. */
export type ParsedCredential = {
  vendor: CredentialVendor;
  accessToken: string;
  refreshToken?: string;
  /** RFC 3339 UTC instant; absent when the file does not say and the token does not carry it. */
  expiresAt?: string;
  accountId?: string;
};

/**
 * Why a file cannot be imported. The UI maps each code to localised text (for
 * example a Record<ParsedCredentialErrorCode, MessageKey>), so a new code is a
 * compile error there until it is given a message.
 */
export type ParsedCredentialErrorCode =
  | 'not_json'
  | 'not_object'
  | 'unrecognised'
  | 'ambiguous'
  | 'claude_no_access_token'
  | 'codex_id_token_only'
  | 'codex_no_access_token';

/** A token-free reason the file cannot be imported: a stable `code`, and its English text. */
export type ParsedCredentialError = { code: ParsedCredentialErrorCode; error: string };

/** Narrows a parse result to its error arm; a credential never has an `error` key. */
export function isParsedCredentialError(
  result: ParsedCredential | ParsedCredentialError,
): result is ParsedCredentialError {
  return 'error' in result;
}

const ERROR_MESSAGES: Record<ParsedCredentialErrorCode, string> = {
  not_json: 'The file is not valid JSON. Upload the credential file unchanged.',
  not_object:
    'The file must contain a JSON object, as in Codex auth.json or Claude Code .credentials.json.',
  unrecognised:
    'The file is not a recognised credential file. Upload a Codex auth.json or Claude Code .credentials.json.',
  ambiguous:
    'The file looks like both a Codex auth.json and a Claude Code .credentials.json. Rename it to auth.json or .credentials.json, or upload only one of them.',
  claude_no_access_token:
    'The Claude Code file has no claudeAiOauth.accessToken. Upload a .credentials.json that holds an access token.',
  codex_id_token_only:
    'This Codex auth.json has an id_token but no tokens.access_token. The id_token is an identity token and cannot be imported; upload a file that holds the access_token (or paste the access_token itself).',
  codex_no_access_token:
    'This Codex auth.json has no tokens.access_token. Sign in to Codex with ChatGPT so that auth.json holds an access_token; an API key alone cannot be imported.',
};

function fail(code: ParsedCredentialErrorCode): ParsedCredentialError {
  return { code, error: ERROR_MESSAGES[code] };
}

type JsonObject = Record<string, unknown>;

function isJsonObject(value: unknown): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** A non-blank string, trimmed; anything else is "not provided". */
function nonBlankString(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined;
  const trimmed = value.trim();
  return trimmed === '' ? undefined : trimmed;
}

/**
 * The instant as an RFC 3339 UTC string, or undefined when it has none: not a
 * valid Date, or a year outside 0000-9999. toISOString() prints such a year in an
 * expanded form ("+010000-01-01..."), which RFC 3339 and the gateway's decoder
 * reject, so the expiry is dropped rather than sent.
 */
function toRfc3339(epochMillis: number): string | undefined {
  const instant = new Date(epochMillis);
  // toISOString() throws a RangeError on an invalid Date, so test first.
  if (Number.isNaN(instant.getTime())) return undefined;
  const year = instant.getUTCFullYear();
  return year >= 0 && year <= 9999 ? instant.toISOString() : undefined;
}

/**
 * The `exp` claim (seconds since the epoch) of a JWT, as an RFC 3339 UTC string.
 * The token is not verified: this only reads when the issuer says it lapses.
 * Undefined for an opaque token, a payload that is not JSON, or a missing or
 * non-numeric `exp`.
 */
function jwtExpiry(token: string): string | undefined {
  const segments = token.split('.');
  if (segments.length < 2) return undefined;
  try {
    // base64url -> base64. atob() accepts the missing '=' padding (forgiving-base64)
    // and yields one char per byte; only `exp` is read and any multi-byte text sits
    // inside JSON strings, so there is no need to decode the bytes as UTF-8.
    const claims: unknown = JSON.parse(atob(segments[1].replaceAll('-', '+').replaceAll('_', '/')));
    if (!isJsonObject(claims) || typeof claims.exp !== 'number') return undefined;
    return toRfc3339(claims.exp * 1000);
  } catch {
    return undefined;
  }
}

/**
 * The last path segment, lower-cased: a user may upload from a path or a renamed
 * copy. A non-string name (it is only a hint) is the empty name.
 */
function baseName(filename: unknown): string {
  if (typeof filename !== 'string') return '';
  return (filename.split(/[\\/]/).pop() ?? '').toLowerCase();
}

function parseClaudeCode(oauth: JsonObject): ParsedCredential | ParsedCredentialError {
  const accessToken = nonBlankString(oauth.accessToken);
  if (accessToken === undefined) return fail('claude_no_access_token');

  const credential: ParsedCredential = { vendor: 'anthropic', accessToken };
  const refreshToken = nonBlankString(oauth.refreshToken);
  if (refreshToken !== undefined) credential.refreshToken = refreshToken;
  // Claude Code stores the expiry as a millisecond epoch number.
  if (typeof oauth.expiresAt === 'number') {
    const expiresAt = toRfc3339(oauth.expiresAt);
    if (expiresAt !== undefined) credential.expiresAt = expiresAt;
  }
  return credential;
}

function parseCodex(tokens: JsonObject | undefined): ParsedCredential | ParsedCredentialError {
  const accessToken = nonBlankString(tokens?.access_token);
  if (tokens === undefined || accessToken === undefined) {
    // The live mistake this guards against: the id_token is the token that is
    // easy to copy out of auth.json, and it is not one the gateway can use.
    return fail(
      nonBlankString(tokens?.id_token) !== undefined
        ? 'codex_id_token_only'
        : 'codex_no_access_token',
    );
  }

  const credential: ParsedCredential = { vendor: 'openai', accessToken };
  const refreshToken = nonBlankString(tokens.refresh_token);
  if (refreshToken !== undefined) credential.refreshToken = refreshToken;
  const expiresAt = jwtExpiry(accessToken);
  if (expiresAt !== undefined) credential.expiresAt = expiresAt;
  const accountId = nonBlankString(tokens.account_id);
  if (accountId !== undefined) credential.accountId = accountId;
  return credential;
}

/**
 * Reads a Codex `auth.json` or a Claude Code `.credentials.json` and returns the
 * fields a token import needs, or an error (`code` + English `error`) saying why
 * it cannot be used. It never throws, whatever it is given: the parameters are
 * `unknown` so a caller may pass a FileReader result (string | ArrayBuffer | null)
 * straight in, and anything but a string is not JSON text.
 *
 * The vendor is detected from the file's CONTENT, because a user may rename the
 * file; `filename` only breaks the tie in the unlikely case the content carries
 * both shapes.
 */
export function parseCredentialFile(
  filename: unknown,
  text: unknown,
): ParsedCredential | ParsedCredentialError {
  if (typeof text !== 'string') return fail('not_json');
  let root: unknown;
  try {
    // A byte-order mark (some Windows editors add one) is not valid JSON.
    root = JSON.parse(text.replace(/^\uFEFF/, ''));
  } catch {
    return fail('not_json');
  }
  if (!isJsonObject(root)) return fail('not_object');

  const claude = isJsonObject(root.claudeAiOauth) ? root.claudeAiOauth : undefined;
  // `tokens` is the ChatGPT sign-in; a sibling OPENAI_API_KEY marks the file as
  // Codex even when `tokens` is null (API-key mode), so the user gets the
  // Codex-specific reason rather than "unrecognised".
  const isCodex = isJsonObject(root.tokens) || 'OPENAI_API_KEY' in root;
  const codex = isJsonObject(root.tokens) ? root.tokens : undefined;

  if (claude !== undefined && isCodex) {
    const name = baseName(filename);
    if (name === '.credentials.json') return parseClaudeCode(claude);
    if (name === 'auth.json') return parseCodex(codex);
    return fail('ambiguous');
  }
  if (claude !== undefined) return parseClaudeCode(claude);
  if (isCodex) return parseCodex(codex);
  // A flat id_token with no `tokens` object is the same mistake as an id_token-only
  // auth.json, so it gets the same hint to use the access_token.
  if (nonBlankString(root.id_token) !== undefined) return fail('codex_id_token_only');
  return fail('unrecognised');
}
