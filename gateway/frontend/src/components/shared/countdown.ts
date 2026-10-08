// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Time remaining until a future instant, as a compact string whose unit
// abbreviations (d, h, min) read the same in German and English, so it needs no
// translation: "< 1 min", "42 min", "2 h 14 min", "3 d 4 h". The two most
// significant units only -- a reset a day away does not need its minutes -- and
// a zero lower unit is dropped ("2 h", not "2 h 0 min"). Remaining time is
// rounded DOWN to the whole minute; a negative or zero span reads as "< 1 min"
// (callers decide what a reset that has already passed means).
export function formatCountdown(ms: number): string {
  const totalMinutes = Math.floor(Math.max(0, ms) / 60_000);
  if (totalMinutes < 1) return '< 1 min';
  if (totalMinutes < 60) return `${totalMinutes} min`;
  const totalHours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (totalHours < 24) return minutes === 0 ? `${totalHours} h` : `${totalHours} h ${minutes} min`;
  const days = Math.floor(totalHours / 24);
  const hours = totalHours % 24;
  return hours === 0 ? `${days} d` : `${days} d ${hours} h`;
}
