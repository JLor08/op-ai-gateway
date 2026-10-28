// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { PortalApiError } from '../../api';
import {
  createLiveSession,
  type LiveErrorDetail,
  type LiveSessionArgs,
  type LiveSessionState,
} from './liveTranscription';

// createLiveSession is glue between the browser's audio stack and a WebSocket, neither of which
// jsdom has. These tests stand a small fake browser in for them, so that the way the session
// REACTS to each failure (what it tells the caller, and that it lets go of the microphone) is
// pinned. The audio itself (resampling, batching) is livePcm.test.ts's.

class FakeAudioContext {
  static instances: FakeAudioContext[] = [];
  // What the context is after resume(): a browser that keeps it suspended is a failure.
  static resumeTo: 'running' | 'suspended' = 'running';
  state: string = 'suspended';
  destination = {};
  audioWorklet = { addModule: vi.fn(async () => undefined) };
  resume = vi.fn(async () => {
    this.state = FakeAudioContext.resumeTo;
  });
  close = vi.fn(async () => {
    this.state = 'closed';
  });
  createMediaStreamSource = vi.fn(() => ({ connect: vi.fn(), disconnect: vi.fn() }));
  constructor() {
    FakeAudioContext.instances.push(this);
  }
}

class FakeWorkletNode {
  static last: FakeWorkletNode | undefined;
  port = { onmessage: null as unknown, postMessage: vi.fn() };
  onprocessorerror: (() => void) | null = null;
  connect = vi.fn();
  disconnect = vi.fn();
  constructor() {
    FakeWorkletNode.last = this;
  }
}

class FakeSocket {
  static last: FakeSocket | undefined;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
  send = vi.fn();
  close = vi.fn();
  constructor(
    public url: string,
    public protocols: string[],
  ) {
    FakeSocket.last = this;
  }
  open() {
    this.onopen?.();
  }
  say(frame: unknown) {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }
  closeWith(code: number, reason = '') {
    this.onclose?.({ code, reason });
  }
}

let track: { stop: ReturnType<typeof vi.fn>; onended: unknown };
let getUserMedia: ReturnType<typeof vi.fn>;

beforeEach(() => {
  FakeAudioContext.instances = [];
  FakeAudioContext.resumeTo = 'running';
  FakeWorkletNode.last = undefined;
  FakeSocket.last = undefined;
  track = { stop: vi.fn(), onended: null };
  getUserMedia = vi.fn(async () => ({ getTracks: () => [track], getAudioTracks: () => [track] }));
  Object.defineProperty(navigator, 'mediaDevices', { value: { getUserMedia }, configurable: true });
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
  vi.stubGlobal('WebSocket', FakeSocket);
});

afterEach(() => {
  Reflect.deleteProperty(navigator, 'mediaDevices');
  vi.unstubAllGlobals();
});

type LiveTicketFn = (args: { model: string; language?: string }) => Promise<{ ticket: string }>;

type Run = {
  session: ReturnType<typeof createLiveSession>;
  states: LiveSessionState[];
  errors: { message: string; detail: LiveErrorDetail }[];
  onDone: Mock<(text: string) => void>;
  ticket: Mock<LiveTicketFn>;
};

function start(
  ticket: Mock<LiveTicketFn> = vi.fn<LiveTicketFn>().mockResolvedValue({ ticket: 'tkt' }),
  extra: Partial<LiveSessionArgs> = {},
): Run {
  const states: LiveSessionState[] = [];
  const errors: Run['errors'] = [];
  const onDone = vi.fn();
  const session = createLiveSession({
    api: { createLiveTranscriptionTicket: ticket },
    model: 'whisper',
    onDelta: vi.fn(),
    onPartial: vi.fn(),
    onDone,
    onError: (message, detail) => errors.push({ message, detail }),
    onState: (state) => states.push(state),
    ...extra,
  });
  return { session, states, errors, onDone, ticket };
}

// The session has connected capture, minted its ticket and opened its socket.
async function listening(run: Run): Promise<FakeSocket> {
  await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
  const socket = FakeSocket.last!;
  socket.open();
  expect(run.states.at(-1)).toBe('listening');
  return socket;
}

describe('createLiveSession in a (fake) browser', () => {
  it('goes listening once the socket is open, offering the ticket as a subprotocol', async () => {
    const run = start();
    const socket = await listening(run);
    expect(socket.protocols).toEqual(['op-live-transcription', 'tkt']);
    expect(run.errors).toEqual([]);
    run.session.abort();
  });

  describe('failures of audio capture surface at once, not after the grace period', () => {
    it('a worklet processor error is a capture error, and releases the microphone', async () => {
      const run = start();
      await vi.waitFor(() => expect(FakeWorkletNode.last).toBeDefined());
      await listening(run);

      FakeWorkletNode.last!.onprocessorerror?.();

      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].detail.kind).toBe('capture');
      expect(run.errors[0].message).not.toBe('');
      expect(run.states.at(-1)).toBe('error');
      expect(track.stop).toHaveBeenCalled();
    });

    it('a context the browser keeps suspended is a capture error, before any ticket is minted', async () => {
      FakeAudioContext.resumeTo = 'suspended';
      const run = start();

      await vi.waitFor(() => expect(run.errors).toHaveLength(1));
      expect(run.errors[0].detail.kind).toBe('capture');
      expect(run.errors[0].message).toMatch(/audio/i);
      expect(run.ticket).not.toHaveBeenCalled();
      expect(FakeSocket.last).toBeUndefined();
      // The microphone it had been granted is let go again.
      expect(track.stop).toHaveBeenCalled();
      expect(run.states.at(-1)).toBe('error');
    });

    it('a refused microphone is "denied"', async () => {
      getUserMedia.mockRejectedValueOnce(
        Object.assign(new Error('Permission denied'), { name: 'NotAllowedError' }),
      );
      const run = start();
      await vi.waitFor(() => expect(run.errors).toHaveLength(1));
      expect(run.errors[0].detail.kind).toBe('denied');
      expect(run.ticket).not.toHaveBeenCalled();
    });

    it('any other microphone failure (no device) is a capture error', async () => {
      getUserMedia.mockRejectedValueOnce(
        Object.assign(new Error('Requested device not found'), { name: 'NotFoundError' }),
      );
      const run = start();
      await vi.waitFor(() => expect(run.errors).toHaveLength(1));
      expect(run.errors[0].detail.kind).toBe('capture');
    });

    it('a browser without microphone capture is "unsupported"', async () => {
      Reflect.deleteProperty(navigator, 'mediaDevices');
      const run = start();
      await vi.waitFor(() => expect(run.errors).toHaveLength(1));
      expect(run.errors[0].detail.kind).toBe('unsupported');
    });
  });

  describe('what a failure after capture means for the caller: try again, or not', () => {
    it('a ticket the gateway refused (4xx) is "refused", carrying the gateway code', async () => {
      const ticket = vi
        .fn<LiveTicketFn>()
        .mockRejectedValue(
          new PortalApiError(409, 'transcription.live_not_streaming', 'not streaming'),
        );
      const run = start(ticket);
      await vi.waitFor(() => expect(run.errors).toHaveLength(1));
      expect(run.errors[0].detail).toEqual({
        kind: 'refused',
        code: 'transcription.live_not_streaming',
      });
      expect(FakeSocket.last).toBeUndefined();
      expect(track.stop).toHaveBeenCalled();
    });

    it('a ticket request that did not get through (network, 5xx) is "dropped": worth trying again', async () => {
      const network = start(
        vi.fn<LiveTicketFn>().mockRejectedValue(new TypeError('Failed to fetch')),
      );
      await vi.waitFor(() => expect(network.errors).toHaveLength(1));
      expect(network.errors[0].detail.kind).toBe('dropped');

      FakeSocket.last = undefined;
      const server = start(
        vi
          .fn<LiveTicketFn>()
          .mockRejectedValue(
            new PortalApiError(500, 'portal.live_transcription_ticket_failed', 'failed'),
          ),
      );
      await vi.waitFor(() => expect(server.errors).toHaveLength(1));
      // The live transcription's own 5xx keeps the plain "dropped": only a Voice Chat code
      // (portal.voice_chat_*) is carried along, so the live recording's behavior is unchanged.
      expect(server.errors[0].detail).toEqual({ kind: 'dropped' });
    });

    it('an error frame is "refused": the gateway said no', async () => {
      const run = start();
      const socket = await listening(run);
      socket.say({ type: 'error', message: 'live transcription upstream error' });
      expect(run.errors).toEqual([
        { message: 'live transcription upstream error', detail: { kind: 'refused' } },
      ]);
      expect(run.states.at(-1)).toBe('error');
    });

    it('an error frame that carries the gateway code is "refused" with that code', async () => {
      // What the portal gets when the gateway's /v1 refuses the looped-back session: the /v1
      // code rides the frame, so a busy model reads as busy and not as a generic failure.
      const run = start();
      const socket = await listening(run);
      socket.say({
        type: 'error',
        message: 'a live transcription session is already active for this model',
        code: 'transcription.live_busy',
      });
      expect(run.errors).toEqual([
        {
          message: 'a live transcription session is already active for this model',
          detail: { kind: 'refused', code: 'transcription.live_busy' },
        },
      ]);
      expect(run.states.at(-1)).toBe('error');
    });

    it('a busy close (1013) is "refused" with the busy code', async () => {
      const run = start();
      const socket = await listening(run);
      socket.closeWith(1013, 'try again later');
      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].detail).toEqual({ kind: 'refused', code: 'transcription.live_busy' });
    });

    it('a close with no `done` and no error frame is "dropped"', async () => {
      const run = start();
      const socket = await listening(run);
      socket.closeWith(1006);
      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].detail.kind).toBe('dropped');
      expect(run.errors[0].message).toContain('1006');
    });

    it('a socket that never opens (handshake refused) is "dropped" as well', async () => {
      const run = start();
      await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
      FakeSocket.last!.closeWith(1006);
      expect(run.errors[0].detail.kind).toBe('dropped');
    });

    it('a finished session reports no error afterwards, whatever the socket does', async () => {
      const run = start();
      const socket = await listening(run);
      socket.say({ type: 'done', text: 'all said' });
      expect(run.onDone).toHaveBeenCalledWith('all said');
      expect(run.states.at(-1)).toBe('closed');
      socket.closeWith(1006);
      expect(run.errors).toEqual([]);
    });
  });
});
