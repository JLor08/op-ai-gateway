// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
)

// The frames of the event stream audio.cpp answers a streamed transcription with
// (OpenAI-style SSE): a transcript.text.delta per increment, a transcript.text.done
// that restates the whole text, then the [DONE] sentinel.
const (
	transcriptionEventDelta = "transcript.text.delta"
	transcriptionEventDone  = "transcript.text.done"
	sseDoneSentinel         = "[DONE]"
)

// transcriptionStreamScan is the SSE-aware tee a streamed transcription is relayed
// through. nativeCopier feeds it every chunk AFTER that chunk has been written to the
// client and flushed, so it only ever looks: it never holds a byte back, and the stream
// is as unbuffered as it was without it. What it reads is two things the relay cannot
// get any other way:
//
//   - whether the stream COMPLETED: a transcript.text.done event followed by [DONE]. A
//     stream cut anywhere before that -- the upstream's connection drops, or its
//     process dies after the 200 -- still ends, from the relay's side, in a status
//     that says "success" when the body simply stops, so the only evidence that a
//     transcript was delivered whole is that its terminal frames arrived. Billing
//     is decided on that (setStreamedTranscriptionBillingQuantity).
//   - the TRANSCRIPT, for the capture: the done event's text when it states one (the
//     authoritative statement of the whole text), else the deltas assembled so far,
//     which is what a stream that broke off delivered. Kept only when the request is
//     being captured, and bounded by the capture cap (one byte past it, so the capture
//     can tell a clipped transcript from one that fits exactly).
//
// A nil *transcriptionStreamScan is a scan that sees nothing: a relay that is not a
// streamed transcription carries no scan. The methods nativeCopier and proxyNative
// call on it -- feed, finish, complete, transcript and stampCapture -- are nil-safe;
// the helpers behind them are reached only through those and are not.
//
// Frames are read line by line, as usageScanner reads them: a frame can straddle two
// chunks, so the unterminated tail of a chunk is carried to the next. The carry is
// bounded (maxLine) so an upstream that never ends a line cannot make the gateway hold
// it without limit; a line that outgrows the bound while it is being carried is dropped
// whole, which at worst costs the completion it carried -- billing then fails closed,
// to a loud zero.
type transcriptionStreamScan struct {
	// maxLine bounds one line held between chunks. A done frame restates the whole
	// transcript, so it is the transcription cap (what a transcript is ever allowed to
	// be), not the capture cap.
	maxLine int
	// keepText says the transcript is wanted (the request is captured); textCap is the
	// most of it that is kept.
	keepText bool
	textCap  int

	carry    []byte // the unterminated tail of the chunks so far
	skipping bool   // inside a line dropped for its length: discard up to its newline

	deltas   []byte // the deltas' text, in order
	doneText []byte // the done event's text
	sawDone  bool   // a transcript.text.done event was seen
	sawEnd   bool   // [DONE] was seen AFTER it
}

// newTranscriptionStreamScan returns the scan for a relay of endpoint -- nil for every
// endpoint but the transcription's, so the relays that do not stream a transcription
// are not touched. capturing says the request's payload is being captured, and so the
// transcript is wanted.
func (s *Server) newTranscriptionStreamScan(endpoint sessionEndpoint, capturing bool) *transcriptionStreamScan {
	if endpoint != endpointTranscription {
		return nil
	}
	textCap := s.captureMaxBytes
	if textCap <= 0 {
		textCap = defaultCaptureMaxBytes
	}
	return &transcriptionStreamScan{
		maxLine:  int(s.transcriptionBodyLimit()),
		keepText: capturing,
		textCap:  textCap,
	}
}

// feed takes one chunk of the stream, as it came off the upstream.
func (s *transcriptionStreamScan) feed(chunk []byte) {
	if s == nil {
		return
	}
	for {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.hold(chunk)
			return
		}
		s.endLine(chunk[:i])
		chunk = chunk[i+1:]
	}
}

// finish takes the end of the stream: a last line the upstream never terminated is
// still a line (a stream may end on `data: [DONE]` with no newline after it).
func (s *transcriptionStreamScan) finish() {
	if s == nil {
		return
	}
	if !s.skipping && len(s.carry) > 0 {
		s.scanLine(s.carry)
	}
	s.carry = nil
}

// hold carries the unterminated tail of a chunk to the next one, or -- when the line
// has outgrown maxLine -- gives it up and discards the rest of it.
func (s *transcriptionStreamScan) hold(part []byte) {
	if s.skipping || len(part) == 0 {
		return
	}
	if len(s.carry)+len(part) > s.maxLine {
		s.carry, s.skipping = nil, true
		return
	}
	s.carry = append(s.carry, part...)
}

// endLine finishes a line: tail is what came before its newline in this chunk, and
// whatever was carried is its beginning.
func (s *transcriptionStreamScan) endLine(tail []byte) {
	switch {
	case s.skipping:
		s.skipping = false
	case len(s.carry) == 0:
		s.scanLine(tail)
	case len(s.carry)+len(tail) > s.maxLine:
		s.carry = nil
	default:
		s.carry = append(s.carry, tail...)
		s.scanLine(s.carry)
		s.carry = nil
	}
}

// scanLine reads one line: only a `data:` line carries anything, and what it carries
// is either the [DONE] sentinel or a JSON event; every other line (a comment, an
// `event:` field, a blank separator) and every event of another type is ignored.
func (s *transcriptionStreamScan) scanLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if string(payload) == sseDoneSentinel {
		// [DONE] ends a stream that completed; one that arrives without (or ahead of) the
		// done event is not a completion.
		s.sawEnd = s.sawEnd || s.sawDone
		return
	}
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var event struct {
		Type  string  `json:"type"`
		Delta string  `json:"delta"`
		Text  *string `json:"text"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	switch event.Type {
	case transcriptionEventDelta:
		s.addDelta(event.Delta)
	case transcriptionEventDone:
		s.sawDone = true
		if event.Text != nil {
			s.setDoneText(*event.Text)
		}
	}
}

// room is how many more bytes of transcript may be kept after have: one byte past the
// cap, which is what lets the capture report a clipped transcript as clipped.
func (s *transcriptionStreamScan) room(have int) int {
	if !s.keepText {
		return 0
	}
	return max(s.textCap+1-have, 0)
}

func (s *transcriptionStreamScan) addDelta(text string) {
	n := min(len(text), s.room(len(s.deltas)))
	s.deltas = append(s.deltas, text[:n]...)
}

func (s *transcriptionStreamScan) setDoneText(text string) {
	if !s.keepText {
		return
	}
	n := min(len(text), s.room(0))
	s.doneText = append(s.doneText[:0], text[:n]...)
}

// complete reports whether the stream ended the way a transcription that was
// delivered whole does: the done event, then [DONE].
func (s *transcriptionStreamScan) complete() bool {
	return s != nil && s.sawDone && s.sawEnd
}

// transcript is the transcript the stream carried, as far as the capture keeps one:
// the done event's text when it states one (it restates the whole text, and is the
// authoritative statement of it), else the deltas that arrived. An EMPTY done text
// states nothing -- a done event that restates no text must not erase the deltas that
// did arrive -- so the deltas stand in front of it.
func (s *transcriptionStreamScan) transcript() []byte {
	if s == nil {
		return nil
	}
	if len(s.doneText) > 0 {
		return s.doneText
	}
	return s.deltas
}

// stampCapture turns the capture of a finished streamed transcription into that of a
// transcript: the endpoint is stamped, so the capture does not mistake it for speech's
// binary audio (both carry the openai_audio flavor, and isBinarySpeechResponse would
// store a 2xx of that flavor as a redacted summary), and the body is the assembled
// transcript instead of the event framing -- the framing is nothing a reader of the
// capture can use, and the done event restates the whole text, so keeping the frames
// would spend the capture cap twice over. The client was sent the frames themselves;
// that is not what is captured here. Nil-safe for both arguments (no scan, or no
// capture).
func (s *transcriptionStreamScan) stampCapture(in *captureInput) {
	if s == nil || in == nil {
		return
	}
	in.Endpoint = endpointTranscription
	in.RespBody = s.transcript()
}

// setStreamedTranscriptionBillingQuantity records, on meta, how many seconds of audio
// a finished STREAMED transcription transcribed -- and does nothing at all for any
// other endpoint or a relay that did not finish cleanly (status == "success"), leaving
// BillingQuantity at 0 as setTranscriptionBillingQuantity does for a one-shot body.
//
// A stream states no duration (its done event carries a time to first token, not
// audio_duration_ms), so the quantity is the duration of the WAV that was UPLOADED,
// read from its header (uploadWavDurationMillis) -- and is billed only for a stream
// that COMPLETED (scan.complete()): a transcript cut short is not the audio
// transcribed. Every case that cannot be billed is a loud zero (ADR-041), said once at
// Error with enough to find the request, never a silent bare zero:
//
//   - the stream ended without the done event and [DONE];
//   - the upload is no WAV the header of which can be believed (uploadWavDurationMillis).
//
// A clip that measures zero (shorter than a millisecond) is a measurement, not a gap:
// it bills 0 and logs nothing.
//
// contentType is the CLIENT's Content-Type, the one whose boundary ex.raw (always the
// client's body, even when a rewritten one was sent upstream) was written with.
func setStreamedTranscriptionBillingQuantity(meta *usageMeta, ex nativeExchange, contentType string, scan *transcriptionStreamScan, status string) {
	if ex.endpoint != endpointTranscription || status != "success" {
		return
	}
	if !scan.complete() {
		slog.Error("streamed transcription ended without both transcript.text.done and [DONE]: billed zero", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName, "saw_done", scan != nil && scan.sawDone, "saw_end", scan != nil && scan.sawEnd)
		return
	}
	ms, ok := uploadWavDurationMillis(ex.raw, contentType)
	if !ok {
		slog.Error("streamed transcription completed but its upload's duration could not be read from a WAV header: billed zero", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName)
		return
	}
	meta.BillingQuantity = float64(ms) / 1000
}

// uploadWavDurationMillis returns the duration of the audio the client uploaded to a
// transcription: the first non-empty `file` part of the multipart body raw, found with
// the boundary of contentType, measured by wavDurationFromReader. It is false when the
// body is no readable form, has no such part, or the part is no WAV whose header can be
// believed.
//
// The part is read the way validation read it (a part of mime/multipart's reader whose
// FormName is "file"; an empty one is no upload), and measured WHERE IT LIES: the
// reader walks the chunk headers and counts the rest, so the upload -- up to the
// transcription cap -- is never copied a second time while raw is already held.
func uploadWavDurationMillis(raw []byte, contentType string) (int64, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return 0, false
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return 0, false
		}
		if part.FormName() != "file" {
			continue
		}
		// The first byte says whether the part is empty (and so no upload) without
		// reading any more of it.
		var first [1]byte
		if _, err := io.ReadFull(part, first[:]); err != nil {
			if errors.Is(err, io.EOF) {
				continue
			}
			return 0, false
		}
		return wavDurationFromReader(io.MultiReader(bytes.NewReader(first[:]), part))
	}
}
