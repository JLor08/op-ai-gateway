// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// React keys for a list of URLs that can repeat (Sonar S6479: an array index names a
// slot, not a thing).
//
// The URL is the thing, but the same URL can legitimately occur twice -- the same
// file attached twice, the same clip returned twice -- and two siblings with one
// key are worse than an index. So the first copy is keyed by the URL itself and each
// further copy gets a `#<n>` suffix, bumped until the key is free: unique by
// construction (the loop checks every key already handed out, so even a URL that
// itself ends in `#1` cannot collide), the same on every render while the list is
// unchanged, and, for a URL that occurs once, independent of where in the list it is.
//
// The list position is returned alongside because the callers that REMOVE an item
// address it by position (removing one of two identical attachments must remove one).
export interface KeyedUrl {
  key: string;
  url: string;
  index: number;
}

export function keyedUrls(urls: readonly string[]): KeyedUrl[] {
  const taken = new Set<string>();
  return urls.map((url, index) => {
    let key = url;
    let copy = 0;
    while (taken.has(key)) {
      copy += 1;
      key = `${url}#${copy}`;
    }
    taken.add(key);
    return { key, url, index };
  });
}
