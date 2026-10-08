// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Client-side reader for the two vendor credential files a user can import a
// subscription from: Codex's `auth.json` and Claude Code's `.credentials.json`.
// It runs in the browser, so the file never leaves the page until the user
// submits the extracted fields; it keeps ONLY the fields the token import needs.
//
// The result is UI-facing. Its error text is shown to the user, so it is built
// from fixed strings only: never from the file's content and never from a
// JSON.parse failure (V8's SyntaxError message quotes a fragment of the input,
// which for a credential file is a token).

export type CredentialVendor = 'openai' | 'anthropic';

/** The fields a vendor-account token import takes, extracted from a credential file. */
export type ParsedCredential = {
  vendor: CredentialVendor;
  accessToken: string;
  refreshToken?: string;
  /** RFC 3339 UTC instant; absent when the file does not say and the token does not carry it. */
  expiresAt?: string;
  accountId?: string;
  planType?: string;
};

/** A clear, token-free reason the file cannot be imported. */
export type ParsedCredentialError = { error: string };

/** Narrows a parse result to its error arm; a credential never has an `error` key. */
export function isParsedCredentialError(
  result: ParsedCredential | ParsedCredentialError,
): result is ParsedCredentialError {
  return 'error' in result;
}

const SUPPORTED_FILES = 'Codex auth.json or Claude Code .credentials.json';

const ERROR_NOT_JSON = 'The file is not valid JSON. Upload the credential file unchanged.';
const ERROR_NOT_OBJECT =
  'The file must contain a JSON object, as in Codex auth.json or Claude Code .credentials.json.';
const ERROR_UNRECOGNISED = `The file is not a recognised credential file. Upload a ${SUPPORTED_FILES}.`;
const ERROR_AMBIGUOUS =
  'The file looks like both a Codex auth.json and a Claude Code .credentials.json. Rename it to auth.json or .credentials.json, or upload only one of them.';
const ERROR_CLAUDE_NO_ACCESS_TOKEN =
  'The Claude Code file has no claudeAiOauth.accessToken. Upload a .credentials.json that holds an access token.';
const ERROR_CODEX_ID_TOKEN_ONLY =
  'This Codex auth.json has an id_token but no tokens.access_token. The id_token is an identity token and cannot be imported; upload a file that holds the access_token (or paste the access_token itself).';
const ERROR_CODEX_NO_ACCESS_TOKEN =
  'This Codex auth.json has no tokens.access_token. Sign in to Codex with ChatGPT so that auth.json holds an access_token; an API key alone cannot be imported.';

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

/** The instant as an RFC 3339 UTC string, or undefined when it is not a representable time. */
function toRfc3339(epochMillis: number): string | undefined {
  if (!Number.isFinite(epochMillis)) return undefined;
  const instant = new Date(epochMillis);
  // toISOString() throws a RangeError on an invalid Date, so test first.
  return Number.isNaN(instant.getTime()) ? undefined : instant.toISOString();
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
    const claims: unknown = JSON.parse(atob(segments[1].replace(/-/g, '+').replace(/_/g, '/')));
    if (!isJsonObject(claims) || typeof claims.exp !== 'number') return undefined;
    return toRfc3339(claims.exp * 1000);
  } catch {
    return undefined;
  }
}

/** The last path segment, lower-cased: a user may upload from a path or a renamed copy. */
function baseName(filename: string): string {
  return (filename.split(/[\\/]/).pop() ?? '').toLowerCase();
}

function parseClaudeCode(oauth: JsonObject): ParsedCredential | ParsedCredentialError {
  const accessToken = nonBlankString(oauth.accessToken);
  if (accessToken === undefined) return { error: ERROR_CLAUDE_NO_ACCESS_TOKEN };

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
    return nonBlankString(tokens?.id_token) !== undefined
      ? { error: ERROR_CODEX_ID_TOKEN_ONLY }
      : { error: ERROR_CODEX_NO_ACCESS_TOKEN };
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
 * fields a token import needs, or an `error` saying why it cannot be used. It
 * never throws.
 *
 * The vendor is detected from the file's CONTENT, because a user may rename the
 * file; `filename` only breaks the tie in the unlikely case the content carries
 * both shapes.
 */
export function parseCredentialFile(
  filename: string,
  text: string,
): ParsedCredential | ParsedCredentialError {
  let root: unknown;
  try {
    // A byte-order mark (some Windows editors add one) is not valid JSON.
    root = JSON.parse(text.replace(/^\uFEFF/, ''));
  } catch {
    return { error: ERROR_NOT_JSON };
  }
  if (!isJsonObject(root)) return { error: ERROR_NOT_OBJECT };

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
    return { error: ERROR_AMBIGUOUS };
  }
  if (claude !== undefined) return parseClaudeCode(claude);
  if (isCodex) return parseCodex(codex);
  return { error: ERROR_UNRECOGNISED };
}
