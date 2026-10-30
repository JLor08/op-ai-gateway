// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';

import { isWebkitIOS, voiceCaptureAudioConstraints, type NavigatorLike } from './micConstraints';

const nav = (userAgent: string, maxTouchPoints = 0): NavigatorLike => ({
  userAgent,
  maxTouchPoints,
});

// Real-world user-agent strings. Edge/Chrome/Firefox on iOS are all WebKit and all carry "iPhone".
const IPHONE_SAFARI =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1';
const IPHONE_EDGE =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 EdgiOS/129.0 Mobile/15E148 Safari/604.1';
const IPADOS_AS_MAC =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15';
const MAC_CHROME =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36';
const WINDOWS_CHROME =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36';

describe('isWebkitIOS', () => {
  it('is true for an iPhone, whatever the browser skin', () => {
    expect(isWebkitIOS(nav(IPHONE_SAFARI))).toBe(true);
    expect(isWebkitIOS(nav(IPHONE_EDGE))).toBe(true);
  });

  it('is true for iPadOS 13+ that masquerades as a touch "Macintosh"', () => {
    expect(isWebkitIOS(nav(IPADOS_AS_MAC, 5))).toBe(true);
  });

  it('is false for a real Mac (no touch) and for Windows', () => {
    expect(isWebkitIOS(nav(MAC_CHROME, 0))).toBe(false);
    expect(isWebkitIOS(nav(WINDOWS_CHROME, 0))).toBe(false);
  });

  it('is false when there is no navigator', () => {
    expect(isWebkitIOS(undefined)).toBe(false);
  });
});

describe('voiceCaptureAudioConstraints', () => {
  it('drops all voice processing on iOS so the TTS playout stays clean', () => {
    expect(voiceCaptureAudioConstraints(nav(IPHONE_SAFARI))).toEqual({
      echoCancellation: false,
      noiseSuppression: false,
      autoGainControl: false,
    });
  });

  it('keeps echo cancellation and noise suppression on every other platform', () => {
    expect(voiceCaptureAudioConstraints(nav(MAC_CHROME))).toEqual({
      echoCancellation: true,
      noiseSuppression: true,
    });
  });
});
