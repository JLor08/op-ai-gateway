// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
)

// The frames of the event stream a streamed speech request is answered with (OpenAI-style
// SSE): a speech.audio.delta per chunk, its PCM base64-encoded in the `audio` field, then
// a speech.audio.done that states NO duration. audio.cpp's done event states a time to
// first token and is followed by the [DONE] sentinel (sseDoneSentinel). sglang-omni's
// (v0.2.0) states token usage, and its body then simply ends: it writes no [DONE], and on
// a failure it writes an `error` event in the done event's place. The relay writes the
// sentinel for it once the body has closed cleanly after the done event
// (speechStreamScan.endFrame, ADR-103), so every speech event stream the gateway relays
// ends the same way. With stream_format=audio the body is instead headerless raw PCM
// under application/octet-stream (audio/pcm from sglang-omni).
const (
	speechEventDelta = "speech.audio.delta"
	speechEventDone  = "speech.audio.done"
)

// speechStreamEndFrame is the [DONE] frame the relay ends an sglang-omni event stream
// with. endFrame puts one or two newlines ahead of it, so it is always an event of its own.
const speechStreamEndFrame = "data: " + sseDoneSentinel + "\n\n"

// The response headers sglang-omni (v0.2.0) states the PCM format of a streamed synthesis
// in, on both shapes: the sample rate, the channel count and the bits a sample. Together
// they are the stream's byte rate (speechHeaderByteRate), which the billing of a stream
// falls back to when the mapping has no cached rate.
const (
	speechSampleRateHeader = "X-Sample-Rate"
	speechChannelsHeader   = "X-Channels"
	speechBitDepthHeader   = "X-Bit-Depth"
)

// The media types a streamed speech request is answered in: the event stream
// (audio.cpp's stream_format=sse, its default) and raw PCM, which audio.cpp labels
// application/octet-stream (stream_format=audio) and sglang-omni audio/pcm (v0.2.0
// sglang_omni/serve/openai_api.py; its stream_format defaults to "audio"). They, and
// nothing else, make an answer a stream (newSpeechStreamScan).
const (
	speechStreamSSEType    = "text/event-stream"
	speechStreamRawType    = "application/octet-stream"
	speechStreamRawPCMType = "audio/pcm"
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
// sglang-omni's event stream has no [DONE] of its own: its body ends after the done event.
// For it the scan also answers the one thing the relay WRITES rather than relays -- the
// sentinel that stream lacks (endFrame), which the copier sends the client at a clean EOF
// after the done event and feeds back through the scan like any chunk. The completion rule
// above is unchanged by it: what completes the stream is still the done event and then
// [DONE], and the gateway writes that [DONE] only for a stream that is done. And because
// sglang-omni states its PCM format in response headers, the scan keeps the byte rate they
// add up to (headerRate), which the billing falls back to when the mapping has no cached
// rate. Every other upstream gets neither: audio.cpp's stream is relayed exactly as it was.
//
// A count the scan could not make whole is never offered as one: an event it could not
// decode, a delta whose audio is not base64, a line dropped for its length each mark
// the scan lost, and a lost scan is not complete -- the bytes the client was sent are
// then more than the bytes counted, and billing the count would be a silent under-bill.
//
// A nil *speechStreamScan is a scan that sees nothing: a relay that is not a streamed
// speech call carries none. Every method nativeCopier and proxyNative call on it -- feed,
// finish, cleanClose, endFrame, complete, pcmBytes and stampCapture -- is nil-safe; the
// helpers behind them are reached only through those and are not.
//
// Frames are read line by line, as usageScanner and transcriptionStreamScan read them:
// a frame can straddle two chunks, so the unterminated tail of a chunk is carried to the
// next, bounded by maxLine.
type speechStreamScan struct {
	// raw is the shape of the body: headerless PCM (true) or an event stream (false).
	raw bool
	// maxLine bounds one line held between chunks (event stream only).
	maxLine int
	// writeEnd marks an event stream from sglang-omni, which ends a completed synthesis at
	// its done event with no [DONE]: the relay writes the sentinel itself (endFrame). False
	// for every other upstream, and for the raw shape, which has no terminal frame to write.
	writeEnd bool
	// headerRate is the byte rate the answer's format headers state (speechHeaderByteRate),
	// read for sglang-omni only, on either shape; 0 is none. The billing falls back to it
	// when the mapping has no cached rate.
	headerRate uint32

	carry    []byte // the unterminated tail of the chunks so far
	skipping bool   // inside a line dropped for its length: discard up to its newline
	lastLF   bool   // the last byte fed was a newline (event stream only)

	n           int64 // PCM bytes counted
	lost        bool  // audio relayed that n does not account for
	sawDone     bool  // a speech.audio.done event was seen
	sawEnd      bool  // [DONE] was seen AFTER it
	sawSentinel bool  // [DONE] was seen at all, wherever it came
	closed      bool  // the upstream closed the body cleanly
}

// newSpeechStreamScan returns the scan for a 2xx answer to a streamed speech request,
// keyed on the answer's own Content-Type: an event stream or raw PCM. Any other answer
// -- the one-shot audio/wav of an upstream that did not stream, a JSON document -- is no
// stream and gets no scan (nil), and is relayed and billed as the document it is.
//
// kind is the target's effective kind (routing.Target.EffectiveKind). For sglang-omni the
// scan reads the byte rate its format headers state (headerRate), on either shape, and an
// event stream from it is one the relay ends itself (writeEnd); every other kind gets
// neither.
func newSpeechStreamScan(header http.Header, kind string) *speechStreamScan {
	mediaType, _, err := mime.ParseMediaType(header.Get(contentTypeHeader))
	if err != nil {
		return nil
	}
	omni := kind == string(routing.RuntimeSpecTypeSGLangOmni)
	var scan *speechStreamScan
	switch mediaType {
	case speechStreamSSEType:
		scan = &speechStreamScan{maxLine: speechStreamMaxLine, writeEnd: omni}
	case speechStreamRawType, speechStreamRawPCMType:
		scan = &speechStreamScan{raw: true}
	default:
		return nil
	}
	if omni {
		scan.headerRate = speechHeaderByteRate(header)
	}
	return scan
}

// speechHeaderByteRate returns the bytes a second of the PCM a stream carries, as its
// format headers state it: sample rate times channels times bytes a sample. Each header
// must be a plain decimal positive integer (strconv.ParseUint's base 10: no sign, no
// space, no fraction, no unit), the bit depth a whole number of bytes, and the product a
// usable rate by the one definition a cached rate is held to (routing.ParseOutputByteRate:
// positive and within a uint32). Anything else -- a header missing, a zero, a 12-bit
// depth, a product past the range -- is 0, no rate: a rate is never guessed, because a
// guessed one would bill a wrong duration under a plausible unit.
func speechHeaderByteRate(header http.Header) uint32 {
	sampleRate, okRate := speechFormatHeader(header, speechSampleRateHeader)
	channels, okChannels := speechFormatHeader(header, speechChannelsHeader)
	bitDepth, okDepth := speechFormatHeader(header, speechBitDepthHeader)
	if !okRate || !okChannels || !okDepth || bitDepth%8 != 0 {
		return 0
	}
	// Each factor is below 2^32, so the bytes a frame fit a uint64. A frame past a uint32
	// makes a product past it too (the sample rate is at least 1), so it is refused before
	// the multiplication that could overflow.
	frameBytes := channels * (bitDepth / 8)
	if frameBytes > math.MaxUint32 {
		return 0
	}
	rate, ok := routing.ParseOutputByteRate(strconv.FormatUint(sampleRate*frameBytes, 10))
	if !ok {
		return 0
	}
	return rate
}

// speechFormatHeader reads one format header as a plain decimal positive integer within
// a uint32; false for anything else, an absent header included.
func speechFormatHeader(header http.Header, name string) (uint64, bool) {
	n, err := strconv.ParseUint(header.Get(name), 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
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
	if len(chunk) > 0 {
		s.lastLF = chunk[len(chunk)-1] == '\n'
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

// cleanClose records that the upstream closed the body cleanly (the copier read io.EOF),
// and reads the line the body may have left unterminated as its last (finish), so the
// scan has judged the whole of what the upstream sent before endFrame is asked. A clean
// close is the raw shape's only terminal signal; an event stream's completion is its own
// terminal frames, which a clean close does not stand in for -- for sglang-omni it is only
// what lets the relay write the [DONE] its stream lacks (endFrame).
func (s *speechStreamScan) cleanClose() {
	if s == nil {
		return
	}
	s.finish()
	s.closed = true
}

// endFrame returns the frame the relay writes to end an sglang-omni event stream, or nil
// when it writes none. It is the [DONE] that stream lacks, and it is written only for a
// stream that is done: the upstream closed the body cleanly (cleanClose) after a
// speech.audio.done event, sent no [DONE] of its own, anywhere, and nothing of it was
// lost. Every other stream -- one that ended in an error event, one cut or ended before
// its done event, one whose audio could not be counted, any stream from another upstream,
// and the raw shape -- gets nothing, and stays the incomplete stream it is.
//
// The frame is always an event of its own: after a last byte that is a newline one more
// newline ends whatever event the upstream left open, and after any other byte two do, so
// the sentinel never fuses into the data of a frame the upstream left unterminated. An
// extra blank line is a no-op in SSE.
func (s *speechStreamScan) endFrame() []byte {
	if s == nil || !s.writeEnd || !s.closed || !s.sawDone || s.sawSentinel || s.lost {
		return nil
	}
	if s.lastLF {
		return []byte("\n" + speechStreamEndFrame)
	}
	return []byte("\n\n" + speechStreamEndFrame)
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
		// done event is not a completion. Either way the stream has its sentinel, and the
		// relay writes no second one (endFrame).
		s.sawEnd = s.sawEnd || s.sawDone
		s.sawSentinel = true
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
// A stream states no duration, so what a clean completion bills is what was relayed. When
// the mapping's output byte rate is cached (cachedOutputByteRate: what its speech
// benchmark measured) that is a duration, the PCM bytes over the bytes a second, in the
// endpoint's own unit, audio_second -- the unit a one-shot call bills in too. When it is
// not, but the stream itself stated its PCM format in usable headers (scan.headerRate,
// sglang-omni's X-Sample-Rate, X-Channels and X-Bit-Depth), that rate makes it a duration
// the same way; the cached rate goes first because it is this mapping's measured figure.
// When neither is known, it is the PCM bytes themselves, in audio_bytes
// (usage.BillingUnitAudioBytes): the rate is never guessed, because a guessed one would
// bill a wrong duration under a plausible unit. Every other outcome is a zero in the
// endpoint's own unit, audio_second -- the unit every refusal and failed relay of this
// endpoint carries (ADR-041(c)), so a failed streamed request is never mislabelled -- and
// the quantity is never a partial count: a stream cut short, torn down, or ended without
// its terminal events delivered some audio and billed none, which is a loud zero and
// never an over- or under-bill of what a failed relay may or may not have produced.
//
// The loud part (ADR-041) is said once at Error, with enough to find the request, for the
// two cases whose row would otherwise look like a success that billed nothing: a stream
// that ended without its terminal events or with a count that could not be made whole,
// and one that completed having carried no audio. A row that is already an error row
// (status != "success": an idle timeout, a copy error, a client that went away) carries
// its cause in its own code and in nativeTerminalStatus's own log line, and logging it
// again here would make every closed player an Error.
//
// ex names the request for that log line and the mapping (ex.target.RouteID) whose
// cached rate is read. The unit is chosen here from what the stream delivered, and from
// whether its rate is known, rather than from the endpoint alone; that is ADR-068's one
// amendment of ADR-041(c), confined to the success path.
//
// This runs once the stream has ended. A client that went away is not a case this billing
// ever sees as a success: a disconnect seen when the stream terminates, mid-stream or
// right after the final frame -- for sglang-omni, the [DONE] the relay writes itself
// (speechStreamScan.endFrame), whose failed write is the copy's error -- has already made
// the status provider.client_disconnected (nativeTerminalStatus decides it as soon as the
// copy returns, before this setter reads the rate), and the status != "success" return
// below turns it into the error row (audio_second, 0), never a billed stream. The rate
// read is on a context that survives a cancelled ctx (context.WithoutCancel) purely
// defensively: it covers only the window after that status decision, so a request context
// cancelled between the decision and the store read cannot make the read fail and drop a
// stream already judged a success to bytes.
func (s *Server) setStreamedSpeechBillingQuantity(ctx context.Context, meta *usageMeta, ex nativeExchange, scan *speechStreamScan, status string) {
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
	// and nowhere else -- a duration when the rate that turns bytes into one is cached or
	// stated by the stream's headers, the bytes themselves when neither is known.
	if rate, ok := s.cachedOutputByteRate(context.WithoutCancel(ctx), ex.target.RouteID); ok {
		meta.BillingUnit, meta.BillingQuantity = usage.BillingUnitAudioSecond, float64(bytesRelayed)/float64(rate)
		return
	}
	if scan.headerRate > 0 {
		meta.BillingUnit, meta.BillingQuantity = usage.BillingUnitAudioSecond, float64(bytesRelayed)/float64(scan.headerRate)
		return
	}
	meta.BillingUnit, meta.BillingQuantity = usage.BillingUnitAudioBytes, float64(bytesRelayed)
}

// shape names the stream's shape for a log line.
func (s *speechStreamScan) shape() string {
	if s.raw {
		return "raw"
	}
	return "sse"
}
