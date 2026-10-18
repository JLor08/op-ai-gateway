// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// wavPut writes values little-endian into buf, failing the test on an encode error.
func wavPut(t *testing.T, buf *bytes.Buffer, values ...any) {
	t.Helper()
	for _, v := range values {
		if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
			t.Fatalf("encode %T: %v", v, err)
		}
	}
}

// wavChunk builds one RIFF chunk: id, declared size, body, and the pad byte RIFF
// requires after an odd-length body. declaredSize is separate from len(body) so a
// test can lie about the size.
func wavChunk(t *testing.T, id string, declaredSize uint32, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(id)
	wavPut(t, &buf, declaredSize)
	buf.Write(body)
	if len(body)%2 == 1 {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

// wavFmtChunk builds a canonical 16-byte PCM `fmt ` chunk: a header that agrees with
// itself (block align = channels x bytes per sample, byte rate = sample rate x block
// align).
func wavFmtChunk(t *testing.T, sampleRate uint32, channels, bits uint16) []byte {
	t.Helper()
	blockAlign := channels * bits / 8
	return wavFmtChunkWith(t, channels, sampleRate, sampleRate*uint32(blockAlign), blockAlign, bits)
}

// wavFmtChunkWith builds a 16-byte PCM `fmt ` chunk from every field as given, so a
// test can write a header that contradicts itself.
func wavFmtChunkWith(t *testing.T, channels uint16, sampleRate, byteRate uint32, blockAlign, bits uint16) []byte {
	t.Helper()
	var body bytes.Buffer
	wavPut(t, &body,
		uint16(1), // audio format: PCM
		channels,
		sampleRate,
		byteRate,
		blockAlign,
		bits,
	)
	return wavChunk(t, "fmt ", uint32(body.Len()), body.Bytes())
}

// wavDataChunk builds a `data` chunk holding dataBytes of silence.
func wavDataChunk(t *testing.T, dataBytes int) []byte {
	t.Helper()
	return wavChunk(t, "data", uint32(dataBytes), make([]byte, dataBytes))
}

// wavFile wraps chunks in a RIFF/WAVE container.
func wavFile(t *testing.T, chunks ...[]byte) []byte {
	t.Helper()
	payload := []byte("WAVE")
	for _, c := range chunks {
		payload = append(payload, c...)
	}
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	wavPut(t, &buf, uint32(len(payload)))
	buf.Write(payload)
	return buf.Bytes()
}

// buildPCMWav builds a minimal PCM WAV: fmt chunk, then dataBytes of silence.
func buildPCMWav(t *testing.T, sampleRate uint32, channels, bits uint16, dataBytes int) []byte {
	t.Helper()
	return wavFile(t, wavFmtChunk(t, sampleRate, channels, bits), wavDataChunk(t, dataBytes))
}

// buildPCMWavWithLeadingChunk is buildPCMWav with an odd-length LIST chunk (and
// its pad byte) between `fmt ` and `data`, so a parser must skip a foreign chunk
// and honor the padding to find the audio.
func buildPCMWavWithLeadingChunk(t *testing.T, sampleRate uint32, channels, bits uint16, dataBytes int) []byte {
	t.Helper()
	list := []byte("INFOx") // 5 bytes: odd, so wavChunk appends a pad byte
	return wavFile(t,
		wavFmtChunk(t, sampleRate, channels, bits),
		wavChunk(t, "LIST", uint32(len(list)), list),
		wavDataChunk(t, dataBytes),
	)
}

// wavDurationMillis measures a WAV held in memory: wavDurationFromReader, the
// production entry point (it reads the upload where it lies in the multipart
// body), over a byte slice. It lives here because only these tests hold their
// files as slices.
func wavDurationMillis(b []byte) (int64, bool) {
	return wavDurationFromReader(bytes.NewReader(b))
}

func TestWavDurationMillis(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sampleRate     uint32
		channels, bits uint16
		dataBytes      int
		wantMillis     int64
	}{
		// 16 kHz mono 16-bit PCM: byteRate = 32000, so 16000 bytes = 0.5 s.
		{"16kHz mono half second", 16000, 1, 16, 16000, 500},
		{"44.1kHz stereo one second", 44100, 2, 16, 176400, 1000},
		{"partial millisecond rounds down", 16000, 1, 16, 33, 1},
		{"empty data chunk", 16000, 1, 16, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := wavDurationMillis(buildPCMWav(t, tc.sampleRate, tc.channels, tc.bits, tc.dataBytes))
			if !ok {
				t.Fatal("wavDurationMillis ok = false, want true")
			}
			if ms != tc.wantMillis {
				t.Fatalf("wavDurationMillis = %d ms, want %d ms", ms, tc.wantMillis)
			}
		})
	}
}

func TestWavDurationMillisRejectsNonWav(t *testing.T) {
	validFmt := wavFmtChunk(t, 16000, 1, 16)
	zeroRateFmt := func() []byte {
		var body bytes.Buffer
		wavPut(t, &body, uint16(1), uint16(1), uint32(16000), uint32(0), uint16(2), uint16(16))
		return wavChunk(t, "fmt ", uint32(body.Len()), body.Bytes())
	}()
	shortFmt := wavChunk(t, "fmt ", 8, make([]byte, 8))

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"not a wav", []byte("not a wav")},
		{"riff but webm", []byte("RIFF\x00\x00\x00\x00WEBM")},
		{"header only, no chunks", []byte("RIFF\x04\x00\x00\x00WAVE")},
		{"truncated chunk header", append([]byte("RIFF\x0a\x00\x00\x00WAVE"), 'f', 'm', 't', ' ', 16)},
		{"no fmt chunk", wavFile(t, wavDataChunk(t, 100))},
		{"no data chunk", wavFile(t, validFmt)},
		{"data before fmt", wavFile(t, wavDataChunk(t, 100), validFmt)},
		{"fmt chunk too short", wavFile(t, shortFmt, wavDataChunk(t, 100))},
		{"zero byte rate", wavFile(t, zeroRateFmt, wavDataChunk(t, 100))},
		{"zero channels", wavFile(t, wavFmtChunkWith(t, 0, 16000, 0, 0, 16), wavDataChunk(t, 100))},
		{"zero sample rate", wavFile(t, wavFmtChunkWith(t, 1, 0, 32000, 2, 16), wavDataChunk(t, 100))},
		{"zero bits per sample", wavFile(t, wavFmtChunkWith(t, 1, 16000, 32000, 2, 0), wavDataChunk(t, 100))},
		{"data size exceeds buffer", wavFile(t, validFmt, wavChunk(t, "data", 1000, make([]byte, 100)))},
		{"data size 4 GiB, no body", wavFile(t, validFmt, wavChunk(t, "data", math.MaxUint32, nil))},
		{"foreign chunk size exceeds buffer", wavFile(t, validFmt, wavChunk(t, "LIST", math.MaxUint32, nil), wavDataChunk(t, 100))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := wavDurationMillis(tc.in)
			if ok || ms != 0 {
				t.Fatalf("wavDurationMillis = (%d, %v), want (0, false)", ms, ok)
			}
		})
	}
}

// RIFF pads odd-length chunks to an even boundary; ignoring the pad byte would
// misread the next chunk header, so the audio after an odd chunk is only found
// when the pad is honored.
func TestWavDurationMillisHandlesChunkOrderAndPadding(t *testing.T) {
	wav := buildPCMWavWithLeadingChunk(t, 16000, 1, 16, 32000)
	ms, ok := wavDurationMillis(wav)
	if !ok {
		t.Fatal("wavDurationMillis ok = false, want true")
	}
	if ms != 1000 {
		t.Fatalf("wavDurationMillis = %d ms, want 1000 ms", ms)
	}
}

// The byte rate is the divisor of the duration and is written by the CLIENT, so a
// header that inflates it would bill a long clip as a short one. Only a header that
// agrees with itself is believed: block align must be channels x bytes per sample
// and the byte rate sample rate x block align. Each case below changes ONE field of
// a 16 kHz mono 16-bit header (32000 bytes per second, 2 per frame) and so makes the
// header contradict itself.
func TestWavDurationMillisRejectsAHeaderThatContradictsItself(t *testing.T) {
	data := wavDataChunk(t, 32000)
	for _, tc := range []struct {
		name string
		fmt  []byte
	}{
		{"byte rate inflated 100x", wavFmtChunkWith(t, 1, 16000, 3200000, 2, 16)},
		{"byte rate halved", wavFmtChunkWith(t, 1, 16000, 16000, 2, 16)},
		{"byte rate off by one", wavFmtChunkWith(t, 1, 16000, 32001, 2, 16)},
		{"block align inflated with a byte rate that follows it", wavFmtChunkWith(t, 1, 16000, 3200000, 200, 16)},
		{"block align not channels x sample size", wavFmtChunkWith(t, 2, 16000, 32000, 2, 16)},
		{"sample rate inflated, byte rate kept", wavFmtChunkWith(t, 1, 1600000, 32000, 2, 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := wavDurationMillis(wavFile(t, tc.fmt, data))
			if ok || ms != 0 {
				t.Fatalf("wavDurationMillis = (%d, %v), want (0, false): the header is not believable", ms, ok)
			}
		})
	}
}

// Samples that do not fill whole bytes are stored in whole-byte containers, so the
// consistency check rounds the sample size UP: a 12-bit mono header (2-byte frames)
// is as sound as a 16-bit one, and 8-, 24- and 32-bit headers are believed too.
func TestWavDurationMillisAcceptsEverySampleSizeAConsistentHeaderCanCarry(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sampleRate     uint32
		channels, bits uint16
		blockAlign     uint16
		dataBytes      int
		wantMillis     int64
	}{
		{"8-bit mono", 8000, 1, 8, 1, 8000, 1000},
		{"12-bit mono in 2-byte containers", 16000, 1, 12, 2, 32000, 1000},
		{"24-bit stereo", 48000, 2, 24, 6, 288000, 1000},
		{"32-bit mono", 16000, 1, 32, 4, 64000, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fmtChunk := wavFmtChunkWith(t, tc.channels, tc.sampleRate, tc.sampleRate*uint32(tc.blockAlign), tc.blockAlign, tc.bits)
			ms, ok := wavDurationMillis(wavFile(t, fmtChunk, wavDataChunk(t, tc.dataBytes)))
			if !ok || ms != tc.wantMillis {
				t.Fatalf("wavDurationMillis = (%d, %v), want (%d, true)", ms, ok, tc.wantMillis)
			}
		})
	}
}

// The `data` chunk's declared size is the other number the duration rests on, and it
// is the client's too: a header that declares an empty (or tiny) `data` chunk and puts
// the real audio AFTER it would measure 0 and bill nothing. So the declared size is
// checked against what the upload actually holds after the chunk header: bytes that
// the declaration does not account for, beyond the slack a trailing metadata chunk
// needs, make the header unbelievable.
func TestWavDurationMillisRejectsADataChunkSmallerThanTheUploadHoldingAfterIt(t *testing.T) {
	fmtChunk := wavFmtChunk(t, 16000, 1, 16)
	pcm := make([]byte, 32000) // 1 s of audio
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"declares zero, the audio follows", wavFile(t, fmtChunk, wavChunk(t, "data", 0, nil), pcm)},
		{"declares a few bytes, the audio follows", wavFile(t, fmtChunk, wavChunk(t, "data", 100, make([]byte, 100)), pcm)},
		{"declares zero, the audio follows as a second data chunk", wavFile(t, fmtChunk, wavChunk(t, "data", 0, nil), wavDataChunk(t, len(pcm)))},
		{"declares zero, the audio follows wrapped in a foreign chunk", wavFile(t, fmtChunk, wavChunk(t, "data", 0, nil), wavChunk(t, "LIST", uint32(len(pcm)), pcm))},
		{"one byte more than the slack allows", wavFile(t, fmtChunk, wavChunk(t, "data", 0, nil), make([]byte, wavTrailingSlackBytes+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := wavDurationMillis(tc.in)
			if ok || ms != 0 {
				t.Fatalf("wavDurationMillis = (%d, %v), want (0, false): the declared data size is not believable", ms, ok)
			}
		})
	}
}

// What follows the audio in a genuine file stays valid: trailing metadata chunks (an
// ffmpeg LIST/INFO is ~100 bytes) up to the slack, with or without the pad byte an odd
// `data` size is followed by, and a foreign chunk of any size BEFORE `data`.
func TestWavDurationMillisAcceptsATrailingChunkAndALargeLeadingOne(t *testing.T) {
	fmtChunk := wavFmtChunk(t, 16000, 1, 16)
	list := func(n int) []byte { return wavChunk(t, "LIST", uint32(n), make([]byte, n)) }
	for _, tc := range []struct {
		name       string
		in         []byte
		wantMillis int64
	}{
		{"a small trailing LIST chunk", wavFile(t, fmtChunk, wavDataChunk(t, 32000), list(100)), 1000},
		{"a trailing chunk of exactly the slack", wavFile(t, fmtChunk, wavDataChunk(t, 32000), list(wavTrailingSlackBytes-wavChunkHeaderLen)), 1000},
		{"an odd data size and its pad byte", wavFile(t, fmtChunk, wavDataChunk(t, 32001)), 1000},
		{"an odd data size, its pad byte and a trailing chunk", wavFile(t, fmtChunk, wavDataChunk(t, 32001), list(100)), 1000},
		{"an odd data size, its pad byte and a trailing chunk of exactly the slack", wavFile(t, fmtChunk, wavDataChunk(t, 32001), list(wavTrailingSlackBytes-wavChunkHeaderLen)), 1000},
		{"an odd data size whose pad byte was left off", wavFile(t, fmtChunk, wavChunk(t, "data", 32001, make([]byte, 32001))[:wavChunkHeaderLen+32001]), 1000},
		{"a 1 MiB leading chunk before data", wavFile(t, fmtChunk, list(1<<20), wavDataChunk(t, 32000)), 1000},
		{"a leading chunk of odd length", wavFile(t, fmtChunk, wavChunk(t, "LIST", 5, []byte("INFOx")), wavDataChunk(t, 32000)), 1000},
		// 8 kHz would measure 2000 ms, so a result of 1000 shows the trailing `fmt ` was
		// never read: the byte rate is the one that stood before `data`.
		{"a trailing fmt chunk that disagrees with the first is never read", wavFile(t, fmtChunk, wavDataChunk(t, 32000), wavFmtChunk(t, 8000, 1, 16)), 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := wavDurationMillis(tc.in)
			if !ok || ms != tc.wantMillis {
				t.Fatalf("wavDurationMillis = (%d, %v), want (%d, true)", ms, ok, tc.wantMillis)
			}
		})
	}
}

// A `fmt ` chunk is allowed to be longer than its fixed 16-byte PCM part: an 18-byte
// one carries the cbSize field, and WAVE_FORMAT_EXTENSIBLE (what many encoders write
// for anything beyond 16-bit stereo) is 40 bytes. Only the fixed part is read, so the
// rest must be passed over -- and an odd-length one followed by its pad byte -- to land
// on the next chunk header. A parser that stopped reading the chunk at 16 bytes would
// take the extension for a chunk header and lose the `data` chunk.
func TestWavDurationMillisPassesOverTheTailOfALongerFmtChunk(t *testing.T) {
	fixed := func(tag uint16) *bytes.Buffer {
		var body bytes.Buffer
		wavPut(t, &body, tag, uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16))
		return &body
	}
	withTail := func(tag uint16, tail ...any) []byte {
		body := fixed(tag)
		wavPut(t, body, tail...)
		return wavChunk(t, "fmt ", uint32(body.Len()), body.Bytes())
	}
	extensible := withTail(0xFFFE,
		uint16(22), // cbSize
		uint16(16), // valid bits per sample
		uint32(4),  // channel mask: front centre
		// KSDATAFORMAT_SUBTYPE_PCM, the sub-format GUID
		[]byte{0x01, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71},
	)
	if got := len(extensible) - wavChunkHeaderLen; got != 40 {
		t.Fatalf("the extensible fmt body is %d bytes, want 40 -- the test no longer builds the chunk it names", got)
	}
	data := wavDataChunk(t, 32000)
	for _, tc := range []struct {
		name string
		fmt  []byte
	}{
		{"18 bytes: with a cbSize of zero", withTail(1, uint16(0))},
		{"40 bytes: WAVE_FORMAT_EXTENSIBLE", extensible},
		{"odd length: 17 bytes and its pad byte", withTail(1, uint8(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, ok := wavDurationMillis(wavFile(t, tc.fmt, data))
			if !ok || ms != 1000 {
				t.Fatalf("wavDurationMillis = (%d, %v), want (1000, true)", ms, ok)
			}
		})
	}
}

// A second `fmt ` chunk before `data` replaces the first: the walk keeps no first-wins
// rule, so the byte rate in force is the last one read before `data`. That is the
// stricter reading of a file the format does not define, because every `fmt ` read
// must still agree with itself: a later chunk that contradicts itself voids the file
// even though the first one was sound, so a header cannot hide an inflated rate
// behind a sound decoy (or the reverse).
func TestWavDurationMillisTakesTheLastFmtChunkBeforeData(t *testing.T) {
	data := wavDataChunk(t, 16000)
	t.Run("the later fmt chunk wins", func(t *testing.T) {
		// 16 kHz would measure 500 ms, 8 kHz 1000 ms.
		ms, ok := wavDurationMillis(wavFile(t, wavFmtChunk(t, 16000, 1, 16), wavFmtChunk(t, 8000, 1, 16), data))
		if !ok || ms != 1000 {
			t.Fatalf("wavDurationMillis = (%d, %v), want (1000, true): the later fmt chunk's byte rate", ms, ok)
		}
	})
	t.Run("a later fmt chunk that contradicts itself voids a sound first one", func(t *testing.T) {
		forged := wavFmtChunkWith(t, 1, 16000, 3200000, 2, 16)
		ms, ok := wavDurationMillis(wavFile(t, wavFmtChunk(t, 16000, 1, 16), forged, data))
		if ok || ms != 0 {
			t.Fatalf("wavDurationMillis = (%d, %v), want (0, false)", ms, ok)
		}
	})
}
