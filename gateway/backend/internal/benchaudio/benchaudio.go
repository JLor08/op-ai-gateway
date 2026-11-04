// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package benchaudio is the speech the gateway's ASR benchmark transcribes: one bundled
// clip of real German speech, its transcript, and the language to ask for.
//
// The clip is embedded in the binary rather than synthesized or fetched at run time, so
// every run of every gateway measures the same audio -- an ASR rate is only comparable
// between models and between runs when the input is fixed -- and a benchmark needs no
// network access and no TTS model to have something to transcribe. It is six sentences
// of the Thorsten-Voice dataset (CC0 1.0), joined with short pauses and stored as 16 kHz
// mono 16-bit PCM WAV, the shape audio.cpp's live endpoint takes as is; SOURCE.md next
// to it names the recordings and their licence.
package benchaudio

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Language is the language of the clip, as the transcription request states it: the
// clip is German, and a model left to detect the language would add that detection
// (and its mistakes) to what is measured.
const Language = "de"

//go:embed asr-benchmark-de.wav
var wav []byte

//go:embed asr-benchmark-de.txt
var transcript string

// WAV is the clip as a WAV file, for a buffered transcription request. Every call
// returns its own copy, so a caller that writes into it cannot change the clip for the
// next one.
func WAV() []byte {
	return bytes.Clone(wav)
}

// Transcript is what the clip says, without the file's trailing line break.
func Transcript() string {
	return strings.TrimSpace(transcript)
}

// PCM is the clip's audio as headerless samples, for a live transcription, with the
// format they are in: 16-bit little-endian integer samples (s16le) at sampleRate Hz in
// channels interleaved channels. Like WAV, every call returns its own copy.
//
// The shape is read from the WAV itself rather than assumed, so the format a live
// request states is the format the samples are in. The bundled clip is pinned by the
// package's tests to 16000 Hz mono; if it could not be read, PCM returns no audio and a
// 0 Hz rate, which a measurement refuses before it sends anything.
func PCM() (pcm []byte, sampleRate, channels int) {
	return pcmOf(wav)
}

// pcmOf is PCM for any WAV: a copy of its audio and its format, or nothing at all when
// the WAV cannot be read (parsePCM).
func pcmOf(w []byte) (pcm []byte, sampleRate, channels int) {
	c, err := parsePCM(w)
	if err != nil {
		return nil, 0, 0
	}
	return bytes.Clone(c.pcm), c.sampleRate, c.channels
}

// pcmClip is the audio of a WAV and the format its `fmt ` chunk states. pcm aliases the
// WAV it was read from.
type pcmClip struct {
	pcm                  []byte
	sampleRate, channels int
}

// The RIFF/WAVE layout parsePCM reads: a 12-byte header ("RIFF", the size, "WAVE"),
// then chunks of an 8-byte header (a four-character id, the body's size) and the body,
// padded to an even length. The `fmt ` body's first 16 bytes are fixed (format tag,
// channels, sample rate, byte rate, block align, bits per sample); anything after them
// is an extension.
const (
	riffHeaderSize  = 12
	chunkHeaderSize = 8
	fmtFixedSize    = 16
	formatTagPCM    = 1
	pcmBitsS16      = 16
)

// parsePCM walks a WAV's chunks to its audio: the `fmt ` chunk for the format and the
// first `data` chunk after it for the samples. It walks rather than reading at fixed
// offsets because a WAV may carry other chunks (LIST, JUNK, fact, ...) before or
// between the two, and a `fmt ` longer than its 16 fixed bytes; the conventional
// 44-byte header is only the case where it carries none.
//
// It accepts only what the live endpoint is told the samples are -- integer PCM of 16
// bits a sample, at least one channel, a positive sample rate -- with the format stated
// before the audio, every chunk up to the audio whole, and the audio a whole number of
// frames; anything else is an error, never audio of a guessed shape.
func parsePCM(w []byte) (pcmClip, error) {
	if len(w) < riffHeaderSize || string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" {
		return pcmClip{}, errors.New("benchaudio: not a RIFF/WAVE file")
	}
	le := binary.LittleEndian
	var c pcmClip
	haveFormat := false
	for off := riffHeaderSize; ; {
		if len(w)-off < chunkHeaderSize {
			return pcmClip{}, errors.New("benchaudio: the WAV has no data chunk")
		}
		id := string(w[off : off+4])
		size := int(le.Uint32(w[off+4:]))
		body := off + chunkHeaderSize
		if size > len(w)-body {
			return pcmClip{}, fmt.Errorf("benchaudio: the %q chunk is cut short", id)
		}
		switch id {
		case "fmt ":
			if err := c.readFormat(w[body : body+size]); err != nil {
				return pcmClip{}, err
			}
			haveFormat = true
		case "data":
			if !haveFormat {
				return pcmClip{}, errors.New("benchaudio: the data chunk comes before the fmt chunk")
			}
			if frame := c.channels * pcmBitsS16 / 8; size%frame != 0 {
				return pcmClip{}, fmt.Errorf("benchaudio: the audio (%d bytes) is not a whole number of %d-byte frames", size, frame)
			}
			c.pcm = w[body : body+size]
			return c, nil
		}
		off = body + size + size%2
	}
}

// readFormat takes the format a `fmt ` chunk's body states, refusing anything that is not
// 16-bit integer PCM with at least one channel and a positive sample rate.
func (c *pcmClip) readFormat(f []byte) error {
	if len(f) < fmtFixedSize {
		return fmt.Errorf("benchaudio: the fmt chunk is %d bytes, shorter than its %d fixed ones", len(f), fmtFixedSize)
	}
	le := binary.LittleEndian
	tag, channels, rate, bits := le.Uint16(f[0:]), le.Uint16(f[2:]), le.Uint32(f[4:]), le.Uint16(f[14:])
	if tag != formatTagPCM || bits != pcmBitsS16 || channels == 0 || rate == 0 {
		return fmt.Errorf("benchaudio: the WAV is format tag %d, %d channel(s), %d-bit at %d Hz; want 16-bit integer PCM", tag, channels, bits, rate)
	}
	c.sampleRate, c.channels = int(rate), int(channels)
	return nil
}
