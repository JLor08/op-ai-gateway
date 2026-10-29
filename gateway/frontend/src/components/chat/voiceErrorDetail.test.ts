// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import type { VoiceChatError } from './useVoiceChat';
import { errorDetailText } from './voiceErrorDetail';
import { VOICE_TURN_FALLBACK_MESSAGE } from './voiceSession';

const failure = (error: VoiceChatError): VoiceChatError => error;

describe('errorDetailText', () => {
  it('shows the message of a failed turn that carries no code', () => {
    const message = 'live request body stalled: no data within the idle timeout';
    expect(errorDetailText(failure({ kind: 'turn', message }))).toBe(message);
  });

  it('shows both the code and the message when they differ', () => {
    const text = errorDetailText(
      failure({
        kind: 'refused',
        code: 'portal.voice_chat_asr_unavailable',
        message: 'asr leg failed',
      }),
    );
    expect(text).toContain('portal.voice_chat_asr_unavailable');
    expect(text).toContain('asr leg failed');
  });

  it('shows the code first and the message after it, each on its own line', () => {
    expect(
      errorDetailText(failure({ kind: 'turn', code: 'portal.some_code', message: 'it broke' })),
    ).toBe('portal.some_code\nit broke');
  });

  it('shows a value once when the message is the code', () => {
    const text = errorDetailText(failure({ kind: 'turn', code: 'x', message: 'x' }));
    expect(text).toBe('x');
  });

  it('shows the code alone when the message is empty', () => {
    expect(errorDetailText(failure({ kind: 'turn', code: 'portal.some_code', message: '' }))).toBe(
      'portal.some_code',
    );
  });

  it('shows nothing for the generic fallback sentence, which says no more than the headline', () => {
    expect(
      errorDetailText(failure({ kind: 'turn', message: VOICE_TURN_FALLBACK_MESSAGE })),
    ).toBeNull();
  });

  it('shows only the code when the message is the generic fallback sentence', () => {
    expect(
      errorDetailText(
        failure({ kind: 'turn', code: 'portal.some_code', message: VOICE_TURN_FALLBACK_MESSAGE }),
      ),
    ).toBe('portal.some_code');
  });

  it('shows nothing when there is neither a code nor a message', () => {
    expect(errorDetailText(failure({ kind: 'turn', message: '' }))).toBeNull();
    expect(errorDetailText(failure({ kind: 'turn', message: '   ' }))).toBeNull();
    expect(errorDetailText(failure({ kind: 'turn', code: '  ', message: ' ' }))).toBeNull();
  });

  it('trims the code and the message before it compares and shows them', () => {
    expect(errorDetailText(failure({ kind: 'turn', code: ' x ', message: 'x' }))).toBe('x');
    expect(errorDetailText(failure({ kind: 'turn', code: ' a ', message: ' b ' }))).toBe('a\nb');
  });

  it('shows what a dropped connection says about itself', () => {
    const message = 'voice chat connection closed (1006)';
    expect(errorDetailText(failure({ kind: 'dropped', message }))).toBe(message);
  });

  it.each(['denied', 'unsupported', 'capture'] as const)(
    'has nothing to disclose for a %s failure, whatever it carries',
    (kind) => {
      expect(
        errorDetailText(failure({ kind, code: 'portal.some_code', message: 'some message' })),
      ).toBeNull();
    },
  );
});
