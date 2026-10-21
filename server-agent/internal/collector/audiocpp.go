// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// AudioCppModelsPath is audio.cpp's OpenAI-shaped model listing. It is the ONLY
// place audio.cpp states what a model can do: there is no separate capability
// document like sd-server's /sdcpp/v1/capabilities, so the capability signal
// is the task field of each listed model and, for an asr model, its mode.
const AudioCppModelsPath = "/v1/models"

// audio.cpp task values. The listing's task names what the listed model DOES:
// "tts" synthesises speech from text and "clone" synthesises it in a reference
// voice, so both are a speech "yes"; "asr" transcribes speech to text, so it is
// a transcription "yes". Every other task (the music tasks, ...) is a model
// that does neither. The speech_clone verdict is a task answer too, and the only
// one that tells "tts" from "clone".
const (
	audioCppTaskTTS   = "tts"
	audioCppTaskClone = "clone"
	audioCppTaskASR   = "asr"
)

// audio.cpp mode values. The listing's mode names how the model is LOADED:
// "streaming" is a model that serves a streamed transcription (stream=true), and
// "offline" one that answers only a one-shot request and refuses stream=true.
// Any other value is no statement this code can act on. The mode is read for an
// ASR model only (parseAudioCppModels): it says how a TRANSCRIBER streams.
const (
	audioCppModeStreaming = "streaming"
	audioCppModeOffline   = "offline"
)

// audioCppModelsDoc is the part of the listing this code reads: each entry's id,
// task and mode. The rest of an entry (object, owned_by, family, loaded, path)
// answers no capability question and is left undecoded, so an unknown or added
// field never breaks the read.
//
// Mode is held raw and decoded on its own (audioCppStreamingVerdict), and only
// for an asr entry: a mode of a type this code does not expect must cost only
// the streaming verdict, never the speech and transcription verdicts the task
// decides, and a typed string field would fail the whole document's decode on
// one odd mode.
type audioCppModelsDoc struct {
	Data []struct {
		ID   string          `json:"id"`
		Task string          `json:"task"`
		Mode json.RawMessage `json:"mode"`
	} `json:"data"`
}

// parseAudioCppModels is a DUPLICATE, on purpose, of the gateway's
// provider.parseAudioCppModels (gateway/backend/internal/provider/
// audiocpp_models.go): the two Go modules share no package. The two copies
// must decide identically on identical input, and audioCppModelCases pins
// both (the gateway's TestAudioCppModelsCasesMatchTheAgentsTable holds the
// two copies of that table byte-identical). Change them together.
//
// It derives the speech, transcription, transcription streaming and speech clone
// verdicts for model from a /v1/models body, each "yes", "no" or "" (no answer).
// It is
// TOLERANT: a body that is not the listing, or an entry of the wrong shape,
// yields no verdict rather than an error. The rule:
//   - the entry whose id is model, with task "tts" or "clone" ->
//     speech "yes", transcription "no"
//   - the entry whose id is model, with task "asr" ->
//     speech "no", transcription "yes"
//   - the entry whose id is model, with any other non-empty task -> both "no"
//   - the entry whose id is model, with no task -> both "" (an absent field is
//     not a statement, and a "no" can switch the model's route off)
//   - no entry for model, an empty data list, or an unreadable body -> both ""
//
// The streaming verdict is decided beside those, for an ASR model only, from
// the entry's mode:
//   - the entry whose id is model, with task "asr" and mode "streaming" ->
//     streaming "yes"
//   - the entry whose id is model, with task "asr" and mode "offline" ->
//     streaming "no"
//   - the entry whose id is model, with task "asr" and no mode, an empty, blank
//     or unknown one, or one that is not a string -> streaming ""
//   - the entry whose id is model, with any other task (tts, clone, music, an
//     unknown one) or none -> streaming "", WHATEVER its mode: "transcription
//     streaming" is the transcriber's capability, so a model that does not
//     transcribe is no "no" on it either
//   - no entry for model, an empty data list, or an unreadable body ->
//     streaming ""
//
// A mode of an unexpected type leaves the task's verdicts alone: it costs only
// the streaming verdict.
//
// The speech_clone verdict is decided beside those, for the two speech tasks
// only, from the entry's task alone: it refines speech by saying whether the
// model synthesises only in a reference voice (which a text-only caller cannot
// supply), and gates no route:
//   - the entry whose id is model, with task "tts" -> speech_clone "no"
//   - the entry whose id is model, with task "clone" -> speech_clone "yes"
//   - the entry whose id is model, with any other task (asr, music, an unknown
//     one) or none -> speech_clone "", WHATEVER its mode: "speech clone" refines
//     speech, so a model that does not synthesise speech is no "no" on it either
//   - no entry for model, an empty data list, or an unreadable body ->
//     speech_clone ""
//
// The id match is exact. The task and mode matches ignore case and surrounding
// whitespace, so a "TTS" or "ASR" the server might one day emit does not fall
// to the "no" default. The first entry that carries the id answers. Whether the
// model is loaded is the health axis and is deliberately not decoded.
func parseAudioCppModels(body []byte, model string) (speech, transcription, streaming, speechClone string) {
	if model == "" {
		return "", "", "", ""
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", "", ""
	}
	for _, entry := range doc.Data {
		if entry.ID != model {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(entry.Task)) {
		case "":
			return "", "", "", ""
		case audioCppTaskTTS:
			return "yes", "no", "", "no"
		case audioCppTaskClone:
			return "yes", "no", "", "yes"
		case audioCppTaskASR:
			return "no", "yes", audioCppStreamingVerdict(entry.Mode), ""
		default:
			return "no", "no", "", ""
		}
	}
	return "", "", "", ""
}

// audioCppStreamingVerdict maps one ASR entry's raw mode onto the transcription
// streaming verdict (the caller asks it for an asr entry only): "streaming" is
// "yes" and "offline" "no", matched ignoring case and surrounding whitespace,
// and everything else is "". Everything else covers an absent field, a null, an
// empty or blank string, a mode this code does not know (a "no" is only ever
// the server's own word for a model that cannot stream, never a guess), and a
// value that is not a string at all.
func audioCppStreamingVerdict(raw json.RawMessage) string {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case audioCppModeStreaming:
		return "yes"
	case audioCppModeOffline:
		return "no"
	default:
		return ""
	}
}

// ProbeAudioCppVerdicts GETs baseURL+AudioCppModelsPath and returns the speech,
// transcription, transcription streaming and speech clone verdicts the listing
// states for model, in the same PropsVerdicts shape and under the same stable/transient
// contract as ProbeSdcppVerdicts, ProbePropsVerdicts and ProbeOllamaVerdicts:
//   - 404/401/403/405 are fixed properties of the running binary, so they
//     are conclusive (stable, no verdict);
//   - no response and every other non-2xx (5xx above all) are transient;
//   - a body that is not syntactically JSON reads as a child still
//     mid-response, so it is transient;
//   - a JSON body that does not name model, names it with no task, or has a
//     shape the rule cannot read is conclusive with no verdict, because the
//     same build will answer the same way.
//
// LiveProgress is always "" and ImageOptions always nil: audio.cpp has no
// timings surface and no image options.
func ProbeAudioCppVerdicts(ctx context.Context, client *http.Client, baseURL, model string) (PropsVerdicts, bool) {
	body, status, err := fetchProbeBody(ctx, client, baseURL, AudioCppModelsPath)
	if err != nil {
		stable := status == http.StatusNotFound ||
			status == http.StatusUnauthorized ||
			status == http.StatusForbidden ||
			status == http.StatusMethodNotAllowed
		return PropsVerdicts{}, stable
	}
	if !json.Valid(body) {
		return PropsVerdicts{}, false
	}
	speech, transcription, streaming, speechClone := parseAudioCppModels(body, model)
	return PropsVerdicts{Caps: Capabilities{
		Speech:                 speech,
		Transcription:          transcription,
		TranscriptionStreaming: streaming,
		SpeechClone:            speechClone,
	}}, true
}
