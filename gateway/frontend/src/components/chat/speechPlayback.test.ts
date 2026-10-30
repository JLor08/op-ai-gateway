// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  acceptChunk,
  createSpeechPlayback,
  decodeBase64,
  pcm16ToFloat32,
  StreamingPcmPlayer,
  type AudioChunk,
  type LiveState,
  type PlaybackContext,
  type PlaybackSink,
} from './speechPlayback';

// base64 of the given bytes, the way the gateway carries a chunk's PCM.
function b64(bytes: number[]): string {
  return btoa(String.fromCharCode(...bytes));
}

// 16-bit little-endian PCM of the given samples.
function pcm(samples: number[]): number[] {
  return samples.flatMap((s) => [s & 0xff, (s >> 8) & 0xff]);
}

const chunk = (seq: number, data = b64(pcm([0, 1])), rate = 44100): AudioChunk => ({
  seq,
  rate,
  data,
});

describe('decodeBase64', () => {
  it('decodes standard padded base64 to its bytes', () => {
    expect(Array.from(decodeBase64(b64([0, 1, 254, 255])) ?? [])).toEqual([0, 1, 254, 255]);
  });

  it('is null for text that is not base64, never a throw', () => {
    expect(decodeBase64('not base64 !!')).toBeNull();
  });

  it('decodes the empty string to no bytes', () => {
    expect(decodeBase64('')?.length).toBe(0);
  });
});

describe('pcm16ToFloat32', () => {
  it('maps signed 16-bit little-endian samples to [-1, 1)', () => {
    const { samples, carry } = pcm16ToFloat32(
      new Uint8Array(pcm([0, 16384, -16384, -32768])),
      null,
    );
    expect(carry).toBeNull();
    expect(Array.from(samples)).toEqual([0, 0.5, -0.5, -1]);
  });

  it('carries the odd byte of a chunk that splits a sample into the next chunk', () => {
    const whole = pcm([1000, -1000, 5]);
    const first = pcm16ToFloat32(new Uint8Array(whole.slice(0, 3)), null);
    expect(first.samples).toHaveLength(1);
    expect(first.carry).toBe(whole[2]);
    const second = pcm16ToFloat32(new Uint8Array(whole.slice(3)), first.carry);
    expect(second.carry).toBeNull();
    // Together they are exactly the samples of the unsplit audio.
    const all = pcm16ToFloat32(new Uint8Array(whole), null).samples;
    expect([...first.samples, ...second.samples]).toEqual(Array.from(all));
  });

  it('keeps a single carried byte waiting when the next chunk is empty', () => {
    const r = pcm16ToFloat32(new Uint8Array(0), 7);
    expect(r.samples).toHaveLength(0);
    expect(r.carry).toBe(7);
  });
});

// The progressive-playback reducer: a chunk is played only while the stream is whole.
describe('acceptChunk', () => {
  const waiting: LiveState = { phase: 'waiting' };

  it('starts live playback at chunk 0 and expects the next one', () => {
    expect(acceptChunk(waiting, chunk(0))).toEqual({
      state: { phase: 'live', rate: 44100, nextSeq: 1 },
      play: true,
    });
  });

  it('plays the chunks of an unbroken stream in order', () => {
    let state: LiveState = waiting;
    for (let seq = 0; seq < 5; seq++) {
      const r = acceptChunk(state, chunk(seq));
      expect(r.play).toBe(true);
      state = r.state;
    }
    expect(state).toEqual({ phase: 'live', rate: 44100, nextSeq: 5 });
  });

  it('abandons live playback at the first gap, and stays abandoned', () => {
    let state = acceptChunk(waiting, chunk(0)).state;
    const gap = acceptChunk(state, chunk(2)); // chunk 1 never arrived
    expect(gap).toEqual({ state: { phase: 'abandoned', reason: 'gap' }, play: false });
    state = gap.state;
    // A later in-order-looking chunk does not resume a stream that has a hole in it.
    expect(acceptChunk(state, chunk(3)).play).toBe(false);
  });

  it('abandons a stream joined late (the first chunk seen is not chunk 0)', () => {
    expect(acceptChunk(waiting, chunk(4))).toEqual({
      state: { phase: 'abandoned', reason: 'gap' },
      play: false,
    });
  });

  it('abandons a repeated chunk (a reconnect replay) rather than playing it twice', () => {
    const state = acceptChunk(waiting, chunk(0)).state;
    expect(acceptChunk(state, chunk(0)).play).toBe(false);
  });

  it('abandons a stream whose sample rate changes', () => {
    const state = acceptChunk(waiting, chunk(0, undefined, 44100)).state;
    expect(acceptChunk(state, chunk(1, undefined, 24000))).toEqual({
      state: { phase: 'abandoned', reason: 'format' },
      play: false,
    });
  });

  it.each([0, -1, 7999, 192001, 44100.5, Number.NaN])(
    'refuses to play at an implausible sample rate (%s)',
    (rate) => {
      expect(acceptChunk(waiting, chunk(0, undefined, rate))).toEqual({
        state: { phase: 'abandoned', reason: 'format' },
        play: false,
      });
    },
  );

  it('refuses a chunk with no usable audio field', () => {
    const bad = { seq: 0, rate: 44100, data: undefined } as unknown as AudioChunk;
    expect(acceptChunk(waiting, bad).state).toEqual({ phase: 'abandoned', reason: 'format' });
  });
});

// A recording stand-in for the AudioWorklet sink the real player feeds: jsdom has no audio
// thread, so the player's whole observable behaviour is what it writes to the sink and how it
// reacts to the sink reporting a drain. `drain` plays the worklet's `drained` message back.
type RecordingSink = PlaybackSink & {
  writes: Float32Array[];
  muted: boolean;
  paused: boolean;
  stopped: boolean;
  pauseCount: number;
  resumeCount: number;
  // Total samples written so far: what the player counts as pending until a drain reports it played.
  written(): number;
  // The worklet drained to empty after outputting `played` samples in total.
  drain(played: number): void;
};

function fakeSink(): RecordingSink {
  const sink: RecordingSink = {
    writes: [],
    muted: false,
    paused: false,
    stopped: false,
    pauseCount: 0,
    resumeCount: 0,
    onDrained: null,
    write(samples) {
      sink.writes.push(samples);
    },
    setMuted(muted) {
      sink.muted = muted;
    },
    pause() {
      sink.paused = true;
      sink.pauseCount += 1;
    },
    resume() {
      sink.paused = false;
      sink.resumeCount += 1;
    },
    stop() {
      sink.stopped = true;
      sink.onDrained = null;
    },
    written() {
      return sink.writes.reduce((total, w) => total + w.length, 0);
    },
    drain(played) {
      sink.onDrained?.(played);
    },
  };
  return sink;
}

function fakeContext(sampleRate = 48000): PlaybackContext {
  return { sampleRate };
}

// A chunk of `count` identical samples: large enough that resampling always yields output.
const toneBytes = (count: number, value = 1000): Uint8Array =>
  new Uint8Array(pcm(new Array(count).fill(value)));

describe('StreamingPcmPlayer', () => {
  function withSink(sampleRate = 48000) {
    const sink = fakeSink();
    const player = new StreamingPcmPlayer(fakeContext(sampleRate), () => sink);
    return { player, sink };
  }

  it('decodes a chunk and writes its resampled audio to the sink', () => {
    const { player, sink } = withSink();
    expect(player.push(toneBytes(100), 44100)).toBe(true);
    expect(sink.writes).toHaveLength(1);
    expect(sink.writes[0].length).toBeGreaterThan(0);
  });

  it('writes nothing for a chunk that holds no whole sample yet (a carried byte)', () => {
    const { player, sink } = withSink();
    expect(player.push(new Uint8Array([7]), 44100)).toBe(true); // one byte: half a sample
    expect(sink.writes).toHaveLength(0);
  });

  it('refuses a chunk at an impossible sample rate, with nothing written', () => {
    const { player, sink } = withSink();
    expect(player.push(toneBytes(100), 0)).toBe(false);
    expect(sink.writes).toHaveLength(0);
  });

  it('refuses every chunk once stopped, and stops the sink', () => {
    const { player, sink } = withSink();
    player.push(toneBytes(100), 44100);
    player.stop();
    expect(sink.stopped).toBe(true);
    expect(player.push(toneBytes(100), 44100)).toBe(false);
    expect(sink.writes).toHaveLength(1);
  });

  it('counts written audio as pending until the sink reports it drained', () => {
    const { player, sink } = withSink(48000);
    player.push(toneBytes(100), 44100);
    const written = sink.written();
    expect(written).toBeGreaterThan(0);
    expect(player.pending).toBe(written);
    expect(player.remainingSeconds()).toBeCloseTo(written / 48000, 6);
    sink.drain(written);
    expect(player.pending).toBe(0);
    expect(player.remainingSeconds()).toBe(0);
  });

  it('fires onEmpty on every drain the sink reports (a gap, then the end)', () => {
    const { player, sink } = withSink();
    const onEmpty = vi.fn();
    player.onEmpty = onEmpty;
    player.push(toneBytes(100), 44100);
    sink.drain(sink.written());
    expect(onEmpty).toHaveBeenCalledTimes(1);
    player.push(toneBytes(100), 44100);
    sink.drain(sink.written());
    expect(onEmpty).toHaveBeenCalledTimes(2);
    expect(player.pending).toBe(0);
  });

  it('does not fire onEmpty for a drain that lands while paused: a pause is not a drain', () => {
    const { player, sink } = withSink();
    const onEmpty = vi.fn();
    player.onEmpty = onEmpty;
    player.push(toneBytes(100), 44100);
    player.pause();
    sink.drain(sink.written()); // a stale drain delivered after the pause
    expect(onEmpty).not.toHaveBeenCalled();
  });

  it('does not fire onEmpty after stop (its drain hook is detached)', () => {
    const { player, sink } = withSink();
    const onEmpty = vi.fn();
    player.onEmpty = onEmpty;
    player.push(toneBytes(100), 44100);
    player.stop();
    sink.drain(100);
    expect(onEmpty).not.toHaveBeenCalled();
  });

  it('mutes and unmutes through the sink, including a mute asked for before the first chunk', () => {
    const { player, sink } = withSink();
    player.setMuted(true);
    expect(sink.muted).toBe(true);
    player.push(toneBytes(100), 44100);
    expect(sink.muted).toBe(true);
    player.setMuted(false);
    expect(sink.muted).toBe(false);
  });

  it('pauses and resumes the sink, and is idempotent', () => {
    const { player, sink } = withSink();
    player.push(toneBytes(100), 44100);
    player.pause();
    player.pause(); // a second pause does nothing
    expect(sink.paused).toBe(true);
    expect(sink.pauseCount).toBe(1);
    player.resume();
    player.resume(); // a resume that is not paused does nothing
    expect(sink.paused).toBe(false);
    expect(sink.resumeCount).toBe(1);
  });

  it('does not touch the sink for pause/resume/push after stop', () => {
    const { player, sink } = withSink();
    player.stop();
    player.pause();
    player.resume();
    expect(sink.pauseCount).toBe(0);
    expect(sink.resumeCount).toBe(0);
  });
});

describe('createSpeechPlayback', () => {
  function setup(withContext = true, sampleRate = 48000) {
    const ctx: PlaybackContext & { state: string; resume: ReturnType<typeof vi.fn> } = {
      sampleRate,
      state: 'running',
      resume: vi.fn(() => Promise.resolve()),
    };
    const sinks: RecordingSink[] = [];
    const makePlayer = (c: PlaybackContext) =>
      new StreamingPcmPlayer(c, () => {
        const sink = fakeSink();
        sinks.push(sink);
        return sink;
      });
    const playback = createSpeechPlayback(() => (withContext ? ctx : null), makePlayer);
    return { playback, ctx, sinks };
  }

  it('plays the chunks of a run as they arrive and reports the chat live', () => {
    const { playback, sinks } = setup();
    expect(playback.isLive('c1')).toBe(false);
    expect(playback.push('c1', 'r1', chunk(0))).toBe(true);
    expect(playback.push('c1', 'r1', chunk(1))).toBe(true);
    expect(sinks[0].writes).toHaveLength(2);
    expect(playback.isLive('c1')).toBe(true);
    expect(playback.isLive('c2')).toBe(false);
  });

  it('stops pushing audio at a gap, without cutting what is already queued', () => {
    const { playback, sinks } = setup();
    playback.push('c1', 'r1', chunk(0));
    expect(playback.push('c1', 'r1', chunk(2))).toBe(false);
    expect(playback.isLive('c1')).toBe(false);
    expect(sinks[0].writes).toHaveLength(1);
    expect(sinks[0].stopped).toBe(false);
  });

  it('starts a fresh session for a new run of the same chat', () => {
    const { playback, sinks } = setup();
    playback.push('c1', 'r1', chunk(0));
    playback.push('c1', 'r1', chunk(2)); // r1 is abandoned
    expect(playback.push('c1', 'r2', chunk(0))).toBe(true); // r2 is its own stream
    expect(sinks).toHaveLength(2);
    expect(sinks[1].writes).toHaveLength(1);
  });

  it('lets a completed run play out, and silences one that did not complete', () => {
    const { playback, sinks } = setup();
    playback.push('c1', 'r1', chunk(0));
    playback.end('c1', true);
    expect(playback.isLive('c1')).toBe(false); // the run is done...
    expect(sinks[0].stopped).toBe(false); // ...but the tail of the audio is still playing

    playback.push('c2', 'r2', chunk(0));
    playback.end('c2', false); // a Stop or an error: no more sound
    expect(sinks[1].stopped).toBe(true);
  });

  it('is silent, and never throws, where the browser has no audio context', () => {
    const { playback, sinks } = setup(false);
    expect(playback.push('c1', 'r1', chunk(0))).toBe(false);
    expect(playback.isLive('c1')).toBe(false);
    expect(sinks).toHaveLength(0);
    expect(() => {
      playback.prime();
      playback.end('c1', true);
      playback.stopAll();
    }).not.toThrow();
  });

  it('abandons a chunk whose audio is not base64 rather than playing noise', () => {
    const { playback, sinks } = setup();
    expect(playback.push('c1', 'r1', chunk(0, '%%%'))).toBe(false);
    expect(sinks[0].writes).toHaveLength(0);
    expect(playback.push('c1', 'r1', chunk(1))).toBe(false); // and stays abandoned
  });

  it('stopAll silences every chat', () => {
    const { playback, sinks } = setup();
    playback.push('c1', 'r1', chunk(0));
    playback.push('c2', 'r2', chunk(0));
    playback.stopAll();
    expect(sinks.every((s) => s.stopped)).toBe(true);
    expect(playback.isLive('c1')).toBe(false);
  });

  // A tab that re-adopts a still-running speech run before its first chunk (a reload, a second
  // tab, during a model's cold start) has never been primed: its context is suspended, and audio
  // played on a suspended context sits on a frozen clock, to be heard -- stale, and possibly over
  // a newer run -- whenever something later resumes it. A run that cannot start on a running
  // context is not played live at all; its committed WAV is the reply.
  describe('on a context that is not running', () => {
    function suspendedSetup(state = 'suspended') {
      const ctx: PlaybackContext & { state: string; resume: ReturnType<typeof vi.fn> } = {
        sampleRate: 48000,
        state,
        resume: vi.fn(() => Promise.resolve()),
      };
      const sinks: RecordingSink[] = [];
      const playback = createSpeechPlayback(
        () => ctx,
        (c) =>
          new StreamingPcmPlayer(c, () => {
            const sink = fakeSink();
            sinks.push(sink);
            return sink;
          }),
      );
      return { playback, ctx, resume: ctx.resume, sinks };
    }

    it('does not play a run adopted on a suspended context, and a later resume plays nothing', () => {
      const { playback, ctx, resume, sinks } = suspendedSetup();
      expect(playback.push('c1', 'r1', chunk(0))).toBe(false);
      expect(resume).toHaveBeenCalled(); // it still asks, so a primed page unlocks for the NEXT run
      expect(sinks[0].writes).toHaveLength(0);
      expect(playback.isLive('c1')).toBe(false);

      // The context comes up later (a gesture resumed it): the run stays abandoned, so nothing
      // that arrived before is played late and nothing from the middle of the stream starts.
      ctx.state = 'running';
      expect(playback.push('c1', 'r1', chunk(1))).toBe(false);
      expect(sinks[0].writes).toHaveLength(0);
    });

    it('treats an interrupted context (iOS) like a suspended one', () => {
      const { playback, sinks } = suspendedSetup('interrupted');
      expect(playback.push('c1', 'r1', chunk(0))).toBe(false);
      expect(sinks[0].writes).toHaveLength(0);
    });

    it('starts the NEXT run of the chat normally once the context is running', () => {
      const { playback, ctx, sinks } = suspendedSetup();
      playback.push('c1', 'r1', chunk(0));
      ctx.state = 'running';
      expect(playback.push('c1', 'r2', chunk(0))).toBe(true);
      expect(sinks[1].writes).toHaveLength(1);
    });

    it('plays on a context that says it is running', () => {
      const { playback, sinks } = suspendedSetup('running');
      expect(playback.push('c1', 'r1', chunk(0))).toBe(true);
      expect(sinks[0].writes).toHaveLength(1);
    });

    it('abandons a live run whose context is suspended mid-run and silences its queue', () => {
      const { playback, ctx, sinks } = suspendedSetup('running');
      expect(playback.push('c1', 'r1', chunk(0))).toBe(true);
      ctx.state = 'suspended';
      expect(playback.push('c1', 'r1', chunk(1))).toBe(false);
      expect(playback.isLive('c1')).toBe(false);
      expect(sinks[0].writes).toHaveLength(1);
      // What was queued on the frozen clock must not play stale when the context resumes.
      expect(sinks[0].stopped).toBe(true);
    });

    it('silences the queue of a COMPLETED run whose context is not running', () => {
      const { playback, ctx, sinks } = suspendedSetup('running');
      playback.push('c1', 'r1', chunk(0));
      ctx.state = 'suspended';
      playback.end('c1', true);
      expect(sinks[0].stopped).toBe(true);
    });

    it('still lets a completed run play out on a running context', () => {
      const { playback, sinks } = suspendedSetup('running');
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true);
      expect(sinks[0].stopped).toBe(false);
    });
  });

  // The live mute of a chat's run (#281): reversible, local to playback, and nothing the run
  // itself notices -- it stays live, keeps queueing, and finishes as ever.
  describe('setMuted', () => {
    it('toggles the run mute on and off while the run stays live', () => {
      const { playback, sinks } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.setMuted('c1', true);
      expect(sinks[0].muted).toBe(true);
      // Silenced, not stopped: the run is live and keeps queueing audio (muted).
      expect(playback.isLive('c1')).toBe(true);
      expect(playback.push('c1', 'r1', chunk(1))).toBe(true);
      expect(sinks[0].writes).toHaveLength(2);
      expect(sinks[0].stopped).toBe(false);
      expect(sinks[0].muted).toBe(true);

      playback.setMuted('c1', false);
      expect(sinks[0].muted).toBe(false);
      expect(playback.isLive('c1')).toBe(true);
    });

    it('mutes only the chat it names', () => {
      const { playback, sinks } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.push('c2', 'r2', chunk(0));
      playback.setMuted('c1', true);
      expect(sinks.map((s) => s.muted)).toEqual([true, false]);
    });

    it('keeps a completed run muted while its queue plays out', () => {
      const { playback, sinks } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.setMuted('c1', true);
      playback.end('c1', true);
      expect(sinks[0].stopped).toBe(false);
      expect(sinks[0].muted).toBe(true);
    });

    it('does not carry over to the next run of the chat', () => {
      const { playback, sinks } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.setMuted('c1', true);
      playback.end('c1', true);
      playback.push('c1', 'r2', chunk(0));
      expect(sinks).toHaveLength(2);
      expect(sinks[1].muted).toBe(false);
    });

    it('is a no-op, and never throws, for a chat with no run or no audio context', () => {
      const { playback } = setup(false);
      expect(() => playback.setMuted('nope', true)).not.toThrow();
      const live = setup();
      expect(() => live.playback.setMuted('nope', true)).not.toThrow();
      expect(live.sinks).toHaveLength(0);
    });
  });

  // The draining tail (#281): a completed run's queued audio keeps playing after the run commits
  // and its saved player appears. The session is KEPT alive -- reachable by Mute and a tail-stop --
  // in a draining state until the sink reports its ring drained, and only then is the drain
  // signalled (onDrained) and the session dropped.
  describe('the draining tail of a completed run', () => {
    it('stays reachable until the tail ends, then signals the drain', () => {
      const { playback, sinks } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.push('c1', 'r1', chunk(1));

      playback.end('c1', true);
      // No longer "live" (the run is done), but the tail is still playing out: nothing stopped,
      // the drain has not fired, and the session is still reachable (Mute still silences it).
      expect(playback.isLive('c1')).toBe(false);
      expect(sinks[0].stopped).toBe(false);
      expect(drained).toEqual([]);
      playback.setMuted('c1', true);
      expect(sinks[0].muted).toBe(true);

      // The ring drains to empty: the chat drains.
      sinks[0].drain(sinks[0].written());
      expect(drained).toEqual(['c1']);
      expect(sinks[0].stopped).toBe(true);
    });

    it('signals the drain at once when the completed run has nothing left queued', () => {
      const { playback, sinks } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      sinks[0].drain(sinks[0].written()); // the only chunk finished before the run ended
      playback.end('c1', true);
      expect(drained).toEqual(['c1']);
    });

    it('abort() stops the draining tail at once and signals the drain', () => {
      const { playback, sinks } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true);
      expect(sinks[0].stopped).toBe(false); // still playing out

      playback.abort('c1');
      expect(sinks[0].stopped).toBe(true);
      expect(drained).toEqual(['c1']);
    });

    // A chat-delete / a Stop of a following run routes forget -> end(chatId, false), which must
    // still cut the tail even though the run already ended (the session is draining).
    it('silences the draining tail on a non-completed end (chat-delete / cancel)', () => {
      const { playback, sinks } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true); // r1 draining its tail
      expect(sinks[0].stopped).toBe(false);

      playback.end('c1', false); // the chat is deleted (forget) / a following run is cancelled
      expect(sinks[0].stopped).toBe(true); // the tail is cut, not left audible
      expect(drained).toEqual(['c1']);
    });

    it('stopAll silences a chat that is draining its tail', () => {
      const { playback, sinks } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true); // draining
      playback.stopAll();
      expect(sinks[0].stopped).toBe(true);
      expect(playback.isLive('c1')).toBe(false);
    });

    it("a new run's push for the same chat stops the prior draining tail, with no overlap", () => {
      const { playback, sinks } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true); // r1 is now draining
      expect(sinks[0].stopped).toBe(false);

      // A new run of the same chat begins while the old tail is still draining.
      expect(playback.push('c1', 'r2', chunk(0))).toBe(true);
      expect(sinks[0].stopped).toBe(true); // the old tail was cut -- no overlap
      expect(sinks[1].writes).toHaveLength(1); // r2 wrote its own first chunk
      expect(playback.isLive('c1')).toBe(true);
    });

    // A new run replacing a prior session (live OR draining) must signal the old run's drain, so
    // its render mirror (runAudioLive, and with it the mute mark) clears before the new run goes
    // live again -- otherwise a mute left on the old tail carries over to the new run.
    it.each([
      ['a draining tail', true],
      ['a still-live run', false],
    ])("signals the replaced run's drain when a new run's push takes over (%s)", (_name, endIt) => {
      const { playback } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      if (endIt) playback.end('c1', true);
      drained.length = 0; // ignore any drain from end() above; watch the replacement

      playback.push('c1', 'r2', chunk(0));
      expect(drained).toEqual(['c1']);
    });

    it('clears the tail via the fallback timer when the drain is never reported', () => {
      vi.useFakeTimers();
      try {
        const { playback, sinks } = setup();
        const drained: string[] = [];
        playback.onDrained((chatId) => drained.push(chatId));
        playback.push('c1', 'r1', chunk(0));
        playback.end('c1', true);
        expect(drained).toEqual([]);
        // The drain is never reported (a context suspended mid-tail, or a browser quirk): the
        // fallback elapses so the controls can never hang on screen forever.
        vi.advanceTimersByTime(60_000);
        expect(drained).toEqual(['c1']);
        expect(sinks[0].stopped).toBe(true);
      } finally {
        vi.useRealTimers();
      }
    });

    it('resolves the drain immediately for a completed run whose context is not running', () => {
      const ctx: PlaybackContext & { state: string; resume: ReturnType<typeof vi.fn> } = {
        sampleRate: 48000,
        state: 'running',
        resume: vi.fn(() => Promise.resolve()),
      };
      const sinks: RecordingSink[] = [];
      const playback = createSpeechPlayback(
        () => ctx,
        (c) =>
          new StreamingPcmPlayer(c, () => {
            const sink = fakeSink();
            sinks.push(sink);
            return sink;
          }),
      );
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      ctx.state = 'suspended'; // the clock froze before the run ended
      playback.end('c1', true);
      expect(drained).toEqual(['c1']);
      expect(sinks[0].stopped).toBe(true);
    });
  });

  it('prime() resumes a suspended context from inside the user gesture', () => {
    const resume = vi.fn(() => Promise.resolve());
    const ctx: PlaybackContext & { state: string; resume: typeof resume } = {
      sampleRate: 48000,
      state: 'suspended',
      resume,
    };
    const playback = createSpeechPlayback(() => ctx);
    playback.prime();
    expect(resume).toHaveBeenCalledTimes(1);
  });
});

// The real AudioWorklet sink, exercised through the default StreamingPcmPlayer construction (the
// other suites inject a fake sink): the async worklet setup, the transfer, and -- the point of
// these -- the degraded path when the worklet does not load, which must still report a drain so
// the voice session is not left waiting on the gateway's ceiling to reopen the microphone.
describe('createWorkletSink (the real AudioWorklet sink)', () => {
  // A stand-in for the browser's AudioWorkletNode: records what the page posts and lets a test play
  // the worklet's messages back. `connect` returns its argument so node.connect(gain).connect(
  // destination) chains as in the real graph.
  class FakeWorkletNode {
    static last: FakeWorkletNode | undefined;
    port = {
      onmessage: null as ((event: { data: unknown }) => void) | null,
      postMessage: vi.fn(),
    };
    connect = vi.fn((destination: unknown) => destination);
    disconnect = vi.fn();
    constructor(
      public ctx: unknown,
      public name: string,
      public options: unknown,
    ) {
      FakeWorkletNode.last = this;
    }
  }

  function workletContext() {
    const gain = { gain: { value: 1 }, connect: vi.fn((d: unknown) => d), disconnect: vi.fn() };
    const addModule = vi.fn(() => Promise.resolve());
    const destination = { name: 'destination' };
    const ctx = {
      sampleRate: 48000,
      destination,
      audioWorklet: { addModule },
      createGain: vi.fn(() => gain),
    };
    return { ctx: ctx as unknown as PlaybackContext, gain, addModule, destination };
  }

  // Flush the async setup (addModule -> node) and any queued-microtask drain.
  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

  beforeEach(() => {
    FakeWorkletNode.last = undefined;
    vi.stubGlobal('AudioWorkletNode', FakeWorkletNode);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('adds the module, wires the node through a gain to the destination, and flushes queued audio', async () => {
    const { ctx, gain, addModule, destination } = workletContext();
    const player = new StreamingPcmPlayer(ctx);
    player.push(toneBytes(100), 44100); // written before the node exists: held, then flushed
    await flush();
    const node = FakeWorkletNode.last!;
    expect(addModule).toHaveBeenCalledTimes(1);
    expect(node.connect).toHaveBeenCalledWith(gain);
    expect(gain.connect).toHaveBeenCalledWith(destination);
    const pcmPosts = node.port.postMessage.mock.calls.filter(([m]) => m instanceof Float32Array);
    expect(pcmPosts).toHaveLength(1);
    expect(pcmPosts[0][1]).toEqual([expect.any(ArrayBuffer)]); // transferred, not copied
  });

  it('fires onEmpty and applies the played count when the worklet reports a drain', async () => {
    const { ctx } = workletContext();
    const player = new StreamingPcmPlayer(ctx);
    const onEmpty = vi.fn();
    player.onEmpty = onEmpty;
    player.push(toneBytes(100), 44100);
    await flush();
    const before = player.pending;
    FakeWorkletNode.last!.port.onmessage!({ data: { type: 'drained', played: 48 } });
    expect(onEmpty).toHaveBeenCalledTimes(1);
    expect(player.pending).toBe(before - 48);
  });

  it('mutes through the gain and pauses/resumes/stops through the port', async () => {
    const { ctx, gain } = workletContext();
    const player = new StreamingPcmPlayer(ctx);
    await flush();
    const node = FakeWorkletNode.last!;
    player.setMuted(true);
    expect(gain.gain.value).toBe(0);
    player.pause();
    player.resume();
    expect(node.port.postMessage).toHaveBeenCalledWith('pause');
    expect(node.port.postMessage).toHaveBeenCalledWith('resume');
    player.stop();
    expect(node.port.postMessage).toHaveBeenCalledWith('stop');
    expect(node.disconnect).toHaveBeenCalled();
    expect(gain.disconnect).toHaveBeenCalled();
  });

  it('still reports a drain so turn-taking is not stuck when the worklet fails to load', async () => {
    const { ctx, addModule } = workletContext();
    addModule.mockImplementationOnce(() => Promise.reject(new Error('no worklet')));
    const player = new StreamingPcmPlayer(ctx);
    const onEmpty = vi.fn();
    player.onEmpty = onEmpty;
    player.push(toneBytes(100), 44100);
    await flush();
    // The audio is silent (the committed WAV covers it), but playout is reported drained so the
    // session can still tell the gateway the answer finished and reopen the microphone.
    expect(onEmpty).toHaveBeenCalled();
    expect(player.pending).toBe(0);
  });

  it('retries the worklet load for a later player after a transient failure (no cached rejection)', async () => {
    const { ctx, addModule } = workletContext();
    addModule.mockImplementationOnce(() => Promise.reject(new Error('transient')));
    const first = new StreamingPcmPlayer(ctx);
    await flush();
    first.stop();
    // A second player on the SAME context tries addModule again rather than reusing the rejection.
    const second = new StreamingPcmPlayer(ctx);
    await flush();
    expect(addModule).toHaveBeenCalledTimes(2);
    expect(FakeWorkletNode.last).toBeDefined(); // the retry built a node
    second.stop();
  });
});
