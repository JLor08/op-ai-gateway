// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "encoding/binary"

const (
	wavRIFFHeaderLen  = 12 // "RIFF" + uint32 size + "WAVE"
	wavChunkHeaderLen = 8  // 4-byte chunk id + uint32 little-endian size
	wavFmtMinLen      = 16 // the fixed PCM part of the `fmt ` chunk
)

// Offsets of the fixed PCM fields within the body of a `fmt ` chunk (the 2-byte
// audio format tag at offset 0 is not read: the consistency check below is what
// separates a PCM header from one that is not).
const (
	wavChannelsOffset   = 2
	wavSampleRateOffset = 4
	wavByteRateOffset   = 8
	wavBlockAlignOffset = 12
	wavBitsOffset       = 14
)

// wavDurationMillis returns the audio duration of a PCM WAV file in milliseconds,
// computed from the container header alone (data chunk bytes / byte rate), and
// true. It returns (0, false) when b is not a structurally sound RIFF/WAVE file:
// a missing or truncated chunk, a chunk that declares more bytes than b holds, a
// `fmt ` chunk after `data` or too short to carry a byte rate, a zero byte rate,
// or a `fmt ` chunk whose fields contradict each other (see wavConsistentByteRate).
// A streamed transcription response carries no audio_duration_ms, so the billable
// duration is derived from the uploaded WAV instead.
//
// (0, true) is a real answer: a clip shorter than a millisecond, or an empty `data`
// chunk, measures zero. Only ok says whether the header could be believed.
func wavDurationMillis(b []byte) (int64, bool) {
	if len(b) < wavRIFFHeaderLen || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, false
	}

	var byteRate uint32
	for pos := wavRIFFHeaderLen; pos+wavChunkHeaderLen <= len(b); {
		id := string(b[pos : pos+4])
		size := binary.LittleEndian.Uint32(b[pos+4 : pos+wavChunkHeaderLen])
		body := pos + wavChunkHeaderLen
		if uint64(size) > uint64(len(b)-body) {
			return 0, false
		}

		switch id {
		case "fmt ":
			if size < wavFmtMinLen {
				return 0, false
			}
			rate, ok := wavConsistentByteRate(b[body : body+wavFmtMinLen])
			if !ok {
				return 0, false
			}
			byteRate = rate
		case "data":
			if byteRate == 0 {
				return 0, false
			}
			// size < 2^32 and 1000 < 2^10, so the product fits easily in a uint64.
			return int64(uint64(size) * 1000 / uint64(byteRate)), true
		}

		// Chunks are word-aligned: an odd-length body is followed by one pad byte.
		pos = body + int(size) + int(size&1)
	}
	return 0, false
}

// wavConsistentByteRate returns the byte rate of a `fmt ` chunk (f is its first
// wavFmtMinLen bytes) and true only when the chunk agrees with itself: a PCM header
// is fully determined by its channel count, sample rate and sample size, so
//
//	blockAlign = channels * ceil(bitsPerSample / 8)
//	byteRate   = sampleRate * blockAlign
//
// and every field must be non-zero.
//
// The byte rate is the divisor of the duration and the header is written by the
// CLIENT, which pays for the audio by the second: a forged, inflated byte rate would
// bill a long clip as a short one. Cross-checking the fields it must follow from,
// and the block align the byte rate could be inflated THROUGH, leaves no field that
// can be moved on its own. What it cannot catch is a header forged consistently
// throughout (a sample rate claimed 100x too high, with the byte rate to match): that
// header is internally sound, and the gateway has no second source for the duration.
// A format whose byte rate is only an average -- ADPCM, MP3 in a WAV container --
// fails the check too, and is treated as not measurable.
func wavConsistentByteRate(f []byte) (uint32, bool) {
	channels := uint32(binary.LittleEndian.Uint16(f[wavChannelsOffset:]))
	sampleRate := uint64(binary.LittleEndian.Uint32(f[wavSampleRateOffset:]))
	byteRate := uint64(binary.LittleEndian.Uint32(f[wavByteRateOffset:]))
	blockAlign := uint64(binary.LittleEndian.Uint16(f[wavBlockAlignOffset:]))
	bits := uint32(binary.LittleEndian.Uint16(f[wavBitsOffset:]))
	if channels == 0 || sampleRate == 0 || byteRate == 0 || bits == 0 {
		return 0, false
	}
	if blockAlign != uint64(channels)*uint64((bits+7)/8) || byteRate != sampleRate*blockAlign {
		return 0, false
	}
	return uint32(byteRate), true
}
