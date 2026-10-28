// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { PortalApiError } from '../../api';
import { StreamingPcmPlayer, type PlaybackContext } from './speechPlayback';
import {
  VOICE_WS_PATH,
  VOICE_WS_SUBPROTOCOL,
  createVoiceSession,
  parseVoiceDownFrame,
  voiceChatWsUrl,
  type VoiceAnswer,
  type VoiceAudioSection,
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

let track: {
  stop: ReturnType<typeof vi.fn>;
  onended: (() => void) | null;
  enabled: boolean;
};
let getUserMedia: ReturnType<typeof vi.fn>;

beforeEach(() => {
  FakeAudioContext.instances = [];
  FakeAudioContext.resumeTo = 'running';
  FakeWorkletNode.last = undefined;
  FakeSocket.last = undefined;
  track = { stop: vi.fn(), onended: null, enabled: true };
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
  voice: string;
}) => Promise<{ ticket: string }>;

type FakePlayer = VoicePlayer & {
  push: Mock<VoicePlayer['push']>;
  stop: Mock<VoicePlayer['stop']>;
  pause: Mock<VoicePlayer['pause']>;
  resume: Mock<VoicePlayer['resume']>;
  // How many chunks the player still has to play out; a test sets it, as the real one counts.
  pending: number;
};

type Run = {
  session: ReturnType<typeof createVoiceSession>;
  states: VoiceSessionState[];
  transcripts: VoiceTranscript[];
  answers: VoiceAnswer[];
  errors: { message: string; detail: VoiceErrorDetail }[];
  onAnswerReset: Mock<() => void>;
  onAudioDone: Mock<() => void>;
  // Every change of the pause the session reports, and every section its audio moved to.
  paused: boolean[];
  sections: VoiceAudioSection[];
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
    paused: [],
    sections: [],
    ticket,
    players: [],
  };
  run.session = createVoiceSession({
    api: { createVoiceChatTicket: ticket },
    chatId: 'chat_1',
    asrModel: 'asr',
    llmModel: 'llm',
    ttsModel: 'tts',
    voice: 'alba',
    onTranscript: (update) => run.transcripts.push(update),
    onAnswer: (update) => run.answers.push(update),
    onAnswerReset: run.onAnswerReset,
    onAudioDone: run.onAudioDone,
    onState: (state) => run.states.push(state),
    onError: (message, detail) => run.errors.push({ message, detail }),
    onPausedChange: (paused) => run.paused.push(paused),
    onAudioSection: (section) => run.sections.push(section),
    createPlayer: () => {
      // onEmpty starts null and is wired by the session at creation (the #304 drain hook); a
      // test drives the drain by invoking whatever the session wired.
      // Like the real one, it has a chunk still to play for every chunk pushed (it never plays
      // out here: a test that wants it drained says so, with onEmpty or by setting `pending`).
      const player: FakePlayer = {
        push: vi.fn(() => {
          player.pending += 1;
          return true;
        }),
        stop: vi.fn(),
        pause: vi.fn(),
        resume: vi.fn(),
        pending: 0,
        onEmpty: null,
      };
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

function audioDelta(seq: number, bytes: number[] = [1, 0, 2, 0], rate = 24000, section?: number) {
  return {
    type: 'audio_delta',
    seq,
    rate,
    audio: b64(...bytes),
    ...(section === undefined ? {} : { section }),
  };
}

describe('createVoiceSession in a (fake) browser', () => {
  it('starts connecting, mints the ticket for the chat, its three models and the voice, and offers the ticket as a subprotocol', async () => {
    const run = start();
    expect(run.states).toEqual(['connecting']);

    const socket = await connected(run);

    expect(run.ticket).toHaveBeenCalledWith({
      chatId: 'chat_1',
      asrModel: 'asr',
      llmModel: 'llm',
      ttsModel: 'tts',
      voice: 'alba',
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

    // The player's queue plays out -> the session reports playout finished, carrying the count of
    // audio received so far (one chunk) as played_through (#322).
    player.onEmpty!();
    expect(socket.controls()).toContainEqual({ type: 'playback_done', played_through: 1 });
  });

  it('played_through on playback_done counts the audio received, matching what the gateway relayed, and resets per answer (#322)', async () => {
    const run = start();
    const socket = await connected(run);

    // First answer: three chunks received. A gap signal mid-stream carries the count so far, and
    // the end-of-audio signal accounts for all three -- which is how the gateway tells them apart.
    socket.say(audioDelta(0));
    socket.say(audioDelta(1));
    const player1 = run.players[0];
    player1.onEmpty!(); // a gap drain after two chunks
    socket.say(audioDelta(2));
    player1.onEmpty!(); // the genuine end, after all three
    expect(socket.controls()).toContainEqual({ type: 'playback_done', played_through: 2 });
    expect(socket.controls()).toContainEqual({ type: 'playback_done', played_through: 3 });

    // The next answer is a new stream numbered from 0: the counter resets, so its signal is not
    // inflated by the previous answer's chunks.
    socket.say(audioDelta(0));
    run.players[1].onEmpty!();
    expect(socket.controls()).toContainEqual({ type: 'playback_done', played_through: 1 });

    run.session.stop();
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

  describe('pause, resume and stopping the spoken answer', () => {
    // The answer is being spoken: the gateway says so, and its audio is arriving at the player.
    async function speaking(run: Run): Promise<FakeSocket> {
      const socket = await connected(run);
      socket.say({ type: 'state', state: 'speaking' });
      socket.say(audioDelta(0));
      expect(run.players).toHaveLength(1);
      return socket;
    }

    it('pause tells the gateway AND halts the local player, and reports the pause', async () => {
      const run = start();
      const socket = await speaking(run);

      run.session.pause();

      // Both halves: the gateway holds its drain (the mic cannot reopen over paused audio), and
      // this end stops playing -- a pause on one side only would desync the floor from the sound.
      expect(socket.controls()).toEqual([{ type: 'pause' }]);
      expect(run.players[0].pause).toHaveBeenCalledTimes(1);
      expect(run.paused).toEqual([true]);
      run.session.stop();
    });

    it('pause is also accepted while the gateway drains the answer`s tail', async () => {
      const run = start();
      const socket = await speaking(run);
      socket.say({ type: 'audio_done' });
      socket.say({ type: 'state', state: 'draining' });

      run.session.pause();

      expect(socket.controls()).toEqual([{ type: 'pause' }]);
      expect(run.players[0].pause).toHaveBeenCalledTimes(1);
      run.session.stop();
    });

    it('resume tells the gateway AND continues the local player, and reports it', async () => {
      const run = start();
      const socket = await speaking(run);
      run.session.pause();

      run.session.resume();

      expect(socket.controls()).toEqual([{ type: 'pause' }, { type: 'resume' }]);
      expect(run.players[0].resume).toHaveBeenCalledTimes(1);
      expect(run.paused).toEqual([true, false]);
      run.session.stop();
    });

    it('pause and resume are idempotent: a repeat sends nothing more', async () => {
      const run = start();
      const socket = await speaking(run);

      run.session.resume(); // nothing is paused yet
      run.session.pause();
      run.session.pause();
      run.session.resume();
      run.session.resume();

      expect(socket.controls()).toEqual([{ type: 'pause' }, { type: 'resume' }]);
      expect(run.players[0].pause).toHaveBeenCalledTimes(1);
      expect(run.players[0].resume).toHaveBeenCalledTimes(1);
      expect(run.paused).toEqual([true, false]);
      run.session.stop();
    });

    it('pause is dropped when no audio is playing: before the first chunk, or while not speaking', async () => {
      const run = start();
      const socket = await connected(run);

      // Before any chunk there is nothing to pause, and the gateway would ignore it as well.
      socket.say({ type: 'state', state: 'speaking' });
      run.session.pause();
      // Listening and thinking are not playing either, whatever a player of the last answer holds.
      socket.say(audioDelta(0));
      socket.say({ type: 'state', state: 'listening' });
      run.session.pause();
      socket.say({ type: 'state', state: 'thinking' });
      run.session.pause();

      expect(socket.controls()).toEqual([]);
      expect(run.players[0].pause).not.toHaveBeenCalled();
      expect(run.paused).toEqual([]);
      run.session.stop();
    });

    it('chunks that arrive while paused still go to the player, which holds them for the resume', async () => {
      const run = start();
      const socket = await speaking(run);
      run.session.pause();

      socket.say(audioDelta(1, [3, 0, 4, 0]));

      expect(run.players).toHaveLength(1);
      expect(run.players[0].push).toHaveBeenCalledTimes(2);
      run.session.stop();
    });

    it('stopSpeaking sends {type:"stop"} and silences the local audio, and the session goes on', async () => {
      const run = start();
      const socket = await speaking(run);

      run.session.stopSpeaking();

      expect(socket.controls()).toEqual([{ type: 'stop' }]);
      expect(run.players[0].stop).toHaveBeenCalledTimes(1);
      // Not the end of the conversation: no goodbye, the socket and the microphone stay up.
      expect(socket.close).not.toHaveBeenCalled();
      expect(track.stop).not.toHaveBeenCalled();
      expect(run.states.at(-1)).not.toBe('closed');
      // The gateway puts the session back to listening, and the next answer plays on a new player.
      socket.say({ type: 'state', state: 'listening' });
      socket.say(audioDelta(0));
      expect(run.players).toHaveLength(2);
      expect(run.players[1].push).toHaveBeenCalledTimes(1);
      run.session.stop();
    });

    it('stopSpeaking while paused leaves no pause behind', async () => {
      const run = start();
      const socket = await speaking(run);
      run.session.pause();

      run.session.stopSpeaking();

      expect(socket.controls()).toEqual([{ type: 'pause' }, { type: 'stop' }]);
      expect(run.paused).toEqual([true, false]);
      // So a resume now has nothing to continue and sends nothing.
      run.session.resume();
      expect(socket.controls()).toEqual([{ type: 'pause' }, { type: 'stop' }]);
      run.session.stop();
    });

    it('stopSpeaking is safe at any time: with no audio, before the socket is open, and after the end', async () => {
      const run = start();
      await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
      const socket = FakeSocket.last!;

      run.session.pause();
      run.session.resume();
      run.session.stopSpeaking(); // the socket is not open: dropped, like every control
      socket.open();
      expect(socket.send).not.toHaveBeenCalled();

      run.session.stopSpeaking(); // open, with nothing playing: the gateway ignores it
      expect(socket.controls()).toEqual([{ type: 'stop' }]);

      run.session.stop();
      run.session.pause();
      run.session.resume();
      run.session.stopSpeaking();
      expect(socket.controls()).toEqual([{ type: 'stop' }, { type: 'bye' }]);
    });

    it('a pause does not outlive the answer: the next answer, a cancel or an error clears it', async () => {
      const run = start();
      const socket = await speaking(run);

      // A cancel silences the answer, which takes the pause with it.
      run.session.pause();
      run.session.cancel();
      expect(run.paused).toEqual([true, false]);

      // A new answer (chunk 0) is a fresh stream on a fresh player: not paused, whatever the
      // gateway's own pause was (it clears that when the turn starts over).
      socket.say(audioDelta(0));
      run.session.pause();
      socket.say(audioDelta(0));
      expect(run.paused).toEqual([true, false, true, false]);

      // A failed turn silences it as well.
      run.session.pause();
      socket.say({ type: 'error', code: 'speech.upstream_error', message: 'degraded' });
      expect(run.paused).toEqual([true, false, true, false, true, false]);
      run.session.stop();
    });

    it('the gateway moving on to listening while paused drops the pause and what the player held', async () => {
      const run = start();
      const socket = await speaking(run);
      run.session.pause();

      // The gateway holds a pause for as long as it lasts, so this is a session that was reset
      // under it: the mic is open, and held audio must not play into it on a later resume.
      socket.say({ type: 'state', state: 'listening' });

      expect(run.paused).toEqual([true, false]);
      expect(run.players[0].stop).toHaveBeenCalledTimes(1);
      run.session.resume();
      expect(socket.controls()).toEqual([{ type: 'pause' }]);
      run.session.stop();
    });

    it('no callback of a pause is delivered once the session is over', async () => {
      const run = start();
      await speaking(run);
      run.session.pause();
      expect(run.paused).toEqual([true]);

      run.session.stop();

      // The session's own teardown silences the player, which is not a change anyone is told of.
      expect(run.paused).toEqual([true]);
    });

    describe('a pause that lands as the answer`s last chunk finishes playing', () => {
      // The real player on a recording stand-in for the browser's clock: what is under test is
      // that the drain a pause swallowed is still reported once the session resumes.
      function playbackContext() {
        const sources: {
          onended: ((event: Event) => void) | null;
        }[] = [];
        const ctx = {
          currentTime: 0,
          destination: {},
          createBuffer: (_channels: number, length: number, rate: number) => ({
            duration: length / rate,
            copyToChannel: () => undefined,
          }),
          createGain: () => ({ gain: { value: 1 }, connect: () => undefined }),
          createBufferSource: () => {
            const source = {
              buffer: null,
              onended: null as ((event: Event) => void) | null,
              connect: () => undefined,
              start: () => undefined,
              stop: () => undefined,
            };
            sources.push(source);
            return source;
          },
        };
        return { ctx: ctx as unknown as PlaybackContext & { currentTime: number }, sources };
      }

      // 0.1 s of silence at 24 kHz: 2400 samples, 4800 bytes.
      const tenthOfASecond = new Array<number>(4800).fill(0);

      async function drainingOnARealPlayer() {
        const playback = playbackContext();
        const run = start(undefined, { createPlayer: () => new StreamingPcmPlayer(playback.ctx) });
        const socket = await connected(run);
        socket.say({ type: 'state', state: 'speaking' });
        socket.say(audioDelta(0, tenthOfASecond)); // scheduled 0.12 s ahead: plays 0.12 - 0.22 s
        socket.say({ type: 'audio_done' });
        socket.say({ type: 'state', state: 'draining' });
        return { run, socket, playback };
      }

      it('still sends playback_done when it resumes, so the gateway is not left to wait out its ceiling', async () => {
        const { run, socket, playback } = await drainingOnARealPlayer();

        // The clock is past the end of the last chunk, but its `ended` has not been delivered when
        // the pause arrives: the player drops it as heard, and its drain is swallowed.
        playback.ctx.currentTime = 1;
        run.session.pause();
        expect(socket.controls()).toEqual([{ type: 'pause' }]);
        run.session.resume();

        // The resume frame comes first (the gateway re-arms its drain from it), then the drain
        // signal, with the count of audio received.
        expect(socket.controls()).toEqual([
          { type: 'pause' },
          { type: 'resume' },
          { type: 'playback_done', played_through: 1 },
        ]);
        run.session.stop();
      });

      it('does not report a drain on resume when audio is still left to play: that one is sent at the true end', async () => {
        const { run, socket, playback } = await drainingOnARealPlayer();

        playback.ctx.currentTime = 0.15; // inside the chunk: the rest of it is held
        run.session.pause();
        run.session.resume();
        expect(socket.controls()).toEqual([{ type: 'pause' }, { type: 'resume' }]);

        // The resumed remainder plays out, and the drain hook the session wired reports it, once.
        playback.sources.at(-1)!.onended!(new Event('ended'));
        expect(socket.controls()).toEqual([
          { type: 'pause' },
          { type: 'resume' },
          { type: 'playback_done', played_through: 1 },
        ]);
        run.session.stop();
      });

      it('resuming a pause the player had nothing to play for also sends the drain, with the count so far', async () => {
        const run = start();
        const socket = await connected(run);
        socket.say({ type: 'state', state: 'speaking' });
        socket.say(audioDelta(0));
        socket.say(audioDelta(1));
        run.session.pause();
        run.players[0].pending = 0; // everything it held had already been heard

        run.session.resume();

        expect(socket.controls()).toEqual([
          { type: 'pause' },
          { type: 'resume' },
          { type: 'playback_done', played_through: 2 },
        ]);
        run.session.stop();
      });

      it('sends no drain on resume while the player still has chunks to play', async () => {
        const run = start();
        const socket = await connected(run);
        socket.say({ type: 'state', state: 'speaking' });
        socket.say(audioDelta(0));
        run.session.pause();
        expect(run.players[0].pending).toBe(1);

        run.session.resume();

        expect(socket.controls()).toEqual([{ type: 'pause' }, { type: 'resume' }]);
        run.session.stop();
      });
    });
  });

  describe('mic-mute', () => {
    it('stops sending the microphone up while muted, and sends it again when unmuted', async () => {
      const run = start();
      const socket = await connected(run);
      const worklet = FakeWorkletNode.last!;
      worklet.pcm(1, 1);
      expect(socket.send).toHaveBeenCalledTimes(1);

      run.session.setMicMuted(true);
      worklet.pcm(2, 2);
      worklet.pcm(3, 3);
      expect(socket.send).toHaveBeenCalledTimes(1);

      run.session.setMicMuted(false);
      worklet.pcm(4, 4);
      expect(socket.send).toHaveBeenCalledTimes(2);
      expect(new Uint8Array(socket.send.mock.calls[1][0])).toEqual(new Uint8Array([4, 4]));
      run.session.stop();
    });

    it('also disables the microphone track while muted, so nothing is captured to be sent late', async () => {
      const run = start();
      await connected(run);
      expect(track.enabled).toBe(true);

      run.session.setMicMuted(true);
      expect(track.enabled).toBe(false);

      run.session.setMicMuted(false);
      expect(track.enabled).toBe(true);
      run.session.stop();
    });

    it('needs no frame of its own: the gateway simply hears nothing, and detects no turn', async () => {
      const run = start();
      const socket = await connected(run);

      run.session.setMicMuted(true);
      run.session.setMicMuted(false);
      run.session.setMicMuted(true);

      expect(socket.controls()).toEqual([]);
      run.session.stop();
    });

    it('a mute asked for while the microphone is still being set up holds from the first batch', async () => {
      const run = start();
      run.session.setMicMuted(true);
      const socket = await connected(run);

      expect(track.enabled).toBe(false);
      FakeWorkletNode.last!.pcm(5, 5);
      expect(socket.send).not.toHaveBeenCalled();
      run.session.stop();
    });

    it('audio held for the socket is dropped by a mute, not sent when it opens', async () => {
      const run = start();
      await vi.waitFor(() => expect(FakeWorkletNode.last).toBeDefined());
      await vi.waitFor(() => expect(FakeSocket.last).toBeDefined());
      const socket = FakeSocket.last!;
      FakeWorkletNode.last!.pcm(1, 1);

      run.session.setMicMuted(true);
      FakeWorkletNode.last!.pcm(2, 2);
      socket.open();

      expect(socket.send).not.toHaveBeenCalled();
      run.session.stop();
    });

    it('leaves the push-to-talk controls and the answer alone: it silences the microphone, nothing else', async () => {
      const run = start();
      const socket = await connected(run);
      socket.say({ type: 'state', state: 'speaking' });
      socket.say(audioDelta(0));
      run.session.setMicMuted(true);

      run.session.pushStart();
      socket.say(audioDelta(1));

      expect(socket.controls()).toEqual([{ type: 'push_start' }]);
      expect(run.players[0].push).toHaveBeenCalledTimes(2);
      expect(run.players[0].stop).not.toHaveBeenCalled();
      run.session.stop();
    });

    it('is ignored once the session is over', async () => {
      const run = start();
      await connected(run);
      run.session.stop();

      run.session.setMicMuted(true);

      // The microphone was released with the session: nothing is touched again.
      expect(track.enabled).toBe(true);
    });
  });

  describe('which section the live audio belongs to', () => {
    it('reports each section as its first chunk arrives, with its place among the sections that have audio', async () => {
      const run = start();
      const socket = await connected(run);

      // One monotonic seq across the sections; the section tag is separate (the gateway leaves
      // the zero one out of the frame).
      socket.say(audioDelta(0));
      socket.say(audioDelta(1));
      socket.say(audioDelta(2, [1, 0, 2, 0], 24000, 1));
      socket.say(audioDelta(3, [1, 0, 2, 0], 24000, 1));

      expect(run.sections).toEqual([
        { section: 0, ordinal: 0 },
        { section: 1, ordinal: 1 },
      ]);
      run.session.stop();
    });

    it('attributes by the tag, across a gap: a section whose speech failed leaves a hole in the indices, not in the ordinals', async () => {
      const run = start();
      const socket = await connected(run);

      // Section 1's leg failed, so the gateway's launch indices are 0 and 2. The committed turn
      // keeps two blocks, in order, so the second block is the one tagged 2.
      socket.say(audioDelta(0, [1, 0, 2, 0], 24000, 0));
      socket.say(audioDelta(1, [1, 0, 2, 0], 24000, 2));
      socket.say(audioDelta(2, [1, 0, 2, 0], 24000, 2));
      socket.say(audioDelta(3, [1, 0, 2, 0], 24000, 5));

      expect(run.sections).toEqual([
        { section: 0, ordinal: 0 },
        { section: 2, ordinal: 1 },
        { section: 5, ordinal: 2 },
      ]);
      run.session.stop();
    });

    it('does not break the one-reset-per-answer rule: a later section continues the seq, it does not restart the stream', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say(audioDelta(0));
      socket.say(audioDelta(1));
      socket.say(audioDelta(2, [1, 0, 2, 0], 24000, 1));
      socket.say(audioDelta(3, [1, 0, 2, 0], 24000, 1));
      socket.say({ type: 'audio_done' });

      // One answer: one player, never silenced, every chunk of both sections in it, and one count
      // for the playout counter.
      expect(run.players).toHaveLength(1);
      expect(run.players[0].stop).not.toHaveBeenCalled();
      expect(run.players[0].push).toHaveBeenCalledTimes(4);
      run.players[0].onEmpty!();
      expect(socket.controls()).toContainEqual({ type: 'playback_done', played_through: 4 });
      run.session.stop();
    });

    it('starts counting again with the next answer (chunk 0)', async () => {
      const run = start();
      const socket = await connected(run);
      socket.say(audioDelta(0));
      socket.say(audioDelta(1, [1, 0, 2, 0], 24000, 1));

      socket.say(audioDelta(0, [1, 0, 2, 0], 24000, 2));

      expect(run.players).toHaveLength(2);
      expect(run.sections).toEqual([
        { section: 0, ordinal: 0 },
        { section: 1, ordinal: 1 },
        { section: 2, ordinal: 0 },
      ]);
      run.session.stop();
    });

    it('says nothing for audio that is not played live: a stream abandoned at a hole in it', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say(audioDelta(0));
      socket.say(audioDelta(2, [1, 0, 2, 0], 24000, 1)); // seq 1 never arrived: abandoned

      expect(run.sections).toEqual([{ section: 0, ordinal: 0 }]);
      run.session.stop();
    });

    it('says nothing for a chunk that could not be played either: audio that is not base64 abandons the stream', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say({ type: 'audio_delta', seq: 0, rate: 24000, audio: '!!not base64!!', section: 0 });
      socket.say(audioDelta(1, [1, 0, 2, 0], 24000, 1));

      expect(run.players[0].push).not.toHaveBeenCalled();
      expect(run.sections).toEqual([]);
      run.session.stop();
    });

    it('reads a section tag that is not a whole non-negative number as no tag', async () => {
      const run = start();
      const socket = await connected(run);

      socket.say({ type: 'audio_delta', seq: 0, rate: 24000, audio: b64(1, 0, 2, 0), section: -1 });
      socket.say({
        type: 'audio_delta',
        seq: 1,
        rate: 24000,
        audio: b64(1, 0, 2, 0),
        section: 'x',
      });

      // The audio is still played, as part of the first section.
      expect(run.players[0].push).toHaveBeenCalledTimes(2);
      expect(run.sections).toEqual([{ section: 0, ordinal: 0 }]);
      run.session.stop();
    });
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

    it('an error frame silences what is still queued to play, as a cancel does', async () => {
      const run = start();
      const socket = await connected(run);
      // A turn that degraded after some of its audio was already sent down.
      socket.say(audioDelta(0));
      socket.say(audioDelta(1));
      expect(run.players).toHaveLength(1);
      expect(run.players[0].stop).not.toHaveBeenCalled();

      socket.say({ type: 'error', code: 'speech.upstream_error', message: 'degraded' });

      // The queued audio is stopped, so a later push_start does not reopen the microphone into it.
      expect(run.players[0].stop).toHaveBeenCalledTimes(1);
      expect(run.errors).toHaveLength(1);
      // The next answer is heard on a player of its own, not the silenced one.
      socket.say(audioDelta(0));
      expect(run.players).toHaveLength(2);
      expect(run.players[1].push).toHaveBeenCalledTimes(1);
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

      // A 5xx that is not the gateway's own Voice Chat answer stays the plain "dropped".
      const proxy = start(
        vi.fn<TicketFn>().mockRejectedValue(new PortalApiError(502, 'proxy.bad_gateway', 'oops')),
      );
      await vi.waitFor(() => expect(proxy.errors).toHaveLength(1));
      expect(proxy.errors[0].detail).toEqual({ kind: 'dropped' });
    });

    // The gateway's own 500 for a mint that failed (portal.voice_chat_ticket_failed) is still a
    // failure worth trying again -- so "dropped", the switch stays open -- but it carries its
    // code, so the control can say what it is (its own sentence) instead of the generic one.
    it('a 5xx the gateway answers with its own Voice Chat code is "dropped" and carries that code', async () => {
      const server = start(
        vi
          .fn<TicketFn>()
          .mockRejectedValue(
            new PortalApiError(500, 'portal.voice_chat_ticket_failed', 'ticket failed'),
          ),
      );
      await vi.waitFor(() => expect(server.errors).toHaveLength(1));
      expect(server.errors[0].detail).toEqual({
        kind: 'dropped',
        code: 'portal.voice_chat_ticket_failed',
      });
      expect(FakeSocket.last).toBeUndefined();
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
      { type: 'audio_delta', seq: 4, rate: 24000, audio: 'AQA=', section: 2 },
      { type: 'audio_delta', seq: 4, rate: 24000, audio: 'AQA=', section: 2 },
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

  it('leaves the section off an audio frame when it is missing or not a whole non-negative number', () => {
    // The gateway leaves the zero section out, so a missing one is the first section; a tag that
    // is not a whole number is no tag, and the frame is still the audio it is.
    const base = { type: 'audio_delta', seq: 1, rate: 24000, audio: 'AQA=' };
    for (const section of [undefined, -1, 1.5, '2', null, {}]) {
      expect(parseVoiceDownFrame(JSON.stringify({ ...base, section }))).toEqual(base);
    }
    expect(parseVoiceDownFrame(JSON.stringify({ ...base, section: 0 }))).toEqual({
      ...base,
      section: 0,
    });
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
