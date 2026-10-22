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

// Offsets of the fixed PCM fields within the body of a `fmt ` chunk. The 2-byte audio
// format tag at offset 0 is not read by the duration and byte-rate measurements (the
// consistency check below is what separates a PCM header from one that is not); only
// wavFormatFromReader reads it, for the caller that has to know WHAT the samples are.
const (
	wavFormatTagOffset  = 0
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
	if !wavReadRIFFHeader(r) {
		return 0, false
	}

	var byteRate uint32
	for {
		id, size, ok := wavReadChunkHeader(r)
		if !ok {
			return 0, false
		}

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

// wavByteRateFromReader returns the byte rate (sample rate x channels x bytes per
// sample) of a PCM WAV stream, read from its `fmt ` chunk, and true. It returns
// (0, false) for a stream that is not RIFF/WAVE and for a `fmt ` chunk that is
// truncated, too short to carry a byte rate or not consistent with itself (see
// wavConsistentByteRate), and for a stream whose `data` chunk comes before any `fmt `
// chunk (wavDurationFromReader does not believe one that follows `data` either).
//
// It stops at the `fmt ` chunk: the audio is never read, so a response can be
// measured from its first bytes, and any chunk ahead of `fmt ` is passed over. The
// rate is the PCM FORMAT's constant, the divisor that turns PCM bytes into seconds
// (a streamed speech response is billed as pcmBytes / byteRate), not a throughput.
func wavByteRateFromReader(r io.Reader) (uint32, bool) {
	fields, ok := wavFmtFieldsFromReader(r)
	if !ok {
		return 0, false
	}
	return wavConsistentByteRate(fields[:])
}

// wavFormat is the sample format a WAV's `fmt ` chunk states: what the byte rate alone
// cannot tell. A byte rate is sample rate x channels x bytes per sample, so one number
// fits many formats -- 96000 B/s is 48 kHz mono 16-bit, 24 kHz stereo 16-bit and 24 kHz
// mono float32 alike -- and a caller that must PLAY the audio, or write a header for it,
// has to know which.
type wavFormat struct {
	Tag        uint16 // 1 = integer PCM, 3 = IEEE float, 0xFFFE = extensible, ...
	Channels   uint16
	SampleRate uint32
	ByteRate   uint32
	Bits       uint16
}

// wavFormatPCM is the `fmt ` audio format tag of integer PCM.
const wavFormatPCM = 1

// isMono16PCM reports whether the format is integer PCM, one channel, 16 bits a sample:
// signed 16-bit little-endian mono, the format a streamed speech response is played and
// stored as (ADR-068). Any other tag -- float32, extensible -- is not it, whatever its
// byte rate says.
func (f wavFormat) isMono16PCM() bool {
	return f.Tag == wavFormatPCM && f.Channels == 1 && f.Bits == 16
}

// wavFormatFromReader is wavByteRateFromReader's sibling for the caller that needs the
// whole format: it reads the same `fmt ` chunk, under the same rules (a stream that is not
// RIFF/WAVE, a `fmt ` that is truncated, too short or not consistent with itself, or a
// `data` chunk ahead of any `fmt ` is (wavFormat{}, false)), and returns its tag, channels,
// sample rate, byte rate and bit depth. It stops at the `fmt ` chunk, like its sibling.
func wavFormatFromReader(r io.Reader) (wavFormat, bool) {
	fields, ok := wavFmtFieldsFromReader(r)
	if !ok {
		return wavFormat{}, false
	}
	return wavFormatFromFields(fields[:])
}

// wavFormatFromFields decodes the first wavFmtMinLen bytes of a `fmt ` chunk into its
// wavFormat, once they agree with each other (wavConsistentByteRate): the one decode behind
// wavFormatFromReader and wavStripToData.
func wavFormatFromFields(fields []byte) (wavFormat, bool) {
	byteRate, ok := wavConsistentByteRate(fields)
	if !ok {
		return wavFormat{}, false
	}
	le := binary.LittleEndian
	return wavFormat{
		Tag:        le.Uint16(fields[wavFormatTagOffset:]),
		Channels:   le.Uint16(fields[wavChannelsOffset:]),
		SampleRate: le.Uint32(fields[wavSampleRateOffset:]),
		ByteRate:   byteRate,
		Bits:       le.Uint16(fields[wavBitsOffset:]),
	}, true
}

// wavStripToData walks a RIFF/WAVE stream to its `data` chunk and stops at the first byte
// of its body: it is wavDurationFromReader's chunk loop for the caller that wants the audio
// itself rather than its length (a speech response relayed as raw PCM, the WAV header
// stripped off). It returns the format the `fmt ` chunk states and the size the `data`
// chunk declares, and true; the caller reads exactly dataSize bytes from r to get the PCM.
//
// It returns (wavFormat{}, 0, false) when the stream is not RIFF/WAVE, when a `fmt ` chunk
// is truncated, too short or not consistent with itself (see wavConsistentByteRate), when a
// `data` chunk comes before any `fmt ` (there is then no format to say what its bytes are),
// and when the stream ends before the `data` chunk header. Foreign chunks are passed over,
// and a later `fmt ` chunk replaces an earlier one, as in wavDurationFromReader.
//
// Whether the format is one the caller can relay (see isMono16PCM) is not judged here, and
// nor is whether the stream holds dataSize bytes: the size is the header's word, and the
// caller meters and bounds what it reads.
func wavStripToData(r io.Reader) (format wavFormat, dataSize int64, ok bool) {
	if !wavReadRIFFHeader(r) {
		return wavFormat{}, 0, false
	}

	for {
		id, size, ok := wavReadChunkHeader(r)
		if !ok {
			return wavFormat{}, 0, false
		}

		consumed := uint64(0) // bytes of the chunk body already read
		switch id {
		case "fmt ":
			fields, ok := wavReadConsistentFmtFields(r, size)
			if !ok {
				return wavFormat{}, 0, false
			}
			if format, ok = wavFormatFromFields(fields[:]); !ok {
				return wavFormat{}, 0, false
			}
			consumed = wavFmtMinLen
		case "data":
			if format.ByteRate == 0 { // no `fmt ` before it
				return wavFormat{}, 0, false
			}
			return format, int64(size), true
		}

		if !wavSkipChunkRest(r, size, consumed) {
			return wavFormat{}, 0, false
		}
	}
}

// wavFmtFieldsFromReader walks a RIFF/WAVE stream to its `fmt ` chunk and returns the
// first wavFmtMinLen bytes of its body, once they agree with each other (see
// wavConsistentByteRate): the one parse behind wavByteRateFromReader and
// wavFormatFromReader. Chunks ahead of `fmt ` are passed over; a `data` chunk before it
// ends the search, as the audio is never read.
func wavFmtFieldsFromReader(r io.Reader) ([wavFmtMinLen]byte, bool) {
	var fields [wavFmtMinLen]byte
	if !wavReadRIFFHeader(r) {
		return fields, false
	}
	for {
		id, size, ok := wavReadChunkHeader(r)
		if !ok || id == "data" {
			return fields, false
		}
		if id == "fmt " {
			return wavReadConsistentFmtFields(r, size)
		}
		if !wavSkipChunkRest(r, size, 0) {
			return fields, false
		}
	}
}

// wavReadConsistentFmtFields reads the first wavFmtMinLen bytes of a `fmt ` chunk whose
// declared body size is size, and reports true only when the chunk is long enough to
// carry them, they can be read, and they agree with each other (wavConsistentByteRate).
// It leaves the reader wavFmtMinLen bytes into the chunk body. It is
// wavReadFmtByteRate's counterpart for the caller that needs the fields themselves.
func wavReadConsistentFmtFields(r io.Reader, size uint32) ([wavFmtMinLen]byte, bool) {
	var fields [wavFmtMinLen]byte
	if size < wavFmtMinLen {
		return fields, false
	}
	if _, err := io.ReadFull(r, fields[:]); err != nil {
		return fields, false
	}
	if _, ok := wavConsistentByteRate(fields[:]); !ok {
		return fields, false
	}
	return fields, true
}

// wavReadRIFFHeader consumes the 12-byte RIFF container header and reports whether
// it declares a WAVE file.
func wavReadRIFFHeader(r io.Reader) bool {
	var riff [wavRIFFHeaderLen]byte
	_, err := io.ReadFull(r, riff[:])
	return err == nil && string(riff[0:4]) == "RIFF" && string(riff[8:12]) == "WAVE"
}

// wavReadChunkHeader consumes one chunk header and returns the chunk's id and the
// size of its body, and false when the stream ends inside the header.
func wavReadChunkHeader(r io.Reader) (id string, size uint32, ok bool) {
	var header [wavChunkHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", 0, false
	}
	return string(header[0:4]), binary.LittleEndian.Uint32(header[4:]), true
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
//
// A declaration that overruns the stream (a truncated file, or the 0xFFFFFFFF a
// streaming WAV writer emits) is a loud zero, not a bill of the bytes held. That is no
// under-bill because audio.cpp refuses such an upload ("failed to read WAV data chunk",
// verified live against v0.9.1, buffered and stream=true alike): it is never transcribed.
// A future audio.cpp that decodes it would make the zero an under-bill; re-check on upgrade.
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
