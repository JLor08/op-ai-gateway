// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  CONFIG_FILES_MAX,
  CONFIG_FILE_MAX_BYTES,
  configFileIssues,
  configFileNameIssue,
} from './configFiles';

describe('configFileNameIssue', () => {
  it.each(['models.ini', 'config.yaml', '.env', 'a b.txt', 'ümlaut.json', 'x'])(
    'accepts %j as one plain file name',
    (name) => {
      expect(configFileNameIssue(name)).toBeNull();
    },
  );

  it.each(['', '   ', '\t'])('flags the blank name %j', (name) => {
    expect(configFileNameIssue(name)).toBe('name_blank');
  });

  // The gateway's configFileNameIsSingleSegment (portal/service_runtime.go),
  // case by case: either OS's separator, the dot names, the volume/stream
  // marker and any control character.
  it.each([
    '.',
    '..',
    'a/b.ini',
    '../x',
    '/abs',
    'a\\b.ini',
    'C:\\x',
    'c:x',
    'stream:alt',
    'a\u0000b',
    'a\nb',
    'tab\there',
    'del\u007fx',
    'c1\u0085x',
  ])('flags %j as not a single file name', (name) => {
    expect(configFileNameIssue(name)).toBe('name_invalid');
  });
});

describe('configFileIssues', () => {
  const ok = { name: 'models.ini', content: 'x = 1\n' };

  it('is null for every row of a clean list, and for an empty list', () => {
    expect(configFileIssues([])).toEqual([]);
    expect(configFileIssues([ok, { name: 'b.ini', content: '' }])).toEqual([null, null]);
  });

  it('flags a repeated name on the LATER row, compared case-insensitively', () => {
    expect(
      configFileIssues([
        { name: 'Models.INI', content: '' },
        { name: 'models.ini', content: '' },
      ]),
    ).toEqual([null, 'name_duplicate']);
  });

  it('reports the name rule before the content rules, like the gateway', () => {
    expect(configFileIssues([{ name: '../x', content: '${API_TOKEN}' }])).toEqual(['name_invalid']);
  });

  it.each(['token=${API_TOKEN}', 'k=${AGENT_ENV:HF_TOKEN}', '${AGENT_ENV:'])(
    'flags the secret placeholder in %j',
    (content) => {
      expect(configFileIssues([{ name: 'a', content }])).toEqual(['content_secret']);
    },
  );

  it('lets the non-secret placeholders through', () => {
    expect(
      configFileIssues([{ name: 'a', content: 'port=${PORT}\nmodel=${MODEL}\ndir=${CONFIG_DIR}' }]),
    ).toEqual([null]);
  });

  it('bounds the content in BYTES, not characters', () => {
    const atLimit = 'a'.repeat(CONFIG_FILE_MAX_BYTES);
    expect(configFileIssues([{ name: 'a', content: atLimit }])).toEqual([null]);
    expect(configFileIssues([{ name: 'a', content: atLimit + 'a' }])).toEqual([
      'content_too_large',
    ]);
    // 3 bytes per "€": well under the limit in characters, well over in bytes.
    const euros = '€'.repeat(Math.floor(CONFIG_FILE_MAX_BYTES / 3) + 1);
    expect(euros.length).toBeLessThan(CONFIG_FILE_MAX_BYTES);
    expect(configFileIssues([{ name: 'a', content: euros }])).toEqual(['content_too_large']);
  });

  it('pins the limits to the gateway constants', () => {
    // runtimeSpecConfigFilesMax / runtimeSpecConfigFileMaxBytes in
    // portal/service_runtime.go: 16 files of 64 KiB.
    expect(CONFIG_FILES_MAX).toBe(16);
    expect(CONFIG_FILE_MAX_BYTES).toBe(64 * 1024);
  });
});
