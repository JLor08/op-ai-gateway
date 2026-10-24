// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import {
  acceptChunk,
  createSpeechPlayback,
  decodeBase64,
  pcm16ToFloat32,
  StreamingPcmPlayer,
  type AudioChunk,
  type LiveState,
  type PlaybackContext,
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

// A recording stand-in for the browser's AudioContext: what the player schedules is the
// whole observable behavior (the sound itself cannot be heard in jsdom).
function fakeContext(initialTime = 0) {
  const scheduled: { start: number; duration: number; rate: number; samples: Float32Array }[] = [];
  const sources: { stopped: boolean; ended: (() => void) | null; connected: unknown[] }[] = [];
  // The gain nodes the player made: the value is what the speakers would get multiplied by.
  const gains: { gain: { value: number }; connected: unknown[] }[] = [];
  const ctx = {
    currentTime: initialTime,
    destination: { name: 'destination' },
    createGain: () => {
      const node = {
        gain: { value: 1 },
        connected: [] as unknown[],
        connect(destination: unknown) {
          node.connected.push(destination);
          return destination;
        },
      };
      gains.push(node);
      return node;
    },
    createBuffer: (_channels: number, length: number, rate: number) => {
      let copied: Float32Array = new Float32Array(0);
      return {
        duration: length / rate,
        rate,
        copyToChannel: (source: Float32Array) => {
          copied = source;
        },
        get samples() {
          return copied;
        },
      };
    },
    createBufferSource: () => {
      const entry = {
        stopped: false,
        ended: null as (() => void) | null,
        connected: [] as unknown[],
      };
      sources.push(entry);
      const src = {
        buffer: null as unknown as ReturnType<PlaybackContext['createBuffer']> | null,
        connect: vi.fn((destination: unknown) => {
          entry.connected.push(destination);
          return destination;
        }),
        start(when: number) {
          const b = src.buffer as unknown as {
            duration: number;
            rate: number;
            samples: Float32Array;
          };
          scheduled.push({ start: when, duration: b.duration, rate: b.rate, samples: b.samples });
        },
        stop() {
          entry.stopped = true;
        },
        set onended(cb: (() => void) | null) {
          entry.ended = cb;
        },
        get onended() {
          return entry.ended;
        },
      };
      return src;
    },
  };
  return {
    ctx: ctx as unknown as PlaybackContext & { currentTime: number },
    scheduled,
    sources,
    gains,
  };
}

describe('StreamingPcmPlayer', () => {
  it('schedules the first chunk a short lead ahead of now, at the stream rate', () => {
    const { ctx, scheduled } = fakeContext(10);
    const player = new StreamingPcmPlayer(ctx, 0.1);
    player.push(new Uint8Array(pcm(new Array(441).fill(100))), 44100);
    expect(scheduled).toHaveLength(1);
    expect(scheduled[0].start).toBeCloseTo(10.1, 6);
    expect(scheduled[0].rate).toBe(44100);
    expect(scheduled[0].duration).toBeCloseTo(0.01, 6);
  });

  it('schedules each chunk exactly where the previous one ends: gapless', () => {
    const { ctx, scheduled } = fakeContext(0);
    const player = new StreamingPcmPlayer(ctx, 0.1);
    for (let i = 0; i < 3; i++) player.push(new Uint8Array(pcm(new Array(4410).fill(1))), 44100);
    expect(scheduled.map((s) => s.start)).toEqual([
      expect.closeTo(0.1, 6),
      expect.closeTo(0.2, 6),
      expect.closeTo(0.3, 6),
    ]);
  });

  it('re-anchors after an underrun instead of scheduling audio in the past', () => {
    const { ctx, scheduled } = fakeContext(0);
    const player = new StreamingPcmPlayer(ctx, 0.1);
    player.push(new Uint8Array(pcm(new Array(4410).fill(1))), 44100); // 0.1 .. 0.2
    ctx.currentTime = 5; // the stream stalled for seconds; the queue ran dry
    player.push(new Uint8Array(pcm(new Array(4410).fill(1))), 44100);
    expect(scheduled[1].start).toBeCloseTo(5.1, 6);
  });

  it('plays a sample split across two chunks as one sample', () => {
    const { ctx, scheduled } = fakeContext(0);
    const player = new StreamingPcmPlayer(ctx, 0);
    const bytes = pcm([1000, -1000]);
    player.push(new Uint8Array(bytes.slice(0, 3)), 44100); // one whole sample + half of the next
    player.push(new Uint8Array(bytes.slice(3)), 44100);
    expect(scheduled).toHaveLength(2);
    expect(Array.from(scheduled[0].samples)).toEqual([1000 / 32768]);
    expect(Array.from(scheduled[1].samples)).toEqual([-1000 / 32768]);
  });

  it('stop() silences everything scheduled and ignores later chunks', () => {
    const { ctx, scheduled, sources } = fakeContext(0);
    const player = new StreamingPcmPlayer(ctx, 0.1);
    player.push(new Uint8Array(pcm([1, 2, 3, 4])), 44100);
    player.push(new Uint8Array(pcm([1, 2, 3, 4])), 44100);
    player.stop();
    expect(sources.every((s) => s.stopped)).toBe(true);
    player.push(new Uint8Array(pcm([1, 2, 3, 4])), 44100);
    expect(scheduled).toHaveLength(2);
  });

  it('forgets a source once it has ended, so stop() does not touch a finished one', () => {
    const { ctx, sources } = fakeContext(0);
    const player = new StreamingPcmPlayer(ctx, 0.1);
    player.push(new Uint8Array(pcm([1, 2])), 44100);
    sources[0].ended?.();
    player.stop();
    expect(sources[0].stopped).toBe(false);
  });

  // The mute (#281): every chunk reaches the speakers through ONE persistent gain node, so a
  // mute is a gain of 0 on what is already queued AND on every chunk still to come -- local
  // playback only, nothing is stopped or dropped.
  describe('mute', () => {
    const tone = () => new Uint8Array(pcm([1, 2, 3, 4]));

    it('routes every chunk through one gain node that feeds the destination', () => {
      const { ctx, sources, gains } = fakeContext(0);
      const player = new StreamingPcmPlayer(ctx, 0.1);
      player.push(tone(), 44100);
      player.push(tone(), 44100);
      expect(gains).toHaveLength(1);
      expect(sources.map((s) => s.connected)).toEqual([[gains[0]], [gains[0]]]);
      expect(gains[0].connected).toEqual([ctx.destination]);
      expect(gains[0].gain.value).toBe(1);
    });

    it('setMuted(true) silences the queue and the chunks to come, and setMuted(false) restores it', () => {
      const { ctx, scheduled, sources, gains } = fakeContext(0);
      const player = new StreamingPcmPlayer(ctx, 0.1);
      player.push(tone(), 44100);
      player.setMuted(true);
      expect(gains[0].gain.value).toBe(0);
      // A chunk that arrives while muted joins the SAME gain: still silent.
      expect(player.push(tone(), 44100)).toBe(true);
      expect(gains).toHaveLength(1);
      expect(sources[1].connected).toEqual([gains[0]]);
      expect(gains[0].gain.value).toBe(0);
      player.setMuted(false);
      expect(gains[0].gain.value).toBe(1);
      // Reversible, and nothing was stopped, dropped or rescheduled on the way.
      expect(sources.every((s) => !s.stopped)).toBe(true);
      expect(scheduled).toHaveLength(2);
    });

    it('remembers a mute asked for before the first chunk', () => {
      const { ctx, gains } = fakeContext(0);
      const player = new StreamingPcmPlayer(ctx, 0.1);
      player.setMuted(true);
      player.push(tone(), 44100);
      expect(gains[0].gain.value).toBe(0);
    });
  });
});

describe('createSpeechPlayback', () => {
  function setup(withContext = true) {
    const fake = fakeContext(0);
    const playback = createSpeechPlayback(() => (withContext ? fake.ctx : null));
    return { playback, ...fake };
  }

  it('plays the chunks of a run as they arrive and reports the chat live', () => {
    const { playback, scheduled } = setup();
    expect(playback.isLive('c1')).toBe(false);
    expect(playback.push('c1', 'r1', chunk(0))).toBe(true);
    expect(playback.push('c1', 'r1', chunk(1))).toBe(true);
    expect(scheduled).toHaveLength(2);
    expect(playback.isLive('c1')).toBe(true);
    expect(playback.isLive('c2')).toBe(false);
  });

  it('stops pushing audio at a gap, without cutting what is already queued', () => {
    const { playback, scheduled, sources } = setup();
    playback.push('c1', 'r1', chunk(0));
    expect(playback.push('c1', 'r1', chunk(2))).toBe(false);
    expect(playback.isLive('c1')).toBe(false);
    expect(scheduled).toHaveLength(1);
    expect(sources[0].stopped).toBe(false);
  });

  it('starts a fresh session for a new run of the same chat', () => {
    const { playback, scheduled } = setup();
    playback.push('c1', 'r1', chunk(0));
    playback.push('c1', 'r1', chunk(2)); // r1 is abandoned
    expect(playback.push('c1', 'r2', chunk(0))).toBe(true); // r2 is its own stream
    expect(scheduled).toHaveLength(2);
  });

  it('lets a completed run play out, and silences one that did not complete', () => {
    const { playback, sources } = setup();
    playback.push('c1', 'r1', chunk(0));
    playback.end('c1', true);
    expect(playback.isLive('c1')).toBe(false);
    // The run is no longer "live" (it is done), but its queued tail is still playing out.
    expect(playback.isPlayingOut('c1')).toBe(true);
    expect(sources[0].stopped).toBe(false); // the tail of the audio is still playing

    playback.push('c2', 'r2', chunk(0));
    playback.end('c2', false); // a Stop or an error: no more sound
    expect(sources[1].stopped).toBe(true);
    expect(playback.isPlayingOut('c2')).toBe(false);
  });

  it('is silent, and never throws, where the browser has no audio context', () => {
    const { playback, scheduled } = setup(false);
    expect(playback.push('c1', 'r1', chunk(0))).toBe(false);
    expect(playback.isLive('c1')).toBe(false);
    expect(scheduled).toHaveLength(0);
    expect(() => {
      playback.prime();
      playback.end('c1', true);
      playback.stopAll();
    }).not.toThrow();
  });

  it('abandons a chunk whose audio is not base64 rather than playing noise', () => {
    const { playback, scheduled } = setup();
    expect(playback.push('c1', 'r1', chunk(0, '%%%'))).toBe(false);
    expect(scheduled).toHaveLength(0);
    expect(playback.push('c1', 'r1', chunk(1))).toBe(false); // and stays abandoned
  });

  it('stopAll silences every chat', () => {
    const { playback, sources } = setup();
    playback.push('c1', 'r1', chunk(0));
    playback.push('c2', 'r2', chunk(0));
    playback.stopAll();
    expect(sources.every((s) => s.stopped)).toBe(true);
    expect(playback.isLive('c1')).toBe(false);
  });

  // A tab that re-adopts a still-running speech run before its first chunk (a reload, a second
  // tab, during a model's cold start) has never been primed: its context is suspended, and a
  // chunk scheduled on a suspended context sits on a frozen clock, to be played -- stale, and
  // possibly over a newer run -- whenever something later resumes it. A run that cannot start
  // on a running context is not played live at all; its committed WAV is the reply.
  describe('on a context that is not running', () => {
    function suspendedSetup(state = 'suspended') {
      const fake = fakeContext(0);
      const resume = vi.fn(() => Promise.resolve());
      const ctx = Object.assign(fake.ctx, { state, resume });
      const playback = createSpeechPlayback(() => ctx);
      return { playback, ctx, resume, scheduled: fake.scheduled, sources: fake.sources };
    }

    it('does not schedule a run adopted on a suspended context, and a later resume plays nothing', () => {
      const { playback, ctx, resume, scheduled } = suspendedSetup();
      expect(playback.push('c1', 'r1', chunk(0))).toBe(false);
      expect(resume).toHaveBeenCalled(); // it still asks, so a primed page unlocks for the NEXT run
      expect(scheduled).toHaveLength(0);
      expect(playback.isLive('c1')).toBe(false);

      // The context comes up later (a gesture resumed it): the run stays abandoned, so nothing
      // that arrived before is played late and nothing from the middle of the stream starts.
      Object.assign(ctx, { state: 'running' });
      expect(playback.push('c1', 'r1', chunk(1))).toBe(false);
      expect(scheduled).toHaveLength(0);
    });

    it('treats an interrupted context (iOS) like a suspended one', () => {
      const { playback, scheduled } = suspendedSetup('interrupted');
      expect(playback.push('c1', 'r1', chunk(0))).toBe(false);
      expect(scheduled).toHaveLength(0);
    });

    it('starts the NEXT run of the chat normally once the context is running', () => {
      const { playback, ctx, scheduled } = suspendedSetup();
      playback.push('c1', 'r1', chunk(0));
      Object.assign(ctx, { state: 'running' });
      expect(playback.push('c1', 'r2', chunk(0))).toBe(true);
      expect(scheduled).toHaveLength(1);
    });

    it('plays on a context that says it is running', () => {
      const { playback, scheduled } = suspendedSetup('running');
      expect(playback.push('c1', 'r1', chunk(0))).toBe(true);
      expect(scheduled).toHaveLength(1);
    });

    it('abandons a live run whose context is suspended mid-run and silences its queue', () => {
      const { playback, ctx, sources, scheduled } = suspendedSetup('running');
      expect(playback.push('c1', 'r1', chunk(0))).toBe(true);
      Object.assign(ctx, { state: 'suspended' });
      expect(playback.push('c1', 'r1', chunk(1))).toBe(false);
      expect(playback.isLive('c1')).toBe(false);
      expect(scheduled).toHaveLength(1);
      // What was queued on the frozen clock must not play stale when the context resumes.
      expect(sources[0].stopped).toBe(true);
    });

    it('silences the queue of a COMPLETED run whose context is not running', () => {
      const { playback, ctx, sources } = suspendedSetup('running');
      playback.push('c1', 'r1', chunk(0));
      Object.assign(ctx, { state: 'suspended' });
      playback.end('c1', true);
      expect(sources[0].stopped).toBe(true);
    });

    it('still lets a completed run play out on a running context', () => {
      const { playback, sources } = suspendedSetup('running');
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true);
      expect(sources[0].stopped).toBe(false);
    });
  });

  // The live mute of a chat's run (#281): reversible, local to playback, and nothing the run
  // itself notices -- it stays live, keeps queueing, and finishes as ever.
  describe('setMuted', () => {
    it('toggles the run gain to 0 and back while the run stays live', () => {
      const { playback, gains, sources, scheduled } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.setMuted('c1', true);
      expect(gains[0].gain.value).toBe(0);
      // Silenced, not stopped: the run is live and keeps queueing audio (muted).
      expect(playback.isLive('c1')).toBe(true);
      expect(playback.push('c1', 'r1', chunk(1))).toBe(true);
      expect(scheduled).toHaveLength(2);
      expect(sources.every((s) => !s.stopped)).toBe(true);
      expect(gains[0].gain.value).toBe(0);

      playback.setMuted('c1', false);
      expect(gains[0].gain.value).toBe(1);
      expect(playback.isLive('c1')).toBe(true);
    });

    it('mutes only the chat it names', () => {
      const { playback, gains } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.push('c2', 'r2', chunk(0));
      playback.setMuted('c1', true);
      expect(gains.map((g) => g.gain.value)).toEqual([0, 1]);
    });

    it('keeps a completed run muted while its queue plays out', () => {
      const { playback, gains, sources } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.setMuted('c1', true);
      playback.end('c1', true);
      expect(sources[0].stopped).toBe(false);
      expect(gains[0].gain.value).toBe(0);
    });

    it('does not carry over to the next run of the chat', () => {
      const { playback, gains } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.setMuted('c1', true);
      playback.end('c1', true);
      playback.push('c1', 'r2', chunk(0));
      expect(gains).toHaveLength(2);
      expect(gains[1].gain.value).toBe(1);
    });

    it('is a no-op, and never throws, for a chat with no run or no audio context', () => {
      const { playback } = setup(false);
      expect(() => playback.setMuted('nope', true)).not.toThrow();
      const live = setup();
      expect(() => live.playback.setMuted('nope', true)).not.toThrow();
      expect(live.gains).toHaveLength(0);
    });
  });

  // The draining tail (#281): a completed run's queued audio keeps playing after the run commits
  // and its saved player appears. The session is KEPT alive -- reachable by Mute and a tail-stop
  // -- in a draining state until the last scheduled source ends, and only then is the drain
  // signalled (onDrained) and the session dropped. This is what keeps the live controls on screen
  // until the audio has actually finished, instead of vanishing the instant the run commits.
  describe('the draining tail of a completed run', () => {
    it('stays "playing out" and reachable until the tail ends, then signals the drain', () => {
      const { playback, sources, gains } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.push('c1', 'r1', chunk(1));

      playback.end('c1', true);
      // No longer "live" (the run is done), but the tail is still playing out.
      expect(playback.isLive('c1')).toBe(false);
      expect(playback.isPlayingOut('c1')).toBe(true);
      // The session is still reachable: Mute still silences the draining tail.
      playback.setMuted('c1', true);
      expect(gains[0].gain.value).toBe(0);
      expect(drained).toEqual([]);
      expect(sources.some((s) => s.stopped)).toBe(false);

      // Each scheduled source ends in turn; the last one empties the queue.
      sources.forEach((s) => s.ended?.());
      expect(playback.isPlayingOut('c1')).toBe(false);
      expect(drained).toEqual(['c1']);
    });

    it('signals the drain at once when the completed run has nothing left queued', () => {
      const { playback, sources } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      sources[0].ended?.(); // the only chunk finished before the run ended
      playback.end('c1', true);
      expect(drained).toEqual(['c1']);
      expect(playback.isPlayingOut('c1')).toBe(false);
    });

    it('abort() stops the draining tail at once and signals the drain', () => {
      const { playback, sources } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true);
      expect(playback.isPlayingOut('c1')).toBe(true);

      playback.abort('c1');
      expect(sources[0].stopped).toBe(true);
      expect(playback.isPlayingOut('c1')).toBe(false);
      expect(drained).toEqual(['c1']);
    });

    it("a new run's push for the same chat stops the prior draining tail, with no overlap", () => {
      const { playback, sources, scheduled } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true); // r1 is now draining
      expect(sources[0].stopped).toBe(false);

      // A new run of the same chat begins while the old tail is still draining.
      expect(playback.push('c1', 'r2', chunk(0))).toBe(true);
      expect(sources[0].stopped).toBe(true); // the old tail was cut -- no overlap
      expect(scheduled).toHaveLength(2); // r2 scheduled its own first chunk
      expect(playback.isLive('c1')).toBe(true);
    });

    it('clears the tail via the fallback timer when onended never fires', () => {
      vi.useFakeTimers();
      try {
        const { playback, sources } = setup();
        const drained: string[] = [];
        playback.onDrained((chatId) => drained.push(chatId));
        playback.push('c1', 'r1', chunk(0));
        playback.end('c1', true);
        expect(drained).toEqual([]);
        // onended never fires (a context suspended mid-tail, or a browser quirk): the fallback
        // elapses so the controls can never hang on screen forever.
        vi.advanceTimersByTime(60_000);
        expect(drained).toEqual(['c1']);
        expect(sources[0].stopped).toBe(true);
        expect(playback.isPlayingOut('c1')).toBe(false);
      } finally {
        vi.useRealTimers();
      }
    });

    it('resolves the drain immediately for a completed run whose context is not running', () => {
      const fake = fakeContext(0);
      const ctx = Object.assign(fake.ctx, {
        state: 'running',
        resume: vi.fn(() => Promise.resolve()),
      });
      const playback = createSpeechPlayback(() => ctx);
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      Object.assign(ctx, { state: 'suspended' }); // the clock froze before the run ended
      playback.end('c1', true);
      expect(drained).toEqual(['c1']);
      expect(fake.sources[0].stopped).toBe(true);
      expect(playback.isPlayingOut('c1')).toBe(false);
    });
  });

  it('prime() resumes a suspended context from inside the user gesture', () => {
    const resume = vi.fn(() => Promise.resolve());
    const fake = fakeContext(0);
    const ctx = Object.assign(fake.ctx, { state: 'suspended', resume });
    const playback = createSpeechPlayback(() => ctx);
    playback.prime();
    expect(resume).toHaveBeenCalledTimes(1);
  });
});
