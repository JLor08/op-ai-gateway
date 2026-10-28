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

  // Pause / resume (#315): the timeline of THIS player is halted -- its scheduled sources are
  // stopped and what has not yet been heard is held -- and resumed by scheduling the rest
  // anew on the live clock. The page's AudioContext is shared by every chat and every other
  // player, so it is never suspended; nothing here may touch anything but this player's nodes.
  describe('pause and resume', () => {
    const RATE = 44100;
    // 0.1 s of PCM whose every sample is `value`, so a chunk can be told apart by its content.
    const tenth = (value: number) => new Uint8Array(pcm(new Array(4410).fill(value)));
    // What a scheduled entry holds, as one comparable value: [rate, sample count, first value].
    const shape = (s: { samples: Float32Array; rate: number }) => [
      s.rate,
      s.samples.length,
      Math.round(s.samples[0] * 32768),
    ];

    // Three 0.1 s chunks valued 1, 2, 3, scheduled at 0.1-0.2, 0.2-0.3 and 0.3-0.4.
    function threeChunks(lead = 0.1) {
      const fake = fakeContext(0);
      const player = new StreamingPcmPlayer(fake.ctx, lead);
      for (const v of [1, 2, 3]) player.push(tenth(v), RATE);
      return { ...fake, player };
    }

    it('halts the playout: every scheduled source is stopped and nothing new is scheduled', () => {
      const { ctx, scheduled, sources, player } = threeChunks();
      ctx.currentTime = 0.25;
      player.pause();
      expect(sources.every((s) => s.stopped)).toBe(true);
      ctx.currentTime = 9;
      expect(scheduled).toHaveLength(3);
    });

    it('fires no onEmpty: a pause is not a drain, not even from a halted source ending late', () => {
      const { ctx, sources, player } = threeChunks();
      const onEmpty = vi.fn();
      player.onEmpty = onEmpty;
      // The browser dispatches `ended` for a source asked to stop; keep each source's handler
      // as the browser would have it, and deliver it after the pause.
      const lateEnded = sources.map((s) => s.ended);
      ctx.currentTime = 0.25;
      player.pause();
      for (const ended of lateEnded) ended?.();
      expect(onEmpty).not.toHaveBeenCalled();
    });

    it('resumes from where it paused: the finished chunk is not replayed, the playing one continues', () => {
      const { ctx, scheduled, player } = threeChunks();
      ctx.currentTime = 0.25; // chunk 1 (0.2-0.3) is 0.05 s in; chunk 0 is over; chunk 2 unstarted
      player.pause();
      ctx.currentTime = 4; // time passes while paused: the resumed audio is placed on the LIVE clock
      player.resume();
      // 3 original + the remainder of chunk 1 (2205 samples left) + all of chunk 2.
      expect(scheduled).toHaveLength(5);
      expect(scheduled.slice(3).map(shape)).toEqual([
        [RATE, 2205, 2],
        [RATE, 4410, 3],
      ]);
      expect(scheduled[3].start).toBeCloseTo(4.1, 6); // the lead ahead of "now", as after a stall
      expect(scheduled[4].start).toBeCloseTo(4.15, 6); // gapless behind the remainder
      // The remainder is the TAIL of the chunk, not its head.
      expect(scheduled[3].samples.every((s) => s === 2 / 32768)).toBe(true);
    });

    it('re-anchors on resume even straight away: not behind the old timeline it halted', () => {
      const { ctx, scheduled, player } = threeChunks(); // the old timeline runs to 0.4
      ctx.currentTime = 0.25;
      player.pause();
      player.resume(); // no time has passed: "now" is still inside the halted timeline
      expect(scheduled[3].start).toBeCloseTo(0.35, 6); // now + lead, not 0.4 (a gap of silence)
      expect(scheduled[4].start).toBeCloseTo(0.4, 6);
    });

    it('holds a chunk that had not started yet whole, and one that has just ended not at all', () => {
      const { ctx, scheduled, player } = threeChunks();
      ctx.currentTime = 0.05; // nothing has started (the first chunk starts at 0.1)
      player.pause();
      ctx.currentTime = 1;
      player.resume();
      expect(scheduled.slice(3).map(shape)).toEqual([
        [RATE, 4410, 1],
        [RATE, 4410, 2],
        [RATE, 4410, 3],
      ]);

      const second = threeChunks();
      second.ctx.currentTime = 0.45; // every chunk is over, though no `ended` was delivered yet
      second.player.pause();
      second.ctx.currentTime = 1;
      second.player.resume();
      expect(second.scheduled).toHaveLength(3); // nothing replayed
    });

    it('keeps the order and drops or duplicates nothing across repeated pauses', () => {
      const { ctx, scheduled, player } = threeChunks();
      ctx.currentTime = 0.25;
      player.pause();
      ctx.currentTime = 1;
      player.resume(); // remainder of chunk 1 at 1.1-1.15, chunk 2 at 1.15-1.25
      ctx.currentTime = 1.12; // 0.02 s (882 samples) into the remainder
      player.pause();
      ctx.currentTime = 2;
      player.resume();
      expect(scheduled.slice(5).map(shape)).toEqual([
        [RATE, 1323, 2], // 2205 - 882: the rest of chunk 1
        [RATE, 4410, 3], // chunk 2, whole
      ]);
      expect(scheduled.slice(5).map((s) => s.start)).toEqual([
        expect.closeTo(2.1, 6),
        expect.closeTo(2.1 + 1323 / RATE, 6),
      ]);
    });

    it('holds what is pushed while paused, in order, and schedules none of it until resume', () => {
      const { ctx, scheduled, player } = threeChunks();
      ctx.currentTime = 0.25;
      player.pause();
      expect(player.push(tenth(4), RATE)).toBe(true);
      expect(player.push(tenth(5), RATE)).toBe(true);
      expect(scheduled).toHaveLength(3);
      ctx.currentTime = 2;
      player.resume();
      expect(scheduled.slice(3).map(shape)).toEqual([
        [RATE, 2205, 2],
        [RATE, 4410, 3],
        [RATE, 4410, 4],
        [RATE, 4410, 5],
      ]);
      // ...and a chunk pushed AFTER the resume joins the end of the same gapless timeline.
      player.push(tenth(6), RATE);
      expect(scheduled[7].start).toBeCloseTo(scheduled[6].start + 0.1, 6);
    });

    it('still refuses a chunk the browser cannot make a buffer of while paused', () => {
      const { ctx, player } = threeChunks();
      player.pause();
      vi.spyOn(ctx, 'createBuffer').mockImplementationOnce(() => {
        throw new Error('unsupported rate');
      });
      expect(player.push(tenth(4), RATE)).toBe(false);
    });

    it('counts what is held as pending and as audio still to play while paused', () => {
      const { ctx, player } = threeChunks();
      ctx.currentTime = 0.25;
      player.pause();
      expect(player.pending).toBe(2); // the rest of chunk 1 and chunk 2
      expect(player.remainingSeconds()).toBeCloseTo(0.15, 6);
      player.push(tenth(4), RATE);
      expect(player.pending).toBe(3);
      expect(player.remainingSeconds()).toBeCloseTo(0.25, 6);
      ctx.currentTime = 100; // the clock running on does not drain a held queue
      expect(player.remainingSeconds()).toBeCloseTo(0.25, 6);
      player.resume();
      expect(player.pending).toBe(3);
      expect(player.remainingSeconds()).toBeCloseTo(0.1 + 0.25, 6); // lead + the held audio
    });

    it('is idempotent: a second pause, or a resume that is not paused, does nothing', () => {
      const { ctx, scheduled, sources, player } = threeChunks();
      player.resume(); // not paused
      expect(scheduled).toHaveLength(3);
      expect(sources.some((s) => s.stopped)).toBe(false);
      ctx.currentTime = 0.25;
      player.pause();
      ctx.currentTime = 3;
      player.pause(); // already paused: must not re-measure the offset from the later "now"
      player.resume();
      player.resume(); // a second resume must not schedule the remainder twice
      expect(scheduled.slice(3).map(shape)).toEqual([
        [RATE, 2205, 2],
        [RATE, 4410, 3],
      ]);
    });

    it('pause then resume on a player that has nothing is a safe no-op: no nodes, no onEmpty', () => {
      const { ctx, sources, gains } = fakeContext(0);
      const player = new StreamingPcmPlayer(ctx, 0.1);
      const onEmpty = vi.fn();
      player.onEmpty = onEmpty;
      player.pause();
      player.resume();
      expect(sources).toHaveLength(0);
      expect(gains).toHaveLength(0);
      expect(player.pending).toBe(0);
      expect(onEmpty).not.toHaveBeenCalled();
    });

    it('a pause made before the first chunk holds that chunk until resume', () => {
      const fake = fakeContext(0);
      const { ctx, scheduled } = fake;
      const player = new StreamingPcmPlayer(ctx, 0.1);
      const onEmpty = vi.fn();
      player.onEmpty = onEmpty;
      player.pause(); // nothing scheduled yet
      player.push(tenth(1), RATE);
      expect(scheduled).toHaveLength(0);
      expect(player.pending).toBe(1);
      ctx.currentTime = 1;
      player.resume();
      expect(scheduled.map(shape)).toEqual([[RATE, 4410, 1]]);
      expect(scheduled[0].start).toBeCloseTo(1.1, 6);
      expect(onEmpty).not.toHaveBeenCalled();
    });

    it('resumed with nothing held: no sources, no onEmpty', () => {
      const { ctx, scheduled, sources, player } = threeChunks();
      ctx.currentTime = 0.45; // all played
      player.pause();
      const onEmpty = vi.fn();
      player.onEmpty = onEmpty;
      player.resume();
      expect(scheduled).toHaveLength(3);
      expect(sources).toHaveLength(3);
      expect(player.pending).toBe(0);
      expect(onEmpty).not.toHaveBeenCalled();
    });

    it('drains after a resume and fires onEmpty once, at the true end', () => {
      const { ctx, sources, player } = threeChunks();
      const onEmpty = vi.fn();
      player.onEmpty = onEmpty;
      ctx.currentTime = 0.25;
      player.pause();
      ctx.currentTime = 1;
      player.resume();
      expect(sources).toHaveLength(5);
      sources[3].ended?.(); // the remainder of chunk 1
      expect(onEmpty).not.toHaveBeenCalled();
      sources[4].ended?.(); // chunk 2: the last one
      expect(onEmpty).toHaveBeenCalledTimes(1);
      expect(player.pending).toBe(0);
    });

    it('ignores a halted source that reports ended after the resume, keeping the new ones', () => {
      const { ctx, sources, player } = threeChunks();
      const onEmpty = vi.fn();
      player.onEmpty = onEmpty;
      const staleEnded = sources.map((s) => s.ended);
      ctx.currentTime = 0.25;
      player.pause();
      ctx.currentTime = 1;
      player.resume();
      for (const ended of staleEnded) ended?.();
      expect(onEmpty).not.toHaveBeenCalled();
      expect(player.pending).toBe(2); // the two resumed sources are still tracked
    });

    it('leaves another player on the same context, and the context itself, alone', () => {
      const fake = fakeContext(0);
      const suspend = vi.fn();
      Object.assign(fake.ctx, { suspend });
      const mine = new StreamingPcmPlayer(fake.ctx, 0.1);
      const other = new StreamingPcmPlayer(fake.ctx, 0.1);
      mine.push(tenth(1), RATE); // sources[0]
      other.push(tenth(7), RATE); // sources[1]
      other.push(tenth(8), RATE); // sources[2]
      const otherEmpty = vi.fn();
      other.onEmpty = otherEmpty;
      fake.ctx.currentTime = 0.15;
      mine.pause();
      expect(fake.sources[0].stopped).toBe(true);
      expect(fake.sources[1].stopped).toBe(false);
      expect(fake.sources[2].stopped).toBe(false);
      expect(suspend).not.toHaveBeenCalled();
      expect(other.pending).toBe(2);
      // The other player still plays out and drains on its own.
      fake.sources[1].ended?.();
      fake.sources[2].ended?.();
      expect(otherEmpty).toHaveBeenCalledTimes(1);
      // ...and resuming mine neither touches it nor needs the context to be resumed.
      mine.resume();
      expect(fake.sources[1].stopped).toBe(false);
      expect(suspend).not.toHaveBeenCalled();
    });

    it('keeps the mute through a pause: the same gain node, still at the mute', () => {
      const { ctx, sources, gains, player } = threeChunks();
      player.setMuted(true);
      ctx.currentTime = 0.25;
      player.pause();
      expect(gains[0].gain.value).toBe(0); // pausing is not an unmute
      ctx.currentTime = 1;
      player.resume();
      expect(gains).toHaveLength(1);
      expect(sources[3].connected).toEqual([gains[0]]);
      expect(gains[0].gain.value).toBe(0);
    });

    it('stop() while paused discards what was held, and a resume afterwards plays nothing', () => {
      const { ctx, scheduled, player } = threeChunks();
      ctx.currentTime = 0.25;
      player.pause();
      player.stop();
      expect(player.pending).toBe(0);
      player.resume();
      expect(player.push(tenth(4), RATE)).toBe(false);
      expect(scheduled).toHaveLength(3);
    });

    it('pause after stop() is a no-op', () => {
      const { scheduled, player } = threeChunks();
      player.stop();
      player.pause();
      player.resume();
      expect(scheduled).toHaveLength(3);
      expect(player.pending).toBe(0);
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
    expect(playback.isLive('c1')).toBe(false); // the run is done...
    expect(sources[0].stopped).toBe(false); // ...but the tail of the audio is still playing

    playback.push('c2', 'r2', chunk(0));
    playback.end('c2', false); // a Stop or an error: no more sound
    expect(sources[1].stopped).toBe(true);
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
    it('stays reachable until the tail ends, then signals the drain', () => {
      const { playback, sources, gains } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.push('c1', 'r1', chunk(1));

      playback.end('c1', true);
      // No longer "live" (the run is done), but the tail is still playing out: nothing stopped,
      // the drain has not fired, and the session is still reachable (Mute still silences it).
      expect(playback.isLive('c1')).toBe(false);
      expect(sources.some((s) => s.stopped)).toBe(false);
      expect(drained).toEqual([]);
      playback.setMuted('c1', true);
      expect(gains[0].gain.value).toBe(0);

      // Each scheduled source ends in turn; the last one empties the queue and drains the chat.
      sources.forEach((s) => s.ended?.());
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
    });

    it('abort() stops the draining tail at once and signals the drain', () => {
      const { playback, sources } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true);
      expect(sources[0].stopped).toBe(false); // still playing out

      playback.abort('c1');
      expect(sources[0].stopped).toBe(true);
      expect(drained).toEqual(['c1']);
    });

    // Finding 1: a chat-delete / a Stop of a following run routes forget -> end(chatId, false),
    // which must still cut the tail even though the run already ended (the session is draining).
    it('silences the draining tail on a non-completed end (chat-delete / cancel)', () => {
      const { playback, sources } = setup();
      const drained: string[] = [];
      playback.onDrained((chatId) => drained.push(chatId));
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true); // r1 draining its tail
      expect(sources[0].stopped).toBe(false);

      playback.end('c1', false); // the chat is deleted (forget) / a following run is cancelled
      expect(sources[0].stopped).toBe(true); // the tail is cut, not left audible
      expect(drained).toEqual(['c1']);
    });

    it('stopAll silences a chat that is draining its tail', () => {
      const { playback, sources } = setup();
      playback.push('c1', 'r1', chunk(0));
      playback.end('c1', true); // draining
      playback.stopAll();
      expect(sources[0].stopped).toBe(true);
      expect(playback.isLive('c1')).toBe(false);
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

    // Finding 3: a new run replacing a prior session (live OR draining) must signal the old run's
    // drain, so its render mirror (runAudioLive, and with it the mute mark) clears before the new
    // run goes live again -- otherwise a mute left on the old tail carries over to the new run.
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
