// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The multi-part speech turn (#279): one long clip is spread over several WAV parts, each
// small enough to pass the per-blob chat-storage cap on its own. These are the pure
// helpers that cut the clip and shape the content array; the executors that call them are
// tested where they live.

const partsTestRate = 24000

// checkedWAVData checks that wav is a canonical mono 16-bit PCM WAV at partsTestRate whose
// header agrees with its body, and returns the data chunk.
func checkedWAVData(t *testing.T, wav []byte) []byte {
	t.Helper()
	if len(wav) < wavHeaderLen {
		t.Fatalf("a %d byte WAV is shorter than its %d byte header", len(wav), wavHeaderLen)
	}
	le := binary.LittleEndian
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Fatalf("not a canonical WAV header: %q", wav[:wavHeaderLen])
	}
	if got, want := le.Uint16(wav[20:]), uint16(1); got != want {
		t.Fatalf("audio format = %d, want %d (PCM)", got, want)
	}
	if got, want := le.Uint16(wav[22:]), uint16(1); got != want {
		t.Fatalf("channels = %d, want %d", got, want)
	}
	if got := le.Uint32(wav[24:]); got != partsTestRate {
		t.Fatalf("sample rate = %d, want %d", got, partsTestRate)
	}
	if got, want := le.Uint16(wav[34:]), uint16(16); got != want {
		t.Fatalf("bits per sample = %d, want %d", got, want)
	}
	size := int(le.Uint32(wav[40:]))
	if size != len(wav)-wavHeaderLen {
		t.Fatalf("data chunk claims %d bytes, the WAV holds %d", size, len(wav)-wavHeaderLen)
	}
	if got := int(le.Uint32(wav[4:])); got != len(wav)-8 {
		t.Fatalf("RIFF size = %d, want %d", got, len(wav)-8)
	}
	if size%speechStreamBytesPerFrame != 0 {
		t.Fatalf("data chunk of %d bytes is not a whole number of frames", size)
	}
	return wav[wavHeaderLen:]
}

// dataSizes returns the PCM size of every part, after checking each is a valid WAV, and the
// concatenation of their data chunks.
func dataSizes(t *testing.T, wavs [][]byte) (sizes []int, joined []byte) {
	t.Helper()
	for _, wav := range wavs {
		data := checkedWAVData(t, wav)
		sizes = append(sizes, len(data))
		joined = append(joined, data...)
	}
	return sizes, joined
}

// A clip of 2.5 parts is three WAVs: two full ones and the remainder, in order, and the
// parts' audio put end to end is the clip, sample for sample.
func TestSplitPCMToWAVPartsSplitsAClipIntoFullPartsAndARemainder(t *testing.T) {
	const partBytes = 1000
	pcm := speechPCM(partBytes*5/2, 3)

	wavs := splitPCMToWAVParts(pcm, partsTestRate, partBytes)

	sizes, joined := dataSizes(t, wavs)
	if want := []int{1000, 1000, 500}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the input PCM")
	}
}

// A clip that fits one part is that part -- the same bytes the single-part turn has always
// committed, so a short clip is unchanged by the chunking.
func TestSplitPCMToWAVPartsKeepsAClipThatFitsAsOneIdenticalPart(t *testing.T) {
	for _, n := range []int{2, 999, 1000} {
		pcm := speechPCM(n&^1, 5)
		wavs := splitPCMToWAVParts(pcm, partsTestRate, 1000)
		if len(wavs) != 1 {
			t.Fatalf("%d bytes of PCM: %d parts, want 1", len(pcm), len(wavs))
		}
		if !bytes.Equal(wavs[0], wavFromPCM16(pcm, partsTestRate)) {
			t.Fatalf("%d bytes of PCM: the one part is not wavFromPCM16's WAV", len(pcm))
		}
	}
}

// A clip that is an exact multiple of the part size has no empty trailing part.
func TestSplitPCMToWAVPartsHasNoEmptyPartAfterAnExactMultiple(t *testing.T) {
	pcm := speechPCM(3000, 7)
	sizes, joined := dataSizes(t, splitPCMToWAVParts(pcm, partsTestRate, 1000))
	if want := []int{1000, 1000, 1000}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the input PCM")
	}
}

// A clip may end on half a sample (a stream can): that stray byte is dropped, as it is
// from a one-part WAV, and it never becomes a part of its own -- not even when it is all
// that is left after the last full part.
func TestSplitPCMToWAVPartsDropsAStrayTrailingByte(t *testing.T) {
	odd := speechPCM(2501, 11)
	sizes, joined := dataSizes(t, splitPCMToWAVParts(odd, partsTestRate, 1000))
	if want := []int{1000, 1000, 500}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, odd[:2500]) {
		t.Fatal("the parts' data chunks are not the input PCM minus its stray byte")
	}

	// The stray byte would be a part of its own: a header over no samples.
	strayOnly := speechPCM(2001, 13)
	sizes, joined = dataSizes(t, splitPCMToWAVParts(strayOnly, partsTestRate, 1000))
	if want := []int{1000, 1000}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v (no part for the stray byte)", sizes, want)
	}
	if !bytes.Equal(joined, strayOnly[:2000]) {
		t.Fatal("the parts' data chunks are not the input PCM minus its stray byte")
	}
}

// A part target that is not a whole number of samples cuts at the sample below it, so a
// part never ends mid-sample and never holds more than it was asked to.
func TestSplitPCMToWAVPartsCutsOnWholeSamples(t *testing.T) {
	pcm := speechPCM(2500, 17)
	sizes, joined := dataSizes(t, splitPCMToWAVParts(pcm, partsTestRate, 1001))
	if want := []int{1000, 1000, 500}; !slices.Equal(sizes, want) {
		t.Fatalf("part data sizes = %v, want %v", sizes, want)
	}
	if !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the input PCM")
	}
}

// A part target below one sample cannot hold anything; the split still terminates and
// yields one-sample parts instead of looping or dropping the clip.
func TestSplitPCMToWAVPartsClampsATargetBelowOneSample(t *testing.T) {
	pcm := speechPCM(6, 19)
	for _, partBytes := range []int{1, 0, -4} {
		sizes, joined := dataSizes(t, splitPCMToWAVParts(pcm, partsTestRate, partBytes))
		if want := []int{2, 2, 2}; !slices.Equal(sizes, want) {
			t.Fatalf("partBytes %d: part data sizes = %v, want %v", partBytes, sizes, want)
		}
		if !bytes.Equal(joined, pcm) {
			t.Fatalf("partBytes %d: the parts' data chunks are not the input PCM", partBytes)
		}
	}
}

// No audio is no parts -- nil, so the caller's "produced nothing" check is one test, not a
// header-only WAV that would be stored and played as a silent blob. That includes a clip
// of less than one sample.
func TestSplitPCMToWAVPartsOfNoAudioIsNil(t *testing.T) {
	for name, pcm := range map[string][]byte{"nil": nil, "empty": {}, "half a sample": {0x7f}} {
		if wavs := splitPCMToWAVParts(pcm, partsTestRate, 1000); wavs != nil {
			t.Fatalf("%s: got %d parts, want nil", name, len(wavs))
		}
	}
}

// The per-part target is derived from the per-blob cap so that a part can never itself trip
// it: half the cap leaves the WAV header, the base64 expansion's headroom and the rest.
func TestSpeechPartBytesIsDerivedFromTheBlobCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int
		want int
	}{
		{"a small cap is halved", 4 << 20, 2 << 20},
		{"the production default cap leaves the default part size", 20 << 20, DefaultAudioPartBytes},
		{"a large cap is held to the default part size", 1 << 30, DefaultAudioPartBytes},
		{"an odd half rounds down to a whole sample", 4<<20 + 6, 2<<20 + 2},
		{"a cap below a sample's worth still yields one sample", 3, speechStreamBytesPerFrame},
		{"a zero cap still yields one sample", 0, speechStreamBytesPerFrame},
		{"a negative cap still yields one sample", -1, speechStreamBytesPerFrame},
	} {
		got := speechPartBytes(tc.cap)
		if got != tc.want {
			t.Errorf("%s: speechPartBytes(%d) = %d, want %d", tc.name, tc.cap, got, tc.want)
		}
		if got%speechStreamBytesPerFrame != 0 {
			t.Errorf("%s: speechPartBytes(%d) = %d is not a whole number of samples", tc.name, tc.cap, got)
		}
	}
	if DefaultAudioPartBytes != 8<<20 {
		t.Errorf("DefaultAudioPartBytes = %d, want 8 MiB", DefaultAudioPartBytes)
	}
}

// End to end for the cap: a clip several parts long, cut at the target derived from a 4 MiB
// cap, yields parts that each fit the cap with their header included.
func TestSplitPCMToWAVPartsFitTheBlobCapTheyAreDerivedFrom(t *testing.T) {
	const blobCap = 4 << 20
	partBytes := speechPartBytes(blobCap)
	pcm := speechPCM(partBytes*2+partBytes/2+2, 23)

	wavs := splitPCMToWAVParts(pcm, partsTestRate, partBytes)

	if len(wavs) != 3 {
		t.Fatalf("got %d parts, want 3", len(wavs))
	}
	for i, wav := range wavs {
		if len(wav) > blobCap-wavHeaderLen {
			t.Errorf("part %d is %d bytes, over the cap less a header (%d)", i, len(wav), blobCap-wavHeaderLen)
		}
	}
	if _, joined := dataSizes(t, wavs); !bytes.Equal(joined, pcm) {
		t.Fatal("the parts' data chunks put end to end are not the input PCM")
	}
}

// The content array the turn is committed with: one standard audio_url part per WAV, in
// order, each a data: URL of that part's bytes.
func TestMarshalSpeechPartsEmitsOneAudioPartPerWAVInOrder(t *testing.T) {
	wavs := splitPCMToWAVParts(speechPCM(2500, 29), partsTestRate, 1000)
	if len(wavs) != 3 {
		t.Fatalf("setup: got %d parts, want 3", len(wavs))
	}

	raw, err := marshalSpeechParts(wavs)
	if err != nil {
		t.Fatalf("marshalSpeechParts: %v", err)
	}

	urls := audioPartURLs(t, raw)
	if len(urls) != len(wavs) {
		t.Fatalf("got %d audio parts, want %d: %s", len(urls), len(wavs), raw)
	}
	for i, url := range urls {
		payload, ok := strings.CutPrefix(url, "data:audio/wav;base64,")
		if !ok {
			t.Fatalf("part %d: url %.60q is not a data:audio/wav;base64 url", i, url)
		}
		got, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("part %d: payload is not standard padded base64: %v", i, err)
		}
		if !bytes.Equal(got, wavs[i]) {
			t.Errorf("part %d is not the %d-th WAV", i, i)
		}
	}

	// The shape is the documented one, byte for byte: `type` first, then the nested url.
	parts := make([]speechAudioPart, len(wavs))
	for i, wav := range wavs {
		if parts[i], err = speechPartFrom(speechAudioMediaType, wav); err != nil {
			t.Fatal(err)
		}
	}
	want, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("the array is not the marshalled speechAudioParts:\n got %.160s\nwant %.160s", raw, want)
	}
}

// A single WAV is a one-element array, the shape the single-part turn has always had.
func TestMarshalSpeechPartsOfOneWAVIsASinglePartArray(t *testing.T) {
	wav := wavFromPCM16(speechPCM(100, 31), partsTestRate)
	raw, err := marshalSpeechParts([][]byte{wav})
	if err != nil {
		t.Fatalf("marshalSpeechParts: %v", err)
	}
	part, err := speechPartFrom(speechAudioMediaType, wav)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal([]speechAudioPart{part})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("got %.120s, want %.120s", raw, want)
	}
}

// An empty WAV is not audio: the error is speechPartFrom's own terminal code, passed
// through, not a part with an empty payload.
func TestMarshalSpeechPartsRefusesAnEmptyWAV(t *testing.T) {
	raw, err := marshalSpeechParts([][]byte{wavFromPCM16(speechPCM(100, 37), partsTestRate), nil})
	if err == nil {
		t.Fatalf("an empty part marshalled to %s, want an error", raw)
	}
	if err.Error() != speechRunNoAudioMessage {
		t.Fatalf("error = %q, want %q", err, speechRunNoAudioMessage)
	}
}

// No WAVs is an empty array, not JSON null: a consumer walking the parts finds none rather
// than a document that is not an array.
func TestMarshalSpeechPartsOfNoWAVsIsAnEmptyArray(t *testing.T) {
	for _, wavs := range [][][]byte{nil, {}} {
		raw, err := marshalSpeechParts(wavs)
		if err != nil {
			t.Fatalf("marshalSpeechParts: %v", err)
		}
		if string(raw) != "[]" {
			t.Fatalf("got %s, want []", raw)
		}
	}
}
