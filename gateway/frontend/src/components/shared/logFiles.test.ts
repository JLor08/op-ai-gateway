// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { LOG_FILES_MAX, logFileIssues } from './logFiles';

const ok = { name: 'app', path: '${CONFIG_DIR}/app.log' };

function issue(file: { name: string; path: string }) {
  return logFileIssues([file])[0];
}

describe('logFileIssues', () => {
  it('exposes the gateway limit', () => {
    expect(LOG_FILES_MAX).toBe(4);
  });

  it.each([
    ['${CONFIG_DIR}/logs/app.log'],
    ['${CONFIG_DIR}\\logs\\app.log'],
    ['${CONFIG_DIR}'],
    ['/var/log/app.log'],
    ['C:\\logs\\app.log'],
    ['c:/logs/app.log'],
    ['\\\\host\\share\\app.log'],
    ['//host/share/app.log'],
    ['\\\\?\\c:\\x\\app.log'],
    ['\\\\.\\c:\\x'],
    ['\\??\\c:\\x'],
  ])('accepts the path %j', (path) => {
    expect(issue({ name: 'app', path })).toBeNull();
  });

  it('accepts names at the pattern edge', () => {
    expect(issue({ name: 'a.B_c-9', path: ok.path })).toBeNull();
    expect(issue({ name: 'x'.repeat(32), path: ok.path })).toBeNull();
  });

  it.each(['', '   '])('flags the blank name %j', (name) => {
    expect(issue({ name, path: ok.path })).toBe('name_blank');
  });

  it.each(['has space', 'a/b', 'x'.repeat(33), 'ümlaut', 'a:b', 'new\nline'])(
    'flags the invalid name %j',
    (name) => {
      expect(issue({ name, path: ok.path })).toBe('name_invalid');
    },
  );

  it.each(['process', 'Process', 'PROCESS'])('flags the reserved name %j', (name) => {
    expect(issue({ name, path: ok.path })).toBe('name_reserved');
  });

  it('flags the later of two names that collide case-insensitively', () => {
    expect(logFileIssues([ok, { name: 'APP', path: '/x/y' }])).toEqual([null, 'name_duplicate']);
  });

  it('does not let an invalid row claim its name', () => {
    expect(logFileIssues([{ name: 'a b', path: ok.path }, ok])).toEqual(['name_invalid', null]);
  });

  it('flags a blank path', () => {
    expect(issue({ name: 'app', path: '' })).toBe('path_blank');
  });

  it('measures the path length in UTF-8 bytes', () => {
    const atLimit = '/' + 'a'.repeat(1023);
    expect(issue({ name: 'app', path: atLimit })).toBeNull();
    expect(issue({ name: 'app', path: atLimit + 'a' })).toBe('path_too_long');
    // 512 two-byte characters = 1024 bytes plus the slash: over, though 513 characters.
    expect(issue({ name: 'app', path: '/' + 'ü'.repeat(512) })).toBe('path_too_long');
    expect(issue({ name: 'app', path: '/' + 'ü'.repeat(511) })).toBeNull();
  });

  it.each(['/var/log/a\nb', '/var/log/a\u0000b', '/var/log/a\u007fb', '/var/log/a\u0085b'])(
    'flags a control character in %j',
    (path) => {
      expect(issue({ name: 'app', path })).toBe('path_control');
    },
  );

  it.each(['${CONFIG_DIR}/${API_TOKEN}.log', '/x/${AGENT_ENV:FOO}', '/x/${AGENT_ENV:'])(
    'flags the secret placeholder in %j',
    (path) => {
      expect(issue({ name: 'app', path })).toBe('path_secret');
    },
  );

  it.each([
    'relative/app.log',
    './app.log',
    '${CONFIG_DIR}x/app.log',
    '${CONFIG_DIR}logs',
    '${PORT}/app.log',
    'C:app.log',
    'C:',
    '\\app.log',
  ])('flags the non-absolute path %j', (path) => {
    // "\\app.log" is root-relative on Windows and not POSIX-absolute.
    expect(issue({ name: 'app', path })).toBe('path_not_absolute');
  });

  it('reports the first problem in the gateway order: name, reserved, duplicate, path', () => {
    expect(issue({ name: 'process', path: '' })).toBe('name_reserved');
    expect(issue({ name: 'app', path: '${API_TOKEN}' })).toBe('path_secret');
  });

  it('is index-aligned', () => {
    expect(logFileIssues([ok, { name: 'b', path: 'rel' }, ok])).toEqual([
      null,
      'path_not_absolute',
      'name_duplicate',
    ]);
  });
});
