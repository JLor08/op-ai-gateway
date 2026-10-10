// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  isValidBaseUrl,
  isValidCodexClientVersion,
  isValidModelPrefix,
  MODEL_PREFIX_MAX_LENGTH,
  normalizeModelPrefix,
} from './vendorInputs';

describe('isValidModelPrefix', () => {
  it.each([
    '',
    '   ',
    'chatgpt/',
    'work',
    'a.b_c~d:e/f@g+h-i',
    'ABC123',
    'x'.repeat(MODEL_PREFIX_MAX_LENGTH),
    '  chatgpt/  ',
    'a.b',
  ])('accepts %j', (prefix) => {
    expect(isValidModelPrefix(prefix)).toBe(true);
  });

  it.each([
    ['a space inside', 'chat gpt/'],
    ['a query character', 'chat?gpt'],
    ['a fragment character', 'chat#gpt'],
    ['a percent escape', 'chat%2Fgpt'],
    ['a quote', 'chat"gpt'],
    ['an angle bracket', '<chat>'],
    ['an ampersand', 'chat&gpt'],
    ['a backslash', 'chat\\gpt'],
    ['a non-ASCII letter', 'chätgpt/'],
    ['an emoji', 'chat🙂'],
    ['a newline inside', 'chat\ngpt'],
    ['a dot-dot', 'chat..gpt'],
    ['a leading dot-dot', '../chat'],
  ])('rejects a prefix with %s', (_why, prefix) => {
    expect(isValidModelPrefix(prefix)).toBe(false);
  });

  it('rejects a prefix over the length limit and accepts one at the limit', () => {
    expect(isValidModelPrefix('x'.repeat(MODEL_PREFIX_MAX_LENGTH))).toBe(true);
    expect(isValidModelPrefix('x'.repeat(MODEL_PREFIX_MAX_LENGTH + 1))).toBe(false);
  });

  it('judges the trimmed value, like the backend', () => {
    // 64 characters plus padding is still 64 once trimmed.
    expect(isValidModelPrefix(` ${'x'.repeat(MODEL_PREFIX_MAX_LENGTH)} `)).toBe(true);
    expect(normalizeModelPrefix('  chatgpt/ ')).toBe('chatgpt/');
  });
});

describe('isValidCodexClientVersion', () => {
  it.each(['26.930.61225', '0.46.0-alpha.1', '1', '9a', '1.2.3+build_5', ` 26.1 `, ''])(
    'accepts %j',
    (version) => {
      expect(isValidCodexClientVersion(version)).toBe(true);
    },
  );

  it.each([
    ['a leading v', 'v26.1'],
    ['a leading dot', '.1'],
    ['a leading letter', 'abc'],
    ['a space inside', '26 1'],
    ['a slash', '26/1'],
    ['a non-ASCII character', '26.1é'],
    ['a newline inside', '26\n1'],
    ['an over-long value', `1${'0'.repeat(64)}`],
  ])('rejects %s', (_why, version) => {
    expect(isValidCodexClientVersion(version)).toBe(false);
  });

  it('accepts a 64 character version and rejects 65', () => {
    expect(isValidCodexClientVersion(`1${'0'.repeat(63)}`)).toBe(true);
    expect(isValidCodexClientVersion(`1${'0'.repeat(64)}`)).toBe(false);
  });
});

describe('isValidBaseUrl', () => {
  it.each([
    '',
    '   ',
    'https://api.x.ai',
    'https://openrouter.ai/api',
    'https://llm.example.com/',
    'https://llm.internal:8443/proxy/v2',
    'https://10.0.0.5:8443',
    'https://[::1]:8443/api',
    ' https://api.x.ai ',
    'HTTPS://API.X.AI',
  ])('accepts %j', (value) => {
    expect(isValidBaseUrl(value)).toBe(true);
  });

  it.each([
    ['plain http', 'http://x.test'],
    ['no scheme', 'api.x.ai'],
    ['not a url', 'not-a-url'],
    ['another scheme', 'ftp://x.test'],
    ['an empty hostname', 'https://'],
    ['a port but no hostname', 'https://:443'],
    ['userinfo with a password', 'https://u:p@x.test'],
    ['userinfo with a name only', 'https://u@x.test'],
    ['an empty userinfo', 'https://@x.test'],
    ['a query', 'https://x.test/?a=1'],
    ['a bare question mark', 'https://x.test/?'],
    ['a fragment', 'https://x.test/#f'],
    ['a bare hash', 'https://x.test/#'],
    ['a space inside', 'https://x.test/a b'],
    ['a newline inside', 'https://x.test/a\nb'],
  ])('rejects a URL with %s', (_why, value) => {
    expect(isValidBaseUrl(value)).toBe(false);
  });
});
