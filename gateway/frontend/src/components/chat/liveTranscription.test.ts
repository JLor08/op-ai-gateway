// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import {
  LIVE_WS_SUBPROTOCOL,
  createLiveSession,
  liveTranscriptionWsUrl,
  parseLiveDownFrame,
} from './liveTranscription';

describe('parseLiveDownFrame', () => {
  it.each(['delta', 'partial', 'done'] as const)('reads a %s frame with its text', (type) => {
    expect(parseLiveDownFrame(JSON.stringify({ type, text: 'hello wor' }))).toEqual({
      type,
      text: 'hello wor',
    });
  });

  it('reads an error frame with its message', () => {
    expect(parseLiveDownFrame('{"type":"error","message":"upstream unavailable"}')).toEqual({
      type: 'error',
      message: 'upstream unavailable',
    });
  });

  it('reads an error frame with the gateway code the session was refused with', () => {
    expect(
      parseLiveDownFrame(
        '{"type":"error","message":"a session is already active","code":"transcription.live_busy"}',
      ),
    ).toEqual({
      type: 'error',
      message: 'a session is already active',
      code: 'transcription.live_busy',
    });
  });

  it.each([
    ['empty', '""'],
    ['not a string', '503'],
    ['null', 'null'],
    ['an object', '{"a":1}'],
  ])(
    'leaves the code off an error frame whose code is %s, and still reads the error',
    (_n, code) => {
      expect(parseLiveDownFrame(`{"type":"error","message":"nope","code":${code}}`)).toEqual({
        type: 'error',
        message: 'nope',
      });
    },
  );

  it('reads a frame whose empty field the gateway left out (omitempty) as empty', () => {
    // The gateway's envelope drops an empty text/message, so a done with nothing to add (keep
    // what you had) arrives as just its type.
    expect(parseLiveDownFrame('{"type":"done"}')).toEqual({ type: 'done', text: '' });
    expect(parseLiveDownFrame('{"type":"partial"}')).toEqual({ type: 'partial', text: '' });
    expect(parseLiveDownFrame('{"type":"delta"}')).toEqual({ type: 'delta', text: '' });
    expect(parseLiveDownFrame('{"type":"error"}')).toEqual({ type: 'error', message: '' });
  });

  it('keeps whitespace and unicode in the text as it came', () => {
    expect(parseLiveDownFrame(JSON.stringify({ type: 'delta', text: ' Grüße, 世界 \n' }))).toEqual({
      type: 'delta',
      text: ' Grüße, 世界 \n',
    });
  });

  it('ignores fields it does not know', () => {
    expect(parseLiveDownFrame('{"type":"delta","text":"a","seq":4,"extra":{"x":1}}')).toEqual({
      type: 'delta',
      text: 'a',
    });
  });

  it.each([
    ['not json', 'hello'],
    ['an empty string', ''],
    ['truncated json', '{"type":"delta","text":'],
    ['a json string', '"delta"'],
    ['a json number', '7'],
    ['json null', 'null'],
    ['a json array', '[{"type":"delta","text":"a"}]'],
    ['an object without a type', '{"text":"a"}'],
    ['an unknown type', '{"type":"ping","text":"a"}'],
    ['a non-string type', '{"type":3,"text":"a"}'],
    ['a type of the wrong case', '{"type":"DELTA","text":"a"}'],
    ['a text that is not a string', '{"type":"delta","text":5}'],
    ['a null text', '{"type":"done","text":null}'],
    ['a message that is not a string', '{"type":"error","message":{"a":1}}'],
  ])('is null for %s', (_label, data) => {
    expect(parseLiveDownFrame(data)).toBeNull();
  });
});

describe('liveTranscriptionWsUrl', () => {
  it('is the absolute portal API path on this origin, ws for http', () => {
    expect(liveTranscriptionWsUrl({ protocol: 'http:', host: 'localhost:4173' })).toBe(
      'ws://localhost:4173/api/portal/transcription-live',
    );
  });

  it('is wss for https', () => {
    expect(liveTranscriptionWsUrl({ protocol: 'https:', host: 'gw.example.com' })).toBe(
      'wss://gw.example.com/api/portal/transcription-live',
    );
  });

  it('names the subprotocol the gateway selects', () => {
    expect(LIVE_WS_SUBPROTOCOL).toBe('op-live-transcription');
  });
});

describe('createLiveSession without microphone capture', () => {
  // jsdom has no AudioContext and no mediaDevices: the case of an old browser or an insecure
  // page. The session reports it as an error -- never synchronously from inside the call, where
  // the caller has not got its session yet -- and has not asked the gateway for a ticket (which
  // takes an admission it would never use).
  it('reports the failure asynchronously and mints no ticket', async () => {
    const api = { createLiveTranscriptionTicket: vi.fn().mockResolvedValue({ ticket: 't' }) };
    const states: string[] = [];
    const errors: string[] = [];
    const session = createLiveSession({
      api,
      model: 'whisper',
      onDelta: vi.fn(),
      onPartial: vi.fn(),
      onDone: vi.fn(),
      onError: (message) => errors.push(message),
      onState: (state) => states.push(state),
    });
    expect(states).toEqual(['connecting']);
    expect(errors).toEqual([]);

    await vi.waitFor(() => expect(errors).toHaveLength(1));
    expect(errors[0]).not.toBe('');
    expect(states).toEqual(['connecting', 'error']);
    expect(api.createLiveTranscriptionTicket).not.toHaveBeenCalled();

    // The session is over: stop and abort are harmless, and report nothing more.
    session.stop();
    session.abort();
    expect(states).toEqual(['connecting', 'error']);
    expect(errors).toHaveLength(1);
  });

  it('closes quietly when aborted before it reports anything', async () => {
    const onError = vi.fn();
    const states: string[] = [];
    const session = createLiveSession({
      api: { createLiveTranscriptionTicket: vi.fn() },
      model: 'whisper',
      onDelta: vi.fn(),
      onPartial: vi.fn(),
      onDone: vi.fn(),
      onError,
      onState: (state) => states.push(state),
    });
    session.abort();
    await Promise.resolve();
    await Promise.resolve();
    expect(onError).not.toHaveBeenCalled();
    expect(states).toEqual(['connecting', 'closed']);
  });
});
