// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "encoding/binary"

const (
	wavRIFFHeaderLen  = 12 // "RIFF" + uint32 size + "WAVE"
	wavChunkHeaderLen = 8  // 4-byte chunk id + uint32 little-endian size
	wavFmtMinLen      = 16 // the fixed PCM part of the `fmt ` chunk
	wavByteRateOffset = 8  // byte-rate field within the `fmt ` chunk body
)

// wavDurationMillis returns the audio duration of a PCM WAV file in milliseconds,
// computed from the container header alone (data chunk bytes / byte rate), and
// true. It returns (0, false) when b is not a structurally sound RIFF/WAVE file:
// a missing or truncated chunk, a chunk that declares more bytes than b holds, a
// `fmt ` chunk after `data` or too short to carry a byte rate, or a zero byte
// rate. A streamed transcription response carries no audio_duration_ms, so the
// billable duration is derived from the uploaded WAV instead.
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
			byteRate = binary.LittleEndian.Uint32(b[body+wavByteRateOffset : body+wavByteRateOffset+4])
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
