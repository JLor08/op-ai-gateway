// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"op-ai-gateway/internal/usage"
	"strings"
)

// The frames of the event stream audio.cpp answers a streamed speech request with
// (OpenAI-style SSE): a speech.audio.delta per chunk, its PCM base64-encoded in the
// `audio` field, a speech.audio.done that states a time to first token and NO
// duration, then the [DONE] sentinel (sseDoneSentinel). With stream_format=audio the
// body is instead headerless raw PCM under application/octet-stream.
const (
	speechEventDelta = "speech.audio.delta"
	speechEventDone  = "speech.audio.done"
)

// The two media types audio.cpp answers a streamed speech request in: the event stream
// (stream_format=sse, its default) and raw PCM (stream_format=audio). They, and nothing
// else, make an answer a stream (newSpeechStreamScan).
const (
	speechStreamSSEType = "text/event-stream"
	speechStreamRawType = "application/octet-stream"
)

// speechStreamMaxLine bounds one SSE line the scan holds between chunks. A delta line
// is the base64 of one PCM chunk -- a fraction of a second of audio in practice -- so
// the bound is generous (16 MiB is about two minutes of 44.1 kHz mono PCM in ONE
// frame); a line past it is given up whole, and because what it carried was audio
// that the client was relayed and nobody counted, the stream then fails closed
// (speechStreamScan.complete).
const speechStreamMaxLine = 16 << 20

// speechStreamScan is the tee a streamed speech response is relayed through. It is
// transcriptionStreamScan's sibling and is fed the same way: nativeCopier gives it every
// chunk AFTER that chunk has been written to the client and flushed, so it only ever
// looks -- it never holds a byte back, and the stream is as unbuffered as it was
// without it. It answers the two things the relay cannot learn any other way, because
// a stream carries no duration and no usage object:
//
//   - how much audio was relayed (pcmBytes): the sum of the decoded `audio` payloads of
//     the speech.audio.delta events of an event stream, or -- for the raw shape, whose
//     body IS the audio -- the bytes of the body. It counts what the client was sent,
//     never what a request asked for or what the upstream announced.
//   - whether the stream COMPLETED (complete): for an event stream a speech.audio.done
//     event followed by [DONE]; for raw PCM, which has no terminal frame, the upstream
//     closing the body cleanly (cleanClose, which the copier calls at a clean EOF and
//     never on a read error). A stream cut anywhere before that still ends, from the
//     relay's side, in a status that says "success" when the body simply stops, so the
//     only evidence that the audio was delivered whole is that its end arrived.
//     Billing is decided on that (setStreamedSpeechBillingQuantity).
//
// A count the scan could not make whole is never offered as one: an event it could not
// decode, a delta whose audio is not base64, a line dropped for its length each mark
// the scan lost, and a lost scan is not complete -- the bytes the client was sent are
// then more than the bytes counted, and billing the count would be a silent under-bill.
//
// A nil *speechStreamScan is a scan that sees nothing: a relay that is not a streamed
// speech call carries none. Every method nativeCopier and proxyNative call on it -- feed,
// finish, cleanClose, complete, pcmBytes and stampCapture -- is nil-safe; the helpers
// behind them are reached only through those and are not.
//
// Frames are read line by line, as usageScanner and transcriptionStreamScan read them:
// a frame can straddle two chunks, so the unterminated tail of a chunk is carried to the
// next, bounded by maxLine.
type speechStreamScan struct {
	// raw is the shape of the body: headerless PCM (true) or an event stream (false).
	raw bool
	// maxLine bounds one line held between chunks (event stream only).
	maxLine int

	carry    []byte // the unterminated tail of the chunks so far
	skipping bool   // inside a line dropped for its length: discard up to its newline

	n       int64 // PCM bytes counted
	lost    bool  // audio relayed that n does not account for
	sawDone bool  // a speech.audio.done event was seen
	sawEnd  bool  // [DONE] was seen AFTER it
	closed  bool  // raw: the upstream closed the body cleanly
}

// newSpeechStreamScan returns the scan for a 2xx answer to a streamed speech request,
// keyed on the answer's own Content-Type: an event stream or raw PCM. Any other answer
// -- the one-shot audio/wav of an upstream that did not stream, a JSON document -- is no
// stream and gets no scan (nil), and is relayed and billed as the document it is.
func newSpeechStreamScan(header http.Header) *speechStreamScan {
	mediaType, _, err := mime.ParseMediaType(header.Get(contentTypeHeader))
	if err != nil {
		return nil
	}
	switch mediaType {
	case speechStreamSSEType:
		return &speechStreamScan{maxLine: speechStreamMaxLine}
	case speechStreamRawType:
		return &speechStreamScan{raw: true}
	}
	return nil
}

// feed takes one chunk of the stream, as it came off the upstream.
func (s *speechStreamScan) feed(chunk []byte) {
	if s == nil {
		return
	}
	if s.raw {
		s.n += int64(len(chunk))
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
func (s *speechStreamScan) finish() {
	if s == nil || s.raw {
		return
	}
	if !s.skipping && len(s.carry) > 0 {
		s.scanLine(s.carry)
	}
	s.carry = nil
}

// cleanClose records that the upstream closed the body cleanly (the copier read io.EOF).
// It is the raw shape's only terminal signal; an event stream's completion is its own
// terminal frames, which a clean close does not stand in for.
func (s *speechStreamScan) cleanClose() {
	if s == nil {
		return
	}
	s.closed = true
}

// hold carries the unterminated tail of a chunk to the next one, or -- when the line
// has outgrown maxLine -- gives it up and discards the rest of it.
func (s *speechStreamScan) hold(part []byte) {
	if s.skipping || len(part) == 0 {
		return
	}
	if len(s.carry)+len(part) > s.maxLine {
		s.carry, s.skipping, s.lost = nil, true, true
		return
	}
	s.carry = append(s.carry, part...)
}

// endLine finishes a line: tail is what came before its newline in this chunk, and
// whatever was carried is its beginning.
func (s *speechStreamScan) endLine(tail []byte) {
	switch {
	case s.skipping:
		s.skipping = false
	case len(s.carry) == 0:
		s.scanLine(tail)
	case len(s.carry)+len(tail) > s.maxLine:
		s.carry, s.lost = nil, true
	default:
		s.carry = append(s.carry, tail...)
		s.scanLine(s.carry)
		s.carry = nil
	}
}

// scanLine reads one line: only a `data:` line carries anything, and what it carries is
// either the [DONE] sentinel or a JSON event; every other line (a comment, an `event:`
// field, a blank separator) and every event of another type is ignored.
func (s *speechStreamScan) scanLine(line []byte) {
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
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		// A frame that is JSON in shape and cannot be read -- cut mid-frame, or malformed --
		// may be a delta, and a delta's audio would go uncounted.
		s.lost = true
		return
	}
	switch event.Type {
	case speechEventDelta:
		s.addDelta(event.Audio)
	case speechEventDone:
		s.sawDone = true
	}
}

// addDelta counts the PCM one delta event carries: its `audio` payload decoded from
// base64, measured as it is decoded so a chunk is never held a second time. An absent or
// empty payload is an empty delta, nothing to count; one that is not base64 cannot be
// counted at all, and the scan is lost.
func (s *speechStreamScan) addDelta(audio string) {
	if audio == "" {
		return
	}
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(audio)))
	if err != nil {
		s.lost = true
		return
	}
	s.n += n
}

// complete reports whether the stream ended the way a synthesis that was delivered
// whole does, and the count can be trusted: an event stream with its done event and then
// [DONE], raw PCM closed cleanly by the upstream -- and in both cases nothing lost.
func (s *speechStreamScan) complete() bool {
	if s == nil || s.lost {
		return false
	}
	if s.raw {
		return s.closed
	}
	return s.sawDone && s.sawEnd
}

// pcmBytes is the number of PCM bytes the stream carried, as far as the scan counted.
func (s *speechStreamScan) pcmBytes() int64 {
	if s == nil {
		return 0
	}
	return s.n
}

// stampCapture stamps the capture of a finished streamed speech call as speech's. The
// stream is audio output and is captured as the redacted binary summary every speech
// response gets (ADR-061(h)), not as the event text and not as an assembled transcript:
// the base64 audio is no text a reader of the capture can use. The stamp is what makes
// that decision explicit for a body whose Content-Type is text/event-stream, which the
// summary's Content-Type test alone would store as text: isBinarySpeechResponse then
// redacts every 2xx of the speech endpoint. Nil-safe for both arguments (no scan, or no
// capture).
func (s *speechStreamScan) stampCapture(in *captureInput) {
	if s == nil || in == nil {
		return
	}
	in.Endpoint = endpointSpeech
}

// setStreamedSpeechBillingQuantity records, on meta, the measure of a finished STREAMED
// speech call -- and does nothing at all when there is no scan (the call was not a
// stream: setSpeechBillingQuantity is the setter of a one-shot WAV, whose duration is a
// response header).
//
// A stream states no duration, so what a clean completion bills is what was relayed:
// the PCM bytes, in audio_bytes (usage.BillingUnitAudioBytes). Every other outcome is a
// zero in the endpoint's own unit, audio_second -- the unit every refusal and failed
// relay of this endpoint carries (ADR-041(c)), so a failed streamed request is never
// mislabelled -- and the quantity is never a partial count: a stream cut short, torn
// down, or ended without its terminal events delivered some audio and billed none, which
// is a loud zero and never an over- or under-bill of what a failed relay may or may not
// have produced.
//
// The loud part (ADR-041) is said once at Error, with enough to find the request, for the
// two cases whose row would otherwise look like a success that billed nothing: a stream
// that ended without its terminal events or with a count that could not be made whole,
// and one that completed having carried no audio. A row that is already an error row
// (status != "success": an idle timeout, a copy error, a client that went away) carries
// its cause in its own code and in nativeTerminalStatus's own log line, and logging it
// again here would make every closed player an Error.
//
// ex is read only for that log line. The unit is chosen here from what the stream
// delivered rather than from the endpoint alone; that is ADR-068's one amendment of
// ADR-041(c), confined to the success path.
func setStreamedSpeechBillingQuantity(meta *usageMeta, ex nativeExchange, scan *speechStreamScan, status string) {
	if scan == nil {
		return
	}
	meta.BillingUnit, meta.BillingQuantity = usage.BillingUnitAudioSecond, 0
	if status != "success" {
		return
	}
	if !scan.complete() {
		slog.Error("streamed speech call ended without a completion the gateway can count: billed zero", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName, "shape", scan.shape(), "saw_done", scan.sawDone, "saw_end", scan.sawEnd, "closed", scan.closed, "lost", scan.lost, "pcm_bytes", scan.pcmBytes())
		return
	}
	bytesRelayed := scan.pcmBytes()
	if bytesRelayed == 0 {
		slog.Error("streamed speech call completed with no audio: billed zero", "id", ex.id, "path", meta.ReqPath, "model", ex.req.Model, "server", ex.serverName, "shape", scan.shape())
		return
	}
	// A clean completion: what was relayed is what is billed. The measure is stated here
	// and nowhere else, so a better one (a duration, once the output byte rate is known)
	// replaces these two assignments and nothing around them.
	meta.BillingUnit, meta.BillingQuantity = usage.BillingUnitAudioBytes, float64(bytesRelayed)
}

// shape names the stream's shape for a log line.
func (s *speechStreamScan) shape() string {
	if s.raw {
		return "raw"
	}
	return "sse"
}
