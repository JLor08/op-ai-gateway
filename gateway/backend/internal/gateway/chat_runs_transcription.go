// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"slices"
	"strconv"
	"strings"
)

// This file is the transcription kind's half of the run executor. It is the speech
// kind's (chat_runs_audio.go) mirror image and the image edit's (chat_runs_images.go)
// near twin: it shares the reservation, the deadline, the header set and the single
// terminal commit with the text half (chat_runs.go) and nothing else, for the reason
// both of them give -- consumeRunStream assumes a stream of deltas, and
// /v1/audio/transcriptions answers ONE buffered JSON body, so there is nothing to
// checkpoint and no rate that could be measured rather than invented.
//
// What it takes from each: the request is the edit's -- multipart, the clip as a file
// part, a Content-Type that carries the writer's boundary and so replaces the shared
// header set's -- and the settings merge is speech's: the first-class knobs plus a
// raw-JSON passthrough, with the keys the run owns refused. The turn is speech's
// opposite: the input is audio and the output is text, the answer's `text` committed
// as an ordinary {"type":"text"} part, so nothing downstream (the history that
// carries the turn forward, the renderer) learns a new shape.

// transcriptionTextPart is THE content part a transcription turn emits:
//
//	{"type":"text","text":"<the transcript>"}
//
// the ordinary text part, `type` first. A declared type rather than a map literal so
// the emitted key ORDER is the one the shape is documented in.
type transcriptionTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// transcriptionResponse is what the run reads of the endpoint's 2xx answer, the
// document audio.cpp sends -- {"text":"...","timing":{...}}. Only the transcript is
// read: the timing is the relay's business (it meters the billed audio duration from
// it) and nothing in a chat turn is made of it.
type transcriptionResponse struct {
	Text string `json:"text"`
}

// The terminal codes this path adds. Like the image and speech paths' they are CODES,
// not prose: the portal maps a run's error to a localized label, and each names a
// condition a user has to be able to tell apart from an ordinary failure.
const (
	// transcriptionRunDispatchFailedMessage: the run never got its request to the
	// gateway's own /v1/audio/transcriptions at all -- building our own form failed
	// (a passthrough entry that cannot be written into it, a clip of a type with no
	// file name), the request could not be constructed, or the loopback round trip
	// failed outright. The third one's Go error embeds the request URL, so showing it
	// would put an internal address and a transport detail in a portal bubble about a
	// condition no user can act on; the operator's signal is the log line at the call
	// site, which keeps the full Go error. NOT used for a context that ended:
	// runContextOutcome reports a deadline and a cancel distinctly and is checked
	// first at each call site.
	transcriptionRunDispatchFailedMessage = "gateway.chat_run_transcription_dispatch_failed"
	// transcriptionRunSourceUnreadableMessage: the turn's clip cannot be turned into
	// bytes this run may send -- the last user message carries no audio part, or more
	// than one, or one that is an external link (a run never fetches a url), a gateway
	// reference that was not resolved to bytes, or a data: url that does not decode as
	// one of the stored audio types. The run fails and sends NOTHING: transcribing a
	// different clip than the user attached, or none, would answer a request the user
	// did not make, which is the silent drop every other code here exists to refuse.
	// (A clip the chat simply does not hold never gets this far: the run is refused at
	// its start, see portal.Service.resolveTranscriptionRunSource.)
	transcriptionRunSourceUnreadableMessage = "gateway.chat_run_transcription_source_unreadable"
	// transcriptionRunResponseUnreadableMessage: a 2xx the run cannot treat as the
	// transcription's JSON -- the body could not be read to its end (a truncated body,
	// a connection the upstream dropped mid-write), or it is not the {"text":...}
	// document (an HTML error page, a proxy's own body, a JSON of another shape). A
	// context that ended (deadline/cancel) is NOT this code -- runContextOutcome is
	// checked first.
	transcriptionRunResponseUnreadableMessage = "gateway.chat_run_transcription_response_unreadable"
	// transcriptionRunNoTranscriptMessage: a 2xx that is the right document but holds
	// no text -- the clip produced nothing (silence, noise, a model that heard no
	// speech). This is an ERROR, not an empty success: a turn with no text would render
	// as a blank bubble the user cannot tell from a bug, and "nothing was heard" is a
	// fact they can act on (record again), where an unreadable answer is not.
	transcriptionRunNoTranscriptMessage = "gateway.chat_run_transcription_no_transcript"
	// transcriptionRunReservedFieldMessage: the thread's advanced passthrough names a
	// field the run itself owns (model, file, response_format, stream; see
	// transcriptionReservedFields). The run fails and sends NOTHING: forwarding the
	// rest without the offending key would answer a request the user did not make, the
	// silent drop every other code here exists to refuse.
	transcriptionRunReservedFieldMessage = "gateway.chat_run_transcription_reserved_field"
)

// transcriptionReservedFields are the form fields a thread's passthrough may not
// set. model and file are the run's own (the thread's pinned model and the clip of
// the turn); stream and response_format are the two the endpoint's contract -- one
// complete JSON answer -- depends on, and it refuses a streamed request or any
// format but json anyway (validateTranscriptionRequest). Rejecting them here turns
// what would be an opaque upstream refusal, or worse an override of the run's own
// value, into a refusal that names its cause.
//
// stream_format is deliberately not listed, for speechReservedFields' reason: the
// endpoint already refuses any non-empty one with transcription.stream_unsupported,
// which the run surfaces as that code.
var transcriptionReservedFields = []string{"model", "file", "response_format", "stream"}

// errTranscriptionPassthroughReserved is what buildTranscriptionBody returns for a
// passthrough key in transcriptionReservedFields; generateTranscriptionTurn maps it
// to transcriptionRunReservedFieldMessage and logs the key for the operator.
var errTranscriptionPassthroughReserved = errors.New("transcription passthrough names a reserved field")

// transcriptionFilenames is the file name each stored audio type is sent under: the
// upstream reads a part's file name, and the extension is what a server that sniffs
// by name falls back on (audio.cpp picks its decoder by it). The keys are exactly the
// audio allow-list portal.DecodeAudioDataURL enforces, so no other media type -- and
// nothing the url itself chose -- ever reaches a part header.
var transcriptionFilenames = map[string]string{
	"audio/wav":  "audio.wav",
	"audio/webm": "audio.webm",
	"audio/mpeg": "audio.mp3",
	"audio/mp4":  "audio.m4a",
	"audio/ogg":  "audio.ogg",
}

// executeTranscriptionRun is executeRun's transcription branch: one buffered
// multipart request to the gateway's own /v1/audio/transcriptions and one terminal
// commit carrying the transcript as the assistant turn's structured content.
//
// It calls finishRunWithParts EXACTLY ONCE, on every path, because the work is split
// into a function that only COMPUTES the outcome (generateTranscriptionTurn, below)
// and this one, which only commits it -- the same shape, and for the same reason, as
// executeAudioRun and executeImageRun: nothing in the computation can return without
// an outcome, so "one terminal commit per run" is a property of the shape.
//
// The deadline is NOT applied here: executeRun already derived the bounded context
// from the run's kind before branching (see runDeadlineFor).
func (s *Server) executeTranscriptionRun(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) {
	parts, status, errMsg := s.generateTranscriptionTurn(ctx, owner, run, prep)
	s.finishRunWithParts(ctx, owner, run, status, errMsg, parts)
}

// generateTranscriptionTurn performs the single loopback request a transcription run
// makes and returns the turn's outcome: the content parts to commit, the run status,
// and the terminal error message. It never commits, never finishes the run and never
// publishes a delta -- there are none to publish.
//
// The request is a POST of buildTranscriptionBody to /v1/audio/transcriptions under
// the shared loopback header set (setRunLoopbackHeaders), whose Content-Type states
// application/json: transcription is multipart-in, so that one header is replaced
// afterwards by the form's own, which carries its boundary -- the edit hop's
// override (generateImageTurn).
//
// A turn whose clip is unusable fails before anything is built or sent, and a
// reserved passthrough key fails before anything is sent: neither is retried or
// repaired into a request the user did not make.
func (s *Server) generateTranscriptionTurn(ctx context.Context, owner auth.Token, run *ChatRun, prep PrepareRunResult) (json.RawMessage, string, string) {
	mediaType, audio, err := transcriptionInputOf(prep.History)
	if err != nil {
		// The operator's copy says why; the user's is the code. Nothing about the clip
		// itself is logged -- a data: url is the recording.
		log.Printf("chat run %s: the transcription's clip is unusable: %v", run.ID, err)
		return nil, "error", transcriptionRunSourceUnreadableMessage
	}
	body, contentType, err := buildTranscriptionBody(prep, mediaType, audio)
	if err != nil {
		log.Printf("chat run %s: build transcription body failed: %v", run.ID, err)
		if errors.Is(err, errTranscriptionPassthroughReserved) {
			return nil, "error", transcriptionRunReservedFieldMessage
		}
		return nil, "error", transcriptionRunDispatchFailedMessage
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.selfBaseURL+"/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		log.Printf("chat run %s: build transcription request failed: %v", run.ID, err)
		return nil, "error", transcriptionRunDispatchFailedMessage
	}
	s.setRunLoopbackHeaders(req, owner, run, prep)
	// After the shared helper, which sets application/json: a multipart type carries
	// the boundary of the writer that built the body, which only that writer knows.
	req.Header.Set(contentTypeHeader, contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		// The operator's copy keeps the whole Go error, internal URL and all; the
		// user's copy is the code. See transcriptionRunDispatchFailedMessage.
		log.Printf("chat run %s: transcription loopback request failed: %v", run.ID, err)
		return nil, "error", transcriptionRunDispatchFailedMessage
	}
	defer resp.Body.Close()

	// Read the WHOLE body: the answer is one JSON document and cannot be decoded from
	// a prefix of it. Deliberately not bounded by s.captureMaxBytes, the same call the
	// image and speech paths make; a transcript is small, so the chat store's own
	// content cap, which refuses an oversized commit, is the backstop on an absurd one.
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		// Once the status line has arrived, a deadline or a cancel lands here as a
		// read failure; reporting either as a generic error -- or a deadline as a
		// cancel -- would tell the user they pressed Stop when they pressed nothing.
		if status, msg, ended := runContextOutcome(ctx); ended {
			return nil, status, msg
		}
		log.Printf("chat run %s: transcription response unreadable: %v", run.ID, err)
		return nil, "error", transcriptionRunResponseUnreadableMessage
	}
	return transcriptionTurnFromResponse(run, resp, payload)
}

// transcriptionTurnFromResponse turns the endpoint's fully read answer into the
// turn's outcome (parts, status, terminal message), the same triple
// generateTranscriptionTurn returns: a non-2xx answer is the upstream's own error
// code, a 2xx that is not the {"text":...} document is "response unreadable", one
// that holds no transcript is "no transcript", and anything else is the transcript
// as one ordinary text part. It is split out of generateTranscriptionTurn, which
// keeps the request and the read, the way speechPartFrom is split out of
// generateSpeechTurn.
func transcriptionTurnFromResponse(run *ChatRun, resp *http.Response, payload []byte) (json.RawMessage, string, string) {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "error", upstreamErrorCode(payload, resp.Status)
	}

	// A pointer, so that a body of JSON null -- which decodes into a struct without
	// error and without touching it -- is the "not that document" it is, not an
	// empty transcript.
	var decoded *transcriptionResponse
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded == nil {
		// Neither the body nor the error is logged: a body that is nearly the
		// document may hold the transcript, and the decoder's error can quote it.
		log.Printf("chat run %s: transcription response is not the {\"text\":...} document (status %s, content type %q, %d bytes)", run.ID, resp.Status, resp.Header.Get(contentTypeHeader), len(payload))
		return nil, "error", transcriptionRunResponseUnreadableMessage
	}
	if strings.TrimSpace(decoded.Text) == "" {
		return nil, "error", transcriptionRunNoTranscriptMessage
	}
	parts, err := json.Marshal([]transcriptionTextPart{{Type: "text", Text: decoded.Text}})
	if err != nil {
		log.Printf("chat run %s: marshal transcription parts failed: %v", run.ID, err)
		return nil, "error", transcriptionRunDispatchFailedMessage
	}
	return parts, "completed", ""
}

// transcriptionInputOf reads the clip of the LAST user message in the prepared
// history: the media type and the decoded bytes of its one audio part. Only the last
// user message counts -- an earlier clip was an earlier turn's, and none of them is
// sent anywhere (imageEditInputsOf's rule for images).
//
// The message must hold EXACTLY ONE audio part and it must be readable as a data:
// url of a stored audio type (portal.DecodeAudioDataURL, the same definition the run
// commit stores a clip by); anything else fails the call, as imageEditInputsOf's
// refusals do for an edit. A second clip is refused rather than chosen between
// (the endpoint reads a single file, and picking one of two would be a guess about
// which the user meant), and an unreadable part is refused rather than skipped (a
// transcript of the clip that happened to be readable would answer a different
// request). By the time the executor runs, a stored reference has already been
// resolved to such a url (portal.Service.PrepareChatRun,
// resolveTranscriptionRunSource), so what is left that is not one is an external
// link or an unresolved reference, and neither is ever fetched or sent.
//
// The parts are found by portal.MessageAudio, the one definition of an audio part,
// so this cannot count a part the resolution did not see.
func transcriptionInputOf(history []portal.ChatAPIMessage) (string, []byte, error) {
	message, ok := lastUserMessage(history)
	if !ok {
		return "", nil, errors.New("the history has no user message")
	}
	urls := portal.MessageAudio(message.Content)
	if len(urls) != 1 {
		return "", nil, fmt.Errorf("the last user message has %d audio parts, a transcription takes one", len(urls))
	}
	mediaType, audio, err := portal.DecodeAudioDataURL(urls[0])
	if err != nil {
		// The url is logged by its scheme only: a data: url is the clip itself.
		return "", nil, fmt.Errorf("the audio part (%s): %w", urlKind(urls[0]), err)
	}
	return mediaType, audio, nil
}

// buildTranscriptionBody is the transcription endpoint's own request: a
// multipart/form-data body of the run's form fields and the clip as the `file`
// part. It returns the body and the Content-Type to send it under (which carries the
// boundary).
//
// model is the run's own: the thread's pinned model. response_format is NOT stated --
// the endpoint answers JSON when it is absent and refuses every other value
// (validateTranscriptionRequest, ADR-062 g), so there is nothing to choose and an
// unset field is omitted, the discipline buildSpeechBody keeps. The set first-class
// settings (language and prompt when non-empty, temperature when set, in its shortest
// plain decimal form) come from the thread's ChatTranscriptionSettings; an unset one
// is omitted so audio.cpp stays on its own default rather than being handed an inert
// zero or an empty string.
//
// Everything else -- the OpenAI timestamp granularities and any audio.cpp-native knob
// -- rides ChatTranscriptionSettings.Passthrough and is merged in as additional form
// fields. Its values are raw JSON and a form field is text, so each is written as its
// natural text: a JSON string as the string itself (not quoted), a number or a bool
// as its literal, and an object, an array or null as its compact JSON. A key the run
// owns (transcriptionReservedFields, matched ignoring case, as buildSpeechBody
// does) is REJECTED with errTranscriptionPassthroughReserved and no body is returned.
// A key that repeats a first-class setting which IS set (language, prompt,
// temperature, again ignoring case) is not sent a second time -- the endpoint refuses
// a repeated field, and the first-class control is the visible one, so it wins; with
// it unset, the passthrough key is the only statement of it and goes through. A key
// that cannot be a form field name (empty, or holding a control character -- a line
// break in it would end the part's header and begin another) and a value that is not
// JSON fail the build, so nothing malformed is ever sent.
//
// The fields are written in sorted order, ahead of the clip, so the body is
// deterministic. The clip is named for its media type (transcriptionFilenames); a
// type with no name is an error rather than a guess.
func buildTranscriptionBody(prep PrepareRunResult, mediaType string, audio []byte) ([]byte, string, error) {
	filename, ok := transcriptionFilenames[mediaType]
	if !ok {
		return nil, "", fmt.Errorf("no file name for media type %q", mediaType)
	}
	fields := map[string]any{"model": prep.Settings.Model}
	if settings := prep.Settings.Transcription; settings != nil {
		if err := mergeTranscriptionSettings(fields, settings); err != nil {
			return nil, "", err
		}
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if err := mw.WriteField(name, fmt.Sprint(fields[name])); err != nil {
			return nil, "", err
		}
	}
	if err := writeTranscriptionFilePart(mw, filename, mediaType, audio); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// mergeTranscriptionSettings adds the thread's transcription settings to fields: the
// set first-class ones, then the passthrough (see buildTranscriptionBody for the
// rules). The passthrough keys are checked against the reserved set before anything
// is added, so a rejection leaves nothing half-merged.
func mergeTranscriptionSettings(fields map[string]any, settings *portal.ChatTranscriptionSettings) error {
	// Sorted, so the key reported on a rejection and the order of the merge are
	// deterministic whatever the map's iteration order.
	passthroughKeys := slices.Sorted(maps.Keys(settings.Passthrough))
	for _, key := range passthroughKeys {
		if isTranscriptionReservedField(key) {
			return fmt.Errorf("%w: %q", errTranscriptionPassthroughReserved, key)
		}
	}
	if settings.Language != "" {
		fields["language"] = settings.Language
	}
	if settings.Prompt != "" {
		fields["prompt"] = settings.Prompt
	}
	if settings.Temperature != nil {
		// 'f' with the shortest precision: a plain decimal ("0.00001", never "1e-05"),
		// the spelling every form reader takes.
		fields["temperature"] = strconv.FormatFloat(*settings.Temperature, 'f', -1, 64)
	}
	for _, key := range passthroughKeys {
		if !isTranscriptionFieldName(key) {
			return fmt.Errorf("passthrough key %q cannot be a form field name", key)
		}
		if hasKeyFold(fields, key) {
			log.Printf("transcription passthrough key %q repeats a setting the thread already carries; the setting wins", key)
			continue
		}
		value, err := transcriptionFormValue(settings.Passthrough[key])
		if err != nil {
			return fmt.Errorf("passthrough value of %q is not JSON: %w", key, err)
		}
		fields[key] = value
	}
	return nil
}

// transcriptionFormValue is the text a raw-JSON passthrough value is written as in
// the form: a JSON string as the string itself, anything else as its compact JSON
// (which for a number or a bool is exactly its literal).
func transcriptionFormValue(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", err
	}
	return compact.String(), nil
}

// isTranscriptionReservedField reports whether key names a field in
// transcriptionReservedFields, ignoring case.
func isTranscriptionReservedField(key string) bool {
	return slices.ContainsFunc(transcriptionReservedFields, func(reserved string) bool {
		return strings.EqualFold(key, reserved)
	})
}

// isTranscriptionFieldName reports whether key can be written as a form field name:
// non-empty and free of control characters. (A quote or a backslash is escaped by the
// writer; a CR or LF would not be, and would split the part header.)
func isTranscriptionFieldName(key string) bool {
	return key != "" && !strings.ContainsFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// writeTranscriptionFilePart writes the clip as the `file` part of the form.
func writeTranscriptionFilePart(mw *multipart.Writer, filename, mediaType string, audio []byte) error {
	header := make(textproto.MIMEHeader)
	// The canonical spelling every client sends -- quoted values, name first -- and
	// not mime.FormatMediaType's, for the reason writeImageFilePart gives: the
	// upstream's form parser (cpp-httplib) matches this header with a pattern that
	// wants exactly `form-data; name="..."; filename="..."`. Both values are
	// constants (the filename comes from transcriptionFilenames), so there is nothing
	// to escape.
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	header.Set(contentTypeHeader, mediaType)
	part, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(audio)
	return err
}
