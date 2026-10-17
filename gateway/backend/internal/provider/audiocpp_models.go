// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"op-ai-gateway/internal/routing"
	"strings"
)

// audiocppModelsPath is audio.cpp's OpenAI-shaped model listing. It is fixed by
// the server, not configured per application, and it is the ONLY place audio.cpp
// states what a model can do: there is no separate capability document like
// sd-server's /sdcpp/v1/capabilities, so the capability signal is the task
// field of each listed model.
const audiocppModelsPath = "/v1/models"

// audio.cpp task values. The listing's task names what the listed model DOES:
// "tts" synthesises speech from text and "clone" synthesises it in a reference
// voice, so both are a speech "yes"; "asr" transcribes speech to text, so it is
// a transcription "yes". Every other task (the music tasks, ...) is a model that
// does neither. One task answers BOTH capabilities at once: a model is never
// both a synthesiser and a transcriber.
const (
	audioCppTaskTTS   = "tts"
	audioCppTaskClone = "clone"
	audioCppTaskASR   = "asr"
)

// AudioCppVerdicts is what one audio.cpp /v1/models listing says about ONE
// model, one verdict per capability the listing can answer.
//
// Speech and Transcription are each routing.CapabilityYes,
// routing.CapabilityNo, or "" for "no answer". Both are BIDIRECTIONAL: the
// listing states each listed model's task, so a model it names with a task that
// does not serve the capability is a real "no", like sdcpp's exhaustive
// supported_modes and unlike Ollama's capability array. The two verdicts are
// decided together from the one task, so they are either both set or both "".
// "" is reserved for everything the listing does not say: the model is not in
// it (the listing was first taken to name LOADED models only, but audio.cpp
// v0.9.1 also names an unloaded one with loaded:false, so a missing entry says
// only that the server does not name the model), the listing is empty, it
// cannot be read, or the named entry carries no task. See routing.CapabilitySourceAudioCppModels.
//
// The verdict is the capability axis and is decided from the task alone. Whether
// the model is loaded RIGHT NOW is the health axis: an asr model the server
// lists as unloaded still carries task "asr", and stays a transcription "yes".
type AudioCppVerdicts struct {
	Speech        string
	Transcription string
}

// AudioCppModelsProber is an optional provider capability: it GETs
// target.Endpoint + /v1/models and derives the speech and transcription
// verdicts the listing states for modelID. A transport failure or a non-2xx
// status is an error and carries no verdict, so the caller writes nothing. A 2xx
// body that does not name the model, or cannot be read, is NOT an error, only
// empty verdicts -- the caller writes nothing for an empty verdict either.
//
// It is a client method, like ProbeSdcppCapabilities, and not a free function,
// because the request must ride the client's own *http.Client: cmd/gateway
// builds that one on the outbound app transport, which carries the mesh routing
// and TLS settings a separate client would bypass.
type AudioCppModelsProber interface {
	ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (AudioCppVerdicts, error)
}

var _ AudioCppModelsProber = (*OpenAICompatibleClient)(nil)

// ProbeAudioCppModels reads audio.cpp's model listing through this client's
// transport. See AudioCppModelsProber.
func (c *OpenAICompatibleClient) ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (AudioCppVerdicts, error) {
	return fetchAudioCppModels(ctx, c.http, target, modelID)
}

// fetchAudioCppModels is the GET behind ProbeAudioCppModels. Modelled on
// fetchSdcppDoc: it honours target.Timeout, attaches the per-application
// upstream credential, and bounds the body at 1 MiB. A transport failure or a
// non-2xx status is an ErrUnavailable; decoding is the tolerant
// parseAudioCppModels.
func fetchAudioCppModels(ctx context.Context, httpClient *http.Client, target routing.Target, modelID string) (AudioCppVerdicts, error) {
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(target.Endpoint, audiocppModelsPath), nil)
	if err != nil {
		return AudioCppVerdicts{}, fmt.Errorf("%w: create request: %v", ErrUnavailable, err)
	}
	applyUpstreamAuth(ctx, req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return AudioCppVerdicts{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AudioCppVerdicts{}, unavailableStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return AudioCppVerdicts{}, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	return parseAudioCppModels(body, modelID), nil
}

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

// parseAudioCppModels derives the speech and transcription verdicts for modelID
// from a /v1/models body. It is TOLERANT, unlike the strict sdcpp document
// parser: a body that is not the listing, or an entry of the wrong shape, yields
// the empty verdict rather than an error, so a probe never breaks over an
// unexpected shape.
//
// The rule, with the loaded entry's task authoritative:
//   - the entry whose id is modelID, with task "tts" or "clone" ->
//     speech "yes", transcription "no"
//   - the entry whose id is modelID, with task "asr" ->
//     speech "no", transcription "yes"
//   - the entry whose id is modelID, with any other non-empty task ->
//     both "no"
//   - the entry whose id is modelID, with no task -> both "" (an absent field
//     is not a statement, and a "no" can switch the model's route off)
//   - no entry for modelID, an empty data list, or an unreadable body -> both ""
//
// Only id and task are read: loaded (and family, mode, path) is deliberately
// not decoded. Whether a model is loaded is the health axis, not the capability
// axis, so the server listing an asr model with loaded:false does not turn its
// transcription "yes" into anything else.
//
// The id match is exact. The task match ignores case and surrounding
// whitespace: a "TTS" or "ASR" the server might one day emit must not fall to
// the "no" default, which is a bidirectional verdict that switches a route off.
// The first entry that carries the id answers.
func parseAudioCppModels(body []byte, modelID string) AudioCppVerdicts {
	if modelID == "" {
		return AudioCppVerdicts{}
	}
	var doc audioCppModelsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return AudioCppVerdicts{}
	}
	for _, entry := range doc.Data {
		if entry.ID != modelID {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(entry.Task)) {
		case "":
			return AudioCppVerdicts{}
		case audioCppTaskTTS, audioCppTaskClone:
			return AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo}
		case audioCppTaskASR:
			return AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes}
		default:
			return AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityNo}
		}
	}
	return AudioCppVerdicts{}
}
