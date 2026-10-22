// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "encoding/json"

// This file is what lets one long speech clip be committed as several audio parts instead
// of one. Every part of a turn is stored as its own chat blob and must pass the per-blob
// cap by itself, so a clip longer than the cap allows is cut into ordered, self-describing
// WAVs -- each a whole, playable file -- and the turn's content array carries them in
// order. The helpers here are pure: they cut bytes and shape JSON, and know nothing of
// the run, the stream or the store.

// DefaultAudioPartBytes is the most 16-bit PCM one audio part holds by default: 8 MiB,
// about 95 seconds at 44.1 kHz mono. It is the target a part is cut at when the per-blob
// cap is generous; speechPartBytes lowers it when the cap is not.
const DefaultAudioPartBytes = 8 << 20

// speechPartBytes is the PCM size one part of a turn is cut at under a per-blob cap of
// maxChatBlobBytes: DefaultAudioPartBytes, or half the cap when that is less, in whole
// samples and never below one. Half is the margin that keeps a part from tripping the cap
// it is derived from: the part's WAV is its PCM plus the 44-byte header, so it stays
// under the cap with room to spare for whatever the cap is measured against.
func speechPartBytes(maxChatBlobBytes int) int {
	n := min(DefaultAudioPartBytes, maxChatBlobBytes/2)
	n -= n % speechStreamBytesPerFrame
	return max(n, speechStreamBytesPerFrame)
}

// splitPCMToWAVParts cuts pcm (16-bit little-endian mono, at sampleRate) into consecutive
// WAVs, each wrapping at most partBytes of it, in whole samples: every part but the last
// holds partBytes (less the byte of a half sample when partBytes is odd), the last the
// remainder, so the parts' audio put end to end is pcm. Each part is a complete file of
// its own -- wavFromPCM16's header over its slice -- not a fragment, because each is
// stored and served as one.
//
// A trailing half sample of pcm is dropped, as wavFromPCM16 drops it from a one-part WAV,
// and it is dropped first: left in, it could be a part of its own, a header over no audio.
// No audio -- empty pcm, or less than one sample -- is nil, so the caller's "produced
// nothing" check is one test. A partBytes below one sample cannot be honored and is taken
// as one, which keeps the cut terminating.
func splitPCMToWAVParts(pcm []byte, sampleRate, partBytes int) [][]byte {
	pcm = pcm[:len(pcm)-len(pcm)%speechStreamBytesPerFrame]
	if len(pcm) == 0 {
		return nil
	}
	step := max(partBytes-partBytes%speechStreamBytesPerFrame, speechStreamBytesPerFrame)
	wavs := make([][]byte, 0, (len(pcm)+step-1)/step)
	for len(pcm) > 0 {
		n := min(step, len(pcm))
		wavs = append(wavs, wavFromPCM16(pcm[:n], sampleRate))
		pcm = pcm[n:]
	}
	return wavs
}

// marshalSpeechParts is the content array of a speech turn: one speechAudioPart per WAV,
// in order, as speechPartFrom builds it for the single-part turn (so the data: URL, its
// media type and its base64 alphabet are the ones every consumer already reads). An empty
// WAV is speechPartFrom's error, its terminal code returned as it is. No WAVs marshal to
// an empty array, never null.
func marshalSpeechParts(wavs [][]byte) (json.RawMessage, error) {
	parts := make([]speechAudioPart, 0, len(wavs))
	for _, wav := range wavs {
		part, err := speechPartFrom(speechAudioMediaType, wav)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return json.Marshal(parts)
}
