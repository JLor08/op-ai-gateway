// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The reason behind a failed Voice Chat turn (issue #329), for the user to read on request. The
// error card's headline is one generic sentence per kind of failure; the gateway's `error` frame
// (or the closed connection) carries the real reason -- a stable `code` and a free-text message --
// and this is the pure rule for what of it is worth showing, kept apart from the card so it can be
// read and tested on its own.

import type { VoiceChatError } from './useVoiceChat';
import { VOICE_TURN_FALLBACK_MESSAGE } from './voiceSession';

/**
 * The failure's real reason as one text, or `null` when there is nothing to add to the headline.
 *
 * Only the failures that come from the gateway or the connection have one: a `turn`, a `refused`
 * ticket, a `dropped` connection. The others (`denied`, `unsupported`, `capture`) are the browser's
 * own, and their headline already says all there is to say.
 *
 * The code and the message are shown one above the other when both are there and differ; a value
 * that is both (the session's own fallback for a frame with a code and no message) is shown once.
 * The generic fallback sentence is dropped: it is no reason, only the headline again.
 */
export function errorDetailText(error: VoiceChatError): string | null {
  if (error.kind !== 'turn' && error.kind !== 'refused' && error.kind !== 'dropped') return null;
  const code = error.code?.trim() ?? '';
  let msg = error.message?.trim() ?? '';
  if (msg === VOICE_TURN_FALLBACK_MESSAGE) msg = '';
  if (code && msg && code !== msg) return `${code}\n${msg}`;
  return code || msg || null;
}
