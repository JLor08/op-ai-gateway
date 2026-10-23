// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

// bytesPerSample returns the size in bytes of one sample of a live PCM format.
// Only the two formats audio.cpp accepts for live transcription are known.
func bytesPerSample(format string) (int, bool) {
	switch format {
	case "s16le":
		return 2, true
	case "f32le":
		return 4, true
	}
	return 0, false
}

// liveByteRate returns the PCM bytes per second of a live session, the divisor that
// turns a byte count into audio seconds for billing. The sample rate and channel
// ranges are audio.cpp's own; ok is false when any param falls outside them.
func liveByteRate(sampleRate, channels int, format string) (int, bool) {
	size, ok := bytesPerSample(format)
	if !ok {
		return 0, false
	}
	if sampleRate < 1000 || sampleRate > 384000 {
		return 0, false
	}
	if channels < 1 || channels > 16 {
		return 0, false
	}
	rate := sampleRate * channels * size
	if rate <= 0 {
		return 0, false
	}
	return rate, true
}
