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
// is the task field of each listed model.
const AudioCppModelsPath = "/v1/models"

// audio.cpp task values. The listing's task names what the listed model DOES:
// "tts" synthesises speech from text and "clone" synthesises it in a reference
// voice, so both are a speech "yes"; "asr" transcribes speech to text, so it is
// a transcription "yes". Every other task (the music tasks, ...) is a model
// that does neither.
const (
	audioCppTaskTTS   = "tts"
	audioCppTaskClone = "clone"
	audioCppTaskASR   = "asr"
)

// audioCppModelsDoc is the part of the listing this code reads: each entry's id
// and task. The rest of an entry (object, owned_by, family, mode, loaded, path)
// answers no capability question and is left undecoded, so an unknown or added
// field never breaks the read.
type audioCppModelsDoc struct {
	Data []struct {
		ID   string `json:"id"`
		Task string `json:"task"`
	} `json:"data"`
}

// parseAudioCppModels is a DUPLICATE, on purpose, of the gateway's
// provider.parseAudioCppModels (gateway/backend/internal/provider/
// audiocpp_models.go): the two Go modules share no package. The two copies
// must decide identically on identical input, and audioCppTaskCases pins both.
// Change them together.
//
// It derives the speech and transcription verdicts for model from a
// /v1/models body, each "yes", "no" or "" (no answer). It is TOLERANT: a body
// that is not the listing, or an entry of the wrong shape, yields no verdict
// rather than an error. The rule:
//   - the entry whose id is model, with task "tts" or "clone" ->
//     speech "yes", transcription "no"
//   - the entry whose id is model, with task "asr" ->
//     speech "no", transcription "yes"
//   - the entry whose id is model, with any other non-empty task -> both "no"
//   - the entry whose id is model, with no task -> both "" (an absent field is
//     not a statement, and a "no" can switch the model's route off)
//   - no entry for model, an empty data list, or an unreadable body -> both ""
//
// The id match is exact. The task match ignores case and surrounding
// whitespace, so a "TTS" or "ASR" the server might one day emit does not fall
// to the "no" default. The first entry that carries the id answers. Whether the
// model is loaded is the health axis and is deliberately not decoded.
func parseAudioCppModels(body []byte, model string) (speech, transcription string) {
	if model == "" {
		return "", ""
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", ""
	}
	for _, entry := range doc.Data {
		if entry.ID != model {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(entry.Task)) {
		case "":
			return "", ""
		case audioCppTaskTTS, audioCppTaskClone:
			return "yes", "no"
		case audioCppTaskASR:
			return "no", "yes"
		default:
			return "no", "no"
		}
	}
	return "", ""
}

// ProbeAudioCppVerdicts GETs baseURL+AudioCppModelsPath and returns the speech
// and transcription verdicts the listing states for model, in the same
// PropsVerdicts shape and under the same stable/transient contract as
// ProbeSdcppVerdicts, ProbePropsVerdicts and ProbeOllamaVerdicts:
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
	speech, transcription := parseAudioCppModels(body, model)
	return PropsVerdicts{Caps: Capabilities{Speech: speech, Transcription: transcription}}, true
}
