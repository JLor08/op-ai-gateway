// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The microphone `audio` constraints for a Voice Chat capture (#312), kept apart from the session
// (voiceSession.ts) so the platform carve-out can be tested without a browser.
//
// Echo cancellation and noise suppression are a belt-and-suspenders second line under the
// server-authoritative half-duplex gate (spec 6.2): the gate already stops TTS-era audio from
// reaching the ASR, so these only damp room echo the microphone itself picks up. On iOS/WebKit,
// however, requesting any WebRTC voice processing (echoCancellation / noiseSuppression /
// autoGainControl) switches the whole audio session into the voice-processing I/O path, which
// degrades the TTS *playout* -- not just the capture -- into a continuous crackle for the entire
// session. Because the gate, not the browser, prevents the microphone -> TTS -> microphone loop,
// dropping the processing on iOS is safe: it trades a little room-echo damping for clean playback.
// Every other platform keeps the processing, where it helps the ASR and costs nothing audible.

/** The subset of `navigator` the platform check reads; injectable so it can be tested. */
export type NavigatorLike = Pick<Navigator, 'userAgent' | 'maxTouchPoints'>;

function currentNavigator(): NavigatorLike | undefined {
  return typeof navigator === 'undefined' ? undefined : navigator;
}

/**
 * True on an Apple mobile browser (iPhone / iPod / iPad -- every engine, since they are all
 * WebKit). iPadOS 13+ reports itself as "Macintosh", so a touch-capable "Macintosh" is taken to
 * be an iPad.
 */
export function isWebkitIOS(nav: NavigatorLike | undefined = currentNavigator()): boolean {
  if (!nav) return false;
  const ua = nav.userAgent ?? '';
  if (/iPhone|iPod|iPad/.test(ua)) return true;
  return /Macintosh/.test(ua) && (nav.maxTouchPoints ?? 0) > 1;
}

/**
 * The `audio` constraints to open the Voice Chat microphone with. See the file header for why iOS
 * drops all WebRTC voice processing while every other platform keeps echo cancellation and noise
 * suppression.
 */
export function voiceCaptureAudioConstraints(
  nav: NavigatorLike | undefined = currentNavigator(),
): MediaTrackConstraints {
  if (isWebkitIOS(nav)) {
    return { echoCancellation: false, noiseSuppression: false, autoGainControl: false };
  }
  return { echoCancellation: true, noiseSuppression: true };
}
