// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package benchaudio

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// TestBenchaudioClipIsPCM16kMono pins the bundled clip to what the ASR benchmark relies
// on: its `fmt ` chunk states integer PCM, one channel, 16000 Hz, 16 bits a sample; PCM
// hands out that shape and 22.85 s of it, taken from the WAV itself; and the transcript is
// the dataset's text, from the first sentence on, without the file's trailing newline.
func TestBenchaudioClipIsPCM16kMono(t *testing.T) {
	w := WAV()
	// Read the `fmt ` chunk where it is, independently of the parser under test.
	i := bytes.Index(w, []byte("fmt "))
	if i < 0 || len(w) < i+8+16 {
		t.Fatalf("the clip has no complete fmt chunk (%d bytes)", len(w))
	}
	f := w[i+8:]
	le := binary.LittleEndian
	tag, ch, rate, bits := le.Uint16(f[0:]), le.Uint16(f[2:]), le.Uint32(f[4:]), le.Uint16(f[14:])
	if tag != 1 || ch != 1 || rate != 16000 || bits != 16 {
		t.Fatalf("fmt states tag %d, %d channel(s), %d Hz, %d-bit; want PCM (1), 1, 16000, 16", tag, ch, rate, bits)
	}

	pcm, sampleRate, channels := PCM()
	if sampleRate != 16000 || channels != 1 {
		t.Fatalf("PCM() = rate %d, channels %d; want 16000, 1", sampleRate, channels)
	}
	if seconds := float64(len(pcm)) / float64(sampleRate*channels*2); math.Abs(seconds-22.85) > 0.05 {
		t.Fatalf("the clip is %.3f s of audio; want 22.85 s ± 0.05", seconds)
	}
	if !bytes.Contains(w, pcm) {
		t.Fatal("PCM() is not the WAV's own audio")
	}

	text := Transcript()
	if !strings.HasPrefix(text, "der schutz der menschen") || !strings.HasSuffix(text, "die restlichen kosten trägt die stadt.") {
		t.Fatalf("Transcript() = %q; want the six sentences, first to last", text)
	}
	if strings.TrimSpace(text) != text {
		t.Fatalf("Transcript() = %q carries surrounding whitespace", text)
	}
	if Language != "de" {
		t.Fatalf("Language = %q, want de", Language)
	}
}

// TestBenchaudioReturnsCopies: every call hands out its own bytes, so a caller that
// writes into what it got (a request body it reuses, a test that trims it) cannot change
// the clip for the next caller.
func TestBenchaudioReturnsCopies(t *testing.T) {
	w := WAV()
	w[0] = 'X'
	if again := WAV(); again[0] != 'R' {
		t.Fatalf("WAV()[0] = %q after a caller wrote into its copy, want 'R'", again[0])
	}

	pcm, _, _ := PCM()
	first := pcm[0]
	pcm[0] ^= 0xFF
	if again, _, _ := PCM(); again[0] != first {
		t.Fatalf("PCM()[0] = %#x after a caller wrote into its copy, want %#x", again[0], first)
	}
}

// riffChunk builds one RIFF chunk: its id, the body's size, the body, and the pad byte
// RIFF puts after an odd-length body.
func riffChunk(id string, body []byte) []byte {
	out := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// riffWave wraps chunks in a RIFF/WAVE container.
func riffWave(chunks ...[]byte) []byte {
	payload := []byte("WAVE")
	for _, c := range chunks {
		payload = append(payload, c...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...), payload...)
}

// fmtBody is the 16-byte body of a `fmt ` chunk that agrees with itself.
func fmtBody(tag, channels uint16, sampleRate uint32, bits uint16) []byte {
	blockAlign := channels * bits / 8
	le := binary.LittleEndian
	b := le.AppendUint16(nil, tag)
	b = le.AppendUint16(b, channels)
	b = le.AppendUint32(b, sampleRate)
	b = le.AppendUint32(b, sampleRate*uint32(blockAlign))
	b = le.AppendUint16(b, blockAlign)
	return le.AppendUint16(b, bits)
}

// TestBenchaudioPCMWalksChunks: the audio is found by walking the chunks, not at a fixed
// offset. A foreign chunk of odd length before `fmt ` (and its pad byte), a `fmt ` chunk
// with an extension past its 16 fixed bytes, a LIST chunk between `fmt ` and `data` and a
// chunk after the audio are all passed over, and the result is exactly the `data` body
// with the format `fmt ` states.
func TestBenchaudioPCMWalksChunks(t *testing.T) {
	audio := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	wav := riffWave(
		riffChunk("JUNK", []byte{9, 9, 9}),
		riffChunk("fmt ", append(fmtBody(1, 2, 22050, 16), 0, 0)), // cbSize 0: an 18-byte fmt
		riffChunk("LIST", []byte("INFOx")),
		riffChunk("data", audio),
		riffChunk("id3 ", []byte{7, 7}),
	)
	c, err := parsePCM(wav)
	if err != nil {
		t.Fatalf("parsePCM: %v", err)
	}
	if !bytes.Equal(c.pcm, audio) || c.sampleRate != 22050 || c.channels != 2 {
		t.Fatalf("parsePCM = %v at %d Hz, %d channel(s); want %v at 22050 Hz, 2 channels", c.pcm, c.sampleRate, c.channels, audio)
	}
}

// TestBenchaudioPCMRefusesMalformed: a file the walk cannot vouch for is an error, never
// audio of a guessed shape -- the format must be 16-bit integer PCM and stated before the
// audio, every chunk must be whole, and the audio a whole number of frames.
func TestBenchaudioPCMRefusesMalformed(t *testing.T) {
	pcmFmt := riffChunk("fmt ", fmtBody(1, 1, 16000, 16))
	data := riffChunk("data", []byte{1, 2, 3, 4})
	truncated := riffWave(pcmFmt, data)
	truncated = truncated[:len(truncated)-2]
	cases := []struct {
		name string
		wav  []byte
	}{
		{"shorter than a RIFF header", []byte("RIFF")},
		{"not RIFF", append([]byte("RIFX"), riffWave(pcmFmt, data)[4:]...)},
		{"not WAVE", append(riffWave(pcmFmt, data)[:8], append([]byte("AVI "), riffWave(pcmFmt, data)[12:]...)...)},
		{"audio before the format", riffWave(data, pcmFmt)},
		{"no audio", riffWave(pcmFmt)},
		{"format chunk too short", riffWave(riffChunk("fmt ", fmtBody(1, 1, 16000, 16)[:14]), data)},
		{"float samples", riffWave(riffChunk("fmt ", fmtBody(3, 1, 16000, 32)), data)},
		{"8-bit samples", riffWave(riffChunk("fmt ", fmtBody(1, 1, 16000, 8)), data)},
		{"no channels", riffWave(riffChunk("fmt ", fmtBody(1, 0, 16000, 16)), data)},
		{"no sample rate", riffWave(riffChunk("fmt ", fmtBody(1, 1, 0, 16)), data)},
		{"audio chunk cut short", truncated},
		{"half a frame", riffWave(pcmFmt, riffChunk("data", []byte{1, 2, 3}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c, err := parsePCM(tc.wav); err == nil {
				t.Fatalf("parsePCM = %v at %d Hz, %d channel(s), nil; want an error", c.pcm, c.sampleRate, c.channels)
			}
		})
	}
}

// TestBenchaudioUnreadableClipHasNoAudio: a clip the walk cannot read hands out no audio at
// all -- no samples, 0 Hz, no channels, which a measurement refuses before it sends anything
// -- rather than audio of a guessed shape or a panic in the benchmark that asked.
func TestBenchaudioUnreadableClipHasNoAudio(t *testing.T) {
	pcm, sampleRate, channels := pcmOf(riffWave(riffChunk("fmt ", fmtBody(3, 1, 16000, 32)), riffChunk("data", []byte{1, 2, 3, 4})))
	if pcm != nil || sampleRate != 0 || channels != 0 {
		t.Fatalf("pcmOf(float WAV) = %v at %d Hz, %d channel(s); want no audio", pcm, sampleRate, channels)
	}
}
