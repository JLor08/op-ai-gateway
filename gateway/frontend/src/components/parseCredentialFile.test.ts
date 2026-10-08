// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { isParsedCredentialError, parseCredentialFile } from './parseCredentialFile';
import type { ParsedCredential, ParsedCredentialError } from './parseCredentialFile';

// base64url (RFC 4648 section 5, unpadded) of a JSON object, the encoding of a JWT segment.
function b64url(value: unknown): string {
  return btoa(JSON.stringify(value)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// A structurally valid, unsigned JWT. The `note` claim is chosen so the payload's
// standard base64 contains '+' and '/' and ends in '=' padding: the decoder must cope with all three.
function jwt(claims: Record<string, unknown>): string {
  return [
    b64url({ alg: 'none', typ: 'JWT' }),
    b64url({ note: '>>>???~~~', ...claims }),
    'signature',
  ].join('.');
}

// 2026-09-21T14:13:20.000Z, in seconds (JWT exp) and in milliseconds (Claude Code).
const EXP_SECONDS = 1790000000;
const EXP_MILLIS = 1790000000000;
const EXP_RFC3339 = '2026-09-21T14:13:20.000Z';

function expectCredential(result: ParsedCredential | ParsedCredentialError): ParsedCredential {
  if (isParsedCredentialError(result)) {
    throw new Error(`expected a credential, got error: ${result.error}`);
  }
  return result;
}

function expectError(result: ParsedCredential | ParsedCredentialError): string {
  if (!isParsedCredentialError(result)) {
    throw new Error('expected an error, got a credential');
  }
  return result.error;
}

describe('parseCredentialFile: Claude Code .credentials.json', () => {
  it('extracts the tokens and converts the millisecond-epoch expiry to RFC 3339 UTC', () => {
    const text = JSON.stringify({
      claudeAiOauth: {
        accessToken: 'sk-ant-oat01-access',
        refreshToken: 'sk-ant-ort01-refresh',
        expiresAt: EXP_MILLIS,
        scopes: ['user:inference', 'user:profile'],
        subscriptionType: 'max',
      },
    });

    const result = expectCredential(parseCredentialFile('.credentials.json', text));

    expect(result).toStrictEqual({
      vendor: 'anthropic',
      accessToken: 'sk-ant-oat01-access',
      refreshToken: 'sk-ant-ort01-refresh',
      expiresAt: EXP_RFC3339,
    });
  });

  it('treats the refresh token and the expiry as optional', () => {
    const text = JSON.stringify({ claudeAiOauth: { accessToken: 'sk-ant-oat01-access' } });

    const result = expectCredential(parseCredentialFile('.credentials.json', text));

    expect(result).toStrictEqual({ vendor: 'anthropic', accessToken: 'sk-ant-oat01-access' });
  });

  it('drops an expiry that is not a usable millisecond epoch rather than failing', () => {
    for (const expiresAt of ['soon', null, Number.NaN, 1e21, {}]) {
      const text = JSON.stringify({ claudeAiOauth: { accessToken: 'tok', expiresAt } });

      const result = expectCredential(parseCredentialFile('.credentials.json', text));

      expect(result.accessToken).toBe('tok');
      expect(result.expiresAt).toBeUndefined();
    }
  });

  it('detects the shape from the content, not the file name', () => {
    const text = JSON.stringify({ claudeAiOauth: { accessToken: 'tok' } });

    expect(expectCredential(parseCredentialFile('renamed-copy.json', text)).vendor).toBe(
      'anthropic',
    );
    expect(expectCredential(parseCredentialFile('', text)).vendor).toBe('anthropic');
  });

  it('rejects a file with no access token', () => {
    for (const oauth of [
      {},
      { refreshToken: 'sk-ant-ort01-refresh' },
      { accessToken: '' },
      { accessToken: '   ' },
      { accessToken: 42 },
    ]) {
      const text = JSON.stringify({ claudeAiOauth: oauth });

      const error = expectError(parseCredentialFile('.credentials.json', text));

      expect(error).toContain('accessToken');
      expect(error).not.toContain('sk-ant-ort01-refresh');
    }
  });
});

describe('parseCredentialFile: Codex auth.json', () => {
  it('extracts the tokens and account id and derives the expiry from the access-token JWT', () => {
    const accessToken = jwt({ exp: EXP_SECONDS });
    const text = JSON.stringify({
      OPENAI_API_KEY: null,
      tokens: {
        id_token: jwt({ email: 'someone@example.test' }),
        access_token: accessToken,
        refresh_token: 'rt-refresh',
        account_id: 'acct-123',
      },
      last_refresh: '2026-09-20T10:00:00Z',
    });

    const result = expectCredential(parseCredentialFile('auth.json', text));

    expect(result).toStrictEqual({
      vendor: 'openai',
      accessToken,
      refreshToken: 'rt-refresh',
      expiresAt: EXP_RFC3339,
      accountId: 'acct-123',
    });
  });

  it('treats the refresh token and the account id as optional', () => {
    const accessToken = jwt({ exp: EXP_SECONDS });
    const text = JSON.stringify({ tokens: { access_token: accessToken } });

    const result = expectCredential(parseCredentialFile('auth.json', text));

    expect(result).toStrictEqual({
      vendor: 'openai',
      accessToken,
      expiresAt: EXP_RFC3339,
    });
  });

  it('leaves the expiry undefined, but still succeeds, when the access token is not a JWT', () => {
    for (const accessToken of [
      'opaque-access-token',
      'a.b.c',
      // A JWT whose payload carries no usable exp.
      jwt({}),
      jwt({ exp: 'tomorrow' }),
      jwt({ exp: 1e20 }),
    ]) {
      const text = JSON.stringify({ tokens: { access_token: accessToken, refresh_token: 'rt' } });

      const result = expectCredential(parseCredentialFile('auth.json', text));

      expect(result.vendor).toBe('openai');
      expect(result.accessToken).toBe(accessToken);
      expect(result.refreshToken).toBe('rt');
      expect(result.expiresAt).toBeUndefined();
    }
  });

  it('detects the shape from the content, not the file name', () => {
    const text = JSON.stringify({ tokens: { access_token: 'tok' } });

    expect(expectCredential(parseCredentialFile('my-codex-login.json', text)).vendor).toBe(
      'openai',
    );
  });

  it('rejects an id_token-only file and tells the user to use the access_token', () => {
    const idToken = jwt({ email: 'someone@example.test' });
    const text = JSON.stringify({
      OPENAI_API_KEY: 'sk-live-do-not-leak',
      tokens: { id_token: idToken, refresh_token: 'rt-refresh', account_id: 'acct-123' },
    });

    const error = expectError(parseCredentialFile('auth.json', text));

    expect(error).toContain('access_token');
    expect(error).toContain('id_token');
    expect(error).not.toContain(idToken);
    expect(error).not.toContain('sk-live-do-not-leak');
    expect(error).not.toContain('rt-refresh');
  });

  it('rejects a file whose tokens carry no usable access_token', () => {
    for (const tokens of [{}, { access_token: '' }, { access_token: '  ' }, { access_token: 7 }]) {
      const text = JSON.stringify({ tokens });

      expect(expectError(parseCredentialFile('auth.json', text))).toContain('access_token');
    }
  });

  it('rejects an API-key-only file without echoing the key', () => {
    const text = JSON.stringify({ OPENAI_API_KEY: 'sk-live-do-not-leak', tokens: null });

    const error = expectError(parseCredentialFile('auth.json', text));

    expect(error).toContain('access_token');
    expect(error).not.toContain('sk-live-do-not-leak');
  });
});

describe('parseCredentialFile: bad input never throws', () => {
  it('reports malformed JSON without echoing any of the text', () => {
    // V8's own SyntaxError message quotes a fragment of the input, so the error
    // must not be built from it.
    const text = '{"claudeAiOauth": {"accessToken": "sk-ant-oat01-secret"';

    const error = expectError(parseCredentialFile('.credentials.json', text));

    expect(error).toMatch(/json/i);
    expect(error).not.toContain('sk-ant-oat01-secret');
  });

  it('reports an empty file', () => {
    expect(expectError(parseCredentialFile('auth.json', ''))).toMatch(/json/i);
    expect(expectError(parseCredentialFile('auth.json', '  \n'))).toMatch(/json/i);
  });

  it('reads a file that starts with a byte-order mark', () => {
    const text = '\uFEFF' + JSON.stringify({ claudeAiOauth: { accessToken: 'tok' } });

    expect(expectCredential(parseCredentialFile('.credentials.json', text)).accessToken).toBe(
      'tok',
    );
  });

  it('rejects a root that is not an object', () => {
    for (const text of [
      '[]',
      '[{"tokens":{"access_token":"t"}}]',
      '"sk-ant-oat01-secret"',
      '42',
      'null',
      'true',
    ]) {
      const error = expectError(parseCredentialFile('auth.json', text));

      expect(error).toMatch(/object/i);
      expect(error).not.toContain('sk-ant-oat01-secret');
    }
  });

  it('rejects an unrecognised shape and names the two supported files', () => {
    const text = JSON.stringify({ access_token: 'sk-top-level-secret', token: 'x' });

    const error = expectError(parseCredentialFile('credentials.json', text));

    expect(error).toContain('auth.json');
    expect(error).toContain('.credentials.json');
    expect(error).not.toContain('sk-top-level-secret');
  });

  it('rejects a non-object claudeAiOauth or tokens value as unrecognised', () => {
    expect(
      expectError(parseCredentialFile('.credentials.json', '{"claudeAiOauth":"tok"}')),
    ).toContain('.credentials.json');
    expect(expectError(parseCredentialFile('auth.json', '{"tokens":"tok"}'))).toContain(
      'auth.json',
    );
    expect(expectError(parseCredentialFile('auth.json', '{"tokens":[]}'))).toContain('auth.json');
  });
});

describe('parseCredentialFile: a file carrying both shapes', () => {
  const both = JSON.stringify({
    claudeAiOauth: { accessToken: 'claude-tok' },
    tokens: { access_token: 'codex-tok' },
  });

  it('uses the file name as the tie-break hint', () => {
    expect(expectCredential(parseCredentialFile('.credentials.json', both))).toMatchObject({
      vendor: 'anthropic',
      accessToken: 'claude-tok',
    });
    expect(expectCredential(parseCredentialFile('auth.json', both))).toMatchObject({
      vendor: 'openai',
      accessToken: 'codex-tok',
    });
    expect(
      expectCredential(parseCredentialFile('/home/me/.claude/.credentials.json', both)).vendor,
    ).toBe('anthropic');
    expect(
      expectCredential(parseCredentialFile('C:\\Users\\me\\.codex\\auth.json', both)).vendor,
    ).toBe('openai');
  });

  it('refuses to guess when the file name does not break the tie', () => {
    const error = expectError(parseCredentialFile('renamed.json', both));

    expect(error).toContain('auth.json');
    expect(error).toContain('.credentials.json');
    expect(error).not.toContain('claude-tok');
    expect(error).not.toContain('codex-tok');
  });
});
