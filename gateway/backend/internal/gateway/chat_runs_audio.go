// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"op-ai-gateway/internal/auth"
	"slices"
	"strings"
	"time"
)

// This file is the speech kind's half of the run executor, and it is the image
// kind's (chat_runs_images.go) near twin: it shares the reservation, the
// deadline, the header set and the single terminal commit with the text half
// (chat_runs.go) and nothing else, for the same reason -- consumeRunStream
// assumes a stream of deltas, and /v1/audio/speech answers ONE buffered binary
// body, so there is nothing to checkpoint and no rate that could be measured
// rather than invented.
//
// That is the BUFFERED run. When the mapped model streams and its output byte rate
// is known (Server.speechRunStreamRate), the run instead asks for a stream and plays
// it to the client as it arrives; that branch, which commits the same turn, is
// chat_runs_audio_stream.go.
//
// The two differ in the response and in what is reported by whom. The images
// endpoint answers JSON with the format named in the body; the speech endpoint
// answers raw audio/wav with the format named only in the Content-Type header, so
// that header -- not an assumption -- is what the data: URL's media type is
// checked against (speechAudioMediaType).

// speechAudioPart is THE content part a speech turn emits, and the contract its
// consumers read:
//
//	{"type":"audio_url","audio_url":{"url":"data:audio/wav;base64,<standard padded base64>"}}
//
// It is image_url's twin -- same nesting, same `type` first -- so the chat-blob
// extraction that moves an inline image into chat_blobs and the portal renderer
// each add one more part type rather than learning a new document shape. A
// declared type rather than a map literal so the emitted key ORDER is the one the
// shape is documented in, and the shape is stated once here instead of at the
// append site.
type speechAudioPart struct {
	Type     string         `json:"type"`
	AudioURL audioURLTarget `json:"audio_url"`
}

// audioURLTarget is the inner object of speechAudioPart; the field is `url`
// exactly as imageURLTarget's is.
type audioURLTarget struct {
	URL string `json:"url"`
}

const (
	// speechAudioPartType is the `type` of speechAudioPart.
	speechAudioPartType = "audio_url"
	// speechAudioMediaType is the ONE media type a speech turn's data: URL carries.
	// The gateway serves only WAV (validateSpeechRequest refuses every other
	// response_format), and every alias a server may send for it is normalized to
	// this value by speechAudioMediaTypeOf, so the store's allow-list and the
	// renderer key on a single string.
	speechAudioMediaType = "audio/wav"
)

// The terminal codes this path adds. Like the image path's they are CODES, not
// prose: the portal maps a run's error to a localized label, and each names a
// condition a user has to be able to tell apart from an ordinary failure. Each has
// the meaning its image-path namesake has (see chat_runs_images.go), restated for
// speech because the portal's labels for those are worded about images.
const (
	// speechRunDispatchFailedMessage: the run never got its request to the gateway's
	// own /v1/audio/speech at all -- marshalling our own body failed, the request
	// could not be constructed, or the loopback round trip failed outright. The
	// third one's Go error embeds the request URL, so showing it would put an
	// internal address and a transport detail in a portal bubble about a condition
	// no user can act on; the operator's signal is the log line at the call site,
	// which keeps the full Go error. NOT used for a context that ended:
	// runContextOutcome reports a deadline and a cancel distinctly and is checked
	// first at each call site.
	speechRunDispatchFailedMessage = "gateway.chat_run_speech_dispatch_failed"
	// speechRunNoAudioMessage: a 2xx whose body is empty. This is an ERROR, not an
	// empty success -- a turn with no audio would render as a blank bubble the user
	// cannot tell from a bug.
	speechRunNoAudioMessage = "gateway.chat_run_no_audio"
	// speechRunResponseUnreadableMessage: a 2xx the run cannot treat as audio --
	// the body could not be read to its end (a truncated body, a connection the
	// upstream dropped mid-write), or it is not labelled as WAV (an HTML error page,
	// a proxy's own JSON, another audio format). Storing such a body under
	// audio/wav would state a fact nobody measured and keep stating it in the
	// transcript and in every download. A context that ended (deadline/cancel) is
	// NOT this code -- runContextOutcome is checked first.
	speechRunResponseUnreadableMessage = "gateway.chat_run_speech_response_unreadable"
	// speechRunReservedFieldMessage: the thread's advanced passthrough names a field
	// the run itself owns (model, input, stream, response_format; see
	// speechReservedFields). The run fails and sends NOTHING: forwarding the rest
	// without the offending key would answer a request the user did not make, the
	// silent drop every other code here exists to refuse.
	speechRunReservedFieldMessage = "gateway.chat_run_speech_reserved_field"
)

// speechReservedFields are the request fields a thread's passthrough may not set.
// model and input are the run's own (the thread's pinned model and the user's
// text); stream, stream_format and response_format are the three the run's contract
// depends on. The run commits ONE complete audio/wav turn, and it decides whether to
// get it buffered or as an event stream it can read (Server.speechRunStreamRate): it
// states stream and stream_format itself when it streams and states neither when it
// does not, and it states no response_format at all (the endpoint's default is the WAV
// the run commits). A passthrough that set any of them would override the run's own
// value: stream_format=audio would answer raw PCM in a shape the run does not parse, and
// a pcm format a headerless body it cannot commit as a WAV (validateSpeechRequest
// admits wav, pcm or an absent value on a request that does not stream, a pcm answer
// being headerless octet-stream PCM, and pcm or absent on one that does). Rejecting them
// here turns what would be an opaque answer the run cannot use, or worse an override of
// the run's own value, into a refusal that names its cause.
var speechReservedFields = []string{"model", "input", "stream", "stream_format", "response_format"}

// errSpeechPassthroughReserved is what buildSpeechBody returns for a passthrough
// key in speechReservedFields; generateSpeechTurn maps it to
// speechRunReservedFieldMessage and logs the key for the operator.
var errSpeechPassthroughReserved = errors.New("speech passthrough names a reserved field")

// executeAudioRun is executeRun's speech branch: one request to the gateway's own
// /v1/audio/speech and one terminal commit carrying the synthesized audio as the
// assistant turn's structured content.
//
// It calls finishRunWithParts EXACTLY ONCE, on every path, because the work is
// split into a function that only COMPUTES the outcome (generateSpeechTurn,
// below) and this one, which only commits it -- the same shape, and for the same
// reason, as executeImageRun: nothing in the computation can return without an
// outcome, so "one terminal commit per run" is a property of the shape. (A streamed
// answer publishes audio events along the way, but none of that is a commit.)
//
// The deadline is applied HERE, and not by executeRun as it is for the image kind,
// because a speech run has two shapes: a buffered one, which emits nothing between
// dispatch and its finished audio and so is bounded end to end, and a streaming one,
// for which the bound is only the budget for the wait for the first chunk and is
// lifted there (the transcription run's arrangement, see executeTranscriptionRun and
// armRunDeadline). The buffered request never lifts it, so it is bounded to its end
// exactly as before; a stream that delivers a chunk is watched by the gateway relay's
// idle timeout from there on, and one that never does ends at the deadline as a
// timeout, not a cancel.
func (s *Server) executeAudioRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	liftDeadline := armRunDeadline(chatRunKindSpeech, cancel)
	defer liftDeadline()

	parts, status, errMsg := s.generateSpeechTurn(ctx, owner, run, prep, liftDeadline)
	s.finishRunWithParts(ctx, owner, run, status, errMsg, parts)
}

// generateSpeechTurn performs the single loopback request a speech run makes and
// returns the turn's outcome: the content parts to commit, the run status, and the
// terminal error message. It never commits and never finishes the run. A streamed
// answer publishes audio events into the run on the way -- the only thing it does to
// the run before returning -- and calls liftDeadline when the first one arrives; a
// buffered answer publishes nothing.
//
// The request is a POST of the speech body to /v1/audio/speech under the shared
// loopback header set (setRunLoopbackHeaders), which already states
// Content-Type: application/json -- speech is JSON-in, so, unlike the image edit
// hop, nothing overrides it. The body asks for a stream (buildSpeechStreamBody) only
// when the run's gate is open (speechRunStreamRate), and is otherwise exactly the
// buffered body of ADR-063 (buildSpeechBody). Either way the answer is read for what it
// IS: the gateway answers a stream request with a stream only for a route it knows to
// stream, and with the one-shot WAV otherwise (a verdict gone stale), so a 2xx event
// stream is consumed as one (consumeSpeechStream) and anything else is the buffered WAV.
func (s *Server) generateSpeechTurn(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult, liftDeadline func()) (json.RawMessage, string, string) {
	sampleRate, streaming := s.speechRunStreamRate(ctx, owner, prep)
	build := buildSpeechBody
	if streaming {
		build = buildSpeechStreamBody
	}
	body, err := build(prep)
	if err != nil {
		log.Printf("chat run %s: build speech body failed: %v", run.ID, err)
		if errors.Is(err, errSpeechPassthroughReserved) {
			return nil, "error", speechRunReservedFieldMessage
		}
		return nil, "error", speechRunDispatchFailedMessage
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		log.Printf("chat run %s: build speech request failed: %v", run.ID, err)
		return nil, "error", speechRunDispatchFailedMessage
	}
	s.setRunLoopbackHeaders(req, owner, run, prep)

	dispatchedAt := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		// The operator's copy keeps the whole Go error, internal URL and all; the
		// user's copy is the code. See speechRunDispatchFailedMessage.
		log.Printf("chat run %s: speech loopback request failed: %v", run.ID, err)
		return nil, "error", speechRunDispatchFailedMessage
	}
	defer resp.Body.Close()

	// The gateway answered the stream request with an event stream: read it as it
	// comes. (Only a 2xx can be one; anything else, and a one-shot WAV, is read whole
	// below.)
	if streaming && resp.StatusCode >= 200 && resp.StatusCode <= 299 && isSpeechEventStream(resp.Header) {
		return s.consumeSpeechStream(ctx, run, resp, sampleRate, dispatchedAt, liftDeadline)
	}
	return bufferedSpeechTurn(ctx, run, resp)
}

// bufferedSpeechTurn is the outcome of a speech run's loopback answer that is not an
// event stream: the one-shot WAV (or an error answer) read whole, as
// generateSpeechTurn's result -- the content parts to commit, the run status and the
// terminal error message.
func bufferedSpeechTurn(ctx context.Context, run *ChatRun, resp *http.Response) (json.RawMessage, string, string) {
	// Read the WHOLE body: a WAV cannot be played from a prefix of it, and it is
	// deliberately not bounded by s.captureMaxBytes (1 MiB, which an ordinary clip
	// exceeds) -- the same call, and the same backstop, as the image path: the chat
	// store refuses a commit that is too large.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		// Once the status line has arrived, a deadline or a cancel lands here as a
		// read failure; reporting either as a generic error -- or a deadline as a
		// cancel -- would tell the user they pressed Stop when they pressed nothing.
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		log.Printf("chat run %s: speech response unreadable: %v", run.ID, err)
		return nil, "error", speechRunResponseUnreadableMessage
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "error", upstreamErrorCode(payload, resp.Status)
	}

	part, err := speechPartFrom(resp.Header.Get("Content-Type"), payload)
	if err != nil {
		log.Printf("chat run %s: speech response is not usable audio: %v (content type %q, %d bytes)", run.ID, err, resp.Header.Get("Content-Type"), len(payload))
		return nil, "error", err.Error()
	}
	parts, err := json.Marshal([]speechAudioPart{part})
	if err != nil {
		log.Printf("chat run %s: marshal speech parts failed: %v", run.ID, err)
		return nil, "error", speechRunDispatchFailedMessage
	}
	return parts, "completed", ""
}

// isSpeechEventStream reports whether a 2xx answer to the run's stream request is the
// event stream the run asked for, by its Content-Type (the relay passes audio.cpp's own
// through). The one-shot WAV a route that does not stream answers with is not.
func isSpeechEventStream(header http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(header.Get(contentTypeHeader))
	return err == nil && mediaType == speechStreamSSEType
}

// speechPartFrom turns a 2xx speech response into the turn's one audio part, or
// an error whose text is the terminal code. Emptiness is checked first, for the
// reason imagePartsFrom checks "produced nothing" before the media type: "no
// audio at all" is the more fundamental fact, and an empty body should not be
// reported as one whose type could not be named.
func speechPartFrom(contentType string, payload []byte) (speechAudioPart, error) {
	if len(payload) == 0 {
		return speechAudioPart{}, errors.New(speechRunNoAudioMessage)
	}
	if !speechAudioMediaTypeOf(contentType) {
		return speechAudioPart{}, errors.New(speechRunResponseUnreadableMessage)
	}
	return speechAudioPart{
		Type: speechAudioPartType,
		AudioURL: audioURLTarget{
			// Standard, padded base64: the alphabet the chat-blob extraction decodes
			// losslessly and refuses everything else of.
			URL: "data:" + speechAudioMediaType + ";base64," + base64.StdEncoding.EncodeToString(payload),
		},
	}, nil
}

// speechWAVMediaTypes are the media types a server may label a WAV with: the
// registered audio/vnd.wave and the three that are in common use. audio.cpp sends
// audio/wav; the others are accepted so a spelling variant of the same bytes does
// not fail a good run.
var speechWAVMediaTypes = map[string]struct{}{
	"audio/wav":      {},
	"audio/x-wav":    {},
	"audio/wave":     {},
	"audio/vnd.wave": {},
}

// speechAudioMediaTypeOf reports whether a response's Content-Type labels its body
// as WAV. A parameter (charset=...) is ignored and the type is case-insensitive;
// an absent or unparseable header is NOT WAV -- the type is the one thing about
// the audio that must come from the response, and a default would state an
// unmeasured fact (the data: URL's media type, once stored, is stated in every
// later request and download).
func speechAudioMediaTypeOf(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	_, ok := speechWAVMediaTypes[strings.ToLower(mediaType)]
	return ok
}

// buildSpeechBody is the speech endpoint's own request shape: the two fields the
// relay always needs, plus whatever audio settings the thread carries.
//
// model and input are the run's own: the thread's pinned model, and the LAST user
// message's text. response_format is NOT stated -- the endpoint answers WAV when it
// is absent and refuses every other value (validateSpeechRequest), so there is
// nothing to choose and an unset field is omitted, the discipline imageRequestFields
// keeps for every knob. The set first-class settings (voice when non-empty, speed
// when set) come from the thread's ChatAudioSettings; an unset one is omitted so
// audio.cpp stays on its own default rather than being handed an inert zero.
//
// Everything else -- the OpenAI instructions field and any audio.cpp-native knob --
// rides ChatAudioSettings.Passthrough and is merged in as the raw JSON the user
// wrote (the gateway relays the body to audio.cpp unchanged). A key the run owns
// (speechReservedFields, matched ignoring case because the readers of this body
// decode case-insensitively) is REJECTED with errSpeechPassthroughReserved and no
// body is returned. A key that repeats a first-class setting which IS set (voice,
// speed, again ignoring case) is not sent a second time: the first-class control
// is the visible one, so it wins; with it unset, the passthrough key is the only
// statement of it and goes through.
//
// A history with no user text yields an empty input, which is NOT rejected here:
// validateSpeechRequest owns "input is required" (speech.input_required) and a
// second copy of that rule in the executor would be free to drift from it. The
// refusal reaches the run through upstreamErrorCode.
func buildSpeechBody(prep PrepareRunResult) ([]byte, error) {
	fields, err := speechBodyFields(prep)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// speechBodyFields is the body buildSpeechBody and buildSpeechStreamBody share, before it
// is encoded: the fields, or errSpeechPassthroughReserved for a reserved passthrough key.
func speechBodyFields(prep PrepareRunResult) (map[string]any, error) {
	body := map[string]any{
		"model": prep.Settings.Model,
		"input": lastUserPrompt(prep.History),
	}
	audio := prep.Settings.Audio
	if audio == nil {
		return body, nil
	}
	// Sorted, so the key reported on a rejection and the order of the merge are
	// deterministic whatever the map's iteration order.
	passthroughKeys := slices.Sorted(maps.Keys(audio.Passthrough))
	for _, key := range passthroughKeys {
		if isSpeechReservedField(key) {
			return nil, fmt.Errorf("%w: %q", errSpeechPassthroughReserved, key)
		}
	}
	if audio.Voice != "" {
		body["voice"] = audio.Voice
	}
	if audio.Speed != nil {
		body["speed"] = *audio.Speed
	}
	for _, key := range passthroughKeys {
		if hasKeyFold(body, key) {
			log.Printf("speech passthrough key %q repeats a setting the thread already carries; the setting wins", key)
			continue
		}
		body[key] = audio.Passthrough[key]
	}
	return body, nil
}

// isSpeechReservedField reports whether key names a field in speechReservedFields,
// ignoring case.
func isSpeechReservedField(key string) bool {
	return slices.ContainsFunc(speechReservedFields, func(reserved string) bool {
		return strings.EqualFold(key, reserved)
	})
}

// hasKeyFold reports whether the body already holds key, ignoring case.
func hasKeyFold(body map[string]any, key string) bool {
	for existing := range body {
		if strings.EqualFold(existing, key) {
			return true
		}
	}
	return false
}
