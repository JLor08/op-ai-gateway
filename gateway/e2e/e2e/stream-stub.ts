// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// What the audio.cpp stand-ins of the two streaming-transcription suites
// (e2e-transcription and e2e-chat-transcription) share: the SSE frame they write
// and the holds that pause a streamed answer. A test arms holds, the stub's stream
// pauses at each in turn, and the test looks at what the client has so far while the
// upstream is provably still mid-stream -- which is what tells a relayed stream from
// a buffered one.

/**
 * A held stream is released by itself after this long, so a failing assertion can never
 * leave the stub (and whatever relays it: the gateway, the portal's run) waiting for a
 * release that will not come.
 */
export const HOLD_SAFETY_MS = 15000;

/**
 * One pause of a streamed answer. `reached` settles once the stub has written the frame
 * the pause follows and is waiting; `release()` lets the stream go on.
 */
export type Hold = { reached: Promise<void>; release: () => void };

export type StreamHolds = {
  /**
   * Makes the next `count` pauses the stub reaches, in turn, real ones: one hold per
   * pause. A hold nobody releases lets go by itself after HOLD_SAFETY_MS.
   */
  arm(count: number): Hold[];
  /** Called by the stub right after a frame: waits at the next armed hold, if there is one. */
  pause(): Promise<void>;
  /** Whether a streamed answer is paused at a hold right now. */
  held(): boolean;
};

export function createStreamHolds(): StreamHolds {
  const pending: { markReached: () => void; released: Promise<void> }[] = [];
  let held = 0;
  return {
    arm: (count) =>
      Array.from({ length: count }, () => {
        let markReached!: () => void;
        let release!: () => void;
        const reached = new Promise<void>((resolve) => {
          markReached = resolve;
        });
        const released = new Promise<void>((resolve) => {
          release = resolve;
        });
        pending.push({ markReached, released });
        return { reached, release };
      }),
    pause: async () => {
      const hold = pending.shift();
      if (!hold) {
        return;
      }
      held += 1;
      hold.markReached();
      let safety: NodeJS.Timeout | undefined;
      await Promise.race([
        hold.released,
        new Promise<void>((resolve) => {
          safety = setTimeout(resolve, HOLD_SAFETY_MS);
        })
      ]);
      clearTimeout(safety);
      held -= 1;
    },
    held: () => held > 0
  };
}

/** One SSE `data:` frame, exactly as the stub writes it and the gateway relays it. */
export function sseFrame(payload: unknown): string {
  return `data: ${typeof payload === "string" ? payload : JSON.stringify(payload)}\n\n`;
}
