// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/binary"
	"io"
)

const (
	wavRIFFHeaderLen  = 12 // "RIFF" + uint32 size + "WAVE"
	wavChunkHeaderLen = 8  // 4-byte chunk id + uint32 little-endian size
	wavFmtMinLen      = 16 // the fixed PCM part of the `fmt ` chunk

	// wavTrailingSlackBytes is how many bytes may follow the audio in a `data` chunk's
	// own extent without being accounted for by its declared size: the trailing
	// metadata chunks of a genuine file (an ffmpeg LIST/INFO is ~100 bytes, a broadcast
	// WAV's bext/iXML a few KiB). Anything past it is not metadata but audio the
	// declaration hid.
	wavTrailingSlackBytes = 16 << 10
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

// wavDurationFromReader returns the audio duration of a PCM WAV stream in
// milliseconds, computed from the container header alone (data chunk bytes / byte
// rate), and true. It returns (0, false) when the stream is not a structurally sound
// RIFF/WAVE file: a missing or truncated chunk, a chunk that declares more bytes than
// the stream holds, a `fmt ` chunk after `data` or too short to carry a byte rate, a
// zero byte rate, a `fmt ` chunk whose fields contradict each other (see
// wavConsistentByteRate), or a `data` chunk that declares less than the stream holds
// after it (see below). A streamed transcription response carries no
// audio_duration_ms, so the billable duration is derived from the uploaded WAV
// instead.
//
// (0, true) is a real answer: a clip shorter than a millisecond, or an empty `data`
// chunk with nothing after it, measures zero. Only ok says whether the header could
// be believed.
//
// The input is a stream so that an upload held in a multipart body is measured where
// it lies, without a second copy of it: only the chunk headers and the `fmt ` chunk are read
// for their content, and every other byte -- a foreign chunk of any size, the audio
// itself -- is passed over and counted. The tests measure an in-memory file through
// the wavDurationMillis helper of wavduration_test.go.
//
// The count is what makes the declared `data` size believable. That size is written
// by the CLIENT, which pays by the second: a header that declares an empty (or tiny)
// `data` chunk and puts the real audio after it would measure 0 and bill nothing, the
// same forgery as an inflated byte rate. So the bytes that really follow the `data`
// chunk header are compared with the declared size: fewer is a truncated file, and
// more than wavTrailingSlackBytes beyond the declaration (and its pad byte) is audio
// the declaration hid, not the trailing metadata chunk a genuine file may carry. The
// slack bounds what a forger can still get billed as nothing to that many bytes of
// audio per request.
func wavDurationFromReader(r io.Reader) (int64, bool) {
	var riff [wavRIFFHeaderLen]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil || string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return 0, false
	}

	var byteRate uint32
	for {
		var header [wavChunkHeaderLen]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return 0, false
		}
		id := string(header[0:4])
		size := binary.LittleEndian.Uint32(header[4:])

		consumed := uint64(0) // bytes of the chunk body already read
		switch id {
		case "fmt ":
			rate, ok := wavReadFmtByteRate(r, size)
			if !ok {
				return 0, false
			}
			byteRate, consumed = rate, wavFmtMinLen
		case "data":
			return wavDataDurationMillis(r, size, byteRate)
		}

		if !wavSkipChunkRest(r, size, consumed) {
			return 0, false
		}
	}
}

// wavReadFmtByteRate reads the fixed PCM part of a `fmt ` chunk whose declared body
// size is size, and returns its byte rate when the chunk is long enough to carry the
// fields, they can be read, and they agree with each other (wavConsistentByteRate).
// It leaves the reader wavFmtMinLen bytes into the chunk body.
func wavReadFmtByteRate(r io.Reader, size uint32) (uint32, bool) {
	if size < wavFmtMinLen {
		return 0, false
	}
	var fields [wavFmtMinLen]byte
	if _, err := io.ReadFull(r, fields[:]); err != nil {
		return 0, false
	}
	return wavConsistentByteRate(fields[:])
}

// wavDataDurationMillis measures a `data` chunk whose header the reader has just
// consumed: size is the body length the header declares and byteRate the one the
// preceding `fmt ` chunk gave (zero when there was none, which is not measurable). It
// reads the stream to its end -- everything after the chunk header is passed over and
// counted -- and believes the declared size only as far as wavDurationFromReader's
// count allows: the stream holds at least that many bytes, and no more than the pad
// byte and wavTrailingSlackBytes beyond it.
func wavDataDurationMillis(r io.Reader, size uint32, byteRate uint32) (int64, bool) {
	if byteRate == 0 {
		return 0, false
	}
	held, err := io.Copy(io.Discard, r)
	if err != nil || uint64(size) > uint64(held) {
		return 0, false
	}
	unexplained := uint64(held) - uint64(size)
	if size&1 == 1 && unexplained > 0 {
		unexplained-- // the pad byte an odd-length chunk is followed by
	}
	if unexplained > wavTrailingSlackBytes {
		return 0, false
	}
	// size < 2^32 and 1000 < 2^10, so the product fits easily in a uint64.
	return int64(uint64(size) * 1000 / uint64(byteRate)), true
}

// wavSkipChunkRest passes over the rest of a chunk whose body is size bytes long and
// of which consumed bytes have already been read, and reports whether the stream held
// them all. Chunks are word-aligned: an odd-length body is followed by one pad byte.
func wavSkipChunkRest(r io.Reader, size uint32, consumed uint64) bool {
	skip := uint64(size) - consumed + uint64(size&1)
	n, err := io.CopyN(io.Discard, r, int64(skip))
	return err == nil && uint64(n) == skip
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
