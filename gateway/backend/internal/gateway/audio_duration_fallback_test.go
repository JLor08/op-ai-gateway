// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fallbackWAV builds a mono 16-bit WAV at 24 kHz (byte rate 48000) whose `data` chunk
// holds dataBytes of audio, with the RIFF and `data` size fields as given: a streaming
// writer leaves 0 or 0xFFFFFFFF in them.
func fallbackWAV(t *testing.T, riffSize, dataSize uint32, dataBytes int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	wavPut(t, &buf, riffSize)
	buf.WriteString("WAVE")
	buf.Write(wavFmtChunk(t, 24000, 1, 16))
	buf.WriteString("data")
	wavPut(t, &buf, dataSize)
	buf.Write(bytes.Repeat([]byte{0x01, 0x02}, dataBytes/2))
	return buf.Bytes()
}

// fallbackSpeechUpstream answers every request with body as audio/wav, with a
// Content-Length and none of audio.cpp's metering headers (extra adds some).
func fallbackSpeechUpstream(t *testing.T, body []byte, extra map[string]string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		for k, v := range extra {
			w.Header().Set(k, v)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(up.Close)
	return up
}

// A target that sends no duration header (sglang-omni) is billed from the WAV it
// returned: data bytes actually relayed / the `fmt ` byte rate, whatever the header's
// own size fields say.
func TestSpeechBillsFromTheWAVWhenTheHeaderIsAbsent(t *testing.T) {
	cases := []struct {
		name string
		wav  []byte
	}{
		{"exact size fields", fallbackWAV(t, 36+48000, 48000, 48000)},
		{"size fields 0 (streaming writer)", fallbackWAV(t, 0, 0, 48000)},
		{"size fields 0xFFFFFFFF (streaming writer)", fallbackWAV(t, 0xFFFFFFFF, 0xFFFFFFFF, 48000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			srv, _ := newSpeechCapableTestServer(t, fallbackSpeechUpstream(t, tc.wav, nil).URL)

			rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), tc.wav) {
				t.Fatalf("status = %d, body relayed intact = %v", rec.Code, bytes.Equal(rec.Body.Bytes(), tc.wav))
			}
			got := lastUsageEvent(t, srv)
			if math.Abs(got.BillingQuantity-1) > 1e-9 {
				t.Errorf("BillingQuantity = %v, want 1", got.BillingQuantity)
			}
			requireSpeechNonTokenRow(t, got)
			if errs := errorLogs(buf); len(errs) != 0 {
				t.Errorf("Error logs = %+v, want none", errs)
			}
		})
	}
}

// pcm is the upstream WAV with its header stripped; its duration comes from that header's
// byte rate and the PCM bytes relayed.
func TestSpeechPCMBillsFromTheStrippedWAV(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	srv, _ := newSpeechCapableTestServer(t, fallbackSpeechUpstream(t, fallbackWAV(t, 0, 48000, 48000), nil).URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there.","response_format":"pcm"}`)

	if rec.Code != http.StatusOK || rec.Body.Len() != 48000 {
		t.Fatalf("status = %d, pcm bytes = %d, want 200 and 48000", rec.Code, rec.Body.Len())
	}
	if got := lastUsageEvent(t, srv); math.Abs(got.BillingQuantity-1) > 1e-9 {
		t.Errorf("BillingQuantity = %v, want 1", got.BillingQuantity)
	}
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Errorf("Error logs = %+v, want none", errs)
	}
}

// audio.cpp's header wins over the audio, and the relay logs nothing new.
func TestSpeechHeaderWinsOverTheWAVFallback(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	up := fallbackSpeechUpstream(t, fallbackWAV(t, 0, 48000, 48000), map[string]string{speechDurationHeader: "1500"})
	srv, _ := newSpeechCapableTestServer(t, up.URL)

	postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if got := lastUsageEvent(t, srv); got.BillingQuantity != 1.5 {
		t.Errorf("BillingQuantity = %v, want 1.5 from the header", got.BillingQuantity)
	}
	if errs := errorLogs(buf); len(errs) != 0 {
		t.Errorf("Error logs = %+v, want none", errs)
	}
}

// A body that is no WAV, with no header, stays today's logged zero: never a guess.
func TestSpeechNonWAVWithoutHeaderStaysALoggedZero(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	t.Cleanup(restore)
	srv, _ := newSpeechCapableTestServer(t, fallbackSpeechUpstream(t, []byte("ID3 not a wav at all, just some mp3-ish bytes"), nil).URL)

	rec := postSpeech(t, srv, `{"model":"tts-voice","input":"Hello there."}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := lastUsageEvent(t, srv); got.BillingQuantity != 0 {
		t.Errorf("BillingQuantity = %v, want 0", got.BillingQuantity)
	}
	errs := errorLogs(buf)
	if len(errs) != 1 || !strings.Contains(errs[0].Msg, "no usable audio duration") {
		t.Fatalf("Error logs = %+v, want exactly the no-usable-duration one", errs)
	}
}

// speechWavTally unit cases: trailing metadata after a declared data chunk is not audio,
// a head split over many small chunks is still found, and nothing relayed is no duration.
func TestSpeechWavTally(t *testing.T) {
	t.Run("a declared data size smaller than the rest bounds the audio", func(t *testing.T) {
		wav := append(fallbackWAV(t, 0, 48000, 48000), wavChunk(t, "LIST", 4, []byte("INFO"))...)
		tally := newSpeechWavTally()
		for i := 0; i < len(wav); i += 7 {
			tally.feed(wav[i:min(i+7, len(wav))])
		}
		if got, ok := tally.seconds(); !ok || math.Abs(got-1) > 1e-9 {
			t.Fatalf("seconds = (%v, %v), want (1, true)", got, ok)
		}
	})
	t.Run("a LIST chunk between fmt and data is passed over", func(t *testing.T) {
		var wav bytes.Buffer
		wav.WriteString("RIFF")
		wavPut(t, &wav, uint32(0))
		wav.WriteString("WAVE")
		wav.Write(wavFmtChunk(t, 24000, 1, 16))
		wav.Write(wavChunk(t, "LIST", 4, []byte("INFO")))
		wav.WriteString("data")
		wavPut(t, &wav, uint32(0xFFFFFFFF))
		wav.Write(make([]byte, 48000))
		tally := newSpeechWavTally()
		tally.feed(wav.Bytes())
		if got, ok := tally.seconds(); !ok || math.Abs(got-1) > 1e-9 {
			t.Fatalf("seconds = (%v, %v), want (1, true)", got, ok)
		}
	})
	t.Run("fmt is not the first chunk", func(t *testing.T) {
		var wav bytes.Buffer
		wav.WriteString("RIFF")
		wavPut(t, &wav, uint32(0))
		wav.WriteString("WAVE")
		wav.Write(wavChunk(t, "bext", 4, []byte("abcd")))
		wav.Write(wavFmtChunk(t, 24000, 1, 16))
		wav.WriteString("data")
		wavPut(t, &wav, uint32(0))
		wav.Write(make([]byte, 24000))
		tally := newSpeechWavTally()
		tally.feed(wav.Bytes())
		if got, ok := tally.seconds(); !ok || math.Abs(got-0.5) > 1e-9 {
			t.Fatalf("seconds = (%v, %v), want (0.5, true)", got, ok)
		}
	})
	t.Run("a header with no audio is no duration", func(t *testing.T) {
		tally := newSpeechWavTally()
		tally.feed(fallbackWAV(t, 0, 0, 0))
		if got, ok := tally.seconds(); ok {
			t.Fatalf("seconds = (%v, true), want false", got)
		}
	})
	t.Run("a nil tally measures nothing", func(t *testing.T) {
		var tally *speechWavTally
		tally.feed([]byte("x"))
		if _, ok := tally.seconds(); ok {
			t.Fatal("nil tally reported a duration")
		}
	})
}

// A transcription whose response carries no timing block is billed from the uploaded WAV.
func TestTranscriptionBillsFromTheUploadedWAVWithoutTiming(t *testing.T) {
	// 8 kHz mono 16-bit: 16000 B/s, so 40000 bytes are 2.5 s.
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	wavPut(t, &wav, uint32(36+40000))
	wav.WriteString("WAVE")
	wav.Write(wavFmtChunk(t, 8000, 1, 16))
	wav.Write(wavChunk(t, "data", 40000, make([]byte, 40000)))
	parts := func(filename, ctype string, audio []byte) []editPart {
		return []editPart{
			{name: "model", body: []byte(transcriptionGatewayModel)},
			{name: "file", filename: filename, contentType: ctype, body: audio},
		}
	}
	mp3 := append([]byte("ID3\x04\x00"), bytes.Repeat([]byte{0xFF, 0xFB}, 100)...)

	cases := []struct {
		name      string
		response  string
		parts     []editPart
		want      float64
		wantError bool
	}{
		{"no timing, WAV upload", `{"text":"hi"}`, parts("clip.wav", "audio/wav", wav.Bytes()), 2.5, false},
		{"timing present wins", transcriptionDoc("1000"), parts("clip.wav", "audio/wav", wav.Bytes()), 1, false},
		{"timing key absent, WAV upload", `{"text":"hi","timing":{"wall_ms":5}}`, parts("clip.wav", "audio/wav", wav.Bytes()), 2.5, false},
		{"timing present 0 is audio.cpp's own loud zero, WAV upload", transcriptionDoc("0"), parts("clip.wav", "audio/wav", wav.Bytes()), 0, true},
		{"no timing, mp3 upload", `{"text":"hi"}`, parts("clip.mp3", "audio/mpeg", mp3), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			t.Cleanup(restore)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			t.Cleanup(up.Close)
			srv, _ := newTranscriptionCapableTestServer(t, up.URL)

			rec := postTranscriptionParts(t, srv, tc.parts)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.Bytes())
			}
			if got := lastUsageEvent(t, srv); math.Abs(got.BillingQuantity-tc.want) > 1e-9 {
				t.Errorf("BillingQuantity = %v, want %v", got.BillingQuantity, tc.want)
			}
			errs := errorLogs(buf)
			if tc.wantError != (len(errs) == 1 && strings.Contains(errs[0].Msg, "no usable audio duration")) || (!tc.wantError && len(errs) != 0) {
				t.Errorf("Error logs = %+v, wantError = %v", errs, tc.wantError)
			}
		})
	}
}
