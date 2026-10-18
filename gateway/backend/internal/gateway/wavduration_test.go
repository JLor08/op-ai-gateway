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

// wavFmtChunk builds a canonical 16-byte PCM `fmt ` chunk.
func wavFmtChunk(t *testing.T, sampleRate uint32, channels, bits uint16) []byte {
	t.Helper()
	blockAlign := channels * bits / 8
	var body bytes.Buffer
	wavPut(t, &body,
		uint16(1), // audio format: PCM
		channels,
		sampleRate,
		sampleRate*uint32(blockAlign), // byte rate
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
