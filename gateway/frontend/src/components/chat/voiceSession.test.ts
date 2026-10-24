// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { PortalApiError } from '../../api';
import {
  VOICE_WS_PATH,
  VOICE_WS_SUBPROTOCOL,
  createVoiceSession,
  parseVoiceDownFrame,
  voiceChatWsUrl,
  type VoiceAnswer,
  type VoiceErrorDetail,
  type VoicePlayer,
  type VoiceSessionArgs,
  type VoiceSessionState,
  type VoiceTranscript,
} from './voiceSession';

// createVoiceSession is glue between the browser's audio stack and a WebSocket, neither of which
// jsdom has. These tests stand a small fake browser in for them (the same one the live
// transcription session's tests use), plus a fake player in place of the StreamingPcmPlayer, so
// that what the session SENDS, what it ROUTES to its callbacks and what it FEEDS the player is
// pinned. The audio itself (resampling, batching) is livePcm.test.ts's, the playback scheduling
// speechPlayback.test.ts's.

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
  port = {
    onmessage: null as ((event: { data: unknown }) => void) | null,
    postMessage: vi.fn(),
  };
  onprocessorerror: (() => void) | null = null;
  connect = vi.fn();
  disconnect = vi.fn();
  constructor() {
    FakeWorkletNode.last = this;
  }
  // The worklet hands over a batch of PCM, or answers the page's flush request.
  pcm(...bytes: number[]) {
    this.port.onmessage?.({ data: new Uint8Array(bytes).buffer });
  }
  flushed() {
    this.port.onmessage?.({ data: 'flushed' });
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
    this.onmessage?.({ data: typeof frame === 'string' ? frame : JSON.stringify(frame) });
  }
  closeWith(code: number, reason = '') {
    this.onclose?.({ code, reason });
  }
  // The text frames sent so far, parsed (the binary PCM frames are skipped).
  controls(): unknown[] {
    return this.send.mock.calls
      .map(([data]) => data)
      .filter((data): data is string => typeof data === 'string')
      .map((data) => JSON.parse(data));
  }
}

let track: { stop: ReturnType<typeof vi.fn>; onended: (() => void) | null };
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
  vi.useRealTimers();
});

type TicketFn = (args: {
  chatId: string;
  asrModel: string;
  llmModel: string;
  ttsModel: string;
}) => Promise<{ ticket: string }>;

type FakePlayer = VoicePlayer & {
  push: Mock<VoicePlayer['push']>;
  stop: Mock<VoicePlayer['stop']>;
};

type Run = {
  session: ReturnType<typeof createVoiceSession>;
  states: VoiceSessionState[];
  transcripts: VoiceTranscript[];
  answers: VoiceAnswer[];
  errors: { message: string; detail: VoiceErrorDetail }[];
  onAnswerReset: Mock<() => void>;
  onAudioDone: Mock<() => void>;
  ticket: Mock<TicketFn>;
  // Every player the session made, in order (it makes one per stream, and a fresh one after a
  // cancel silenced the last).
  players: FakePlayer[];
};

function start(
  ticket: Mock<TicketFn> = vi.fn<TicketFn>().mockResolvedValue({ ticket: 'tkt' }),
  extra: Partial<VoiceSessionArgs> = {},
): Run {
  const run: Run = {
    session: undefined as unknown as Run['session'],
    states: [],
    transcripts: [],
    answers: [],
    errors: [],
    onAnswerReset: vi.fn(),
    onAudioDone: vi.fn(),
    ticket,
    players: [],
  };
  run.session = createVoiceSession({
    api: { createVoiceChatTicket: ticket },
    chatId: 'chat_1',
    asrModel: 'asr',
    llmModel: 'llm',
    ttsModel: 'tts',
    onTranscript: (update) => run.transcripts.push(update),
    onAnswer: (update) => run.answers.push(update),
    onAnswerReset: run.onAnswerReset,
    onAudioDone: run.onAudioDone,
    onState: (state) => run.states.push(state),
    onError: (message, detail) => run.errors.push({ message, detail }),
    createPlayer: () => {
      // onEmpty starts null and is wired by the session at creation (the #304 drain hook); a
      // test drives the drain by invoking whatever the session wired.
      const player: FakePlayer = { push: vi.fn(() => true), stop: vi.fn(), onEmpty: null };
      run.players.push(player);
      return player;
    },
    ...extra,
  });
  return run;
}

// The session has connected capture, minted its ticket and opened its socket.
async function connected(run: Run): Promise<FakeSocket> {
  await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
  const socket = FakeSocket.last!;
  socket.open();
  expect(run.errors).toEqual([]);
  return socket;
}

// Standard base64 of some bytes, as an audio_delta frame carries its PCM.
function b64(...bytes: number[]): string {
  return btoa(String.fromCharCode(...bytes));
}

function audioDelta(seq: number, bytes: number[] = [1, 0, 2, 0], rate = 24000) {
  return { type: 'audio_delta', seq, rate, audio: b64(...bytes) };
}

describe('createVoiceSession in a (fake) browser', () => {
  it('starts connecting, mints the ticket for the chat and its three models, and offers the ticket as a subprotocol', async () => {
    const run = start();
    expect(run.states).toEqual(['connecting']);

    const socket = await connected(run);

    expect(run.ticket).toHaveBeenCalledWith({
      chatId: 'chat_1',
      asrModel: 'asr',
      llmModel: 'llm',
      ttsModel: 'tts',
    });
    expect(VOICE_WS_SUBPROTOCOL).toBe('op-voice-chat');
    expect(socket.protocols).toEqual(['op-voice-chat', 'tkt']);
    expect(socket.url).toBe(`ws://${window.location.host}${VOICE_WS_PATH}`);
    // The session's own state is the gateway's: nothing is reported until its first `state`.
    expect(run.states).toEqual(['connecting']);
    run.session.stop();
  });

  it('keeps audio captured while the socket is being opened and sends it, in order, on open', async () => {
    const run = start();
    await vi.waitFor(() => expect(FakeWorkletNode.last).toBeDefined());
    await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
    const socket = FakeSocket.last!;
    FakeWorkletNode.last!.pcm(1, 2);
    FakeWorkletNode.last!.pcm(3, 4);
    expect(socket.send).not.toHaveBeenCalled();

    socket.open();

    expect(socket.send).toHaveBeenCalledTimes(2);
    expect(new Uint8Array(socket.send.mock.calls[0][0])).toEqual(new Uint8Array([1, 2]));
    expect(new Uint8Array(socket.send.mock.calls[1][0])).toEqual(new Uint8Array([3, 4]));
    // From then on a batch goes straight up.
    FakeWorkletNode.last!.pcm(5, 6);
    expect(socket.send).toHaveBeenCalledTimes(3);
    run.session.stop();
  });

  it('drops what was captured while the ticket was being minted: a cold start can take minutes, and that audio is not live', async () => {
    let resolveTicket!: (value: { ticket: string }) => void;
    const ticket = vi.fn<TicketFn>(
      () =>
        new Promise((resolve) => {
          resolveTicket = resolve;
        }),
    );
    const run = start(ticket);
    await vi.waitFor(() => expect(FakeWorkletNode.last).toBeDefined());
    await vi.waitFor(() => expect(ticket).toHaveBeenCalled());
    FakeWorkletNode.last!.pcm(9, 9);

    resolveTicket({ ticket: 'tkt' });
    await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
    FakeSocket.last!.open();

    expect(FakeSocket.last!.send).not.toHaveBeenCalled();
    run.session.stop();
  });

  it('parses down-frames and routes asr/answer/audio/state', async () => {
    const run = start();
    const socket = await connected(run);

    socket.say({ type: 'state', state: 'listening' });
    socket.say({ type: 'asr_partial', text: 'hello wor' });
    socket.say({ type: 'asr_partial', text: 'hello world' });
    socket.say({ type: 'asr_final', text: 'hello world', message_id: 'msg_1' });
    socket.say({ type: 'state', state: 'thinking' });
    socket.say({ type: 'answer_delta', text: 'Hi ' });
    socket.say({ type: 'answer_delta', text: 'there' });
    socket.say({ type: 'answer_done', message_id: 'msg_2' });
    socket.say({ type: 'state', state: 'speaking' });
    socket.say(audioDelta(0));
    socket.say({ type: 'audio_done' });
    socket.say({ type: 'state', state: 'draining' });

    // The transcript, one chunk: a partial replaces the last partial, and the final is the
    // chunk's. (The gateway sends no message_id today; one that comes is passed on as a hint.)
    expect(run.transcripts).toEqual([
      { text: 'hello wor', final: false },
      { text: 'hello world', final: false },
      { text: 'hello world', final: true, messageId: 'msg_1' },
    ]);
    // The answer: deltas are increments, and the done is provisional (it has no text of its own).
    expect(run.answers).toEqual([
      { text: 'Hi ', done: false },
      { text: 'there', done: false },
      { text: '', done: true, messageId: 'msg_2' },
    ]);
    expect(run.states).toEqual(['connecting', 'listening', 'thinking', 'speaking', 'draining']);
    expect(run.players[0].push).toHaveBeenCalledTimes(1);
    expect(run.onAudioDone).toHaveBeenCalledTimes(1);
    expect(run.errors).toEqual([]);
    run.session.stop();
  });

  it('answer_reset fires onAnswerReset so the preview clears', async () => {
    const run = start();
    const socket = await connected(run);
    socket.say({ type: 'answer_delta', text: 'a speculative answer' });
    expect(run.onAnswerReset).not.toHaveBeenCalled();

    socket.say({ type: 'answer_reset' });

    expect(run.onAnswerReset).toHaveBeenCalledTimes(1);
    // The reset is a signal, not a delta: the answers heard so far are for the caller to drop.
    expect(run.answers).toEqual([{ text: 'a speculative answer', done: false }]);
    run.session.stop();
  });

  it('feeds audio_delta to the player in seq order', async () => {
    const run = start();
    const socket = await connected(run);

    socket.say(audioDelta(0, [1, 0, 2, 0]));
    socket.say(audioDelta(1, [3, 0, 4, 0]));
    socket.say(audioDelta(2, [5, 0, 6, 0]));

    // One stream, one player, the chunks decoded and in the order they were numbered, at the
    // rate the frames name.
    expect(run.players).toHaveLength(1);
    const calls = run.players[0].push.mock.calls;
    expect(calls.map(([bytes]) => Array.from(bytes))).toEqual([
      [1, 0, 2, 0],
      [3, 0, 4, 0],
      [5, 0, 6, 0],
    ]);
    expect(calls.map(([, rate]) => rate)).toEqual([24000, 24000, 24000]);
    run.session.stop();
  });

  it('sends playback_done when the answer`s player drains (the #304 hook), so the gateway can end its drain at playout-done', async () => {
    const run = start();
    const socket = await connected(run);

    // An answer streams its audio: a player is made for it and the drain hook wired.
    socket.say(audioDelta(0));
    socket.say({ type: 'audio_done' });
    expect(run.players).toHaveLength(1);
    const player = run.players[0];
    expect(player.onEmpty).toBeInstanceOf(Function);
    // Nothing is said until the player actually drains: relay-done is not playout-done.
    expect(socket.controls()).not.toContainEqual({ type: 'playback_done' });

    // The player's queue plays out -> the session reports playout finished.
    player.onEmpty!();
    expect(socket.controls()).toContainEqual({ type: 'playback_done' });
  });

  it('requests echo cancellation and noise suppression on the microphone (belt-and-suspenders under the half-duplex gate)', async () => {
    const run = start();
    await connected(run);
    expect(getUserMedia).toHaveBeenCalledWith({
      audio: { echoCancellation: true, noiseSuppression: true },
    });
    run.session.stop();
  });

  it('abandons the rest of an answer`s audio at a hole in it, and plays the next answer from seq 0 again', async () => {
    const run = start();
    const socket = await connected(run);

    socket.say(audioDelta(0));
    socket.say(audioDelta(2)); // seq 1 never arrived: a gap, which would be audible
    socket.say(audioDelta(3));
    socket.say({ type: 'audio_done' });
    expect(run.players).toHaveLength(1);
    expect(run.players[0].push).toHaveBeenCalledTimes(1);

    // The next answer is a new stream, numbered from 0 again, and has a player of its own: the
    // old one may hold a stray byte of the answer that was cut short.
    socket.say(audioDelta(0));
    socket.say(audioDelta(1));
    expect(run.players).toHaveLength(2);
    expect(run.players[0].stop).toHaveBeenCalledTimes(1);
    expect(run.players[1].push).toHaveBeenCalledTimes(2);
    run.session.stop();
  });

  it('plays the next answer even though the last one ended without an audio_done (a cancelled turn)', async () => {
    const run = start();
    const socket = await connected(run);
    socket.say(audioDelta(0));
    socket.say(audioDelta(1));

    // A turn that was cancelled never says audio_done; the next answer starts at 0 again, and
    // replaces what is left of the cancelled one.
    socket.say(audioDelta(0, [7, 0, 8, 0]));

    expect(run.players).toHaveLength(2);
    expect(run.players[0].push).toHaveBeenCalledTimes(2);
    expect(run.players[0].stop).toHaveBeenCalledTimes(1);
    expect(run.players[1].push.mock.calls.map(([bytes]) => Array.from(bytes))).toEqual([
      [7, 0, 8, 0],
    ]);
    run.session.stop();
  });

  it('pushStart sends {type:"push_start"}', async () => {
    const run = start();
    const socket = await connected(run);

    run.session.pushStart();

    expect(socket.controls()).toEqual([{ type: 'push_start' }]);
    run.session.stop();
  });

  it('pushEnd sends {type:"push_end"}, after the last of the captured audio', async () => {
    const run = start();
    const socket = await connected(run);
    const worklet = FakeWorkletNode.last!;

    run.session.pushEnd();

    // The worklet is asked for the batch it is still filling -- the end of the last word --
    // and push_end follows that batch, not the other way round.
    expect(worklet.port.postMessage).toHaveBeenCalledWith('flush');
    expect(socket.controls()).toEqual([]);
    worklet.pcm(7, 7);
    worklet.flushed();

    expect(socket.send).toHaveBeenCalledTimes(2);
    expect(new Uint8Array(socket.send.mock.calls[0][0])).toEqual(new Uint8Array([7, 7]));
    expect(socket.send.mock.calls[1][0]).toBe(JSON.stringify({ type: 'push_end' }));
    run.session.stop();
  });

  it('pushEnd sends push_end once, however often it is asked, and without a worklet answer after a moment', async () => {
    const run = start();
    const socket = await connected(run);
    vi.useFakeTimers();

    run.session.pushEnd();
    run.session.pushEnd();
    expect(socket.controls()).toEqual([]);
    vi.advanceTimersByTime(1000);

    expect(socket.controls()).toEqual([{ type: 'push_end' }]);
    // The worklet answering late changes nothing.
    FakeWorkletNode.last!.flushed();
    expect(socket.controls()).toEqual([{ type: 'push_end' }]);
    run.session.stop();
  });

  it('cancel sends {type:"cancel"} and silences what is still queued to play', async () => {
    const run = start();
    const socket = await connected(run);
    socket.say(audioDelta(0));
    socket.say(audioDelta(1));
    expect(run.players).toHaveLength(1);

    run.session.cancel();

    expect(socket.controls()).toEqual([{ type: 'cancel' }]);
    expect(run.players[0].stop).toHaveBeenCalledTimes(1);
    // The next answer is heard on a player of its own, not the silenced one.
    socket.say(audioDelta(0));
    expect(run.players).toHaveLength(2);
    expect(run.players[1].push).toHaveBeenCalledTimes(1);
    run.session.stop();
  });

  it('a pushEnd still waiting for the worklet is dropped by a cancel', async () => {
    const run = start();
    const socket = await connected(run);

    run.session.pushEnd();
    run.session.cancel();
    FakeWorkletNode.last!.flushed();

    expect(socket.controls()).toEqual([{ type: 'cancel' }]);
    run.session.stop();
  });

  it('controls asked for before the socket is open are dropped, not queued', async () => {
    const run = start();
    await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
    const socket = FakeSocket.last!;

    run.session.pushStart();
    run.session.pushEnd();
    run.session.cancel();
    socket.open();

    expect(socket.send).not.toHaveBeenCalled();
    run.session.stop();
  });

  it('stop sends {type:"bye"} and tears down', async () => {
    const run = start();
    const socket = await connected(run);
    socket.say({ type: 'state', state: 'speaking' });
    socket.say(audioDelta(0));

    run.session.stop();

    expect(socket.controls()).toEqual([{ type: 'bye' }]);
    expect(socket.close).toHaveBeenCalledWith(1000);
    // The microphone, the audio graph and the answer still playing are all let go.
    expect(track.stop).toHaveBeenCalled();
    expect(FakeAudioContext.instances[0].close).toHaveBeenCalled();
    expect(FakeWorkletNode.last!.disconnect).toHaveBeenCalled();
    expect(run.players[0].stop).toHaveBeenCalledTimes(1);
    expect(run.states.at(-1)).toBe('closed');

    // And nothing is delivered afterwards, whatever the (closing) socket still says or does.
    const delivered = run.states.length;
    socket.say({ type: 'state', state: 'listening' });
    socket.say({ type: 'asr_partial', text: 'late' });
    socket.closeWith(1006);
    FakeWorkletNode.last!.pcm(1, 1);
    run.session.stop();
    run.session.pushStart();
    expect(run.states).toHaveLength(delivered);
    expect(run.transcripts).toEqual([]);
    expect(run.errors).toEqual([]);
    expect(socket.controls()).toEqual([{ type: 'bye' }]);
  });

  it('stop before the socket is open tears down without a bye (nobody is there to hear it)', async () => {
    const run = start();
    await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
    const socket = FakeSocket.last!;

    run.session.stop();

    expect(socket.send).not.toHaveBeenCalled();
    expect(socket.close).toHaveBeenCalledWith(1000);
    expect(track.stop).toHaveBeenCalled();
    expect(run.states.at(-1)).toBe('closed');
  });

  it('stop while the microphone prompt is still open releases the microphone once it is granted, and mints no ticket', async () => {
    let grant!: () => void;
    getUserMedia.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          grant = () => resolve({ getTracks: () => [track], getAudioTracks: () => [track] });
        }),
    );
    const run = start();
    await vi.waitFor(() => expect(getUserMedia).toHaveBeenCalled());

    run.session.stop();
    grant();

    await vi.waitFor(() => expect(track.stop).toHaveBeenCalled());
    expect(run.ticket).not.toHaveBeenCalled();
    expect(FakeSocket.last).toBeUndefined();
    expect(run.states.at(-1)).toBe('closed');
  });

  describe('frames that are not for it', () => {
    it('ignores a frame it does not understand, so that a newer gateway cannot break an older portal', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say('not json');
      socket.say({ type: 'something_new', text: 'x' });
      socket.say({ type: 'state', state: 'levitating' });
      socket.say({ type: 'audio_delta', seq: 'zero', rate: 24000, audio: '' });
      socket.say([1, 2]);
      socket.onmessage?.({ data: new ArrayBuffer(4) });

      expect(run.states).toEqual(['connecting']);
      expect(run.transcripts).toEqual([]);
      expect(run.answers).toEqual([]);
      expect(run.players).toEqual([]);
      expect(run.errors).toEqual([]);
      run.session.stop();
    });

    it('reports a state once, however often the gateway repeats it', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say({ type: 'state', state: 'listening' });
      socket.say({ type: 'state', state: 'listening' });

      expect(run.states).toEqual(['connecting', 'listening']);
      run.session.stop();
    });
  });

  describe('what an error means for the caller', () => {
    it('an error frame is one turn failing: reported with its code, and the session carries on', async () => {
      const run = start();
      const socket = await connected(run);
      socket.say({ type: 'state', state: 'thinking' });

      socket.say({
        type: 'error',
        code: 'speech.upstream_error',
        message: 'the voice turn could not be completed',
      });
      socket.say({ type: 'state', state: 'idle' });

      expect(run.errors).toEqual([
        {
          message: 'the voice turn could not be completed',
          detail: { kind: 'turn', code: 'speech.upstream_error' },
        },
      ]);
      // Not the end: the gateway is back at idle and the user re-engages with push_start.
      expect(run.states).toEqual(['connecting', 'thinking', 'idle']);
      run.session.pushStart();
      expect(socket.controls()).toEqual([{ type: 'push_start' }]);
      expect(socket.close).not.toHaveBeenCalled();
      run.session.stop();
    });

    it('an error frame without a message still says something', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say({ type: 'error', code: 'runtime.admission_blocked' });

      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].message).not.toBe('');
      expect(run.errors[0].detail).toEqual({ kind: 'turn', code: 'runtime.admission_blocked' });
      run.session.stop();
    });

    it('a close the session did not ask for is "dropped", and ends it', async () => {
      const run = start();
      const socket = await connected(run);

      socket.closeWith(1006);

      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].detail).toEqual({ kind: 'dropped' });
      expect(run.errors[0].message).toContain('1006');
      expect(run.states.at(-1)).toBe('error');
      expect(track.stop).toHaveBeenCalled();
      // The socket is already gone: nothing is said to it.
      expect(socket.controls()).toEqual([]);
    });

    it('a socket that never opens (handshake refused) is "dropped" as well', async () => {
      const run = start();
      await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());

      FakeSocket.last!.closeWith(1006);

      expect(run.errors[0].detail.kind).toBe('dropped');
    });

    it('a ticket the gateway refused (4xx) is "refused", carrying the gateway code, and no socket is opened', async () => {
      const ticket = vi
        .fn<TicketFn>()
        .mockRejectedValue(
          new PortalApiError(
            409,
            'portal.voice_chat_coload_failed',
            'could not be loaded together',
          ),
        );
      const run = start(ticket);

      await vi.waitFor(() => expect(run.errors).toHaveLength(1));

      expect(run.errors[0].detail).toEqual({
        kind: 'refused',
        code: 'portal.voice_chat_coload_failed',
      });
      expect(FakeSocket.last).toBeUndefined();
      expect(track.stop).toHaveBeenCalled();
      expect(run.states.at(-1)).toBe('error');
    });

    it('a ticket request that did not get through (network, 5xx) is "dropped": worth trying again', async () => {
      const network = start(vi.fn<TicketFn>().mockRejectedValue(new TypeError('Failed to fetch')));
      await vi.waitFor(() => expect(network.errors).toHaveLength(1));
      expect(network.errors[0].detail).toEqual({ kind: 'dropped' });

      const server = start(
        vi
          .fn<TicketFn>()
          .mockRejectedValue(
            new PortalApiError(500, 'portal.voice_chat_ticket_failed', 'ticket failed'),
          ),
      );
      await vi.waitFor(() => expect(server.errors).toHaveLength(1));
      expect(server.errors[0].detail).toEqual({ kind: 'dropped' });
    });

    it('a refused microphone is "denied", and no ticket is minted', async () => {
      getUserMedia.mockRejectedValueOnce(
        Object.assign(new Error('Permission denied'), { name: 'NotAllowedError' }),
      );
      const run = start();

      await vi.waitFor(() => expect(run.errors).toHaveLength(1));

      expect(run.errors[0].detail).toEqual({ kind: 'denied' });
      expect(run.ticket).not.toHaveBeenCalled();
    });

    it('a browser without microphone capture is "unsupported"', async () => {
      Reflect.deleteProperty(navigator, 'mediaDevices');
      const run = start();

      await vi.waitFor(() => expect(run.errors).toHaveLength(1));

      expect(run.errors[0].detail).toEqual({ kind: 'unsupported' });
    });

    it('a worklet processor error is a capture error, and releases the microphone', async () => {
      const run = start();
      await connected(run);

      FakeWorkletNode.last!.onprocessorerror?.();

      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].detail).toEqual({ kind: 'capture' });
      expect(track.stop).toHaveBeenCalled();
      expect(run.states.at(-1)).toBe('error');
    });

    it('a microphone that goes away is a capture error, said goodbye to', async () => {
      const run = start();
      const socket = await connected(run);

      track.onended?.();

      expect(run.errors).toHaveLength(1);
      expect(run.errors[0].detail).toEqual({ kind: 'capture' });
      expect(socket.controls()).toEqual([{ type: 'bye' }]);
      expect(run.states.at(-1)).toBe('error');
    });
  });
});

describe('parseVoiceDownFrame', () => {
  it.each([
    [
      { type: 'state', state: 'idle' },
      { type: 'state', state: 'idle' },
    ],
    [
      { type: 'state', state: 'listening' },
      { type: 'state', state: 'listening' },
    ],
    [
      { type: 'state', state: 'thinking' },
      { type: 'state', state: 'thinking' },
    ],
    [
      { type: 'state', state: 'speaking' },
      { type: 'state', state: 'speaking' },
    ],
    [
      { type: 'state', state: 'draining' },
      { type: 'state', state: 'draining' },
    ],
    [
      { type: 'asr_partial', text: 'hel' },
      { type: 'asr_partial', text: 'hel' },
    ],
    [
      { type: 'asr_final', text: 'hello', message_id: 'm1' },
      { type: 'asr_final', text: 'hello', messageId: 'm1' },
    ],
    [
      { type: 'answer_delta', text: 'Hi' },
      { type: 'answer_delta', text: 'Hi' },
    ],
    [
      { type: 'answer_done', message_id: 'm2' },
      { type: 'answer_done', messageId: 'm2' },
    ],
    [{ type: 'answer_reset' }, { type: 'answer_reset' }],
    [{ type: 'audio_done' }, { type: 'audio_done' }],
    [
      { type: 'audio_delta', seq: 0, rate: 24000, audio: 'AQA=' },
      { type: 'audio_delta', seq: 0, rate: 24000, audio: 'AQA=' },
    ],
    [
      { type: 'error', code: 'speech.upstream_error', message: 'failed' },
      { type: 'error', code: 'speech.upstream_error', message: 'failed' },
    ],
  ])('reads %j', (wire, parsed) => {
    expect(parseVoiceDownFrame(JSON.stringify(wire))).toEqual(parsed);
  });

  it('reads a field the gateway left out (an empty one) as empty', () => {
    expect(parseVoiceDownFrame('{"type":"asr_partial"}')).toEqual({
      type: 'asr_partial',
      text: '',
    });
    expect(parseVoiceDownFrame('{"type":"asr_final"}')).toEqual({ type: 'asr_final', text: '' });
    expect(parseVoiceDownFrame('{"type":"answer_done"}')).toEqual({ type: 'answer_done' });
    // An empty audio chunk has no `audio` on the wire.
    expect(parseVoiceDownFrame('{"type":"audio_delta","seq":3,"rate":24000}')).toEqual({
      type: 'audio_delta',
      seq: 3,
      rate: 24000,
      audio: '',
    });
    expect(parseVoiceDownFrame('{"type":"error"}')).toEqual({ type: 'error', message: '' });
  });

  it.each([
    ['text that is not JSON', 'not json'],
    ['JSON that is not an object', '"state"'],
    ['an array', '[1]'],
    ['null', 'null'],
    ['an unknown type', '{"type":"something_new"}'],
    ['no type', '{"text":"x"}'],
    ['an unknown state', '{"type":"state","state":"dancing"}'],
    ['a state with none', '{"type":"state"}'],
    ['a text of the wrong type', '{"type":"asr_partial","text":5}'],
    ['an audio chunk with no seq', '{"type":"audio_delta","rate":24000,"audio":"AQA="}'],
    ['an audio chunk with a non-numeric seq', '{"type":"audio_delta","seq":"0","rate":24000}'],
    ['an audio chunk with no rate', '{"type":"audio_delta","seq":0,"audio":"AQA="}'],
    [
      'an audio chunk whose audio is not text',
      '{"type":"audio_delta","seq":0,"rate":24000,"audio":1}',
    ],
    ['an error whose message is not text', '{"type":"error","message":{}}'],
  ])('reads %s as nothing (null)', (_name, data) => {
    expect(parseVoiceDownFrame(data)).toBeNull();
  });

  it('leaves a code off an error frame when it is empty or not text', () => {
    expect(parseVoiceDownFrame('{"type":"error","code":"","message":"m"}')).toEqual({
      type: 'error',
      message: 'm',
    });
    expect(parseVoiceDownFrame('{"type":"error","code":7,"message":"m"}')).toEqual({
      type: 'error',
      message: 'm',
    });
  });
});

describe('voiceChatWsUrl', () => {
  it('is the voice endpoint on the page`s own origin, wss under https', () => {
    expect(voiceChatWsUrl({ protocol: 'http:', host: 'localhost:4173' })).toBe(
      'ws://localhost:4173/api/portal/voice-chat',
    );
    expect(voiceChatWsUrl({ protocol: 'https:', host: 'gateway.example' })).toBe(
      'wss://gateway.example/api/portal/voice-chat',
    );
  });
});
