// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { keyedUrls } from './urlKeys';

describe('keyedUrls', () => {
  it('keys a URL by itself and keeps the list position beside it', () => {
    expect(keyedUrls(['data:a', 'data:b'])).toEqual([
      { key: 'data:a', url: 'data:a', index: 0 },
      { key: 'data:b', url: 'data:b', index: 1 },
    ]);
  });

  it('gives each repeat of a URL its own key, the first copy keeping the bare URL', () => {
    const keyed = keyedUrls(['data:a', 'data:b', 'data:a', 'data:a']);
    expect(keyed.map((k) => k.key)).toEqual(['data:a', 'data:b', 'data:a#1', 'data:a#2']);
    expect(keyed.map((k) => k.url)).toEqual(['data:a', 'data:b', 'data:a', 'data:a']);
  });

  it('keeps every key unique even when a URL already looks like a suffixed key', () => {
    const keys = keyedUrls(['u', 'u', 'u#1', 'u#1']).map((k) => k.key);
    expect(new Set(keys).size).toBe(4);
  });

  it('does not move a once-occurring URL to another key when an earlier item goes', () => {
    const before = keyedUrls(['data:a', 'data:b']);
    const after = keyedUrls(['data:b']);
    expect(after[0].key).toBe(before[1].key);
  });

  it('returns an empty list for an empty list', () => {
    expect(keyedUrls([])).toEqual([]);
  });
});
