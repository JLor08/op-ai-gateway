// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// How an audio figure is written (#340), in the one place both the cells
// (audioMetrics.ts) and the sentences (i18n.ts) take it from. It imports
// nothing, so the messages can use it without pulling in the components.

/** Why an audio concurrency ramp stopped: the closed set the gateway writes. */
export const AUDIO_CONCURRENCY_LIMITS = ['performance', 'resources', 'cap'] as const;
export type AudioConcurrencyLimit = (typeof AUDIO_CONCURRENCY_LIMITS)[number];

export function isAudioConcurrencyLimit(value: string | undefined): value is AudioConcurrencyLimit {
  return (AUDIO_CONCURRENCY_LIMITS as readonly (string | undefined)[]).includes(value);
}

/**
 * A limit from the wire as the portal reads it: one of the three, else ''
 * (unknown) -- none, or one this build does not know, which is never taken
 * for a lower bound.
 */
export function audioLimitOf(value: string | undefined): AudioConcurrencyLimit | '' {
  return isAudioConcurrencyLimit(value) ? value : '';
}

/**
 * A measured rate as a multiple of real time. It is cut, never rounded up:
 * the voice gate refuses a rate below a factor (1.2 by default), and a figure
 * shown above the measured one would look like it passes a gate that refuses
 * it -- 1.199 reads 1.19×, never 1.2×. Below 10× it keeps the second decimal
 * where it says something (1.18×, 1.2×, 3.0×); at 10× and above one decimal.
 */
export function formatRateX(rate: number): string {
  const digits = rate < 10 ? 2 : 1;
  const scale = 10 ** digits;
  // The allowance absorbs the binary representation: 1.16 * 100 is 115.999…
  const cut = Math.floor(rate * scale + 1e-9) / scale;
  const text = cut.toFixed(digits);
  return `${digits === 2 && text.endsWith('0') ? text.slice(0, -1) : text}×`;
}
