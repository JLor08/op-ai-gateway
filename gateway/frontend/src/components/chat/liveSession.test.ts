// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  IDLE_LIVE_SESSION,
  joinTranscript,
  liveSessionReducer,
  liveTranscriptOf,
  type LiveAction,
  type LiveSessionModel,
} from './liveSession';

// Runs a sequence of actions from the idle state, the way the store feeds them.
function run(...actions: LiveAction[]): LiveSessionModel {
  return actions.reduce(liveSessionReducer, IDLE_LIVE_SESSION);
}

describe('joinTranscript', () => {
  it('joins two texts with one space, and no space where either side already has one', () => {
    expect(joinTranscript('hello', 'world')).toBe('hello world');
    expect(joinTranscript('hello ', 'world')).toBe('hello world');
    expect(joinTranscript('hello', ' world')).toBe('hello world');
    expect(joinTranscript('hello\n', 'world')).toBe('hello\nworld');
  });

  it('is the other side when one is empty', () => {
    expect(joinTranscript('', 'world')).toBe('world');
    expect(joinTranscript('hello', '')).toBe('hello');
    expect(joinTranscript('', '')).toBe('');
  });
});

describe('liveSessionReducer', () => {
  it('starts idle, with no transcript and no error', () => {
    expect(IDLE_LIVE_SESSION).toEqual({ state: 'idle', base: '', current: '' });
    expect(liveTranscriptOf(IDLE_LIVE_SESSION)).toBe('');
  });

  it('start goes to connecting and clears what a previous session left', () => {
    const failed = run(
      { type: 'start' },
      { type: 'delta', text: 'old words' },
      { type: 'fail', error: 'boom' },
    );
    expect(failed.state).toBe('error');
    const again = liveSessionReducer(failed, { type: 'start' });
    expect(again).toEqual({ state: 'connecting', base: '', current: '' });
  });

  it('listening follows connecting', () => {
    expect(run({ type: 'start' }, { type: 'listening' }).state).toBe('listening');
  });

  it('a delta appends to the transcript', () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'Hello' },
      { type: 'delta', text: ' wor' },
      { type: 'delta', text: 'ld' },
    );
    expect(liveTranscriptOf(model)).toBe('Hello world');
    expect(model.state).toBe('listening');
  });

  it('a partial REPLACES the transcript so far, which a later delta then extends', () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'Hello wor' },
      { type: 'partial', text: 'Hello, world' },
      { type: 'delta', text: '!' },
    );
    expect(liveTranscriptOf(model)).toBe('Hello, world!');
  });

  it('text that arrives while the session finalizes still counts (the model finishes the audio it holds)', () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'Hello' },
      { type: 'finalizing' },
      { type: 'delta', text: ' there' },
    );
    expect(model.state).toBe('finalizing');
    expect(liveTranscriptOf(model)).toBe('Hello there');
  });

  it('ignores text and state changes while idle or in error', () => {
    expect(liveSessionReducer(IDLE_LIVE_SESSION, { type: 'delta', text: 'x' })).toBe(
      IDLE_LIVE_SESSION,
    );
    expect(liveSessionReducer(IDLE_LIVE_SESSION, { type: 'listening' })).toBe(IDLE_LIVE_SESSION);
    const failed = run({ type: 'start' }, { type: 'fail', error: 'boom' });
    expect(liveSessionReducer(failed, { type: 'delta', text: 'x' })).toBe(failed);
    expect(liveSessionReducer(failed, { type: 'listening' })).toBe(failed);
  });

  it('a drop keeps what was said and starts the next session on a clean slate of its own', () => {
    const dropped = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'First part' },
      { type: 'reconnecting' },
    );
    expect(dropped.state).toBe('reconnecting');
    expect(liveTranscriptOf(dropped)).toBe('First part');

    // The new session's deltas continue after it...
    const steps: LiveAction[] = [
      { type: 'listening' },
      { type: 'delta', text: 'second' },
      { type: 'delta', text: ' part' },
    ];
    const resumed = steps.reduce(liveSessionReducer, dropped);
    expect(resumed.state).toBe('listening');
    expect(liveTranscriptOf(resumed)).toBe('First part second part');

    // ...and its partial replaces only ITS OWN text, never the earlier session's.
    const revised = liveSessionReducer(resumed, { type: 'partial', text: 'Second part, revised' });
    expect(liveTranscriptOf(revised)).toBe('First part Second part, revised');
  });

  it('a second drop in a row keeps everything as well', () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'one' },
      { type: 'reconnecting' },
      { type: 'listening' },
      { type: 'delta', text: 'two' },
      { type: 'reconnecting' },
      { type: 'reconnecting' },
    );
    expect(liveTranscriptOf(model)).toBe('one two');
  });

  it('fail is the error state, with its message and the transcript kept', () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'kept' },
      { type: 'fail', error: 'it broke' },
    );
    expect(model.state).toBe('error');
    expect(model.error).toBe('it broke');
    expect(liveTranscriptOf(model)).toBe('kept');
  });

  it('reset is idle again', () => {
    const model = run({ type: 'start' }, { type: 'delta', text: 'x' }, { type: 'reset' });
    expect(model).toEqual(IDLE_LIVE_SESSION);
  });
});

describe('a done action', () => {
  it("replaces this session's text with the final transcript, after what earlier sessions said", () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'Before.' },
      { type: 'reconnecting' },
      { type: 'listening' },
      { type: 'delta', text: 'provisional' },
      { type: 'done', text: 'After.' },
    );
    expect(liveTranscriptOf(model)).toBe('Before. After.');
  });

  it('keeps what the deltas and partials gave when it has no text of its own', () => {
    const model = run(
      { type: 'start' },
      { type: 'listening' },
      { type: 'delta', text: 'Hello' },
      { type: 'partial', text: 'Hello world' },
      { type: 'done', text: '' },
    );
    expect(liveTranscriptOf(model)).toBe('Hello world');
  });

  it('is ignored when no session is running', () => {
    expect(liveSessionReducer(IDLE_LIVE_SESSION, { type: 'done', text: 'x' })).toBe(
      IDLE_LIVE_SESSION,
    );
  });
});
